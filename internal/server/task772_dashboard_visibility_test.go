package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-772: the dashboard shows a caller only what they may see, in EVERY
// section, for the three ways a caller's view can be narrowed. The existing
// coverage asserted one section (Summary.ByCollection, for a restricted admin
// over a bearer) and two reminder-driven ones for a granted guest; this pins
// summary, active items, active plans, attention and suggested_next for:
//
//  1. a plain member with collection access "specific" (2 of 4 collections),
//  2. a guest holding item grants only,
//  3. a guest holding a collection grant AND an item grant in another
//     collection, both paths at once.
//
// Every hidden item is shaped to land in every section if it were visible
// (in progress, open + high priority + overdue), and every leak check reads
// the whole serialized response, so a hidden title in any field of any
// section fails it. Each case also asserts the VISIBLE side per section,
// because a build that filtered everything would pass the leak check.

const t772Schema = `{"fields":[` +
	`{"key":"status","type":"select","options":["open","in-progress","active","done"],"default":"open"},` +
	`{"key":"priority","type":"select","options":["low","high"]},` +
	`{"key":"due_date","type":"date"}]}`

// An item that, if visible, appears in active items (in progress) and in
// attention (overdue) and suggested_next (in progress).
const t772Busy = `{"status":"in-progress","priority":"high","due_date":"2000-01-01"}`

// An item that, if visible, appears in attention (overdue) and suggested_next
// (open + high + overdue), but not in active items.
const t772Open = `{"status":"open","priority":"high","due_date":"2000-01-01"}`

type t772World struct {
	srv   *Server
	ws    *models.Workspace
	owner *models.User
	colls map[string]*models.Collection
	items map[string]*models.Item
	plan  *models.Item
}

func t772Setup(t *testing.T) *t772World {
	t.Helper()
	srv := testServer(t)
	owner := mustUser(t, srv, "t772-owner@example.com", "t772owner", "")
	ws := mustWorkspace(t, srv, "T772 Dash", owner.ID)
	w := &t772World{srv: srv, ws: ws, owner: owner, colls: map[string]*models.Collection{}, items: map[string]*models.Item{}}
	for _, c := range []struct{ name, slug, prefix string }{
		{"Tasks", "tasks", "T"}, {"Plans", "plans", "P"}, {"Ideas", "ideas", "I"}, {"Bugs", "bugs", "B"},
	} {
		coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: c.name, Slug: c.slug, Prefix: c.prefix, Schema: t772Schema})
		if err != nil {
			t.Fatalf("CreateCollection(%s): %v", c.slug, err)
		}
		w.colls[c.slug] = coll
	}
	mk := func(coll, title, fields string) *models.Item {
		it, err := srv.store.CreateItem(ws.ID, w.colls[coll].ID, models.ItemCreate{Title: title, Fields: fields})
		if err != nil {
			t.Fatalf("CreateItem(%s): %v", title, err)
		}
		w.items[title] = it
		return it
	}
	mk("tasks", "T772 task busy", t772Busy)
	mk("tasks", "T772 task open", t772Open)
	mk("ideas", "T772 idea busy", t772Busy)
	mk("ideas", "T772 idea open", t772Open)
	mk("bugs", "T772 bug busy", t772Busy)
	mk("bugs", "T772 bug open", t772Open)
	w.plan = mk("plans", "T772 plan", `{"status":"active"}`)
	// The plan's children span collections, so its progress counts only the
	// children the caller can see.
	for _, child := range []string{"T772 task busy", "T772 idea busy", "T772 bug busy"} {
		if _, err := srv.store.SetParentLink(ws.ID, w.items[child].ID, w.plan.ID, owner.ID); err != nil {
			t.Fatalf("SetParentLink(%s): %v", child, err)
		}
	}
	return w
}

// dashboardAs signs the user in through a real cookie session and reads the
// dashboard through the full middleware chain.
func (w *t772World) dashboardAs(t *testing.T, u *models.User) (DashboardResponse, string) {
	t.Helper()
	tok, err := w.srv.store.CreateSession(u.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	rr := doRequestWithCookie(w.srv, "GET", "/api/v1/workspaces/"+w.ws.Slug+"/dashboard", nil, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard: %d %s", rr.Code, rr.Body.String())
	}
	var resp DashboardResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp, rr.Body.String()
}

// assertView checks that every `hidden` title is absent from the whole body,
// and that each `visible` title reaches the sections its shape belongs in.
func assertView(t *testing.T, resp DashboardResponse, body string, visible, hidden []string, wantPlanTasks int, planVisible bool) {
	t.Helper()
	for _, h := range hidden {
		if strings.Contains(body, h) {
			t.Errorf("hidden item %q appears in the dashboard: %s", h, body)
		}
	}
	has := func(titles []string, want string) bool {
		for _, s := range titles {
			if s == want {
				return true
			}
		}
		return false
	}
	var active, attention, suggested []string
	for _, a := range resp.ActiveItems {
		active = append(active, a.Title)
	}
	for _, a := range resp.Attention {
		attention = append(attention, a.ItemTitle)
	}
	for _, s := range resp.SuggestedNext {
		suggested = append(suggested, s.ItemTitle)
	}
	for _, v := range visible {
		busy := strings.HasSuffix(v, "busy")
		if busy && !has(active, v) {
			t.Errorf("visible in-progress item %q missing from active_items %v", v, active)
		}
		if !has(attention, v) {
			t.Errorf("visible overdue item %q missing from attention %v", v, attention)
		}
	}
	// suggested_next is capped at 3 (maxSuggestions) and may also offer a
	// visible plan, so its count is not pinned; what is pinned is that it
	// offers something, and only things the caller can see.
	visibleSet := append([]string{}, visible...)
	if planVisible {
		visibleSet = append(visibleSet, "T772 plan")
	}
	if len(suggested) == 0 {
		t.Errorf("suggested_next is empty; a filter that dropped everything would pass the leak check")
	}
	for _, s := range suggested {
		if !has(visibleSet, s) {
			t.Errorf("suggested_next offers %q, which is not a visible item", s)
		}
	}
	if resp.Summary.TotalItems != len(visible)+boolInt(planVisible) {
		t.Errorf("summary total = %d, want %d (visible items%s)", resp.Summary.TotalItems, len(visible)+boolInt(planVisible), map[bool]string{true: " + the plan", false: ""}[planVisible])
	}
	if !planVisible {
		if len(resp.ActivePlans) != 0 {
			t.Errorf("active_plans should be empty: %+v", resp.ActivePlans)
		}
		return
	}
	if len(resp.ActivePlans) != 1 || resp.ActivePlans[0].Title != "T772 plan" {
		t.Fatalf("active_plans = %+v, want the one plan", resp.ActivePlans)
	}
	if resp.ActivePlans[0].TaskCount != wantPlanTasks {
		t.Errorf("plan progress counts %d children, want %d (only the visible ones)", resp.ActivePlans[0].TaskCount, wantPlanTasks)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CONTROL: the owner sees everything, so every section has the hidden items
// in it for the others, and the plan counts all three children.
func TestTASK772_Owner_SeesEveryItemEverywhere(t *testing.T) {
	t.Parallel()
	w := t772Setup(t)
	resp, body := w.dashboardAs(t, w.owner)
	all := []string{"T772 task busy", "T772 task open", "T772 idea busy", "T772 idea open", "T772 bug busy", "T772 bug open"}
	assertView(t, resp, body, all, nil, 3, true)
}

// Case 1: a plain member restricted to Tasks and Plans.
func TestTASK772_RestrictedMember_SeesOnlyTheGrantedCollections(t *testing.T) {
	t.Parallel()
	w := t772Setup(t)
	member := mustUser(t, w.srv, "t772-member@example.com", "t772member", "")
	if err := w.srv.store.AddWorkspaceMember(w.ws.ID, member.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := w.srv.store.SetMemberCollectionAccess(w.ws.ID, member.ID, "specific", []string{w.colls["tasks"].ID, w.colls["plans"].ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	resp, body := w.dashboardAs(t, member)
	assertView(t, resp, body,
		[]string{"T772 task busy", "T772 task open"},
		[]string{"T772 idea busy", "T772 idea open", "T772 bug busy", "T772 bug open"},
		1, true)
	for slug := range resp.Summary.ByCollection {
		if slug != "tasks" && slug != "plans" {
			t.Errorf("summary names a hidden collection %q", slug)
		}
	}
}

// Case 2: a guest with item grants only, one in Tasks and one in Ideas. The
// siblings in the same collections must not leak, and the plan, which the
// guest was not granted, does not appear at all.
func TestTASK772_ItemGrantGuest_SeesOnlyTheGrantedItems(t *testing.T) {
	t.Parallel()
	w := t772Setup(t)
	guest := mustUser(t, w.srv, "t772-guest@example.com", "t772guest", "")
	for _, title := range []string{"T772 task busy", "T772 idea open"} {
		if _, err := w.srv.store.CreateItemGrant(w.ws.ID, w.items[title].ID, guest.ID, "view", w.owner.ID); err != nil {
			t.Fatalf("CreateItemGrant(%s): %v", title, err)
		}
	}
	resp, body := w.dashboardAs(t, guest)
	assertView(t, resp, body,
		[]string{"T772 task busy", "T772 idea open"},
		[]string{"T772 task open", "T772 idea busy", "T772 bug busy", "T772 bug open", "T772 plan"},
		0, false)
}

// Case 3: a guest with a collection grant on Plans and Bugs, plus an item
// grant on one Ideas item. Both paths at once: everything in the granted
// collections, the one granted idea, nothing else, and the plan's progress
// counts the children the guest can see by either path.
func TestTASK772_MixedGuest_CollectionAndItemGrantsCooperate(t *testing.T) {
	t.Parallel()
	w := t772Setup(t)
	guest := mustUser(t, w.srv, "t772-mixed@example.com", "t772mixed", "")
	for _, slug := range []string{"plans", "bugs"} {
		if _, err := w.srv.store.CreateCollectionGrant(w.ws.ID, w.colls[slug].ID, guest.ID, "view", w.owner.ID); err != nil {
			t.Fatalf("CreateCollectionGrant(%s): %v", slug, err)
		}
	}
	if _, err := w.srv.store.CreateItemGrant(w.ws.ID, w.items["T772 idea busy"].ID, guest.ID, "view", w.owner.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	resp, body := w.dashboardAs(t, guest)
	// The plan's children are task busy (hidden), idea busy (item grant)
	// and bug busy (collection grant): two count.
	assertView(t, resp, body,
		[]string{"T772 bug busy", "T772 bug open", "T772 idea busy"},
		[]string{"T772 task busy", "T772 task open", "T772 idea open"},
		2, true)
}
