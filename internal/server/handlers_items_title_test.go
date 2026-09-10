package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// HTTP-layer regressions for item title validation (BUG-2833 empty,
// BUG-2831 length).
//
// These sit alongside the store-level tests in internal/store rather than
// duplicating them, and they assert something the store tests cannot: the
// STATUS CODE. A refusal that reaches the client as writeInternalError's 500
// tells the caller the server broke and that a retry might work, when the
// request was understood and deliberately declined — which is exactly the shape
// BUG-2831 filed against the Postgres path (SQLSTATE 54000 reaching the generic
// error arm).

// TestPatchItemEmptyTitleRefused is the verbatim BUG-2833 repro: the filing
// measured PATCH {"title": ""} being accepted and applied while POST refused
// the same input with 400 "Title is required".
func TestPatchItemEmptyTitleRefused(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, ws, "Original Title", `{"status":"open"}`)

	for _, title := range []string{"", "   ", "\t\n"} {
		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, map[string]interface{}{
			"title": title,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("PATCH title=%q: got %d, want 400 (body: %s)", title, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "Title is required") {
			t.Errorf("PATCH title=%q: body %s, want it to say the title is required", title, rr.Body.String())
		}
	}

	// The item must be untouched — a 400 that already wrote the row would
	// satisfy every assertion above and still be the bug.
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET after refusals: %d", rr.Code)
	}
	var after models.Item
	parseJSON(t, rr, &after)
	if after.Title != "Original Title" {
		t.Errorf("title = %q, want it untouched (%q)", after.Title, "Original Title")
	}
}

// TestPatchItemOverlongTitleRefusedAs400 is the BUG-2831 shape assertion. The
// filing explicitly flagged the status code as READ, not measured end-to-end
// ("NOT independently verified end-to-end: I read the store error and the
// handler's fall-through"). This measures it.
func TestPatchItemOverlongTitleRefusedAs400(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, ws, "Short", `{"status":"open"}`)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, map[string]interface{}{
		"title": strings.Repeat("a", models.MaxItemTitleRunes+1),
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "too long") {
		t.Errorf("body = %s, want it to say the title is too long", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "internal_error") {
		t.Errorf("body = %s, must not be an internal_error envelope", rr.Body.String())
	}
}

// TestCreateItemWhitespaceOnlyTitleRefused pins the deliberate widening of the
// CREATE door. "   " was accepted here before this unit and refused by the
// artifact-import door whose comment claimed to mirror this one.
func TestCreateItemWhitespaceOnlyTitleRefused(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/tasks/items", map[string]interface{}{
		"title": "   ",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Title is required") {
		t.Errorf("body = %s, want it to say the title is required", rr.Body.String())
	}
}

// TestCreateItemOverlongTitleRefused is the create half of BUG-2831 — the
// original measurement was on CreateItem, where a 2 MiB title succeeded on
// SQLite and produced `index row requires 24064 bytes, maximum size is 8191`
// on Postgres.
func TestCreateItemOverlongTitleRefused(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/tasks/items", map[string]interface{}{
		"title": strings.Repeat("a", models.MaxItemTitleRunes+1),
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "too long") {
		t.Errorf("body = %s, want it to say the title is too long", rr.Body.String())
	}
}

// TestItemTitleTrimmedOverTheWire: the API must persist what it validated. A
// door that validates the trimmed string and stores the raw one leaves the row
// holding a value nothing checked.
func TestItemTitleTrimmedOverTheWire(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/tasks/items", map[string]interface{}{
		"title": "  Padded  ",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201 (body: %s)", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)
	if created.Title != "Padded" {
		t.Errorf("created title = %q, want %q", created.Title, "Padded")
	}

	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+created.Slug, map[string]interface{}{
		"title": "  Renamed  ",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var updated models.Item
	parseJSON(t, rr, &updated)
	if updated.Title != "Renamed" {
		t.Errorf("updated title = %q, want %q", updated.Title, "Renamed")
	}
}

// TestWriteInvalidItemTitleMapsTo400 covers the store-refusal arm DIRECTLY.
//
// It exists because a mutation test showed the arm surviving: the handlers'
// own pre-lock checks catch every case reachable from an HTTP test, so the
// store's typed refusal never arrives through the wire in the suite. The arm is
// still load-bearing — it is what a title that only becomes invalid UNDER THE
// LOCK lands on (the handler compares against an item read before any lock, so
// a concurrent rename can turn an echoed legacy title into a genuine one) — and
// an untested error mapping is how a deliberate 400 becomes a 500 in a later
// refactor. Testing the mapping directly is the honest way to cover a branch
// whose only production trigger is a race.
func TestWriteInvalidItemTitleMapsTo400(t *testing.T) {
	t.Run("maps the typed refusal", func(t *testing.T) {
		rr := httptest.NewRecorder()
		if !writeInvalidItemTitle(rr, &store.InvalidItemTitleError{Reason: "Title is required"}) {
			t.Fatal("writeInvalidItemTitle returned false for its own error type")
		}
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "Title is required") {
			t.Errorf("body = %s, want the typed Reason", rr.Body.String())
		}
	})

	t.Run("maps through a wrapper without publishing it", func(t *testing.T) {
		// The call path wraps this error on the way up. The client must get the
		// typed Reason, not the accumulated wrapper text.
		wrapped := fmt.Errorf("update item: %w", &store.InvalidItemTitleError{Reason: "Title is too long: 300 characters, maximum 255"})
		rr := httptest.NewRecorder()
		if !writeInvalidItemTitle(rr, wrapped) {
			t.Fatal("writeInvalidItemTitle returned false for a wrapped refusal; errors.As must see through wrappers")
		}
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rr.Code)
		}
		if strings.Contains(rr.Body.String(), "update item:") {
			t.Errorf("body = %s, must not publish the wrapper text", rr.Body.String())
		}
	})

	t.Run("declines everything else", func(t *testing.T) {
		// The control leg. Without it this test would pass against a helper
		// that returns true unconditionally, which would swallow every other
		// store error into a 400.
		rr := httptest.NewRecorder()
		if writeInvalidItemTitle(rr, errors.New("some unrelated store failure")) {
			t.Error("writeInvalidItemTitle claimed an unrelated error; it must fall through")
		}
		if rr.Code != http.StatusOK || rr.Body.Len() != 0 {
			t.Errorf("declining must write nothing, got status %d body %q", rr.Code, rr.Body.String())
		}
	})
}

// TestCreateItemCheckedMapsStoreTitleRefusalTo400 covers the create path's
// store-refusal arm directly, for the same reason as the update path's helper
// test above: handleCreateItem's own check catches everything reachable over
// the wire, so the arm's absence is a latent 500 rather than a visible one — and
// a mutation test is what surfaced it.
//
// createItemChecked is a separate function from handleCreateItem, and the
// pre-check lives in the handler, so calling it directly reaches the store
// refusal that no HTTP request can.
func TestCreateItemCheckedMapsStoreTitleRefusalTo400(t *testing.T) {
	srv := testServer(t)
	wsSlug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(wsSlug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug(%q): %v", wsSlug, err)
	}
	coll, err := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil || coll == nil {
		t.Fatalf("GetCollectionBySlug: %v", err)
	}
	var schema models.CollectionSchema
	if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	req := httptest.NewRequest("POST", "/", nil)
	for _, tc := range []struct{ name, title, want string }{
		{"empty", "", "Title is required"},
		{"over the bound", strings.Repeat("a", models.MaxItemTitleRunes+1), "too long"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cerr := srv.createItemChecked(req, ws.ID, coll, schema,
				models.ItemCreate{Title: tc.title}, map[string]any{}, "", relationsRefuse)
			if cerr == nil {
				t.Fatal("createItemChecked accepted an invalid title")
			}
			if cerr.status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 — a deliberate refusal must not read as a server fault", cerr.status)
			}
			if !strings.Contains(cerr.message, tc.want) {
				t.Errorf("message = %q, want it to mention %q", cerr.message, tc.want)
			}
		})
	}

	// Control: a valid title still creates, so the arm above is not swallowing
	// the success path.
	if _, cerr := srv.createItemChecked(req, ws.ID, coll, schema,
		models.ItemCreate{Title: "Perfectly Fine"}, map[string]any{}, "", relationsRefuse); cerr != nil {
		t.Fatalf("a valid title must still create: %d %s", cerr.status, cerr.message)
	}
}

// TestWriteTypedItemRefusalIncludesTitleRefusal regresses codex round 1 P1 on the
// collab fallback, PORTED from isDeterministicWriteFailure when PLAN-2975 unit 2
// replaced that classifier with writeTypedItemRefusal.
//
// The property is unchanged and is why the port was worth doing rather than deleting
// the test with the function: a permanent refusal the handler does not RECOGNISE is
// treated as a recoverable routing error, so the request falls through to another
// write path and re-derives the identical refusal from scratch — BUG-2804 measured
// that as running a whole rename cascade twice for one request, and answering by the
// other route.
//
// The store gained a fourth such refusal with the item-title bound and nothing failed
// when it was left out, which is exactly why this exists: the omission is invisible
// from the outside.
func TestWriteTypedItemRefusalIncludesTitleRefusal(t *testing.T) {
	srv := testServer(t)
	item := &models.Item{ID: "item-1", Ref: "TASK-1", Slug: "task-1"}

	// The recorder is inspected, not discarded: the classifier's answer is only half
	// the contract — a mutant that returns true while writing the wrong status would
	// pass a boolean-only assertion (codex round 1, this unit).
	refused := func(err error) bool {
		rec := httptest.NewRecorder()
		got := srv.writeTypedItemRefusal(rec, item, err)
		if got {
			if rec.Code < 400 || rec.Code > 499 {
				t.Errorf("a recognised refusal wrote status %d; a refusal must answer 4xx or the "+
					"caller cannot tell it from success", rec.Code)
			}
			if rec.Body.Len() == 0 {
				t.Error("a recognised refusal wrote no body; the structured envelope is the contract")
			}
		} else if rec.Body.Len() != 0 {
			t.Errorf("an unrecognised error wrote a body (%s) while reporting not-handled; the "+
				"caller will write a second response on top of it", rec.Body.String())
		}
		return got
	}

	if !refused(&store.InvalidItemTitleError{Reason: "Title is required"}) {
		t.Error("an invalid-title refusal is permanent: retrying the same title always refuses")
	}
	// Through a wrapper, since the call path wraps on the way up.
	if !refused(fmt.Errorf("update item: %w", &store.InvalidItemTitleError{Reason: "Title is too long"})) {
		t.Error("must see through wrappers")
	}
	// Controls. Without these a mutant returning true unconditionally would pass —
	// and that mutant would swallow every transient failure as a settled refusal,
	// answering a 4xx for a condition that would have succeeded on retry.
	if refused(nil) {
		t.Error("nil is not a failure")
	}
	if refused(errors.New("transient prune failure")) {
		t.Error("an unrecognised error must stay recoverable so the route still degrades gracefully")
	}

	// The FIFTH arm. Before PLAN-2975 the applier path's row write fell through the
	// ordinary error block and inherited its UNIQUE-constraint mapping; routing that
	// path through this helper dropped it, turning a benign race into a 500 on that
	// route only (codex round 5, a regression rather than a gap). Both wordings are
	// asserted because the store hands back the driver's message verbatim and SQLite
	// and Postgres word it differently — testing one would leave the other route to
	// the 500.
	for _, msg := range []string{
		"UNIQUE constraint failed: items.slug",
		`pq: duplicate key value violates unique constraint "items_invocation_slug_idx"`,
	} {
		rec := httptest.NewRecorder()
		if !srv.writeTypedItemRefusal(rec, item, errors.New(msg)) {
			t.Errorf("a unique-constraint race (%q) must be recognised; unrecognised it answers 500 "+
				"for a request the server understood and declined", msg)
			continue
		}
		if rec.Code != http.StatusConflict {
			t.Errorf("a unique-constraint race answered %d, want 409 to match the create path and "+
				"the ordinary update path", rec.Code)
		}
	}
}

// TestUpdateItemErrorBlocksMapEveryStoreRefusal is a structural guard, and it
// exists because this exact mistake has now been made four times in this one
// function.
//
// handleUpdateItem reaches store.UpdateItemWithParentLink from THREE places —
// the plain path, the collab-snapshot callback, and the collab-edit callback —
// each with its own error block. BUG-2804 mapped the cascade refusal into the
// plain block and missed the other two; its own comment records that as "a
// population error, not a typo". BUG-2833 then mapped the title refusal into
// the plain block, a reviewer named the collab-snapshot one, and the collab-edit
// one was still missing — with that comment sitting directly above the helper
// being called.
//
// So the durable fix is not another careful read. It is this: the arms are
// counted, and adding a fourth block carrying one refusal but not its siblings
// fails here with a message saying which. Counting AST call expressions rather
// than grepping means comments and strings mentioning these names do not count.
//
// If you are here because this test failed: you added (or moved) an error
// block. Give it every arm, in the same order as its siblings, or explain in
// this test why the new block genuinely cannot produce one of these errors.
//
// WHAT IT COVERS AND WHAT IT DOES NOT, stated because a structural test invites
// more confidence than it earns. It detects an arm that is DELETED, one that is
// out of ORDER, and — since codex round 3 — one DISABLED by being made an
// operand of a boolean condition, which is the shape that defeated the
// presence-only version. All three are verified by mutation rather than
// asserted.
//
// It is still LEXICAL, and that is a boundary rather than a to-do. Three review
// rounds each named a new way to satisfy the letter of it — a short-circuited
// condition, an inverted one, a missing return — and each was closed, but a
// lexical instrument has no last one: an early return above the block, a
// wrapping `if false`, or a condition calling something that always returns
// false would all pass. What it buys is protection against the failure that has
// ACTUALLY happened twice in this function, which is an omitted or misordered
// arm.
//
// The behavioural complement is not available here, and it is worth saying why
// rather than leaving it as an obvious gap: the handlers' own pre-checks catch
// every title refusal reachable over the wire, so the store-sourced refusal
// these arms exist for has no HTTP trigger except a concurrent rename inside
// the lock window. Driving that from a test needs a store-level seam that does
// not exist. The helper itself is unit-tested; these arms' job is to call it.
func TestUpdateItemErrorBlocksMapEveryStoreRefusal(t *testing.T) {
	// TWO FILES since PLAN-2975, and the second one is why the expected block
	// count changed rather than the guard weakening.
	//
	// The write-first-apply-second reorder took the applier branch's inline block
	// out of handlers_items.go and replaced it with a call to writeTypedItemRefusal,
	// which maps all four arms once and is consulted by every ordering. That is a
	// STRONGER shape than three parallel blocks — the failure this guard exists to
	// catch is an arm mapped in some routes and not others, and a single shared
	// function cannot drift against itself — but it moves one block into another
	// file, so a scan of handlers_items.go alone now sees two blocks and fails
	// closed. It failed closed when the reorder landed, which is the guard working;
	// teaching it the new shape is the response, and the count below is the part a
	// future restructuring will trip again on purpose.
	want := []string{
		"asOpenChildrenGuardError",
		"asUpdateConflictError",
		"writeItemRenameCascadeTooLarge",
		"writeInvalidItemTitle",
	}
	wantArm := map[string]bool{}
	for _, w := range want {
		wantArm[w] = true
	}

	// THE FILE SET IS DERIVED, NOT LISTED (codex round 5). A hardcoded pair passes
	// while an unmapped block sits in a third file, which is the same
	// under-counting this guard exists to catch — so the sources are every
	// non-test file in the package that calls UpdateItemWithParentLink, and a file
	// that starts calling it joins the scan by doing so.
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var files []*ast.File
	var sources []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(name)
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		// The predicate is a UNION, and fail-open is the failure mode it is fighting
		// (codex round 6): a file reaching the store through a wrapper or a variable
		// would not match the call text, so a file that calls any of the refusal ARMS
		// is scanned too. A refusal block lives where the arms are called, whatever it
		// calls the store through.
		text := string(src)
		relevant := strings.Contains(text, "UpdateItemWithParentLink(")
		for _, arm := range want {
			if strings.Contains(text, arm+"(") {
				relevant = true
			}
		}
		if !relevant {
			continue
		}
		f, perr := parser.ParseFile(fset, name, src, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		files = append(files, f)
		sources = append(sources, name)
	}
	if len(files) == 0 {
		t.Fatal("no non-test file in this package calls UpdateItemWithParentLink; the scan found " +
			"nothing to check, so every assertion below would be vacuous")
	}
	t.Logf("scanning %v", sources)

	// The arms, in the order every block must apply them. Order is part of the
	// contract, not style: the UNIQUE-constraint arm that closes each block
	// matches on error TEXT, so a typed arm placed after it can be swallowed by
	// a substring match rather than reached.

	// ---- membership + order, per block ----
	//
	// PER-BLOCK, not per-file (codex round 2): counting calls across the whole
	// file lets one block's duplicate mask another's omission and says nothing
	// about order.
	//
	// Blocks are recovered from SOURCE POSITION rather than from AST shape,
	// which is what makes this robust: the three blocks are written differently
	// — two as sequential `if` statements, one as an else-if chain — and an
	// ast.Inspect keyed on either shape either misses blocks or double-counts
	// them through nesting. (Both happened while writing this: first 0 blocks,
	// then 8. The block-count guard below is what caught each, instead of the
	// assertions quietly passing over nothing.)
	//
	// Every block opens with the open-children arm, so a new block starts at
	// each occurrence of it, in file order.
	type armRef struct {
		name string
		pos  token.Pos
	}
	var refs []armRef
	inspectAll := func(fn func(ast.Node) bool) {
		for _, f := range files {
			ast.Inspect(f, fn)
		}
	}
	inspectAll(func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		if wantArm[ident.Name] {
			refs = append(refs, armRef{ident.Name, call.Pos()})
		}
		return true
	})
	sort.Slice(refs, func(i, j int) bool { return refs[i].pos < refs[j].pos })

	var blocks [][]string
	var lines []int
	var startPos []token.Pos
	for _, r := range refs {
		if r.name == want[0] {
			blocks = append(blocks, nil)
			lines = append(lines, fset.Position(r.pos).Line)
			startPos = append(startPos, r.pos)
		}
		if len(blocks) == 0 {
			t.Fatalf("arm %q at line %d precedes any %q — the block-splitting assumption is wrong",
				r.name, fset.Position(r.pos).Line, want[0])
		}
		blocks[len(blocks)-1] = append(blocks[len(blocks)-1], r.name)
	}

	// handleUpdateItem has three; handleMoveItem carries a lone open-children
	// arm and cannot produce the others (it writes no title — verified in the
	// BUG-2833 door sweep), so single-arm blocks are not update blocks.
	var updateBlocks [][]string
	var updateLines []int
	var updateStart []token.Pos
	for i, b := range blocks {
		if len(b) > 1 {
			updateBlocks = append(updateBlocks, b)
			updateLines = append(updateLines, lines[i])
			updateStart = append(updateStart, startPos[i])
		}
	}

	// Three: two inline blocks remaining in handlers_items.go, plus
	// writeTypedItemRefusal's single shared block. The number is unchanged from
	// before PLAN-2975 by coincidence — what changed is that one of the three is now
	// reached by every content-PATCH ordering instead of being copied per route.
	//
	// The constant is deliberately brittle. A restructuring that changes the count
	// should stop here and be looked at, because that is the moment a refusal
	// silently stops being mapped on one route.
	const wantBlocks = 3
	if len(updateBlocks) != wantBlocks {
		t.Fatalf("found %d UpdateItem error block(s) at lines %v, want %d — the instrument's block "+
			"detection is out of step with the code, so every assertion below would be meaningless. "+
			"Blocks: %v", len(updateBlocks), updateLines, wantBlocks, updateBlocks)
	}
	for i, arms := range updateBlocks {
		if len(arms) != len(want) {
			t.Errorf("the error block at line %d maps %v; every block must map all of %v. A refusal "+
				"mapped in some blocks and not others answers 400/409/413 down one route and 500 — or "+
				"a silent duplicate write — down another, for the identical store error.",
				updateLines[i], arms, want)
			continue
		}
		for j := range want {
			if arms[j] != want[j] {
				t.Errorf("the error block at line %d applies its arms as %v; want %v",
					updateLines[i], arms, want)
				break
			}
		}
	}

	// ---- the fifth arm: the UNIQUE-constraint race ----
	//
	// It is not in `want` because it is not a call to a named helper — it is a string
	// match on the driver's message, so the AST walk above cannot see it. It is
	// checked anyway, and separately, because dropping it is exactly the regression
	// codex round 5 found: the applier path inherited this mapping from the ordinary
	// block until the reorder routed around it, and a benign race answered 500 on one
	// route and 409 on the other.
	//
	// SCOPED TO THE ENCLOSING FUNCTION, which is what made it discriminate. The first
	// version asked whether a UNIQUE literal appeared between this block's start and
	// the next block's start in token.Pos. Those windows span whole FILES — the gap
	// between the last block of one file and the first block of the next swallows
	// every literal in between, including two in handlers_items.go belonging to the
	// create and restore paths and one in handlers_items_bulk.go. ALL THREE mutation
	// controls survived that version; it asserted nothing. A block's arm lives in the
	// block's own function, so that is the containment to test.
	funcOf := func(pos token.Pos) *ast.FuncDecl {
		for _, f := range files {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if pos >= fn.Pos() && pos <= fn.End() {
					return fn
				}
			}
		}
		return nil
	}
	uniqueInFunc := map[*ast.FuncDecl]bool{}
	inspectAll(func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if !strings.Contains(lit.Value, "UNIQUE constraint") {
			return true
		}
		if fn := funcOf(lit.Pos()); fn != nil {
			uniqueInFunc[fn] = true
		}
		return true
	})

	for i, lo := range updateStart {
		fn := funcOf(lo)
		if fn == nil {
			t.Errorf("the error block at line %d is not inside any function declaration; the "+
				"containment this check relies on does not hold", updateLines[i])
			continue
		}
		if !uniqueInFunc[fn] {
			t.Errorf("%s contains an error block at line %d but no UNIQUE-constraint arm. A "+
				"concurrent slug/title collision answers 409 on the routes that map it and 500 on "+
				"the ones that do not, for the identical store error — which is the regression that "+
				"put this check here.", fn.Name.Name, updateLines[i])
		}
	}

	// ---- reachability ----
	//
	// Counting call expressions detects a DELETED arm but not a disabled one:
	// `false && writeInvalidItemTitle(w, err)` short-circuits at runtime while
	// leaving the call in the AST, so the sequence above reads as correct while
	// every title refusal falls through to a 500. Measured — that mutant
	// survived the presence-only version (codex round 3).
	//
	// WHITELIST, not blacklist. The first attempt rejected conditions that were
	// BinaryExpr, which `!writeInvalidItemTitle(w, err)` — a UnaryExpr — walked
	// straight past while inverting the arm's meaning (codex round 5).
	// Enumerating the ways to disable a call is a losing game; enumerating the
	// two ways these arms are legitimately written is not:
	//
	//	if f(w, err) { ... }            // cond IS the call
	//	if x, ok := f(err); ok { ... }  // cond is the bare `ok` ident
	//
	// Anything else is either a disabling mutation or a restructuring that
	// deserves to be looked at deliberately.
	inspectAll(func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		armsHere := armsIn(ifStmt, want)
		if len(armsHere) == 0 {
			return true
		}
		switch cond := ifStmt.Cond.(type) {
		case *ast.CallExpr:
			if ident, ok := cond.Fun.(*ast.Ident); ok && wantArm[ident.Name] {
				return true
			}
		case *ast.Ident:
			if ifStmt.Init != nil {
				return true
			}
		}
		t.Errorf("the arm(s) %v at line %d are guarded by a condition that is neither the bare call nor "+
			"a plain `ok` ident (%T). They still count as present, but may never execute — which is a "+
			"refusal silently answering 500 while this test reads as green.",
			armsHere, fset.Position(ifStmt.Pos()).Line, ifStmt.Cond)
		return true
	})

	// ---- each arm must actually STOP ----
	//
	// An arm that matches and then falls through has written its response and
	// let the next arm write another, or let the request continue to the
	// generic 500 (codex round 6). The body's last statement is a bare return
	// in all three blocks; anything else is a behaviour change worth looking at.
	//
	// BOUNDARY, stated because the shared block weakened this leg and pretending
	// otherwise would be the exact overclaim this file's header warns about: in
	// writeTypedItemRefusal the arms end in `return true`, a handled-FLAG its
	// callers act on, not a return from the request. This walk therefore proves
	// the arm stops the FUNCTION, and the caller honouring the flag is checked
	// behaviourally by the route tests rather than here.
	inspectAll(func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		armsHere := armsIn(ifStmt, want)
		if len(armsHere) == 0 || ifStmt.Body == nil || len(ifStmt.Body.List) == 0 {
			return true
		}
		if _, ok := ifStmt.Body.List[len(ifStmt.Body.List)-1].(*ast.ReturnStmt); !ok {
			t.Errorf("the arm(s) %v at line %d do not end in a return; a matched arm that falls through "+
				"lets a second response be written, or lets the request reach the generic 500.",
				armsHere, fset.Position(ifStmt.Pos()).Line)
		}
		return true
	})
}

// armsIn returns the named arm functions this if-statement tests, in source
// order. It reads the INIT statement as well as the condition, because the arms
// take both shapes — `if x, ok := f(err); ok {` puts the call in Init, while
// `if f(w, err) {` puts it in Cond. Reading only Cond found zero blocks, and
// the block-count guard is what caught that rather than silently asserting
// nothing.
func armsIn(ifStmt *ast.IfStmt, want []string) []string {
	var found []string
	collect := func(n ast.Node) {
		if n == nil {
			return
		}
		ast.Inspect(n, func(c ast.Node) bool {
			call, ok := c.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			for _, w := range want {
				if ident.Name == w {
					found = append(found, w)
				}
			}
			return true
		})
	}
	collect(ifStmt.Init)
	collect(ifStmt.Cond)
	return found
}

// TestPatchItemVerbatimEchoOfALegacyTitleIsANoOp covers the raw-echo
// grandfathering leg OVER HTTP, which is where the clients that actually do
// this live.
//
// It exists because a mutation exposed the gap: making the handler normalize
// `input.Title` before forwarding destroys the store's raw-echo comparison, and
// that mutant SURVIVED — the only test for the leg called store.UpdateItem
// directly, so nothing measured the path a browser or CLI takes. The handler's
// job here is to refuse early and change nothing; a normalization on the way
// through is a decision, and decisions belong under the lock.
func TestPatchItemVerbatimEchoOfALegacyTitleIsANoOp(t *testing.T) {
	srv := testServer(t)
	wsSlug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, wsSlug, "Ordinary", `{"status":"open"}`)

	// Make it legacy: over the bound AND carrying edge whitespace, written
	// straight to the row so it bypasses the write-time guard exactly as a
	// pre-bound row does.
	legacyTitle := "  " + strings.Repeat("m", models.MaxItemTitleRunes+1) + "  "
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind("UPDATE items SET title = ? WHERE id = ?"),
		legacyTitle, item.ID); err != nil {
		t.Fatalf("fixture: write the legacy title: %v", err)
	}

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+wsSlug+"/items/"+item.Slug, map[string]interface{}{
		"title": legacyTitle,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("echoing the stored title back must be a no-op, got %d: %s", rr.Code, rr.Body.String())
	}
	var updated models.Item
	parseJSON(t, rr, &updated)
	if updated.Title != legacyTitle {
		t.Errorf("title = %q, want the legacy bytes unchanged — a normalization on the way through "+
			"turns a no-op echo into a rename", updated.Title)
	}

	// Control: a genuine rename to an over-bound title is still refused through
	// the same door, so this is grandfathering and not a hole.
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+wsSlug+"/items/"+item.Slug, map[string]interface{}{
		"title": strings.Repeat("n", models.MaxItemTitleRunes+1),
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("a DIFFERENT over-bound title must still be refused, got %d: %s", rr.Code, rr.Body.String())
	}
}
