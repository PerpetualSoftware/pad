package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3517: PUT /workspaces/{ws}/items/sort-order sets sort_order for many
// items at once, all or nothing, with the PATCH's per-item permission.

type sortFixture struct {
	srv      *Server
	ws       *models.Workspace
	coll     *models.Collection
	owner    *models.User
	ownerTok string
	path     string
}

func newSortFixture(t *testing.T) *sortFixture {
	t.Helper()
	srv := testServer(t)
	owner, tok := loginTestUserAs(t, srv, "sort3517@example.test", "Sort Owner", "pw-3517-abcdefgh")
	ws := mustCreateOwnedWorkspace(t, srv, "Sort 3517", owner)
	return &sortFixture{srv: srv, ws: ws, coll: mustCollection(t, srv, ws.ID, "Tasks"), owner: owner, ownerTok: tok,
		path: "/api/v1/workspaces/" + ws.Slug + "/items/sort-order"}
}

func (f *sortFixture) put(t *testing.T, tok string, entries ...any) (int, string) {
	t.Helper()
	rr := doRequestWithCookie(f.srv, http.MethodPut, f.path, map[string]any{"updates": entries}, tok)
	return rr.Code, rr.Body.String()
}

func entry(id string, n int) map[string]any { return map[string]any{"id": id, "sort_order": n} }

func (f *sortFixture) sortOrder(t *testing.T, id string) int {
	t.Helper()
	it, err := f.srv.store.GetItem(id)
	if err != nil || it == nil {
		t.Fatalf("GetItem %s: %v", id, err)
	}
	return it.SortOrder
}

func (f *sortFixture) reorderedRows(t *testing.T) []models.Activity {
	t.Helper()
	acts, err := f.srv.store.ListWorkspaceActivity(f.ws.ID, models.ActivityListParams{Action: "reordered", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return acts
}

func TestTASK3517_ReorderAppliesEveryEntryAndLogsPerItemRows(t *testing.T) {
	f := newSortFixture(t)
	a := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "a")
	b := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "b")
	c := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "c")
	seqBefore := map[string]int64{}
	for _, it := range []*models.Item{a, b, c} {
		got, _ := f.srv.store.GetItem(it.ID)
		seqBefore[it.ID] = got.Seq
	}
	unchanged := f.sortOrder(t, c.ID)

	// b by its ref, to show refs resolve; c keeps the value it holds.
	bItem, _ := f.srv.store.GetItem(b.ID)
	bItem.ComputeRef()
	code, body := f.put(t, f.ownerTok, entry(a.ID, 5), entry(bItem.Ref, 3), entry(c.ID, unchanged))
	if code != http.StatusOK {
		t.Fatalf("reorder: %d %s", code, body)
	}
	if f.sortOrder(t, a.ID) != 5 || f.sortOrder(t, b.ID) != 3 {
		t.Fatalf("sort orders a=%d b=%d, want 5 and 3", f.sortOrder(t, a.ID), f.sortOrder(t, b.ID))
	}

	var resp struct {
		Items []struct {
			ID  string `json:"id"`
			Seq int64  `json:"seq"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil || len(resp.Items) != 3 {
		t.Fatalf("response %s: %v", body, err)
	}
	for _, it := range resp.Items {
		got, _ := f.srv.store.GetItem(it.ID)
		if it.Seq != got.Seq {
			t.Errorf("%s: response seq %d, row seq %d", it.ID, it.Seq, got.Seq)
		}
		moved := it.ID != c.ID
		if moved && got.Seq <= seqBefore[it.ID] {
			t.Errorf("%s moved but its seq did not advance (%d -> %d)", it.ID, seqBefore[it.ID], got.Seq)
		}
		if !moved && got.Seq != seqBefore[it.ID] {
			t.Errorf("the unchanged item's seq moved (%d -> %d)", seqBefore[it.ID], got.Seq)
		}
	}

	// One "reordered" row per MOVED item, sharing one batch id, carrying
	// its from/to. Nothing for the item that kept its value.
	rows := f.reorderedRows(t)
	if len(rows) != 2 {
		t.Fatalf("%d reordered rows, want 2 (one per moved item)", len(rows))
	}
	batch := ""
	for _, r := range rows {
		var meta map[string]string
		if err := json.Unmarshal([]byte(r.Metadata), &meta); err != nil {
			t.Fatalf("metadata %q: %v", r.Metadata, err)
		}
		if meta["reorder_batch"] == "" || (batch != "" && meta["reorder_batch"] != batch) {
			t.Fatalf("rows do not share one reorder_batch: %v", rows)
		}
		batch = meta["reorder_batch"]
		want := map[string]string{a.ID: "5", b.ID: "3"}[r.DocumentID]
		if meta["sort_order_to"] != want || meta["sort_order_from"] == "" {
			t.Errorf("row for %s: meta %v, want to=%s and a from", r.DocumentID, meta, want)
		}
	}
}

func TestTASK3517_ReorderIsAllOrNothingOnAnUneditableItem(t *testing.T) {
	f := newSortFixture(t)
	mine := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "granted for edit")
	viewOnly := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "granted for view")
	hidden := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "not granted")

	guest, guestTok := loginTestUserAs(t, f.srv, "guest3517@example.test", "Guest", "pw-3517-abcdefgh")
	if _, err := f.srv.store.CreateItemGrant(f.ws.ID, mine.ID, guest.ID, "edit", f.owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.CreateItemGrant(f.ws.ID, viewOnly.ID, guest.ID, "view", f.owner.ID); err != nil {
		t.Fatal(err)
	}

	// A guest holding an edit grant may reorder that item: /items/bulk's
	// editor-role gate would have refused this.
	if code, body := f.put(t, guestTok, entry(mine.ID, 9)); code != http.StatusOK {
		t.Fatalf("guest with an edit grant: %d %s", code, body)
	}

	// One view-only item refuses the whole request, naming it, and the
	// editable item in the same request does not move.
	code, body := f.put(t, guestTok, entry(mine.ID, 1), entry(viewOnly.ID, 2))
	if code != http.StatusForbidden {
		t.Fatalf("view-only item: %d %s, want 403", code, body)
	}
	vo, _ := f.srv.store.GetItem(viewOnly.ID)
	vo.ComputeRef()
	if !strings.Contains(body, vo.Ref) {
		t.Errorf("refusal %s does not name %s", body, vo.Ref)
	}
	if got := f.sortOrder(t, mine.ID); got != 9 {
		t.Errorf("the editable item moved to %d in a refused request", got)
	}

	// An item the guest cannot see answers exactly as an unknown one.
	hiddenCode, hiddenBody := f.put(t, guestTok, entry(hidden.ID, 4))
	unknownCode, unknownBody := f.put(t, guestTok, entry("00000000-0000-0000-0000-000000000000", 4))
	if hiddenCode != http.StatusNotFound || hiddenCode != unknownCode || hiddenBody != unknownBody {
		t.Errorf("hidden item %d %s, unknown id %d %s: must both be the same 404", hiddenCode, hiddenBody, unknownCode, unknownBody)
	}

	// Another workspace's item answers the same way too.
	other := mustCreateOwnedWorkspace(t, f.srv, "Sort Other 3517", f.owner)
	foreign := mustItem(t, f.srv, other.ID, mustCollection(t, f.srv, other.ID, "Tasks").ID, "foreign")
	fCode, fBody := f.put(t, f.ownerTok, entry(foreign.ID, 1))
	oCode, oBody := f.put(t, f.ownerTok, entry("00000000-0000-0000-0000-000000000000", 1))
	if fCode != oCode || fBody != oBody {
		t.Errorf("another workspace's item %d %s, unknown %d %s: must match", fCode, fBody, oCode, oBody)
	}
}

func TestTASK3517_ReorderRefusesMalformedRequests(t *testing.T) {
	f := newSortFixture(t)
	a := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "a")
	got, _ := f.srv.store.GetItem(a.ID)
	got.ComputeRef()

	cases := map[string][]any{
		"empty":               {},
		"no sort_order":       {map[string]any{"id": a.ID}},
		"the same item twice": {entry(a.ID, 1), entry(got.Ref, 2)},
		"no id":               {map[string]any{"sort_order": 1}},
	}
	for name, entries := range cases {
		if code, body := f.put(t, f.ownerTok, entries...); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, code, body)
		}
	}
	tooMany := make([]any, maxSortOrderUpdates+1)
	for i := range tooMany {
		tooMany[i] = entry(a.ID, i)
	}
	if code, _ := f.put(t, f.ownerTok, tooMany...); code != http.StatusBadRequest {
		t.Errorf("over the cap: %d, want 400", code)
	}
	if f.sortOrder(t, a.ID) != got.SortOrder {
		t.Error("a refused request moved the item")
	}
}

// The store's own guard: an entry that matches no live row rolls the whole
// batch back, so a delete between the handler's checks and the write cannot
// leave a partial order.
func TestTASK3517_StoreRollsBackOnAMissingRow(t *testing.T) {
	f := newSortFixture(t)
	a := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "a")
	gone := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "gone")
	if err := f.srv.store.DeleteItem(gone.ID); err != nil {
		t.Fatal(err)
	}
	before := f.sortOrder(t, a.ID)
	_, err := f.srv.store.UpdateItemSortOrders(f.ws.ID, []store.ItemSortUpdate{
		{ItemID: a.ID, SortOrder: before + 10},
		{ItemID: gone.ID, SortOrder: 3},
	}, "batch-3517")
	if !errors.Is(err, store.ErrItemSortNotFound) {
		t.Fatalf("err = %v, want ErrItemSortNotFound", err)
	}
	if got := f.sortOrder(t, a.ID); got != before {
		t.Errorf("the rolled-back batch still moved the first item (%d -> %d)", before, got)
	}
}

// Each moved item emits item.updated in the batch, and the batch gets one
// header, so webhooks see one reorder rather than N unrelated updates.
func TestTASK3517_ReorderEmitsBatchedOutboxEvents(t *testing.T) {
	f := newSortFixture(t)
	a := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "a")
	b := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "b")
	pendingBefore, err := f.srv.store.ListPendingOutboxEvents(10000)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range pendingBefore {
		seen[e.ID] = true
	}
	if code, body := f.put(t, f.ownerTok, entry(a.ID, 7), entry(b.ID, 8)); code != http.StatusOK {
		t.Fatalf("reorder: %d %s", code, body)
	}
	after, err := f.srv.store.ListPendingOutboxEvents(10000)
	if err != nil {
		t.Fatal(err)
	}
	members, headers, batch := 0, 0, ""
	for _, e := range after {
		if seen[e.ID] {
			continue
		}
		if batch == "" {
			batch = e.BatchID
		}
		if e.BatchID == "" || e.BatchID != batch {
			t.Errorf("event %s %s has batch %q, want one shared batch", e.EventType, e.SubjectID, e.BatchID)
		}
		switch e.EventType {
		case "item.updated":
			members++
		case "item.bulk_updated":
			headers++
		}
	}
	if members != 2 || headers != 1 {
		t.Errorf("events: %d item.updated, %d headers; want 2 and 1", members, headers)
	}
}

// The dashboard's ten recent entries show a reorder as one entry, counting
// the rows the caller can see, so a long drag does not push everything else
// out (TASK-3517).
func TestTASK3517_DashboardCollapsesAReorderBatch(t *testing.T) {
	f := newSortFixture(t)
	entries := []any{}
	for i := 0; i < 12; i++ {
		it := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "card")
		entries = append(entries, entry(it.ID, 100+i))
	}
	if code, body := f.put(t, f.ownerTok, entries...); code != http.StatusOK {
		t.Fatalf("reorder: %d %s", code, body)
	}
	rr := doRequestWithCookie(f.srv, http.MethodGet, "/api/v1/workspaces/"+f.ws.Slug+"/dashboard", nil, f.ownerTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard: %d %s", rr.Code, rr.Body.String())
	}
	var dash struct {
		Recent []DashboardActivity `json:"recent_activity"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &dash); err != nil {
		t.Fatal(err)
	}
	reorders := 0
	for _, a := range dash.Recent {
		if a.Action == "reordered" {
			reorders++
			if a.ReorderCount != 12 {
				t.Errorf("reorder entry counts %d, want 12", a.ReorderCount)
			}
		}
	}
	if reorders != 1 {
		t.Errorf("%d reorder entries in recent activity, want 1: %+v", reorders, dash.Recent)
	}
}
