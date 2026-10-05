package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3408 (U10c): the delivery ledger. The first terminal decision
// stands; attempts count only real attempts; a drop is counted once.
func TestTask3408_DeliveryLedger(t *testing.T) {
	f := task3408Fixture(t, "inst-ledger")
	item, err := f.s.CreateItem(f.ws.ID, f.companion.ID, models.ItemCreate{Title: "T", Fields: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	var eventID string
	if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM event_outbox WHERE subject_id = ? ORDER BY occurred_at DESC LIMIT 1`), item.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	row := func() (string, int) {
		var st string
		var n int
		if err := f.s.db.QueryRow(f.s.q(`SELECT status, attempts FROM webhook_deliveries WHERE outbox_event_id = ? AND webhook_id = ?`), eventID, f.hookID).Scan(&st, &n); err != nil {
			t.Fatal(err)
		}
		return st, n
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.s.RecordDelivery(eventID, f.hookID, DeliveryRateLimited, "cap", 0))
	must(f.s.RecordDelivery(eventID, f.hookID, DeliveryTransient, "503", 1))
	if st, n := row(); st != DeliveryTransient || n != 1 {
		t.Fatalf("pending row: %s/%d, want transient/1 (a deferral is not an attempt)", st, n)
	}
	must(f.s.RecordDelivery(eventID, f.hookID, DeliveryDelivered, "", 1))
	must(f.s.RecordDelivery(eventID, f.hookID, DeliveryTransient, "late", 1))
	if st, n := row(); st != DeliveryDelivered || n != 2 {
		t.Fatalf("after a late write: %s/%d, want delivered/2 (terminal stands)", st, n)
	}
	if dropped, err := f.s.DropDelivery(eventID, f.hookID, "old"); err != nil || dropped {
		t.Fatalf("dropping a delivered row: %v %v", dropped, err)
	}
	var count int
	must(f.s.db.QueryRow(f.s.q(`SELECT dropped_count FROM webhooks WHERE id = ?`), f.hookID).Scan(&count))
	if count != 0 {
		t.Fatalf("dropped_count %d after a refused drop", count)
	}

	// A pending row drops once, and counts once.
	if _, err := f.s.db.Exec(f.s.q(`DELETE FROM webhook_deliveries WHERE outbox_event_id = ?`), eventID); err != nil {
		t.Fatal(err)
	}
	must(f.s.RecordDelivery(eventID, f.hookID, DeliveryTransient, "503", 1))
	for i := 0; i < 2; i++ {
		if _, err := f.s.DropDelivery(eventID, f.hookID, "old"); err != nil {
			t.Fatal(err)
		}
	}
	must(f.s.db.QueryRow(f.s.q(`SELECT dropped_count FROM webhooks WHERE id = ?`), f.hookID).Scan(&count))
	if st, _ := row(); st != DeliveryDropped || count != 1 {
		t.Fatalf("drop: %s, dropped_count %d, want dropped/1", st, count)
	}

	// Owner rows: only delivered is recorded, and it is read back.
	if ok, err := f.s.OwnerDelivered(eventID, "no-hook"); err != nil || ok {
		t.Fatalf("an unrecorded owner hook: %v %v", ok, err)
	}
}
