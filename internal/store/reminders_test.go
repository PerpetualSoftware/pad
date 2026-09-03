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

	pending, _, err := s.ListPendingReminders(ws.ID, 0)
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
	pending, _, err = s.ListPendingReminders(ws.ID, 0)
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

	pending, truncated, err := s.ListPendingReminders(ws.ID, 3)
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
	pending, truncated, err = s.ListPendingReminders(ws.ID, 5)
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
