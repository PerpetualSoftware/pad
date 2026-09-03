package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
)

// Reminder lifecycle tests (IDEA-2641).
//
// Every one of these was run against the unfixed tree before the
// implementation landed and failed there; the mutation notes on each say what
// specific breakage it detects, because a test that passes on a broken build
// is a coverage claim rather than coverage.

const (
	past   = "2020-01-01T00:00:00Z"
	future = "2099-01-01T00:00:00Z"
)

func armReminder(t *testing.T, s *Store, wsID, itemID, at string) string {
	t.Helper()
	r, err := s.CreateReminder(wsID, itemID, at)
	if err != nil {
		t.Fatalf("CreateReminder: %v", err)
	}
	if r == nil {
		t.Fatal("CreateReminder returned nil")
	}
	if !r.Armed() {
		t.Fatal("a freshly created reminder must be armed")
	}
	return r.ID
}

// nowTS is the instant a tick would pass in. Taken well after `past` and well
// before `future`, so the two fixtures sit on opposite sides of it with no
// dependence on the wall clock.
func nowTS() string { return time.Now().UTC().Format(time.RFC3339) }

// TestFireDueRemindersFiresOnlyArrivedReminders is the core pass: an arrived
// reminder fires and a future one is untouched.
//
// MUTANT: flipping the candidate query's `remind_at <= ?` to `>=` fires the
// future reminder and not the past one — both halves of this test go red, and
// asserting only on the fired one would have let the flip through.
func TestFireDueRemindersFiresOnlyArrivedReminders(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")

	dueID := armReminder(t, s, ws.ID, item.ID, past)
	laterID := armReminder(t, s, ws.ID, item.ID, future)

	fired, err := s.FireDueReminders(nowTS(), 0)
	if err != nil {
		t.Fatalf("FireDueReminders: %v", err)
	}
	if len(fired) != 1 {
		t.Fatalf("expected 1 fired reminder, got %d", len(fired))
	}
	if fired[0].ID != dueID {
		t.Fatalf("fired the wrong reminder: got %s, want %s", fired[0].ID, dueID)
	}

	got, err := s.GetReminder(ws.ID, dueID)
	if err != nil {
		t.Fatalf("GetReminder: %v", err)
	}
	if got.FiredAt == nil {
		t.Error("the fired reminder's fired_at was not persisted")
	}
	if !got.PendingAck() {
		t.Error("a fired, unacknowledged reminder must be pending ack")
	}

	still, err := s.GetReminder(ws.ID, laterID)
	if err != nil {
		t.Fatalf("GetReminder: %v", err)
	}
	if !still.Armed() {
		t.Error("a reminder whose instant has not arrived must stay armed")
	}
}

// TestFireDueRemindersWritesOutboxEvent pins the pairing the whole design
// rests on: the fired_at write and the event are one transaction.
//
// MUTANT: moving the emitReminderEventTx call after tx.Commit (or dropping it)
// leaves fired_at set with no event — the reminder is retired and nobody is
// ever told, which is the silent failure this test exists to make loud.
func TestFireDueRemindersWritesOutboxEvent(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")
	id := armReminder(t, s, ws.ID, item.ID, past)

	if _, err := s.FireDueReminders(nowTS(), 0); err != nil {
		t.Fatalf("FireDueReminders: %v", err)
	}

	var eventType, subjectKind, subjectID, payload string
	err := s.db.QueryRow(s.q(`
		SELECT event_type, subject_kind, subject_id, payload FROM event_outbox WHERE event_type = ?
	`), kernelevents.ItemReminderDue).Scan(&eventType, &subjectKind, &subjectID, &payload)
	if err != nil {
		t.Fatalf("no %s row in the outbox: %v", kernelevents.ItemReminderDue, err)
	}
	if subjectKind != kernelevents.SubjectReminder {
		t.Errorf("subject_kind = %q, want %q", subjectKind, kernelevents.SubjectReminder)
	}
	// The subject is the REMINDER, not the item — the distinction the taxonomy
	// entry argues for. An item-subject event could not say which of an item's
	// reminders fired, so this assertion is the contract, not a detail.
	if subjectID != id {
		t.Errorf("subject_id = %q, want the reminder id %q", subjectID, id)
	}
	if subjectID == item.ID {
		t.Error("subject_id is the ITEM id; a reminder event must be reminder-subject")
	}

	var decoded struct {
		Reminder struct {
			ID       string `json:"id"`
			RemindAt string `json:"remind_at"`
		} `json:"reminder"`
		Item struct {
			ID string `json:"id"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload does not decode: %v", err)
	}
	if decoded.Reminder.ID != id {
		t.Errorf("payload reminder id = %q, want %q", decoded.Reminder.ID, id)
	}
	if decoded.Reminder.RemindAt != past {
		t.Errorf("payload remind_at = %q, want %q", decoded.Reminder.RemindAt, past)
	}
	if decoded.Item.ID != item.ID {
		t.Errorf("payload item id = %q, want %q", decoded.Item.ID, item.ID)
	}
}

// TestFireDueRemindersIsIdempotentAcrossTicks: a second tick must not re-fire.
//
// MUTANT: dropping `AND fired_at IS NULL` from the fire UPDATE makes the
// second tick re-fire and write a second event.
func TestFireDueRemindersIsIdempotentAcrossTicks(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")
	armReminder(t, s, ws.ID, item.ID, past)

	first, err := s.FireDueReminders(nowTS(), 0)
	if err != nil {
		t.Fatalf("first tick: %v", err)
	}
	second, err := s.FireDueReminders(nowTS(), 0)
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if len(first) != 1 || len(second) != 0 {
		t.Fatalf("expected 1 then 0 fired, got %d then %d", len(first), len(second))
	}

	var events int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM event_outbox WHERE event_type = ?`),
		kernelevents.ItemReminderDue).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Errorf("outbox holds %d reminder events, want exactly 1", events)
	}
}

// TestFireOneReminderArbitratesAStaleCandidate drives the arbiter DIRECTLY
// with an id that a concurrent pass already claimed — the race the public
// entry point cannot show, because its candidate query filters the row out
// before it gets there. Same split, and the same reason, as the outbox
// claim's claimOutboxIDs test.
//
// MUTANT: dropping the RowsAffected check makes the loser return a reminder
// and emit a duplicate event.
func TestFireOneReminderArbitratesAStaleCandidate(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")
	id := armReminder(t, s, ws.ID, item.ID, past)

	winner, err := s.fireOneReminder(id, nowTS())
	if err != nil {
		t.Fatalf("first fire: %v", err)
	}
	if winner == nil {
		t.Fatal("the first caller must win the row")
	}

	loser, err := s.fireOneReminder(id, nowTS())
	if err != nil {
		t.Fatalf("second fire: %v", err)
	}
	if loser != nil {
		t.Error("a caller arriving with a stale candidate must win nothing")
	}

	var events int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM event_outbox WHERE event_type = ?`),
		kernelevents.ItemReminderDue).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Errorf("outbox holds %d reminder events, want exactly 1 — the loser emitted", events)
	}
}

// TestRearmClearsBothFireMarks. Re-arming is the only way back to armed.
//
// MUTANT: clearing fired_at but not acked_at leaves a row that is armed AND
// acknowledged — a state the lifecycle has no name for, and one that would
// make the reminder invisible on the poll surface after it fires again.
func TestRearmClearsBothFireMarks(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")
	id := armReminder(t, s, ws.ID, item.ID, past)

	if _, err := s.FireDueReminders(nowTS(), 0); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if _, err := s.AckReminder(ws.ID, id); err != nil {
		t.Fatalf("AckReminder: %v", err)
	}

	rearmed, err := s.RearmReminder(ws.ID, id, future)
	if err != nil {
		t.Fatalf("RearmReminder: %v", err)
	}
	if rearmed == nil {
		t.Fatal("RearmReminder returned nil for an existing reminder")
	}
	if rearmed.FiredAt != nil {
		t.Error("re-arming must clear fired_at")
	}
	if rearmed.AckedAt != nil {
		t.Error("re-arming must clear acked_at")
	}
	if !rearmed.Armed() {
		t.Error("a re-armed reminder must be armed")
	}
	if rearmed.RemindAt != future {
		t.Errorf("remind_at = %q, want %q", rearmed.RemindAt, future)
	}

	// And it is genuinely back in the tick's candidate set, not merely
	// shaped like it: an assertion on the columns alone would pass even if
	// the partial index or the candidate predicate disagreed.
	if _, err := s.RearmReminder(ws.ID, id, past); err != nil {
		t.Fatalf("RearmReminder to the past: %v", err)
	}
	fired, err := s.FireDueReminders(nowTS(), 0)
	if err != nil {
		t.Fatalf("tick after re-arm: %v", err)
	}
	if len(fired) != 1 || fired[0].ID != id {
		t.Errorf("a re-armed reminder must fire again; got %d fired", len(fired))
	}
}

// TestAckRequiresAFiredReminder. Acking an armed reminder must not silently
// mark it acknowledged — it would then never appear on the poll surface at
// all, since that surface reads fired-and-unacked.
//
// MUTANT: dropping `AND fired_at IS NOT NULL` from the ack UPDATE makes the
// first assertion pass an acked-but-never-fired row.
func TestAckRequiresAFiredReminder(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")
	id := armReminder(t, s, ws.ID, item.ID, future)

	acked, err := s.AckReminder(ws.ID, id)
	if err != nil {
		t.Fatalf("AckReminder: %v", err)
	}
	if acked != nil {
		t.Fatal("acking an armed reminder must change nothing")
	}
	got, err := s.GetReminder(ws.ID, id)
	if err != nil {
		t.Fatalf("GetReminder: %v", err)
	}
	if got.AckedAt != nil {
		t.Error("an armed reminder must not carry an acknowledgement")
	}

	// Now fire it, and the same call must land.
	if _, err := s.RearmReminder(ws.ID, id, past); err != nil {
		t.Fatalf("RearmReminder: %v", err)
	}
	if _, err := s.FireDueReminders(nowTS(), 0); err != nil {
		t.Fatalf("tick: %v", err)
	}
	acked, err = s.AckReminder(ws.ID, id)
	if err != nil {
		t.Fatalf("AckReminder after fire: %v", err)
	}
	if acked == nil || acked.AckedAt == nil {
		t.Fatal("acking a fired reminder must record the acknowledgement")
	}

	// A second ack is a no-op rather than a re-stamp: it must not move the
	// recorded moment of acknowledgement.
	again, err := s.AckReminder(ws.ID, id)
	if err != nil {
		t.Fatalf("second AckReminder: %v", err)
	}
	if again != nil {
		t.Error("a second acknowledgement must change nothing")
	}
}

// TestPendingRemindersAreFiredAndUnacked pins the poll surface's query — the
// mandatory delivery path on any instance without a webhook dispatcher.
//
// MUTANT: dropping `acked_at IS NULL` keeps an acknowledged reminder on the
// surface forever; dropping `fired_at IS NOT NULL` shows a reminder before its
// time.
func TestPendingRemindersAreFiredAndUnacked(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")

	firedID := armReminder(t, s, ws.ID, item.ID, past)
	armReminder(t, s, ws.ID, item.ID, future)

	if _, err := s.FireDueReminders(nowTS(), 0); err != nil {
		t.Fatalf("tick: %v", err)
	}

	pending, err := s.ListPendingReminders(ws.ID)
	if err != nil {
		t.Fatalf("ListPendingReminders: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != firedID {
		t.Fatalf("expected exactly the fired reminder pending, got %d", len(pending))
	}
	if pending[0].ItemTitle != "Ship it" {
		t.Errorf("pending reminder carries item title %q, want %q", pending[0].ItemTitle, "Ship it")
	}
	if pending[0].ItemFields == "" {
		t.Error("ItemFields must be carried — the caller's terminal filter reads it")
	}

	if _, err := s.AckReminder(ws.ID, firedID); err != nil {
		t.Fatalf("AckReminder: %v", err)
	}
	pending, err = s.ListPendingReminders(ws.ID)
	if err != nil {
		t.Fatalf("ListPendingReminders after ack: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("an acknowledged reminder must leave the poll surface, got %d pending", len(pending))
	}
}

// TestReminderRejectsANonInstant. A bare date is refused at the store too, not
// only at the HTTP edge — the doc comment that used to say "the caller
// normalizes" protected nothing, since the store is callable from anywhere.
//
// MUTANT: removing normalizeRemindAt's parse lets "2026-08-01" through, and
// the lexicographic comparison then fires it against an RFC3339 clock string
// at a moment nobody chose.
func TestReminderRejectsANonInstant(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")

	for _, bad := range []string{"2026-08-01", "", "tomorrow", "2026-08-01 09:00:00"} {
		if _, err := s.CreateReminder(ws.ID, item.ID, bad); err == nil {
			t.Errorf("CreateReminder(%q) was accepted; only RFC3339 instants may be stored", bad)
		}
	}

	// And the same refusal on the re-arm door, which is a separate call site
	// and would otherwise be the way in.
	id := armReminder(t, s, ws.ID, item.ID, future)
	if _, err := s.RearmReminder(ws.ID, id, "2026-08-01"); err == nil {
		t.Error("RearmReminder accepted a bare date")
	}
}

// TestReminderNormalizesToUTC. An offset instant must be stored as the same
// moment in UTC, because every comparison downstream is a string compare
// against a UTC clock.
//
// MUTANT: dropping the .UTC() from normalizeRemindAt stores "+09:00" and the
// reminder then fires nine hours late — a silent, timezone-shaped error with
// nothing in the row to show why.
func TestReminderNormalizesToUTC(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")

	r, err := s.CreateReminder(ws.ID, item.ID, "2026-08-01T09:00:00+09:00")
	if err != nil {
		t.Fatalf("CreateReminder: %v", err)
	}
	if r.RemindAt != "2026-08-01T00:00:00Z" {
		t.Errorf("remind_at = %q, want the same instant in UTC (2026-08-01T00:00:00Z)", r.RemindAt)
	}
}

// TestGetReminderIsWorkspaceScoped. An unscoped lookup would answer "does this
// id exist" for every workspace on the instance.
//
// MUTANT: dropping `AND workspace_id = ?` returns the other workspace's row.
func TestGetReminderIsWorkspaceScoped(t *testing.T) {
	s := testStore(t)
	wsA := createTestWorkspace(t, s, "A")
	wsB := createTestWorkspace(t, s, "B")
	colA := createTestCollection(t, s, wsA.ID, "Tasks")
	itemA := createTestItem(t, s, wsA.ID, colA.ID, "Ship it", "")
	id := armReminder(t, s, wsA.ID, itemA.ID, future)

	got, err := s.GetReminder(wsB.ID, id)
	if err != nil {
		t.Fatalf("GetReminder: %v", err)
	}
	if got != nil {
		t.Error("a reminder must not be readable through another workspace")
	}
}

// TestReminderCascadesWithItsItem. A reminder about a hard-deleted item has
// nothing to say; the FK is what makes that structural rather than a cleanup
// job somebody has to remember to write.
func TestReminderCascadesWithItsItem(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")
	id := armReminder(t, s, ws.ID, item.ID, future)

	if _, err := s.db.Exec(s.q(`DELETE FROM items WHERE id = ?`), item.ID); err != nil {
		t.Fatalf("hard delete item: %v", err)
	}

	got, err := s.GetReminder(ws.ID, id)
	if err != nil {
		t.Fatalf("GetReminder: %v", err)
	}
	if got != nil {
		t.Error("a reminder must not outlive the item it is about")
	}
}
