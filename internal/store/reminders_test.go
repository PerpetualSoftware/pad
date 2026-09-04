package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
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

	// A second ack is idempotent rather than a re-stamp: it answers with the
	// row (the ack "happened", from the caller's side) and moves neither the
	// recorded moment of acknowledgement nor updated_at. Both are asserted,
	// because COALESCE alone would keep acked_at while a naive SET rewrote
	// updated_at on every repeat.
	//
	// MUTANT: dropping the CASE on updated_at moves it here; dropping COALESCE
	// re-stamps acked_at.
	again, err := s.AckReminder(ws.ID, id)
	if err != nil {
		t.Fatalf("second AckReminder: %v", err)
	}
	if again == nil {
		t.Fatal("a second acknowledgement of a fired reminder must still answer with the row")
	}
	if again.AckedAt == nil || *again.AckedAt != *acked.AckedAt {
		t.Errorf("acked_at moved on a repeat ack: %v -> %v", *acked.AckedAt, again.AckedAt)
	}
	if again.UpdatedAt != acked.UpdatedAt {
		t.Errorf("updated_at moved on a repeat ack: %v -> %v", acked.UpdatedAt, again.UpdatedAt)
	}
}

// TestCreateReminderRefusesAnotherWorkspacesItem — codex round 12, P2, the one
// finding of that round that needs no timing to bite.
//
// item_reminders.item_id carries an FK to items and no same-workspace
// constraint, so without the INSERT's own predicate a (workspace B, item of A)
// pair is accepted, and B's pending-reminder surface — which scopes by
// r.workspace_id and joins the item — then carries A's ref and title into B's
// dashboard and B's webhooks.
//
// MUTANT: dropping `i.workspace_id = ?` from the INSERT's SELECT accepts the
// row and both halves of this test fail.
func TestCreateReminderRefusesAnotherWorkspacesItem(t *testing.T) {
	s := testStore(t)
	wsA := createTestWorkspace(t, s, "A")
	wsB := createTestWorkspace(t, s, "B")
	colA := createTestCollection(t, s, wsA.ID, "Tasks")
	itemA := createTestItem(t, s, wsA.ID, colA.ID, "A's item", "")

	r, err := s.CreateReminder(wsB.ID, itemA.ID, future)
	if !errors.Is(err, ErrReminderItemGone) {
		t.Fatalf("err = %v, want ErrReminderItemGone", err)
	}
	if r != nil {
		t.Fatal("a refused arm must not return a reminder")
	}
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM item_reminders WHERE item_id = ?`), itemA.ID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d reminder row(s) written for a cross-workspace arm, want 0", n)
	}

	// Positive control for the predicate: the same item through its own
	// workspace arms normally, so the refusal above is the workspace half
	// and not a broken INSERT.
	armReminder(t, s, wsA.ID, itemA.ID, future)
}

// TestCreateReminderRefusesASoftDeletedItem. An armed reminder on an archived
// item is a row the candidate scan excludes forever — to the caller, a
// reminder that was accepted and silently never fires. Refuse at the door.
//
// MUTANT: dropping `i.deleted_at IS NULL` from the INSERT accepts it.
func TestCreateReminderRefusesASoftDeletedItem(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Archived", "")
	if _, err := s.db.Exec(s.q(`UPDATE items SET deleted_at = ? WHERE id = ?`), now(), item.ID); err != nil {
		t.Fatalf("soft delete item: %v", err)
	}

	_, err := s.CreateReminder(ws.ID, item.ID, future)
	if !errors.Is(err, ErrReminderItemGone) {
		t.Fatalf("err = %v, want ErrReminderItemGone", err)
	}
	if _, err := s.CreateReminder(ws.ID, "no-such-item", future); !errors.Is(err, ErrReminderItemGone) {
		t.Fatalf("missing item: err = %v, want ErrReminderItemGone", err)
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

	pending, _, err := s.ListPendingReminders(ws.ID, PendingReminderScope{}, 0, 0)
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
	pending, _, err = s.ListPendingReminders(ws.ID, PendingReminderScope{}, 0, 0)
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

// TestSoftDeletedItemsDoNotOccupyTheBatch — codex round 1, P1.
//
// fireOneReminder rolls back when it finds the item gone, which leaves the
// reminder ARMED and therefore a candidate again on every later pass. With
// candidates ordered oldest-first and bounded by `limit`, enough archived
// reminders fill the batch and NO live reminder ever fires — permanently, and
// silently, since the tick then reports zero fired and looks idle.
//
// The fixture uses a limit of 2 with 2 archived reminders older than the live
// one, which is the smallest shape that starves. A test with a generous limit
// would pass against the unfixed code: everything fits in one batch, so the
// live reminder fires anyway and the bug is invisible.
//
// MUTANT: dropping `AND i.deleted_at IS NULL` from the candidate query starves
// the live reminder and this fails.
func TestSoftDeletedItemsDoNotOccupyTheBatch(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Two archived items whose reminders are OLDER than the live one, so they
	// sort ahead of it in the candidate query.
	for i, at := range []string{"2019-01-01T00:00:00Z", "2019-06-01T00:00:00Z"} {
		gone := createTestItem(t, s, ws.ID, col.ID, "Archived", "")
		armReminder(t, s, ws.ID, gone.ID, at)
		if _, err := s.db.Exec(s.q(`UPDATE items SET deleted_at = ? WHERE id = ?`), now(), gone.ID); err != nil {
			t.Fatalf("soft delete %d: %v", i, err)
		}
	}

	live := createTestItem(t, s, ws.ID, col.ID, "Still here", "")
	liveID := armReminder(t, s, ws.ID, live.ID, past)

	fired, err := s.FireDueReminders(nowTS(), 2)
	if err != nil {
		t.Fatalf("FireDueReminders: %v", err)
	}
	if len(fired) != 1 || fired[0].ID != liveID {
		t.Fatalf("the live reminder was starved by archived ones: fired %d", len(fired))
	}

	// The archived reminders are KEPT, not reaped — restoring the item should
	// restore its reminder with it. Asserting only the starvation fix would
	// pass against an implementation that deleted them.
	var armed int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM item_reminders WHERE fired_at IS NULL`)).Scan(&armed); err != nil {
		t.Fatalf("count armed: %v", err)
	}
	if armed != 2 {
		t.Errorf("archived items' reminders should stay armed and intact, got %d armed", armed)
	}
}

// TestOneBrokenReminderDoesNotBlockTheRest — codex round 1, P2.
//
// The per-reminder transaction exists so one unfireable row cannot hold back
// the pass, and returning on the first error made that comment false:
// candidates are ordered oldest-first, so a persistently broken OLD reminder
// would block every newer one forever.
//
// Driven through the injected seam because a real mid-transaction failure is
// not reachable from outside — the database refuses the corrupt rows that
// would cause one (verified while writing this: writing invalid JSON into
// items.fields is rejected by the schema itself). Testing the loop directly is
// the honest shape rather than a contrived fixture that proves something else.
//
// MUTANT: `continue` back to `return fired, err` and the third id never runs.
func TestOneBrokenReminderDoesNotBlockTheRest(t *testing.T) {
	var attempted []string
	fired, err := fireEachReminder([]string{"a", "b", "c"}, nowTS(),
		func(id, _ string) (*models.Reminder, error) {
			attempted = append(attempted, id)
			if id == "b" {
				return nil, errors.New("boom")
			}
			return &models.Reminder{ID: id}, nil
		})

	if len(attempted) != 3 {
		t.Fatalf("the pass stopped early: attempted %v, want all three", attempted)
	}
	if len(fired) != 2 || fired[0].ID != "a" || fired[1].ID != "c" {
		t.Errorf("fired = %d reminders, want a and c", len(fired))
	}
	// The failure must still be REPORTED. Continuing past an error and
	// returning nil would make a pass that failed on half its rows log as a
	// clean one, which is the silent-failure shape this whole file avoids.
	if err == nil {
		t.Error("a failing reminder was swallowed; the pass reported success")
	}
}

// TestEveryReminderFailingIsStillReported is the negative control for the
// aggregation: with nothing fired, the error is the only signal there was one.
func TestEveryReminderFailingIsStillReported(t *testing.T) {
	fired, err := fireEachReminder([]string{"a", "b"}, nowTS(),
		func(string, string) (*models.Reminder, error) { return nil, errors.New("boom") })
	if len(fired) != 0 {
		t.Errorf("fired %d reminders when every attempt failed", len(fired))
	}
	if err == nil {
		t.Error("a pass that fired nothing and failed twice reported success")
	}
}

// TestSkippedRemindersAreNotErrors: a reminder another instance won returns
// (nil, nil), which must not count as a failure — the winner emits, and
// reporting the loser as an error would make every multi-instance tick log
// spurious failures.
func TestSkippedRemindersAreNotErrors(t *testing.T) {
	fired, err := fireEachReminder([]string{"a", "b"}, nowTS(),
		func(id, _ string) (*models.Reminder, error) {
			if id == "a" {
				return nil, nil
			}
			return &models.Reminder{ID: id}, nil
		})
	if err != nil {
		t.Errorf("a skipped reminder was reported as an error: %v", err)
	}
	if len(fired) != 1 || fired[0].ID != "b" {
		t.Errorf("fired = %v, want just b", fired)
	}
}

// TestFractionalSecondsRoundUp — codex round 2.
//
// The column is compared as a string against a whole-second clock, so seconds
// are the stored resolution. Truncating resolved that the wrong way:
// 09:00:00.900Z became 09:00:00Z and fired 900ms BEFORE the moment the caller
// named, silently, having rewritten their value on the way in. Late is a
// reminder; early is a wrong answer.
//
// MUTANT: replace NormalizeInstant's round-up with Truncate and the first case
// stores ...00Z.
func TestFractionalSecondsRoundUp(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")

	for _, tc := range []struct{ in, want string }{
		{"2026-08-01T09:00:00.900Z", "2026-08-01T09:00:01Z"},
		{"2026-08-01T09:00:00.001Z", "2026-08-01T09:00:01Z"},
		// A whole second must round-trip EXACTLY. Without this leg an
		// implementation that added a second unconditionally would pass.
		{"2026-08-01T09:00:00Z", "2026-08-01T09:00:00Z"},
		// The offset case still normalizes to UTC, and rounding must not
		// disturb that.
		{"2026-08-01T09:00:00.500+09:00", "2026-08-01T00:00:01Z"},
	} {
		r, err := s.CreateReminder(ws.ID, item.ID, tc.in)
		if err != nil {
			t.Fatalf("CreateReminder(%q): %v", tc.in, err)
		}
		if r.RemindAt != tc.want {
			t.Errorf("CreateReminder(%q) stored %q, want %q", tc.in, r.RemindAt, tc.want)
		}
	}
}

// TestAFractionalReminderDoesNotFireEarly is the behavioural half — the stored
// STRING being right is only interesting because of what the tick does with
// it. Asserting the column alone would not catch a comparison that ignored it.
func TestAFractionalReminderDoesNotFireEarly(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")

	// Armed for 09:00:00.900Z. A tick at 09:00:00Z is BEFORE that moment and
	// must not fire it; a tick at 09:00:01Z is after and must.
	if _, err := s.CreateReminder(ws.ID, item.ID, "2026-08-01T09:00:00.900Z"); err != nil {
		t.Fatalf("CreateReminder: %v", err)
	}

	fired, err := s.FireDueReminders("2026-08-01T09:00:00Z", 0)
	if err != nil {
		t.Fatalf("early tick: %v", err)
	}
	if len(fired) != 0 {
		t.Errorf("a reminder set for 09:00:00.900Z fired at 09:00:00Z")
	}

	fired, err = s.FireDueReminders("2026-08-01T09:00:01Z", 0)
	if err != nil {
		t.Fatalf("later tick: %v", err)
	}
	if len(fired) != 1 {
		t.Errorf("the reminder did not fire at 09:00:01Z (fired %d)", len(fired))
	}
}

// TestARearmedReminderIsNotFiredByAnInFlightPass — codex round 3.
//
// A re-arm can move a reminder into the future between the candidate scan and
// the fire. It clears fired_at, so a predicate checking only `fired_at IS NULL`
// still matched — and the pass fired a reminder the user had just deferred and
// emitted its event. The re-arm cannot undo that: it can clear the mark, the
// event is already on the outbox.
//
// Driven by calling the arbiter with a STALE candidate id, which is what an
// in-flight pass holds. Same seam as the concurrency test above.
//
// MUTANT: drop `AND remind_at <= ?` from the fire UPDATE and this fires.
func TestARearmedReminderIsNotFiredByAnInFlightPass(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")
	id := armReminder(t, s, ws.ID, item.ID, past)

	// The pass has selected this id. Before it fires, the user defers it.
	ids, err := s.dueReminderCandidates(nowTS(), 0)
	if err != nil {
		t.Fatalf("dueReminderCandidates: %v", err)
	}
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("expected the armed reminder as the only candidate, got %v", ids)
	}
	if _, err := s.RearmReminder(ws.ID, id, future); err != nil {
		t.Fatalf("RearmReminder: %v", err)
	}

	fired, err := s.fireOneReminder(id, nowTS())
	if err != nil {
		t.Fatalf("fireOneReminder: %v", err)
	}
	if fired != nil {
		t.Error("a reminder deferred mid-pass was fired anyway")
	}

	// No event either — the mark can be cleared, an emitted event cannot.
	var events int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM event_outbox WHERE event_type = ?`),
		kernelevents.ItemReminderDue).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Errorf("%d reminder event(s) emitted for a deferred reminder", events)
	}

	// And it is still armed for its NEW time, not left in some third state.
	got, err := s.GetReminder(ws.ID, id)
	if err != nil {
		t.Fatalf("GetReminder: %v", err)
	}
	if !got.Armed() || got.RemindAt != future {
		t.Errorf("reminder is %+v, want armed at %s", got, future)
	}
}

// TestPendingRemindersAreBounded — codex round 3.
//
// Every pending reminder became a suggestion prepended to a three-entry list,
// so the payload grew without limit until somebody acknowledged them — in the
// dashboard response, the hottest read in the product.
//
// MUTANT: remove the LIMIT and both the window and the truncation flag are
// wrong.
func TestPendingRemindersAreBounded(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")

	for i := 0; i < 5; i++ {
		armReminder(t, s, ws.ID, item.ID, past)
	}
	if _, err := s.FireDueReminders(nowTS(), 0); err != nil {
		t.Fatalf("tick: %v", err)
	}

	pending, truncated, err := s.ListPendingReminders(ws.ID, PendingReminderScope{}, 3, 0)
	if err != nil {
		t.Fatalf("ListPendingReminders: %v", err)
	}
	if len(pending) != 3 {
		t.Errorf("window returned %d rows, want the limit of 3", len(pending))
	}
	if !truncated {
		t.Error("five pending reminders through a window of three did not report truncation")
	}

	// COVERAGE BOUNDARY, stated rather than implied. Two different bounds live
	// here and only one is observable from a test: the Go slice cap below
	// bounds the PAYLOAD, and the SQL LIMIT bounds the DATABASE's work. A
	// mutant that removes the LIMIT survives this test — correctly, because
	// the payload stays bounded either way; what is lost is that the query
	// stops scanning and materialising every pending row before discarding
	// them. That is a memory and I/O property with no assertion available at
	// this level, so it is defended by the LIMIT being there and by this
	// comment saying why, not by a green.

	// The probe row must never be returned as a result, and the flag must be
	// FALSE when everything fits — a flag that is always true is not a signal.
	pending, truncated, err = s.ListPendingReminders(ws.ID, PendingReminderScope{}, 5, 0)
	if err != nil {
		t.Fatalf("ListPendingReminders: %v", err)
	}
	if len(pending) != 5 {
		t.Errorf("window of 5 returned %d rows, want all 5", len(pending))
	}
	if truncated {
		t.Error("five reminders through a window of five reported truncation")
	}
}

// TestEmptyScopeSeesNothing pins the non-nil-empty case, which is a THIRD
// state that reads like the second: nil CollectionIDs means unrestricted, and
// an empty-but-present slice means "this caller can see no collections."
// Without the guard those collapse — the switch below matches none of its
// cases at len 0 and adds no clause at all, so "nothing visible" returns the
// whole workspace.
//
// It gets a direct test because no dashboard-level test produces that state:
// the callers that would are refused earlier by workspace access. A guard for
// a state nothing exercises is exactly the one that rots.
//
// MUTANT: delete the guard and this returns every pending reminder.
func TestEmptyScopeSeesNothing(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")
	armReminder(t, s, ws.ID, item.ID, past)
	if _, err := s.FireDueReminders(nowTS(), 0); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// Sanity: unrestricted sees it, so a build where NOTHING is returned would
	// not pass this test by accident.
	all, _, err := s.ListPendingReminders(ws.ID, PendingReminderScope{}, 0, 0)
	if err != nil {
		t.Fatalf("unrestricted: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("unrestricted scope returned %d, want 1", len(all))
	}

	none, truncated, err := s.ListPendingReminders(ws.ID, PendingReminderScope{CollectionIDs: []string{}}, 0, 0)
	if err != nil {
		t.Fatalf("empty scope: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("a caller with no visible collections got %d reminders", len(none))
	}
	// And it must not be told to page: there is nothing behind the window it
	// could ever reach.
	if truncated {
		t.Error("a caller who can see nothing was told there is more")
	}
}

// TestSoftDeletedWorkspacesDoNotFire — codex round 6, P1, and the only defect
// in this unit whose consequence leaves the process.
//
// Workspace soft-delete deliberately keeps items for the 30-day restore
// window, so a filter on the ITEM's deleted_at finds nothing wrong and the
// tick kept going — emitting outbound webhook events for a workspace whose
// owner had deleted it, possibly while deleting their account.
//
// Restoration is asserted too: the reminders must be intact and fire again,
// because "stops firing" and "is destroyed" are very different answers to a
// user who restores a workspace, and only one of them is right.
//
// MUTANT: drop `AND w.deleted_at IS NULL` from the candidate query and the
// deleted workspace's reminder fires.
func TestSoftDeletedWorkspacesDoNotFire(t *testing.T) {
	s := testStore(t)
	live := createTestWorkspace(t, s, "Live")
	gone := createTestWorkspace(t, s, "Gone")

	liveCol := createTestCollection(t, s, live.ID, "Tasks")
	goneCol := createTestCollection(t, s, gone.ID, "Tasks")
	liveItem := createTestItem(t, s, live.ID, liveCol.ID, "Still here", "")
	goneItem := createTestItem(t, s, gone.ID, goneCol.ID, "Deleted workspace", "")

	liveID := armReminder(t, s, live.ID, liveItem.ID, past)
	goneID := armReminder(t, s, gone.ID, goneItem.ID, past)

	if _, err := s.db.Exec(s.q(`UPDATE workspaces SET deleted_at = ? WHERE id = ?`), now(), gone.ID); err != nil {
		t.Fatalf("soft delete workspace: %v", err)
	}

	fired, err := s.FireDueReminders(nowTS(), 0)
	if err != nil {
		t.Fatalf("FireDueReminders: %v", err)
	}
	if len(fired) != 1 || fired[0].ID != liveID {
		t.Fatalf("expected only the live workspace's reminder to fire, got %d", len(fired))
	}

	// No event for the deleted workspace — the point is what left the process,
	// not merely what the return value said.
	var events int
	// Scoped to the REMINDER event type. Counting every event in the workspace
	// made this fail for the wrong reason — item creation writes its own
	// outbox rows, so the assertion was satisfiable by the fixture itself and
	// discriminated nothing.
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM event_outbox WHERE workspace_id = ? AND event_type = ?`),
		gone.ID, kernelevents.ItemReminderDue).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Errorf("%d reminder event(s) emitted for a soft-deleted workspace", events)
	}

	// Restore: the reminder is intact and fires.
	if _, err := s.db.Exec(s.q(`UPDATE workspaces SET deleted_at = NULL WHERE id = ?`), gone.ID); err != nil {
		t.Fatalf("restore workspace: %v", err)
	}
	fired, err = s.FireDueReminders(nowTS(), 0)
	if err != nil {
		t.Fatalf("FireDueReminders after restore: %v", err)
	}
	if len(fired) != 1 || fired[0].ID != goneID {
		t.Errorf("a restored workspace's reminder did not fire; got %d", len(fired))
	}
}

// TestPendingRemindersHideASoftDeletedWorkspace is the read-side half. The
// dashboard for a deleted workspace is not reachable today, so this pins the
// query rather than a user-visible symptom — the same reason the fire-side
// filter is not enough on its own.
func TestPendingRemindersHideASoftDeletedWorkspace(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Gone")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")
	armReminder(t, s, ws.ID, item.ID, past)
	if _, err := s.FireDueReminders(nowTS(), 0); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if pending, _, err := s.ListPendingReminders(ws.ID, PendingReminderScope{}, 0, 0); err != nil {
		t.Fatalf("ListPendingReminders: %v", err)
	} else if len(pending) != 1 {
		t.Fatalf("setup: expected 1 pending reminder before deletion, got %d", len(pending))
	}

	if _, err := s.db.Exec(s.q(`UPDATE workspaces SET deleted_at = ? WHERE id = ?`), now(), ws.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	pending, _, err := s.ListPendingReminders(ws.ID, PendingReminderScope{}, 0, 0)
	if err != nil {
		t.Fatalf("ListPendingReminders: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("a soft-deleted workspace still lists %d pending reminder(s)", len(pending))
	}
}

// TestRemindersRoundTripThroughExport — codex round 8, P1.
//
// WorkspaceExport is a hand-maintained field list, so a new table joins it
// only if someone remembers. Reminders did not, and the loss was silent: a
// backup/restore or a SQLite→Postgres migration dropped every pending
// reminder with nothing in the destination to show anything had gone.
//
// The line that list has always drawn is item-scoped workspace CONTENT
// (comments, links, versions — exported) versus per-user state (stars,
// watches — not). A reminder has no user column and hangs off an item, which
// puts it on the exported side.
//
// All three lifecycle states are in the fixture, because carrying the marks is
// the decision: a fired-unacked reminder is still owed and must arrive
// pending, not reset to armed.
//
// MUTANT: drop the reminder block from either ExportWorkspace or
// ImportWorkspace and this fails.
func TestRemindersRoundTripThroughExport(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "reminder-export@test.com", "Export Owner", "password123")
	src := createTestWorkspace(t, s, "Reminder Export")
	col := createTestCollection(t, s, src.ID, "Tasks")
	item := createTestItem(t, s, src.ID, col.ID, "Ship it", "")

	armedID := armReminder(t, s, src.ID, item.ID, future)
	firedID := armReminder(t, s, src.ID, item.ID, past)
	ackedID := armReminder(t, s, src.ID, item.ID, past)
	if _, err := s.FireDueReminders(nowTS(), 0); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if _, err := s.AckReminder(src.ID, ackedID); err != nil {
		t.Fatalf("AckReminder: %v", err)
	}
	_ = armedID
	_ = firedID

	exp, err := s.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	if len(exp.Reminders) != 3 {
		t.Fatalf("export carried %d reminders, want 3", len(exp.Reminders))
	}

	dst, err := s.ImportWorkspace(exp, "reminder-export-target", owner.ID)
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}

	items, err := s.ListItems(dst.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("imported %d items, want 1", len(items))
	}
	got, err := s.ListRemindersForItem(items[0].ID)
	if err != nil {
		t.Fatalf("ListRemindersForItem: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("imported %d reminders, want 3", len(got))
	}

	// One of each state, by shape rather than by id — the ids are re-minted on
	// import, and asserting the STATES is what the carrying decision is about.
	var armed, pending, acked int
	for _, r := range got {
		switch {
		case r.Armed():
			armed++
		case r.PendingAck():
			pending++
		default:
			acked++
		}
	}
	if armed != 1 || pending != 1 || acked != 1 {
		t.Errorf("imported states armed=%d pending=%d acked=%d, want 1/1/1 — the lifecycle marks were not carried",
			armed, pending, acked)
	}
}

// TestFirePathInvariant is the pin for the invariant stated on
// FireDueReminders: the candidate scan proves nothing, and every condition
// that made a row a candidate is re-asserted inside the transaction that marks
// it fired.
//
// DERIVED FROM THE INVARIANT, NOT FROM THE BUG HISTORY, and that is the point
// of writing it this way. Each case invalidates ONE scan-side condition in the
// window between the scan and the fire, and asserts the same three things:
// nothing fires, no event leaves, and the reminder is left alone rather than
// consumed. Adding a fifth condition to the scan without a row here is
// supposed to feel like an omission.
//
// The earlier per-defect tests (a re-armed instant, a deleted workspace, a
// deleted item) are folded in as rows. They said the same thing one instance
// at a time, which is exactly how four of these shipped.
//
// MUTANT MATRIX: drop any single re-check from the fire UPDATE — the fire
// mark, the instant, or either half of reminderFireable — and the
// corresponding row fails while the others stay green.
func TestFirePathInvariant(t *testing.T) {
	for _, tc := range []struct {
		name string
		// invalidate makes the scanned candidate no longer fireable, standing
		// in for a concurrent writer between the scan and the fire.
		invalidate func(t *testing.T, s *Store, ws, item, reminder string)
	}{
		{
			name: "the instant moves into the future (a re-arm)",
			invalidate: func(t *testing.T, s *Store, ws, _, reminder string) {
				if _, err := s.RearmReminder(ws, reminder, future); err != nil {
					t.Fatalf("RearmReminder: %v", err)
				}
			},
		},
		{
			name: "the item is soft-deleted",
			invalidate: func(t *testing.T, s *Store, _, item, _ string) {
				if _, err := s.db.Exec(s.q(`UPDATE items SET deleted_at = ? WHERE id = ?`), now(), item); err != nil {
					t.Fatalf("soft delete item: %v", err)
				}
			},
		},
		{
			name: "the workspace is soft-deleted",
			invalidate: func(t *testing.T, s *Store, ws, _, _ string) {
				if _, err := s.db.Exec(s.q(`UPDATE workspaces SET deleted_at = ? WHERE id = ?`), now(), ws); err != nil {
					t.Fatalf("soft delete workspace: %v", err)
				}
			},
		},
		{
			name: "another pass already fired it",
			invalidate: func(t *testing.T, s *Store, _, _, reminder string) {
				if _, err := s.db.Exec(s.q(`UPDATE item_reminders SET fired_at = ? WHERE id = ?`), now(), reminder); err != nil {
					t.Fatalf("mark fired: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ws := createTestWorkspace(t, s, "Invariant")
			col := createTestCollection(t, s, ws.ID, "Tasks")
			item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")
			id := armReminder(t, s, ws.ID, item.ID, past)

			// The scan runs and produces the candidate. Asserting it HERE is
			// what makes each case a mid-pass race rather than a filtered
			// scan: if the row were already excluded, the test would prove
			// the scan works and say nothing about the arbiter.
			ids, err := s.dueReminderCandidates(nowTS(), 0)
			if err != nil {
				t.Fatalf("dueReminderCandidates: %v", err)
			}
			if len(ids) != 1 || ids[0] != id {
				t.Fatalf("setup: expected the armed reminder as the only candidate, got %v", ids)
			}

			tc.invalidate(t, s, ws.ID, item.ID, id)

			fired, err := s.fireOneReminder(id, nowTS())
			if err != nil {
				t.Fatalf("fireOneReminder: %v", err)
			}
			if fired != nil {
				t.Error("fired a reminder that stopped qualifying after the scan")
			}

			var events int
			if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM event_outbox WHERE workspace_id = ? AND event_type = ?`),
				ws.ID, kernelevents.ItemReminderDue).Scan(&events); err != nil {
				t.Fatalf("count events: %v", err)
			}
			if events != 0 {
				t.Errorf("%d reminder event(s) left the process", events)
			}
		})
	}
}

// TestFirePathInvariantFiresWhenNothingChanged is the invariant's positive
// control. Every case above asserts that nothing happens, so all four would
// pass against a build that never fires anything at all.
func TestFirePathInvariantFiresWhenNothingChanged(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Invariant")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")
	id := armReminder(t, s, ws.ID, item.ID, past)

	ids, err := s.dueReminderCandidates(nowTS(), 0)
	if err != nil {
		t.Fatalf("dueReminderCandidates: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("setup: expected 1 candidate, got %d", len(ids))
	}

	fired, err := s.fireOneReminder(id, nowTS())
	if err != nil {
		t.Fatalf("fireOneReminder: %v", err)
	}
	if fired == nil {
		t.Fatal("an unchanged candidate did not fire")
	}
	var events int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM event_outbox WHERE workspace_id = ? AND event_type = ?`),
		ws.ID, kernelevents.ItemReminderDue).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Errorf("%d reminder events emitted, want exactly 1", events)
	}
}

// TestPendingRemindersSurviveALegacyItemNumber — codex round 9, P1.
//
// items.item_number is NULLABLE (migration 006 added it to existing rows), and
// scanning NULL into an int fails the Scan — which fails the QUERY, which
// degrades the whole pending-reminder section and hides every reminder in the
// workspace, not just the legacy item's. One old row, and the feature is dark
// for everyone in that workspace.
//
// MUTANT: scan into a plain int and this fails.
func TestPendingRemindersSurviveALegacyItemNumber(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Legacy")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	legacy := createTestItem(t, s, ws.ID, col.ID, "Pre-numbering item", "")
	modern := createTestItem(t, s, ws.ID, col.ID, "Numbered item", "")

	if _, err := s.db.Exec(s.q(`UPDATE items SET item_number = NULL WHERE id = ?`), legacy.ID); err != nil {
		t.Fatalf("clear item_number: %v", err)
	}
	armReminder(t, s, ws.ID, legacy.ID, past)
	armReminder(t, s, ws.ID, modern.ID, past)
	if _, err := s.FireDueReminders(nowTS(), 0); err != nil {
		t.Fatalf("tick: %v", err)
	}

	pending, _, err := s.ListPendingReminders(ws.ID, PendingReminderScope{}, 0, 0)
	if err != nil {
		t.Fatalf("ListPendingReminders: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("one legacy row hid %d of 2 reminders", 2-len(pending))
	}

	// The legacy one carries no ref rather than a wrong one — "PREFIX-0" would
	// name a different item — while the modern one still does.
	for _, pr := range pending {
		if pr.ItemTitle == "Pre-numbering item" && pr.ItemRef != "" {
			t.Errorf("legacy item got a fabricated ref %q", pr.ItemRef)
		}
		if pr.ItemTitle == "Numbered item" && pr.ItemRef == "" {
			t.Error("a numbered item lost its ref")
		}
	}
}

// TestExportSkipsRemindersForSoftDeletedItems — codex round 9, P1.
//
// The items section filters on deleted_at IS NULL, so a soft-deleted item is
// NOT in the bundle and its reminder can never be reunited with it — the
// import drops the orphan on its itemMap lookup. Exporting them shipped rows
// that could only ever be discarded, under a comment claiming a restore
// benefit the bundle cannot deliver.
//
// MUTANT: drop the JOIN's deleted_at filter and the export carries 2.
func TestExportSkipsRemindersForSoftDeletedItems(t *testing.T) {
	s := testStore(t)
	src := createTestWorkspace(t, s, "Partial")
	col := createTestCollection(t, s, src.ID, "Tasks")
	live := createTestItem(t, s, src.ID, col.ID, "Live", "")
	gone := createTestItem(t, s, src.ID, col.ID, "Archived", "")
	armReminder(t, s, src.ID, live.ID, future)
	armReminder(t, s, src.ID, gone.ID, future)

	if _, err := s.db.Exec(s.q(`UPDATE items SET deleted_at = ? WHERE id = ?`), now(), gone.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	exp, err := s.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	if len(exp.Reminders) != 1 {
		t.Fatalf("export carried %d reminders, want only the live item's", len(exp.Reminders))
	}
	if exp.Reminders[0].ItemID != live.ID {
		t.Errorf("export carried the archived item's reminder")
	}
}

// TestImportNormalizesRemindAt — codex round 9, P2.
//
// Import is a WRITER, and a bundle is not necessarily one this server
// produced — it can be hand-edited or come from another instance. Inserting a
// raw remind_at let a local offset into the one column every comparison
// downstream treats as a UTC instant. Every other door normalizes; this one
// was writing underneath them.
//
// MUTANT: insert rm.RemindAt instead of the normalized value and the offset
// value is stored verbatim.
func TestImportNormalizesRemindAt(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "import-norm@test.com", "Import Norm", "password123")
	src := createTestWorkspace(t, s, "Import Norm")
	col := createTestCollection(t, s, src.ID, "Tasks")
	item := createTestItem(t, s, src.ID, col.ID, "Ship it", "")
	armReminder(t, s, src.ID, item.ID, future)

	exp, err := s.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	if len(exp.Reminders) != 1 {
		t.Fatalf("setup: export carried %d reminders", len(exp.Reminders))
	}
	// A bundle carrying an offset instant and a fractional second — both of
	// which the API door would have normalized on the way in.
	exp.Reminders[0].RemindAt = "2099-08-01T09:00:00.500+09:00"
	// And a second reminder whose value is not a time at all.
	exp.Reminders = append(exp.Reminders, models.ReminderExport{
		ItemID: exp.Reminders[0].ItemID, RemindAt: "next tuesday",
		CreatedAt: exp.Reminders[0].CreatedAt, UpdatedAt: exp.Reminders[0].UpdatedAt,
	})

	dst, err := s.ImportWorkspace(exp, "import-norm-target", owner.ID)
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}
	items, err := s.ListItems(dst.ID, models.ItemListParams{})
	if err != nil || len(items) != 1 {
		t.Fatalf("ListItems: %v (%d items)", err, len(items))
	}
	got, err := s.ListRemindersForItem(items[0].ID)
	if err != nil {
		t.Fatalf("ListRemindersForItem: %v", err)
	}
	// The unparseable one is skipped rather than failing the whole restore.
	if len(got) != 1 {
		t.Fatalf("imported %d reminders, want 1 (the unparseable one skipped)", len(got))
	}
	if got[0].RemindAt != "2099-08-01T00:00:01Z" {
		t.Errorf("imported remind_at = %q, want the same instant normalized to UTC and rounded up", got[0].RemindAt)
	}
}

// TestOrphanedItemDoesNotAbortTheImport — codex round 10.
//
// An ORPHANED item — one whose collection is missing from the bundle — still
// gets an itemMap entry, because that entry is written before the skip and
// parent resolution inside the same loop needs it. So `itemMap[x] != ""` is
// satisfied by an id that names no row, and inserting a foreign key to it
// fails. This loop treated that as fatal, so ONE orphaned item carrying a
// reminder aborted an entire workspace restore.
//
// The bundle is hand-built rather than exported, because ExportWorkspace
// cannot produce an orphan — which is exactly why this needed a test: the
// shape only arrives from a hand-edited or foreign bundle, and those are the
// ones import exists to survive.
//
// MUTANT: gate on `itemMap[...] != ""` instead of insertedItems, or restore
// the fatal return, and the import fails.
func TestOrphanedItemDoesNotAbortTheImport(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "orphan-import@test.com", "Orphan Import", "password123")
	src := createTestWorkspace(t, s, "Orphan Source")
	col := createTestCollection(t, s, src.ID, "Tasks")
	item := createTestItem(t, s, src.ID, col.ID, "Real item", "")
	armReminder(t, s, src.ID, item.ID, future)

	exp, err := s.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}

	// Add an item whose collection is NOT in the bundle, and a reminder on it.
	orphanID := "orphan-item-id"
	exp.Items = append(exp.Items, models.ItemExport{
		ID: orphanID, CollectionID: "collection-that-is-not-here",
		Title: "Orphan", Slug: "orphan", Fields: "{}", Tags: "[]",
		CreatedAt: exp.Items[0].CreatedAt, UpdatedAt: exp.Items[0].UpdatedAt,
	})
	exp.Reminders = append(exp.Reminders, models.ReminderExport{
		ItemID: orphanID, RemindAt: future,
		CreatedAt: exp.Items[0].CreatedAt, UpdatedAt: exp.Items[0].UpdatedAt,
	})

	dst, err := s.ImportWorkspace(exp, "orphan-import-target", owner.ID)
	if err != nil {
		t.Fatalf("one orphaned item aborted the whole import: %v", err)
	}

	// The real item and its reminder still arrived — the point is that the
	// orphan was skipped, not that everything was.
	items, err := s.ListItems(dst.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 1 || items[0].Title != "Real item" {
		t.Fatalf("imported %d items, want just the real one", len(items))
	}
	got, err := s.ListRemindersForItem(items[0].ID)
	if err != nil {
		t.Fatalf("ListRemindersForItem: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("the real item's reminder did not survive the orphan (%d reminders)", len(got))
	}
}

// TestImportRefusesAckWithoutFire — codex round 11.
//
// The lifecycle has three states; acked-without-fired is not one of them. A
// bundle carrying it — which this server's export cannot produce, but a
// hand-edited or foreign one can — would import a reminder that fires, is
// excluded from the pending surface because it is already acked, and can
// never be acknowledged because AckReminder requires acked_at IS NULL. It
// emits an event and is then invisible forever.
//
// MUTANT: assign ackedAt unconditionally and the imported row comes back
// neither armed nor pending.
func TestImportRefusesAckWithoutFire(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "ackfire@test.com", "Ack Fire", "password123")
	src := createTestWorkspace(t, s, "Ack Fire")
	col := createTestCollection(t, s, src.ID, "Tasks")
	item := createTestItem(t, s, src.ID, col.ID, "Ship it", "")
	armReminder(t, s, src.ID, item.ID, future)

	exp, err := s.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	// An acknowledgement with no fire behind it.
	exp.Reminders[0].AckedAt = "2026-01-01T00:00:00Z"
	exp.Reminders[0].FiredAt = ""

	dst, err := s.ImportWorkspace(exp, "ack-fire-target", owner.ID)
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}
	items, err := s.ListItems(dst.ID, models.ItemListParams{})
	if err != nil || len(items) != 1 {
		t.Fatalf("ListItems: %v (%d)", err, len(items))
	}
	got, err := s.ListRemindersForItem(items[0].ID)
	if err != nil {
		t.Fatalf("ListRemindersForItem: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("imported %d reminders, want 1 — the schedule is the part worth keeping", len(got))
	}
	if !got[0].Armed() {
		t.Errorf("imported reminder is not armed: fired=%v acked=%v", got[0].FiredAt, got[0].AckedAt)
	}
	if got[0].AckedAt != nil {
		t.Error("an acknowledgement of something that never fired was carried in")
	}
}
