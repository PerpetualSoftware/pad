package server

import (
	"net/http"
	"strings"
	"testing"

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
