package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3409: when retention gives up on an event, each owner endpoint still
// owed it is counted on its hook's dropped_count; a decided endpoint is not,
// and an event under a live claim is neither deleted nor counted.
func TestTask3409_GivingUpOnAnEventCountsTheEndpointsStillOwed(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Drops")
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	owed, err := s.CreateWebhook(ws.ID, models.WebhookCreate{URL: "https://owed.example/h", Events: `["*"]`})
	if err != nil {
		t.Fatal(err)
	}
	decided, err := s.CreateWebhook(ws.ID, models.WebhookCreate{URL: "https://decided.example/h", Events: `["*"]`})
	if err != nil {
		t.Fatal(err)
	}
	// Two events: one past retention and unclaimed, one past retention but
	// under a live claim.
	if _, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Claimed"}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	rows, err := s.db.Query(s.q(`SELECT id FROM event_outbox WHERE dispatched_at IS NULL ORDER BY occurred_at, id`))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) < 2 {
		t.Fatalf("precondition: want 2 pending events, have %d", len(ids))
	}
	oldEv, claimedEv := ids[0], ids[len(ids)-1]
	long := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(s.q(`UPDATE event_outbox SET occurred_at = ?`), long); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE event_outbox SET claimed_at = ?, claimed_by = 'live' WHERE id = ?`), now(), claimedEv); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ ev, hook, status string }{
		{oldEv, owed.ID, DeliveryTransient},
		{oldEv, decided.ID, DeliveryDelivered},
		{claimedEv, owed.ID, DeliveryTransient},
	} {
		if err := s.RecordDelivery(r.ev, r.hook, r.status, "x", 1); err != nil {
			t.Fatal(err)
		}
	}

	before := time.Now().Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	leaseCutoff := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	n, err := s.PruneUndispatchedOutbox(before, leaseCutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("pruned %d events, want the old one", n)
	}
	dropped := func(id string) int {
		t.Helper()
		var c int
		if err := s.db.QueryRow(s.q(`SELECT dropped_count FROM webhooks WHERE id = ?`), id).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if got := dropped(owed.ID); got != 1 {
		t.Errorf("owed endpoint dropped_count = %d, want 1 (the old event only, not the claimed one)", got)
	}
	if got := dropped(decided.ID); got != 0 {
		t.Errorf("decided endpoint dropped_count = %d, want 0", got)
	}
	var left int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM event_outbox WHERE id = ?`), claimedEv).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Error("an event under a live claim was deleted")
	}
	// The count is visible on the API read.
	hooks, err := s.ListWebhooks(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hooks {
		if h.ID == owed.ID && h.DroppedCount != 1 {
			t.Errorf("ListWebhooks DroppedCount = %d, want 1", h.DroppedCount)
		}
	}
}
