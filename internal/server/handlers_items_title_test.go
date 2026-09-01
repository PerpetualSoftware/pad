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
				models.ItemCreate{Title: tc.title}, map[string]any{}, "")
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
		models.ItemCreate{Title: "Perfectly Fine"}, map[string]any{}, ""); cerr != nil {
		t.Fatalf("a valid title must still create: %d %s", cerr.status, cerr.message)
	}
}

// TestIsDeterministicWriteFailureIncludesTitleRefusal regresses codex round 1
// P1 on the collab fallback.
//
// isDeterministicWriteFailure decides whether a direct-write failure is a
// settled answer. A permanent refusal it does not recognise is returned as a
// generic collab-routing error, so the caller falls through to its own direct
// write and re-derives the identical refusal from scratch — BUG-2804 measured
// that as running a whole rename cascade twice for one request, and reported
// the answer by the other route.
//
// The store gained a fourth such refusal with the item-title bound and nothing
// failed when it was left out, which is exactly why this test exists: the
// omission is invisible from the outside.
func TestIsDeterministicWriteFailureIncludesTitleRefusal(t *testing.T) {
	if !isDeterministicWriteFailure(&store.InvalidItemTitleError{Reason: "Title is required"}) {
		t.Error("an invalid-title refusal is permanent: retrying the same title always refuses")
	}
	// Through a wrapper, since the call path wraps on the way up.
	if !isDeterministicWriteFailure(fmt.Errorf("update item: %w", &store.InvalidItemTitleError{Reason: "Title is too long"})) {
		t.Error("must see through wrappers")
	}
	// Controls. Without these, a mutant returning true unconditionally would
	// pass — and that mutant would break the graceful-degradation contract the
	// function exists to protect, by treating a transient prune failure as
	// final.
	if isDeterministicWriteFailure(nil) {
		t.Error("nil is not a failure")
	}
	if isDeterministicWriteFailure(errors.New("transient prune failure")) {
		t.Error("an unrecognised error must stay recoverable so the fallback still degrades gracefully")
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
// It is still lexical. An arm made unreachable some other way — an early
// return above it, a condition that is a call which always returns false —
// would pass. Closing that needs control-flow analysis or a route-level test,
// and a route-level test is not available here: the handlers' own pre-checks
// catch every title refusal reachable over the wire, so the store-sourced one
// these arms exist for has no HTTP trigger except a concurrent rename.
func TestUpdateItemErrorBlocksMapEveryStoreRefusal(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "handlers_items.go", nil, 0)
	if err != nil {
		t.Fatalf("parse handlers_items.go: %v", err)
	}

	// The arms, in the order every block must apply them. Order is part of the
	// contract, not style: the UNIQUE-constraint arm that closes each block
	// matches on error TEXT, so a typed arm placed after it can be swallowed by
	// a substring match rather than reached.
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

	// PER-BLOCK, not per-file (codex round 2): counting calls across the whole
	// file lets one block's duplicate mask another's omission and says nothing
	// about order.
	//
	// Blocks are recovered from SOURCE POSITION rather than from AST shape,
	// which is what makes this robust: the three blocks are written
	// differently — two as sequential `if` statements, one as an else-if chain
	// — and an ast.Inspect that keys on either shape either misses blocks or
	// double-counts them through nesting. (Both happened while writing this:
	// first 0 blocks, then 8. The block-count guard below is what caught each,
	// instead of the assertions quietly passing over nothing.)
	//
	// Every block opens with the open-children arm, so a new block starts at
	// each occurrence of it, in file order.
	type armRef struct {
		name string
		pos  token.Pos
	}
	var refs []armRef
	ast.Inspect(file, func(n ast.Node) bool {
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

	// REACHABILITY, not just presence (codex round 3). Counting call
	// expressions detects a DELETED arm but not a disabled one: `false &&
	// writeInvalidItemTitle(w, err)` short-circuits at runtime while leaving
	// the call in the AST, so the sequence above still reads as correct while
	// every title refusal falls through to a 500. Measured — that mutant
	// survived the presence-only version of this test.
	//
	// The check is narrow on purpose: each arm must be tested DIRECTLY, as the
	// whole of an if-condition, not as one operand of a boolean expression.
	// That is how all three blocks are written, so anything else is either the
	// mutation above or a genuine restructuring that deserves to be looked at.
	ast.Inspect(file, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		bin, ok := ifStmt.Cond.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		ast.Inspect(bin, func(c ast.Node) bool {
			call, ok := c.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			if wantArm[ident.Name] {
				t.Errorf("the arm %q at line %d is an operand of a boolean expression rather than the "+
					"whole condition. It still counts as present below, but it may never execute — "+
					"which is a refusal silently answering 500 while this test reads as green.",
					ident.Name, fset.Position(call.Pos()).Line)
			}
			return true
		})
		return true
	})
	sort.Slice(refs, func(i, j int) bool { return refs[i].pos < refs[j].pos })

	var blocks [][]string
	var lines []int
	for _, r := range refs {
		if r.name == want[0] {
			blocks = append(blocks, nil)
			lines = append(lines, fset.Position(r.pos).Line)
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
	for i, b := range blocks {
		if len(b) > 1 {
			updateBlocks = append(updateBlocks, b)
			updateLines = append(updateLines, lines[i])
		}
	}

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
}
