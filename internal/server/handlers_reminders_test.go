package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Reminder HTTP + poll-surface tests (IDEA-2641).

const (
	pastInstant   = "2020-01-01T00:00:00Z"
	futureInstant = "2099-01-01T00:00:00Z"
)

func armViaAPI(t *testing.T, srv *Server, wsSlug string, item models.Item, at string) models.Reminder {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/items/"+item.Slug+"/reminders",
		map[string]string{"remind_at": at})
	if rr.Code != http.StatusCreated {
		t.Fatalf("arm reminder: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var r models.Reminder
	parseJSON(t, rr, &r)
	return r
}

// TestArmRejectsABareDate. The `date` schema type accepts YYYY-MM-DD, so a
// caller will try it here. It is refused rather than assumed to mean midnight:
// a bare date names a 24-hour span, and picking an hour inside it would be the
// server inventing a time the user did not choose and then firing at it.
func TestArmRejectsABareDate(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Ship it", "fields": `{"status":"open"}`,
	})

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/reminders",
		map[string]string{"remind_at": "2026-08-01"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a bare date, got %d: %s", rr.Code, rr.Body.String())
	}
	// The message has to name the accepted form, or the refusal costs the
	// caller a guess instead of a round trip.
	if !strings.Contains(rr.Body.String(), "RFC3339") {
		t.Errorf("refusal does not name the accepted form: %s", rr.Body.String())
	}
}

// TestAckBeforeFireIsAConflict. "Nothing happened" is the same response for an
// armed reminder (too early) and an already-acked one (already done), and
// those need opposite reactions from the caller — so they get different codes.
func TestAckBeforeFireIsAConflict(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Ship it", "fields": `{"status":"open"}`,
	})
	r := armViaAPI(t, srv, slug, item, futureInstant)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/reminders/"+r.ID+"/ack", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 acking an armed reminder, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestFiredReminderReachesTheSuggestionSurface — the mandatory poll path. On
// an instance with no webhook dispatcher (the common self-hosted shape) the
// outbox acks the event instantly, so this list is the entire delivery.
func TestFiredReminderReachesTheSuggestionSurface(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	// COMPETING CANDIDATES ARE THE POINT of this fixture. With only the
	// reminder's own item in the workspace, the reminder lands at index 0
	// whether it is prepended or appended — the assertion below would hold
	// against an implementation that appends, and the ordering claim would be
	// untested. Three in-progress high-priority tasks fill the cap, so a
	// reminder that is merely appended ends up fourth and invisible.
	for _, title := range []string{"Busy one", "Busy two", "Busy three"} {
		createItem(t, srv, slug, "tasks", map[string]interface{}{
			"title": title, "fields": `{"status":"in-progress","priority":"high"}`,
		})
	}
	item := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Revisit the schema", "fields": `{"status":"open","priority":"low"}`,
	})
	armViaAPI(t, srv, slug, item, pastInstant)

	srv.runReminderTick()

	resp := getDashboard(t, srv, slug)
	if len(resp.PendingReminders) != 1 {
		t.Fatalf("expected 1 pending reminder, got %d", len(resp.PendingReminders))
	}
	if resp.PendingReminders[0].ItemRef != item.Ref {
		t.Errorf("pending reminder names %q, want %q", resp.PendingReminders[0].ItemRef, item.Ref)
	}
	if len(resp.SuggestedNext) == 0 || resp.SuggestedNext[0].ItemTitle != "Revisit the schema" {
		t.Fatalf("a fired reminder must lead suggested_next; got %+v", resp.SuggestedNext)
	}
	if !strings.HasPrefix(resp.SuggestedNext[0].Reason, "REMINDER due") {
		t.Errorf("suggestion reason = %q, want it to say a reminder fired", resp.SuggestedNext[0].Reason)
	}
}

// TestFiredReminderOnADoneItemIsFilteredNotAcked is the lead's pin, and the
// two halves are the whole point: ABSENT from the surface, PRESENT in the
// table. Acking on terminal status would couple every status write to reminder
// state and would consume a reminder armed to fire after the work was done.
//
// Asserting only the absence would pass against an implementation that acked
// the row, which is the behaviour this exists to forbid.
func TestFiredReminderOnADoneItemIsFilteredNotAcked(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Finished work", "fields": `{"status":"open"}`,
	})
	r := armViaAPI(t, srv, slug, item, pastInstant)
	srv.runReminderTick()

	// Sanity: it IS on the surface while the item is open. Without this leg
	// the test would pass on a build where reminders never surface at all.
	if len(getDashboard(t, srv, slug).PendingReminders) != 1 {
		t.Fatal("the reminder is not on the surface even before the item is done")
	}

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug,
		map[string]interface{}{"fields": `{"status":"done"}`})
	if rr.Code != http.StatusOK {
		t.Fatalf("mark done: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := getDashboard(t, srv, slug)
	if len(resp.PendingReminders) != 0 {
		t.Errorf("a reminder on a completed item must not be shown, got %d", len(resp.PendingReminders))
	}
	for _, sug := range resp.SuggestedNext {
		if sug.ItemTitle == "Finished work" {
			t.Error("a completed item is suggested via its reminder")
		}
	}

	// PRESENT IN THE TABLE, and still unacknowledged — the user's intent is
	// preserved and no status write touched the row.
	stored, err := srv.store.GetReminder(item.WorkspaceID, r.ID)
	if err != nil {
		t.Fatalf("GetReminder: %v", err)
	}
	if stored == nil {
		t.Fatal("the reminder row was removed; filtering must not delete")
	}
	if stored.AckedAt != nil {
		t.Error("the reminder was ACKED by the status change; only an explicit ack may do that")
	}
	if !stored.PendingAck() {
		t.Error("the reminder should still be fired-and-unacknowledged in the table")
	}
}

// TestRearmReturnsAReminderToTheArmedSet drives the whole loop through HTTP:
// fired, acknowledged, re-armed, fires again.
func TestRearmReturnsAReminderToTheArmedSet(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Ship it", "fields": `{"status":"open"}`,
	})
	r := armViaAPI(t, srv, slug, item, pastInstant)
	srv.runReminderTick()

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/reminders/"+r.ID+"/ack", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("ack: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(getDashboard(t, srv, slug).PendingReminders) != 0 {
		t.Fatal("an acknowledged reminder must leave the surface")
	}

	// Re-arm into the past and tick again: it must come back.
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/reminders/"+r.ID,
		map[string]string{"remind_at": pastInstant})
	if rr.Code != http.StatusOK {
		t.Fatalf("re-arm: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	srv.runReminderTick()
	if len(getDashboard(t, srv, slug).PendingReminders) != 1 {
		t.Error("a re-armed reminder must fire and reappear on the surface")
	}
}

// TestTickIsQuietWhenNothingIsDue is the negative control for the tick itself.
// Without it, a tick that fired EVERYTHING would satisfy every test above.
func TestTickIsQuietWhenNothingIsDue(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Ship it", "fields": `{"status":"open"}`,
	})
	armViaAPI(t, srv, slug, item, futureInstant)

	srv.runReminderTick()

	if got := len(getDashboard(t, srv, slug).PendingReminders); got != 0 {
		t.Errorf("a tick fired %d reminder(s) whose instant has not arrived", got)
	}
}

// TestPendingRemindersRespectItemGrants — codex round 1, P1.
//
// Every other dashboard section reads `allItems`, which the store already
// scoped to the caller's collections AND their granted item ids. The pending-
// reminder list is a direct workspace-wide query, so it inherited none of that:
// a guest holding a grant on ONE item could read the refs and titles of every
// other item in the collection through its reminders — an item-level leak
// wearing a notification's clothes.
//
// The two items live in the SAME collection deliberately. A collection-level
// filter is already applied, so putting them in different collections would
// make the test pass against the unfixed code and prove nothing.
//
// MUTANT: remove the isItemVisibleToGuest call and the guest sees both.
func TestPendingRemindersRespectItemGrants(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	owner := mustUser(t, srv, "reminder-owner@example.com", "reminderowner", "")
	ws := mustWorkspace(t, srv, "Reminders", owner.ID)
	coll := mustCollection(t, srv, ws.ID, "Tasks")

	granted := mustItem(t, srv, ws.ID, coll.ID, "Granted item")
	secret := mustItem(t, srv, ws.ID, coll.ID, "Not for the guest")

	guest := mustUser(t, srv, "reminder-guest@example.com", "reminderguest", "")
	if _, err := srv.store.CreateItemGrant(ws.ID, granted.ID, guest.ID, "edit", owner.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}

	for _, it := range []*models.Item{granted, secret} {
		if _, err := srv.store.CreateReminder(ws.ID, it.ID, pastInstant); err != nil {
			t.Fatalf("CreateReminder: %v", err)
		}
	}
	srv.runReminderTick()

	req := httptest.NewRequest("GET", "/api/v1/workspaces/"+ws.Slug+"/dashboard", nil)
	ctx := WithCurrentUser(req.Context(), guest)
	ctx = contextWithWorkspaceRoleForTest(ctx, "guest")
	ctx = contextWithResolvedWorkspaceIDForTest(ctx, ws.ID)
	req = req.WithContext(ctx)

	resp, err := srv.buildDashboardResponse(ws.ID, req)
	if err != nil {
		t.Fatalf("buildDashboardResponse: %v", err)
	}

	// The guest must see their own item's reminder — without this leg a build
	// that filtered EVERYTHING would pass the leak assertion below.
	var sawGranted bool
	for _, pr := range resp.PendingReminders {
		if pr.ItemTitle == "Not for the guest" {
			t.Error("a guest read another item's reminder; the pending list is not item-filtered")
		}
		if pr.ItemTitle == "Granted item" {
			sawGranted = true
		}
	}
	if !sawGranted {
		t.Error("the guest cannot see the reminder on the item they were granted")
	}
	for _, sug := range resp.SuggestedNext {
		if sug.ItemTitle == "Not for the guest" {
			t.Error("the leak reaches suggested_next as well")
		}
	}
}

// TestFiredReminderSuggestionCarriesItsID — codex round 1, P2.
//
// The docs tell an agent to acknowledge what it sees in next/ready, and the
// payload did not carry the handle: a stateless poller could read the reminder
// and had no way to retire it, so it would be shown the same item forever.
//
// MUTANT: drop the ReminderID assignment and this fails while every other
// reminder test stays green — the id is invisible to all of them.
func TestFiredReminderSuggestionCarriesItsID(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Revisit the schema", "fields": `{"status":"open"}`,
	})
	armed := armViaAPI(t, srv, slug, item, pastInstant)
	srv.runReminderTick()

	resp := getDashboard(t, srv, slug)
	if len(resp.SuggestedNext) == 0 {
		t.Fatal("no suggestions")
	}
	if resp.SuggestedNext[0].ReminderID != armed.ID {
		t.Fatalf("suggestion carries reminder_id %q, want %q — an agent reading this surface cannot ack",
			resp.SuggestedNext[0].ReminderID, armed.ID)
	}

	// And the id it carries actually works, rather than merely being present:
	// a wrong-but-populated id would satisfy an equality check against itself.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/reminders/"+resp.SuggestedNext[0].ReminderID+"/ack", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("acking the id from the suggestion failed: %d %s", rr.Code, rr.Body.String())
	}
	if len(getDashboard(t, srv, slug).PendingReminders) != 0 {
		t.Error("the reminder survived an ack using the id the surface handed out")
	}
}

// TestReminderSuggestionsAreCapped — codex round 3.
//
// suggested_next is a recommendation list of three; prepending an unbounded
// number of reminders turns it into a second inbox and buries the suggestions
// it exists to make.
//
// The fixture needs MORE reminders than the cap, which is the leg the first
// version of the prepend test lacked: with one reminder, capped and uncapped
// are the same list. That is the second time a single-item fixture hid an
// ordering-or-count property in this file.
//
// MUTANT: remove the cap and eight suggestions come back.
func TestReminderSuggestionsAreCapped(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	for i := 0; i < 8; i++ {
		item := createItem(t, srv, slug, "tasks", map[string]interface{}{
			"title": fmt.Sprintf("Task %d", i), "fields": `{"status":"open"}`,
		})
		armViaAPI(t, srv, slug, item, pastInstant)
	}
	srv.runReminderTick()

	resp := getDashboard(t, srv, slug)
	var reminderSuggestions int
	for _, sug := range resp.SuggestedNext {
		if sug.ReminderID != "" {
			reminderSuggestions++
		}
	}
	if reminderSuggestions != 5 {
		t.Errorf("suggested_next carries %d reminder entries, want the cap of 5", reminderSuggestions)
	}
	// All eight stay addressable in the list that is not a recommendation.
	if len(resp.PendingReminders) != 8 {
		t.Errorf("pending_reminders holds %d, want all 8 — the cap is on the recommendation, not the data", len(resp.PendingReminders))
	}
}

// TestOrdinarySuggestionsCarryNoReminderID is the negative control: the field
// is omitempty and must stay empty on a plain task suggestion, or a consumer
// switching on its presence would try to ack a task.
func TestOrdinarySuggestionsCarryNoReminderID(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Just a task", "fields": `{"status":"in-progress","priority":"high"}`,
	})

	resp := getDashboard(t, srv, slug)
	if len(resp.SuggestedNext) == 0 {
		t.Fatal("no suggestions")
	}
	for _, sug := range resp.SuggestedNext {
		if sug.ReminderID != "" {
			t.Errorf("a plain task suggestion carries reminder_id %q", sug.ReminderID)
		}
	}
}

// TestStartReminderTickFiresOnATick binds the LOOP to the work (CONVE-19).
//
// Every other test here calls runReminderTick directly, which vouches for the
// component and says nothing about whether anything ever calls it. That is the
// exact gap this convention names, and it is the one I keep falling into: a
// tick that is never started is indistinguishable, from those tests, from one
// that is.
//
// Driven through the injectable tick channel so the assertion is pinned to a
// SPECIFIC pass rather than racing a free-running 30-second ticker.
//
// MUTANT: make StartReminderTick's goroutine ignore its channel (or drop the
// runReminderTick call from the select) and this fails while every direct-call
// test stays green.
func TestStartReminderTickFiresOnATick(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Wake me", "fields": `{"status":"open"}`,
	})
	armViaAPI(t, srv, slug, item, pastInstant)

	ticks := make(chan time.Time, 1)
	srv.SetReminderTickChannel(ticks)
	srv.StartReminderTick()
	defer srv.stopReminderTick()

	ticks <- time.Now()

	// Poll rather than sleep a fixed interval: the pass is asynchronous, and a
	// fixed sleep is either flaky or slow. Bounded so a tick that never runs
	// fails rather than hanging the suite.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(getDashboard(t, srv, slug).PendingReminders) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the started tick never fired an armed reminder — the loop is not bound to the work")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestStartReminderTickIsIdempotent: a second Start must not spawn a second
// loop, or Stop() would leave one running and the BUG-842 drain invariant
// would be false for this sweeper.
func TestStartReminderTickIsIdempotent(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ticks := make(chan time.Time, 1)
	srv.SetReminderTickChannel(ticks)
	srv.StartReminderTick()
	srv.StartReminderTick()
	srv.stopReminderTick()
	// A second stop must be safe too — Stop() runs unconditionally.
	srv.stopReminderTick()
}

// TestPendingRemindersAreNotStarvedByCompletedItems — codex round 4, P1.
//
// This is the defect the ROUND-3 fix introduced, and it is the same shape as
// the round-1 one it had just removed from the fire path: a bounded window
// whose rows are discarded ABOVE the bound hides everything behind them
// forever, with no continuation to reach it. Bounding is only safe when the
// discarding happens before the bound.
//
// The fixture puts more terminal-item reminders than the window (50) AHEAD of
// the live one, ordered by fire time. Fewer than the window would fill from the
// first page and prove nothing.
//
// MUTANT: drop the paging loop back to a single ListPendingReminders call and
// the live reminder never appears.
func TestPendingRemindersAreNotStarvedByCompletedItems(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	owner := mustUser(t, srv, "starve-owner@example.com", "starveowner", "")
	ws := mustWorkspace(t, srv, "Starved", owner.ID)
	coll := mustCollection(t, srv, ws.ID, "Tasks")

	// Built through the store rather than the API: sixty items plus sixty
	// status writes trips the write rate limiter, and a 429 mid-fixture is a
	// test that fails for a reason unrelated to what it measures.
	for i := 0; i < 60; i++ {
		done, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
			Title:  fmt.Sprintf("Finished %d", i),
			Fields: `{"status":"done"}`,
		})
		if err != nil {
			t.Fatalf("CreateItem: %v", err)
		}
		if _, err := srv.store.CreateReminder(ws.ID, done.ID, pastInstant); err != nil {
			t.Fatalf("CreateReminder: %v", err)
		}
	}

	live, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
		Title: "Still open", Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := srv.store.CreateReminder(ws.ID, live.ID, pastInstant); err != nil {
		t.Fatalf("CreateReminder: %v", err)
	}
	srv.runReminderTick()

	req := httptest.NewRequest("GET", "/api/v1/workspaces/"+ws.Slug+"/dashboard", nil)
	req = req.WithContext(contextWithResolvedWorkspaceIDForTest(WithCurrentUser(req.Context(), owner), ws.ID))
	resp, err := srv.buildDashboardResponse(ws.ID, req)
	if err != nil {
		t.Fatalf("buildDashboardResponse: %v", err)
	}
	var found bool
	for _, pr := range resp.PendingReminders {
		if pr.ItemTitle == "Still open" {
			found = true
		}
		if strings.HasPrefix(pr.ItemTitle, "Finished ") {
			t.Fatalf("a completed item's reminder reached the surface: %s", pr.ItemTitle)
		}
	}
	if !found {
		t.Errorf("the live reminder was starved behind 60 completed ones (%d shown)", len(resp.PendingReminders))
	}
}

// TestPendingReminderScopeIsAppliedInTheQuery — codex round 4, the other half.
//
// A guest's invisible rows must not consume the window either. The granted
// item is armed LAST, so it sorts after 60 rows the guest may not see: if
// scoping ran above the bound, those 60 would fill the window and the guest's
// own reminder would be unreachable.
//
// MUTANT: drop the scope clause from the SQL and the guest sees nothing (or
// sees other people's items, which the round-1 test catches).
func TestPendingReminderScopeIsAppliedInTheQuery(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	owner := mustUser(t, srv, "scope-owner@example.com", "scopeowner", "")
	ws := mustWorkspace(t, srv, "Scoped", owner.ID)
	coll := mustCollection(t, srv, ws.ID, "Tasks")

	for i := 0; i < 60; i++ {
		other := mustItem(t, srv, ws.ID, coll.ID, fmt.Sprintf("Not yours %d", i))
		if _, err := srv.store.CreateReminder(ws.ID, other.ID, pastInstant); err != nil {
			t.Fatalf("CreateReminder: %v", err)
		}
	}
	mine := mustItem(t, srv, ws.ID, coll.ID, "Yours")
	if _, err := srv.store.CreateReminder(ws.ID, mine.ID, pastInstant); err != nil {
		t.Fatalf("CreateReminder: %v", err)
	}
	srv.runReminderTick()

	guest := mustUser(t, srv, "scope-guest@example.com", "scopeguest", "")
	if _, err := srv.store.CreateItemGrant(ws.ID, mine.ID, guest.ID, "edit", owner.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/v1/workspaces/"+ws.Slug+"/dashboard", nil)
	ctx := WithCurrentUser(req.Context(), guest)
	ctx = contextWithWorkspaceRoleForTest(ctx, "guest")
	ctx = contextWithResolvedWorkspaceIDForTest(ctx, ws.ID)
	resp, err := srv.buildDashboardResponse(ws.ID, req.WithContext(ctx))
	if err != nil {
		t.Fatalf("buildDashboardResponse: %v", err)
	}

	if len(resp.PendingReminders) != 1 || resp.PendingReminders[0].ItemTitle != "Yours" {
		var titles []string
		for _, pr := range resp.PendingReminders {
			titles = append(titles, pr.ItemTitle)
		}
		t.Fatalf("guest saw %v, want exactly the granted item's reminder", titles)
	}
	// And the truncation flag must be FALSE: the guest's own set fits, and
	// telling them to page through rows they can never see would be a lie in
	// the shape of a hint.
	if resp.PendingRemindersTruncated {
		t.Error("a guest whose whole visible set fits was told there is more")
	}
}
