package server

// BUG-2578 — a singular collection name worked only for the seven names the
// client-side alias map hardcodes, so `pad item create spec` failed in a
// workspace whose collections include `specs`. Resolution now happens against
// the workspace's ACTUAL collections, server-side, which covers every client
// at once.
//
// The load-bearing safety property, asserted here as its own case: EXACT MATCH
// ALWAYS WINS. The fallbacks may only fire when the input names nothing, so
// this can never redirect a request that already succeeded.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// makeCollection creates a collection via the API so it goes through the same
// slug/prefix machinery production does.
func makeCollection(t *testing.T, srv *Server, wsSlug, name, slug, prefix string) {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections",
		map[string]any{"name": name, "slug": slug, "prefix": prefix})
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("create collection %q: %d %s", slug, rr.Code, rr.Body.String())
	}
}

func createItemIn(t *testing.T, srv *Server, wsSlug, collSlug, title string) *httptestRecorder {
	t.Helper()
	rr := doRequest(srv, "POST",
		"/api/v1/workspaces/"+wsSlug+"/collections/"+collSlug+"/items",
		map[string]any{"title": title})
	return &httptestRecorder{rr.Code, rr.Body.String()}
}

type httptestRecorder struct {
	Code int
	Body string
}

// itemsIn returns the titles of the items the given collection holds, resolved
// by EXACT slug so the assertion cannot be satisfied by the very fallback
// under test.
func itemsIn(t *testing.T, srv *Server, wsSlug, collSlug string) []string {
	t.Helper()
	ws, err := srv.store.GetWorkspaceBySlug(wsSlug)
	if err != nil || ws == nil {
		t.Fatalf("resolve workspace %q: %v", wsSlug, err)
	}
	coll, err := srv.store.GetCollectionBySlug(ws.ID, collSlug)
	if err != nil {
		t.Fatalf("resolve collection %q: %v", collSlug, err)
	}
	if coll == nil {
		t.Fatalf("collection %q does not exist", collSlug)
	}
	// Filtered by collection ID, not slug: an ID cannot be satisfied by the
	// slug fallback this file is testing, so the assertion reads the real
	// destination rather than re-running the resolver.
	items, err := srv.store.ListItems(ws.ID, models.ItemListParams{CollectionIDs: []string{coll.ID}})
	if err != nil {
		t.Fatalf("list items in %q: %v", collSlug, err)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Title)
	}
	return out
}

// The reported case: a collection the hardcoded client map has never heard of
// gets a working singular form.
func TestResolveItemCollectionSlug_SingularOfANonDefaultCollection(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	makeCollection(t, srv, ws, "Specs", "specs", "SPEC")

	// Control: this is the failure being fixed. If a future refactor makes
	// `spec` an exact collection, this fixture stops testing the fallback.
	if _, err := srv.store.GetWorkspaceBySlug(ws); err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}

	rr := createItemIn(t, srv, ws, "spec", "probe spec")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create via singular `spec` = %d: %s — this is the reported bug: "+
			"a collection with no entry in the hardcoded alias map has no shorthand",
			rr.Code, rr.Body)
	}
	if got := itemsIn(t, srv, ws, "specs"); len(got) != 1 || got[0] != "probe spec" {
		t.Errorf("items in `specs` = %v, want [probe spec]", got)
	}
}

// Listing resolves the same way creation does — they are the two halves of the
// same complaint (`pad item create spec` / `pad item list spec`).
func TestResolveItemCollectionSlug_ListAcceptsTheSingular(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	makeCollection(t, srv, ws, "Specs", "specs", "SPEC")
	if rr := createItemIn(t, srv, ws, "specs", "listed"); rr.Code != http.StatusCreated {
		t.Fatalf("seed item: %d %s", rr.Code, rr.Body)
	}

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/collections/spec/items", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list via singular `spec` = %d: %s", rr.Code, rr.Body.String())
	}
	var items []models.Item
	parseJSON(t, rr, &items)
	if len(items) != 1 || items[0].Title != "listed" {
		t.Errorf("listed %d items via the singular, want the 1 seeded", len(items))
	}
}

// THE SAFETY PROPERTY. A workspace holding BOTH `plan` and `plans` must route
// `plan` to `plan`. If the fallback could outrank an exact match it would
// silently misfile writes — which is exactly what the CLIENT-side map does
// today (BUG-2630), and precisely the behaviour this resolver must not add on
// the server.
func TestResolveItemCollectionSlug_ExactMatchAlwaysWins(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	makeCollection(t, srv, ws, "Plan", "plan", "PLN")

	// `plans` ships with the startup template; assert the collision is real
	// or the test proves nothing.
	wsRow, err := srv.store.GetWorkspaceBySlug(ws)
	if err != nil || wsRow == nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	for _, slug := range []string{"plan", "plans"} {
		coll, err := srv.store.GetCollectionBySlug(wsRow.ID, slug)
		if err != nil || coll == nil {
			t.Fatalf("fixture never armed: collection %q missing (%v)", slug, err)
		}
	}

	if rr := createItemIn(t, srv, ws, "plan", "belongs in plan"); rr.Code != http.StatusCreated {
		t.Fatalf("create in `plan` = %d: %s", rr.Code, rr.Body)
	}

	if got := itemsIn(t, srv, ws, "plan"); len(got) != 1 || got[0] != "belongs in plan" {
		t.Errorf("items in `plan` = %v, want [belongs in plan] — an exact collection "+
			"name was resolved to a different collection", got)
	}
	if got := itemsIn(t, srv, ws, "plans"); len(got) != 0 {
		t.Errorf("items in `plans` = %v, want none — the write was misrouted", got)
	}
}

// The reverse direction: a workspace whose collection is genuinely singular
// still answers to the plural a user might type out of habit.
func TestResolveItemCollectionSlug_PluralOfASingularCollection(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	makeCollection(t, srv, ws, "Spec", "spec", "SPC")

	rr := createItemIn(t, srv, ws, "specs", "typed the plural")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create via plural `specs` = %d: %s", rr.Code, rr.Body)
	}
	if got := itemsIn(t, srv, ws, "spec"); len(got) != 1 {
		t.Errorf("items in `spec` = %v, want the one created via the plural", got)
	}
}

// A name that matches nothing must still 404. The fallbacks widen what
// resolves; they must not make everything resolve.
func TestResolveItemCollectionSlug_UnknownStillNotFound(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)

	rr := createItemIn(t, srv, ws, "definitely-not-a-collection", "nope")
	if rr.Code != http.StatusNotFound {
		t.Errorf("create in an unknown collection = %d, want 404: %s", rr.Code, rr.Body)
	}
}

// `pad item move <ref> <target-collection>` takes a user-typed collection name
// too, and the bug's own body names move alongside create and list.
func TestResolveItemCollectionSlug_MoveAcceptsTheSingular(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	makeCollection(t, srv, ws, "Specs", "specs", "SPEC")

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/tasks/items",
		map[string]any{"title": "movable"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/"+item.Slug+"/move",
		map[string]any{"target_collection": "spec"})
	if rr.Code != http.StatusOK {
		t.Fatalf("move to singular `spec` = %d: %s", rr.Code, rr.Body.String())
	}
	if got := itemsIn(t, srv, ws, "specs"); len(got) != 1 || got[0] != "movable" {
		t.Errorf("items in `specs` after move = %v, want [movable]", got)
	}
}

// Unit-level coverage of the candidate generator, including the cases that
// must produce NOTHING rather than a surprising guess.
func TestCollectionSlugCandidates(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{in: "spec", want: []string{"specs"}},
		{in: "specs", want: []string{"specss", "spec"}},
		{in: "", want: nil},
		{in: "   ", want: nil},
		// A bare "s" must not produce an empty-string candidate, which would
		// query for a collection whose slug is "".
		{in: "s", want: []string{"ss"}},
		// Case folding comes FIRST: `Spec` names `spec` more closely than it
		// names `specs`, and in a workspace holding both, trying the plural
		// first would misfile the write.
		{in: "Spec", want: []string{"spec", "specs"}},
		{in: "Specs", want: []string{"specs", "specss", "spec"}},
	} {
		got := collectionSlugCandidates(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("candidates(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("candidates(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
		for _, c := range got {
			if c == "" {
				t.Errorf("candidates(%q) produced an empty candidate", tc.in)
			}
			if c == tc.in {
				t.Errorf("candidates(%q) included the input itself, which the "+
					"exact-match step already tried", tc.in)
			}
		}
	}
}

// Codex round 1. Bulk move resolved the target for the actual move but kept
// the caller's raw slug for the comparison that decides whether this IS a
// move, for the activity metadata, and for the SSE scope the arrival event is
// addressed to. Reachable only because the resolver made `spec` succeed at
// all, so the inconsistency arrived with this change.
func TestResolveItemCollectionSlug_BulkMoveCanonicalizesTheTarget(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	makeCollection(t, srv, ws, "Specs", "specs", "SPEC")

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/tasks/items",
		map[string]any{"title": "bulk movable"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk",
		map[string]any{"op": "move", "ids": []string{item.ID}, "collection": "spec"})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk move to singular `spec` = %d: %s", rr.Code, rr.Body.String())
	}

	if got := itemsIn(t, srv, ws, "specs"); len(got) != 1 || got[0] != "bulk movable" {
		t.Fatalf("items in `specs` after bulk move = %v, want [bulk movable]", got)
	}

	// The activity trail must name the collection the item actually landed in.
	// A `to_collection` of "spec" points at nothing a reader can look up.
	acts, err := srv.store.ListDocumentActivity(item.ID, models.ActivityListParams{Limit: 20})
	if err != nil {
		t.Fatalf("list activity: %v", err)
	}
	var sawMove bool
	for _, a := range acts {
		if a.Action != "moved" {
			continue
		}
		sawMove = true
		if !strings.Contains(a.Metadata, `"to_collection":"specs"`) {
			t.Errorf("move activity metadata = %s, want to_collection specs (the "+
				"resolved slug, not the caller's input)", a.Metadata)
		}
	}
	if !sawMove {
		t.Error("no 'moved' activity recorded — the assertion above is vacuous " +
			"without one, and a raw-slug comparison can miscategorise the op")
	}
}

// The local-first index filters by exact slug too, so a direct API consumer
// passing a singular got an empty index rather than an error.
func TestResolveItemCollectionSlug_ItemsIndexAcceptsTheSingular(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	makeCollection(t, srv, ws, "Specs", "specs", "SPEC")
	if rr := createItemIn(t, srv, ws, "specs", "indexed"); rr.Code != http.StatusCreated {
		t.Fatalf("seed item: %d %s", rr.Code, rr.Body)
	}

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items-index?collection=spec", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("items-index via singular = %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "indexed") {
		t.Errorf("items-index?collection=spec returned no rows: %s", rr.Body.String())
	}
}

// Codex round 2. The cross-workspace copy's `target_collection` is user-typed
// and was wired to the resolver, but nothing exercised it — every existing
// copy test passes an exact slug, so reverting that call site would have gone
// unnoticed. Drives BOTH halves (preflight and the mutating copy), because
// they resolve the destination separately and a preflight that accepts a name
// the copy then rejects is the worse failure of the two.
func TestResolveItemCollectionSlug_CrossWorkspaceCopyAcceptsTheSingular(t *testing.T) {
	f := newCopyPreflightFixture(t)

	// A destination-only collection whose slug is plural, so `spec` resolves
	// solely through the fallback under test.
	// A permissive schema on purpose: the fixture's dstSchemaJSON has required
	// fields the source item does not carry, and a validation rejection would
	// mask the resolution result this test is about.
	specs, err := f.srv.store.CreateCollection(f.wsB.ID, models.CollectionCreate{
		Name: "Specs", Slug: "specs", Prefix: "SPEC",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection(Specs): %v", err)
	}
	if specs.Slug != "specs" {
		t.Fatalf("fixture never armed: destination collection slug = %q, want specs", specs.Slug)
	}

	both := f.callBoth(f.owner, reqOpts{}, map[string]any{
		"target_workspace":  f.wsB.Slug,
		"target_collection": "spec",
	})

	if both.pre.Code != http.StatusOK {
		t.Errorf("preflight with singular `spec` = %d: %s",
			both.pre.Code, both.pre.Body.String())
	}
	if both.copy.Code != http.StatusOK && both.copy.Code != http.StatusCreated {
		t.Errorf("copy with singular `spec` = %d: %s",
			both.copy.Code, both.copy.Body.String())
	}

	// The copy must land in the resolved collection, not merely return 200.
	items, err := f.srv.store.ListItems(f.wsB.ID, models.ItemListParams{
		CollectionIDs: []string{specs.ID},
	})
	if err != nil {
		t.Fatalf("list destination items: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("destination `specs` holds %d items, want the 1 copied", len(items))
	}
}

// Codex round 3 P1. A case-only difference must resolve to the collection it
// case-insensitively names, not to that name's plural. In a workspace holding
// both `spec` and `specs`, `Spec` is `spec` — trying pluralization first would
// misfile the write into `specs`, which is the same failure the
// exact-match-wins rule prevents, reached by a different route.
func TestResolveItemCollectionSlug_CaseFoldBeatsPluralization(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	makeCollection(t, srv, ws, "Spec", "spec", "SPC")
	makeCollection(t, srv, ws, "Specs", "specs", "SPEC")

	if rr := createItemIn(t, srv, ws, "Spec", "cased"); rr.Code != http.StatusCreated {
		t.Fatalf("create via `Spec` = %d: %s", rr.Code, rr.Body)
	}

	if got := itemsIn(t, srv, ws, "spec"); len(got) != 1 || got[0] != "cased" {
		t.Errorf("items in `spec` = %v, want [cased] — `Spec` names `spec`, "+
			"not its plural", got)
	}
	if got := itemsIn(t, srv, ws, "specs"); len(got) != 0 {
		t.Errorf("items in `specs` = %v, want none — the write was misfiled into "+
			"the plural", got)
	}
}

// Codex round 4 P1 — a VISIBILITY REGRESSION this branch introduced and then
// reverted, pinned so it cannot come back.
//
// The collection-item list was briefly "pinned" by setting
// params.CollectionIDs to the resolved collection, to close a slug-reuse race.
// CollectionIDs is not a scoping filter: it and ItemIDs are a PERMISSION PAIR
// that the store combines with OR ("in a fully-granted collection, OR
// specifically granted"). So pinning it while the item-grant branch set
// ItemIDs rewrote the caller's grants as
// `collection_id IN (this) OR id IN (granted)` — handing a caller whose only
// claim on the collection is ONE item grant every item in it.
//
// Every other test in this file uses an unrestricted owner, which is exactly
// why none of them caught it.
func TestCollectionItemList_ItemGrantOnly_DoesNotLeakSiblings(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)
	f.grantHiddenItemToOwner(t)

	// A sibling in the hidden collection that the caller has NO grant on. If
	// the list leaks, this is what it leaks.
	sibling, err := f.srv.store.CreateItem(f.ws.ID, f.hiddenColl.ID, models.ItemCreate{
		Title: "Sibling the caller may not see", Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("create sibling: %v", err)
	}

	path := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug + "/items"
	for _, tc := range []struct {
		name string
		rr   func() *httptest.ResponseRecorder
	}{
		{"bearer", func() *httptest.ResponseRecorder {
			return doRequestWithHeaders(f.srv, "GET", path, nil, f.bearerHeaders())
		}},
		{"session", func() *httptest.ResponseRecorder {
			return doRequestWithCookie(f.srv, "GET", path, nil, f.sessionToken)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := tc.rr()
			// Whether the endpoint 404s or returns the granted item only is a
			// pre-existing product decision; the invariant under test is that
			// the ungranted sibling never appears.
			if rr.Code != http.StatusOK && rr.Code != http.StatusNotFound {
				t.Fatalf("list hidden collection = %d: %s", rr.Code, rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), sibling.ID) ||
				strings.Contains(rr.Body.String(), "Sibling the caller may not see") {
				t.Errorf("an item-grant-only caller received a sibling they have no "+
					"grant on — the collection leaked: %s", rr.Body.String())
			}
		})
	}
}

// Codex round 4 P2 — the bulk endpoint must answer "collection does not exist"
// and "collection exists but you cannot see it" the SAME way. Answering them
// with different HTTP statuses turns the endpoint into an existence oracle: a
// restricted caller probes slugs and learns which collections they are not
// allowed to know about.
func TestBulkMove_HiddenAndUnknownTargetsAreIndistinguishable(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)

	// An item the restricted caller CAN see, so the request gets far enough
	// for the target to matter.
	path := "/api/v1/workspaces/" + f.ws.Slug + "/items/bulk"

	hidden := doRequestWithHeaders(f.srv, "POST", path, map[string]any{
		"op": "move", "ids": []string{f.visibleItem.ID}, "collection": f.hiddenColl.Slug,
	}, f.bearerHeaders())

	unknown := doRequestWithHeaders(f.srv, "POST", path, map[string]any{
		"op": "move", "ids": []string{f.visibleItem.ID}, "collection": "no-such-collection",
	}, f.bearerHeaders())

	if hidden.Code != unknown.Code {
		t.Errorf("hidden target -> %d, unknown target -> %d; the two must be "+
			"indistinguishable or the endpoint reports which hidden collections exist\n"+
			"hidden: %s\nunknown: %s",
			hidden.Code, unknown.Code, hidden.Body.String(), unknown.Body.String())
	}
	// Status alone is not the whole channel: an implementation that returned
	// both inside a 200 envelope but with different error codes or messages
	// would still be an oracle. Compare the per-item failure shape too
	// (codex round 5).
	if normalizeBulkFailures(t, hidden.Body.String()) != normalizeBulkFailures(t, unknown.Body.String()) {
		t.Errorf("hidden and unknown targets produced different failure envelopes:\n"+
			"hidden: %s\nunknown: %s", hidden.Body.String(), unknown.Body.String())
	}

	// And neither may actually move the item.
	item, err := f.srv.store.GetItem(f.visibleItem.ID)
	if err != nil || item == nil {
		t.Fatalf("reload item: %v", err)
	}
	if item.CollectionID != f.visibleColl.ID {
		t.Errorf("item moved out of its collection despite both targets being refused")
	}
}

// normalizeBulkFailures reduces a bulk response to the per-item failure shape
// a probing caller could observe — codes and messages, with item ids stripped
// since those legitimately differ between two requests.
func normalizeBulkFailures(t *testing.T, body string) string {
	t.Helper()
	// Field names mirror bulkItemFailure exactly. An invented name here would
	// silently decode to the empty string and make every failure normalize
	// identically — the message half of this comparison was dead for exactly
	// that reason until codex round 6 caught it.
	var resp struct {
		Failed []struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		} `json:"failed"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		// A non-JSON body (an error page, say) is compared verbatim rather
		// than silently normalizing to the empty string, which would make
		// two different errors look identical.
		return "unparsed:" + body
	}
	parts := make([]string, 0, len(resp.Failed))
	for _, f := range resp.Failed {
		parts = append(parts, f.Code+"|"+f.Error)
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}
