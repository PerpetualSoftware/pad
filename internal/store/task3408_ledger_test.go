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
	if outcome, err := f.s.DropOwedDelivery(eventID, f.hookID, "item.created", f.companion.ID, now(), "old"); err != nil || outcome != "" {
		t.Fatalf("dropping a delivered row: %q %v", outcome, err)
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
		if _, err := f.s.DropOwedDelivery(eventID, f.hookID, "item.created", f.companion.ID, now(), "old"); err != nil {
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

// codex r4 on U10c: the drop decision runs admission's checks under the
// install lock, so a delivery no longer owed (here: the install disabled
// since) is recorded refused, not dropped and counted.
func TestTask3408_DropOfAnUnowedDeliveryIsARefusal(t *testing.T) {
	f := task3408Fixture(t, "inst-dropref")
	item, err := f.s.CreateItem(f.ws.ID, f.companion.ID, models.ItemCreate{Title: "T", Fields: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	var eventID string
	if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM event_outbox WHERE subject_id = ? ORDER BY occurred_at DESC LIMIT 1`), item.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.BeginInstallTeardown(f.ws.ID, f.installID, TeardownDisable); err != nil {
		t.Fatal(err)
	}
	outcome, err := f.s.DropOwedDelivery(eventID, f.hookID, "item.created", f.companion.ID, now(), "old")
	if err != nil || outcome != "refused:install_disabling" {
		t.Fatalf("outcome %q (%v), want refused:install_disabling", outcome, err)
	}
	var count int
	if err := f.s.db.QueryRow(f.s.q(`SELECT dropped_count FROM webhooks WHERE id = ?`), f.hookID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("dropped_count %d for a delivery that was not owed", count)
	}
}

// codex r5 on U10c: a request admitted before another drainer dropped the
// event did reach the app; "delivered" replaces "dropped" and the drop is
// taken back off the count. Any other terminal row still stands.
func TestTask3408_DeliveredOverridesADrop(t *testing.T) {
	f := task3408Fixture(t, "inst-undrop")
	item, err := f.s.CreateItem(f.ws.ID, f.companion.ID, models.ItemCreate{Title: "T", Fields: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	var eventID string
	if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM event_outbox WHERE subject_id = ? ORDER BY occurred_at DESC LIMIT 1`), item.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if out, err := f.s.DropOwedDelivery(eventID, f.hookID, "item.created", f.companion.ID, now(), "old"); err != nil || out != "dropped" {
		t.Fatalf("drop: %q %v", out, err)
	}
	if err := f.s.RecordDelivery(eventID, f.hookID, DeliveryDelivered, "", 1); err != nil {
		t.Fatal(err)
	}
	var st string
	var count int
	if err := f.s.db.QueryRow(f.s.q(`SELECT d.status, w.dropped_count FROM webhook_deliveries d JOIN webhooks w ON w.id = d.webhook_id WHERE d.outbox_event_id = ? AND d.webhook_id = ?`), eventID, f.hookID).Scan(&st, &count); err != nil {
		t.Fatal(err)
	}
	if st != DeliveryDelivered || count != 0 {
		t.Fatalf("after a late delivery: %s, dropped_count %d; want delivered, 0", st, count)
	}
	// Only delivered may replace dropped.
	if err := f.s.RecordDelivery(eventID, f.hookID, DeliveryPermanent, "late", 1); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(f.s.q(`SELECT status FROM webhook_deliveries WHERE outbox_event_id = ? AND webhook_id = ?`), eventID, f.hookID).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != DeliveryDelivered {
		t.Fatalf("a terminal delivered row was overwritten with %s", st)
	}

	// A drop followed by anything but a delivery stays dropped.
	item2, err := f.s.CreateItem(f.ws.ID, f.companion.ID, models.ItemCreate{Title: "T2", Fields: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	var event2 string
	if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM event_outbox WHERE subject_id = ? ORDER BY occurred_at DESC LIMIT 1`), item2.ID).Scan(&event2); err != nil {
		t.Fatal(err)
	}
	if out, err := f.s.DropOwedDelivery(event2, f.hookID, "item.created", f.companion.ID, now(), "old"); err != nil || out != "dropped" {
		t.Fatalf("drop: %q %v", out, err)
	}
	for _, late := range []string{DeliveryPermanent, DeliveryTransient} {
		if err := f.s.RecordDelivery(event2, f.hookID, late, "late", 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.s.db.QueryRow(f.s.q(`SELECT status FROM webhook_deliveries WHERE outbox_event_id = ? AND webhook_id = ?`), event2, f.hookID).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != DeliveryDropped {
		t.Fatalf("a dropped row was overwritten with %s", st)
	}
}
