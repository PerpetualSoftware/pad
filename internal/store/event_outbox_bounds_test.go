package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// Tests for the outbox's two byte bounds (BUG-2827): the per-row cap the write
// enforces, and the per-pass budget the claim enforces.
//
// WHY THE SEAMS EXIST AT ALL, since a test that lowers the limit it is testing
// deserves the question. The production numbers are 128 MiB and 64 MiB, so
// crossing them honestly costs hundreds of megabytes per case and several
// cases here need to cross them more than once. The seams let each case cross
// a small bound with the SAME code path, and
// TestOutboxLimitSeamsDefaultToTheProductionConstants pins the production
// values so a seam can never quietly become the real limit.

// jsonPayloadOfSize returns a valid JSON document of exactly n bytes.
func jsonPayloadOfSize(t *testing.T, n int) []byte {
	t.Helper()
	// {"p":"<pad>"} is 8 bytes of frame plus the padding.
	const frame = 8
	if n < frame+1 {
		t.Fatalf("jsonPayloadOfSize: %d is below the %d-byte frame", n, frame+1)
	}
	b := []byte(`{"p":"` + strings.Repeat("x", n-frame) + `"}`)
	if len(b) != n {
		t.Fatalf("jsonPayloadOfSize built %d bytes, want %d", len(b), n)
	}
	return b
}

// insertRawOutboxRow writes a row STRAIGHT TO SQL, bypassing writeOutboxTx.
//
// That bypass is the point rather than a shortcut: rows over the row cap
// cannot be written through the guarded path once the guard exists, and rows
// left behind by a binary older than the guard are exactly the population the
// claim-side defence is for. This is the only way to construct one.
func insertRawOutboxRow(t *testing.T, s *Store, id, workspaceID, occurredAt string, payload []byte) {
	t.Helper()
	if _, err := s.db.Exec(s.q(`
		INSERT INTO event_outbox (id, workspace_id, event_type, subject_kind, subject_id, payload, hop, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?)
	`), id, workspaceID, kernelevents.ItemCreated, kernelevents.SubjectItem, id, string(payload), occurredAt); err != nil {
		t.Fatalf("insert raw outbox row %s: %v", id, err)
	}
}

func TestOutboxLimitSeamsDefaultToTheProductionConstants(t *testing.T) {
	// A ZERO-VALUE Store, not just a constructed one. The seams are plain int
	// fields, so the failure worth excluding is a Store that skipped the
	// constructor and thereby enforced no bound at all — which is what a
	// naive "override or zero" read would produce.
	var zero Store
	if got := zero.outboxRowCap(); got != MaxOutboxPayloadBytes {
		t.Errorf("zero-value Store row cap = %d, want %d — an unset seam must mean the production "+
			"constant, never no limit", got, MaxOutboxPayloadBytes)
	}
	if got := zero.outboxClaimBudget(); got != maxOutboxClaimBytes {
		t.Errorf("zero-value Store claim budget = %d, want %d", got, maxOutboxClaimBytes)
	}

	s := testStore(t)
	if got := s.outboxRowCap(); got != MaxOutboxPayloadBytes {
		t.Errorf("row cap = %d, want %d", got, MaxOutboxPayloadBytes)
	}
	if got := s.outboxClaimBudget(); got != maxOutboxClaimBytes {
		t.Errorf("claim budget = %d, want %d", got, maxOutboxClaimBytes)
	}

	// DIALECT-INDEPENDENT, and the reason it is here rather than only inside
	// the Postgres round-trip test: the default suite runs on SQLite, where
	// that test passes trivially, so a claim ceiling collapsed back to the
	// write cap would go unnoticed in every run without PAD_TEST_POSTGRES_URL
	// (codex round 2). This assertion fails on either dialect.
	if MaxOutboxClaimableBytes <= MaxOutboxPayloadBytes {
		t.Errorf("MaxOutboxClaimableBytes (%d) must exceed MaxOutboxPayloadBytes (%d)",
			MaxOutboxClaimableBytes, MaxOutboxPayloadBytes)
	}

	// The row cap must sit ABOVE what MaxItemRenameCascadeBytes can marshal
	// to, or the vaguer outbox refusal preempts the rename cascade's own,
	// better-worded one on the very renames that bound describes. ~1.46x is
	// the measured marshal ratio; 2x is the margin this asserts.
	if MaxOutboxPayloadBytes < 2*MaxItemRenameCascadeBytes {
		t.Errorf("MaxOutboxPayloadBytes = %d, must be at least 2x MaxItemRenameCascadeBytes (%d) so "+
			"rename_cascade_too_large always fires first", MaxOutboxPayloadBytes, MaxItemRenameCascadeBytes)
	}
	// And the claim budget must be at or under the row cap, or the budget
	// stops bounding anything.
	if maxOutboxClaimBytes > MaxOutboxPayloadBytes {
		t.Errorf("claim budget %d exceeds the row cap %d", maxOutboxClaimBytes, MaxOutboxPayloadBytes)
	}
}

func TestOutboxRefusesAPayloadOverTheRowCap(t *testing.T) {
	s := testStore(t)
	s.outboxRowCapOverride = 4096
	ws := createTestWorkspace(t, s, "OutboxRowCap")

	write := func(t *testing.T, subject string, size int) error {
		t.Helper()
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback()
		err = writeOutboxTx(tx, s, OutboxEvent{
			WorkspaceID:   ws.ID,
			EventType:     kernelevents.ItemCreated,
			SubjectID:     subject,
			Payload:       jsonPayloadOfSize(t, size),
			PayloadFamily: kernelevents.PayloadItemSnapshot,
		})
		if err == nil {
			if cerr := tx.Commit(); cerr != nil {
				t.Fatalf("commit: %v", cerr)
			}
		}
		return err
	}

	// BOTH SIDES OF THE BOUNDARY, so a > that becomes >= dies here rather
	// than surviving as an off-by-one nobody can see.
	if err := write(t, "at-cap", s.outboxRowCap()); err != nil {
		t.Fatalf("a payload of exactly the cap was refused: %v", err)
	}

	err := write(t, "over-cap", s.outboxRowCap()+1)
	var typed *OversizedOutboxPayloadError
	if !errors.As(err, &typed) {
		t.Fatalf("a payload one byte over the cap returned %v (%T), want *OversizedOutboxPayloadError", err, err)
	}
	if typed.Bytes != s.outboxRowCap()+1 {
		t.Errorf("Bytes = %d, want %d", typed.Bytes, s.outboxRowCap()+1)
	}
	if typed.Limit != s.outboxRowCap() {
		t.Errorf("Limit = %d, want %d", typed.Limit, s.outboxRowCap())
	}
	if typed.EventType != kernelevents.ItemCreated {
		t.Errorf("EventType = %q, want %q — the caller needs to see which event was refused",
			typed.EventType, kernelevents.ItemCreated)
	}

	// THE REFUSAL MUST FAIL THE MUTATION, not drop the event. The hop bound
	// in the same function returns nil precisely so the mutation survives, so
	// asserting the opposite disposition here is what keeps the two apart.
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM event_outbox WHERE subject_id = ?`), "over-cap").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("over-cap rows = %d, want 0", n)
	}
}

func TestOutboxClaimStopsAtTheByteBudget(t *testing.T) {
	s := testStore(t)
	s.outboxRowCapOverride = 8192
	s.outboxClaimBudgetOverride = 10000
	ws := createTestWorkspace(t, s, "OutboxClaimBudget")

	// Five 4 KiB rows against a 10,000-byte budget: the third crosses it, so
	// a correct pass takes two and leaves three.
	const size = 4096
	for i := 0; i < 5; i++ {
		insertRawOutboxRow(t, s, fmt.Sprintf("row-%02d", i), ws.ID,
			fmt.Sprintf("2026-01-01T00:00:%02dZ", i), jsonPayloadOfSize(t, size))
	}

	events, err := s.ClaimPendingOutboxEvents("drainer", 100, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("claimed %d rows, want 2 — the budget admits two 4096-byte rows and the third "+
			"crosses 10000", len(events))
	}
	total := 0
	for _, ev := range events {
		total += len(ev.Payload)
	}
	if total > s.outboxClaimBudget() {
		t.Errorf("claimed %d payload bytes, over the %d budget", total, s.outboxClaimBudget())
	}
	// COUNTERFACTUAL: without the budget the pass takes all five, since the
	// row limit is 100. Asserting "not all of them" is what distinguishes a
	// working budget from a claim that happened to be short.
	if len(events) == 5 {
		t.Errorf("the pass claimed every row; the byte budget did nothing")
	}

	// The rows it left must still be claimable, or the budget converted a
	// throughput bound into data loss.
	rest, err := s.ClaimPendingOutboxEvents("drainer", 100, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(rest) == 0 {
		t.Errorf("the second pass claimed nothing; rows past the budget must still drain")
	}
}

func TestOutboxClaimAlwaysTakesOneRowOverBudget(t *testing.T) {
	s := testStore(t)
	s.outboxRowCapOverride = 65536
	s.outboxClaimBudgetOverride = 1024
	ws := createTestWorkspace(t, s, "OutboxClaimStarvation")

	// Under the row cap and so legitimately written, but larger than a whole
	// pass's budget. Refusing it for cost would starve it in every pass
	// forever — the failure this arm exists to prevent.
	insertRawOutboxRow(t, s, "fat", ws.ID, "2026-01-01T00:00:00Z", jsonPayloadOfSize(t, 32768))
	insertRawOutboxRow(t, s, "thin", ws.ID, "2026-01-01T00:00:01Z", jsonPayloadOfSize(t, 100))

	events, err := s.ClaimPendingOutboxEvents("drainer", 100, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(events) != 1 || events[0].ID != "fat" {
		t.Fatalf("claimed %d rows (%v), want exactly the one oversized-for-the-budget row: the first "+
			"candidate is taken whatever it costs, and nothing after it fits", len(events), idsOf(events))
	}
}

func TestOutboxClaimSkipsRowsOverTheRowCapWithoutJamming(t *testing.T) {
	s := testStore(t)
	s.outboxRowCapOverride = 4096 // claimable ceiling is 8192
	ws := createTestWorkspace(t, s, "OutboxClaimJam")

	// The oversized row is the OLDEST, so it is what the oldest-first
	// candidate query reaches first. That ordering is the whole test: a
	// version that filtered these in Go rather than in the predicate would
	// let them consume candidate slots and starve everything behind them.
	insertRawOutboxRow(t, s, "legacy-huge", ws.ID, "2026-01-01T00:00:00Z", jsonPayloadOfSize(t, 40000))
	insertRawOutboxRow(t, s, "normal", ws.ID, "2026-01-01T00:00:01Z", jsonPayloadOfSize(t, 100))

	// limit=1 makes the starvation observable. With the exclusion in SQL the
	// single slot goes to "normal"; without it the slot goes to the oversized
	// row and "normal" never drains.
	events, err := s.ClaimPendingOutboxEvents("drainer", 1, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(events) != 1 || events[0].ID != "normal" {
		t.Fatalf("claimed %v, want [normal] — an oversized row must not occupy a candidate slot", idsOf(events))
	}

	// And it must be REPORTED, or an event that will never be delivered
	// disappears with nobody told.
	rows, err := s.OversizedPendingOutbox(5)
	if err != nil {
		t.Fatalf("OversizedPendingOutbox: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "legacy-huge" {
		t.Fatalf("OversizedPendingOutbox = %+v, want the one legacy row", rows)
	}
	// NOT an equality check. The reported size is what the driver will hand to
	// Scan, and on Postgres that is the JSONB — text rendering, which inserts a
	// space after every colon and comma: this 40,000-byte payload reads back as
	// 40,001 there. Asserting the written size would be asserting a SQLite
	// implementation detail on both dialects.
	if rows[0].Bytes < 40000 {
		t.Errorf("Bytes = %d, want at least the 40000 written", rows[0].Bytes)
	}
}

func idsOf(events []OutboxEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.ID)
	}
	return out
}

func TestOutboxClaimSpendsTheBudgetOnBatchSiblingsToo(t *testing.T) {
	s := testStore(t)
	s.outboxRowCapOverride = 8192
	s.outboxClaimBudgetOverride = 10000
	ws := createTestWorkspace(t, s, "OutboxSiblingBudget")

	// One batch of five 4 KiB rows. The candidate query takes the oldest, and
	// the sibling scan then pulls the rest of the batch — historically PAST
	// the row limit and with no byte accounting at all, which is the door
	// that made the budget bypassable: one large batch is an unbounded read
	// no row limit touches.
	const size = 4096
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("sib-%02d", i)
		insertRawOutboxRow(t, s, id, ws.ID, fmt.Sprintf("2026-01-01T00:00:%02dZ", i), jsonPayloadOfSize(t, size))
		if _, err := s.db.Exec(s.q(`UPDATE event_outbox SET batch_id = ? WHERE id = ?`), "batch-1", id); err != nil {
			t.Fatalf("set batch_id: %v", err)
		}
	}

	events, err := s.ClaimPendingOutboxEvents("drainer", 1, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	total := 0
	for _, ev := range events {
		total += len(ev.Payload)
	}
	if total > s.outboxClaimBudget() {
		t.Fatalf("claimed %d payload bytes across %d rows, over the %d budget — siblings are not "+
			"spending it", total, len(events), s.outboxClaimBudget())
	}
	if len(events) == 5 {
		t.Fatalf("the whole batch was claimed; the sibling scan bypassed the byte budget")
	}
	// BOTH BOUNDS, because "fewer than five" alone is satisfied by an
	// implementation that never claims siblings at all (codex round 1). The
	// limit passed above is 1, so anything past the first row IS a sibling.
	if len(events) < 2 {
		t.Fatalf("claimed %d rows with limit=1; siblings are not being claimed at all, which is a "+
			"different bug wearing this test's passing result", len(events))
	}
	// Splitting the batch is defined behaviour, not loss: the rest must still
	// be claimable on a later pass.
	rest, err := s.ClaimPendingOutboxEvents("drainer", 1, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(rest) == 0 {
		t.Fatalf("siblings left behind by the budget never drained")
	}
}

func TestScrubOutboxUserRefsBatchesWithoutLosingRows(t *testing.T) {
	s := testStore(t)
	// Force many small batches so the cursor, the lock ordering and the
	// termination condition are all exercised rather than skipped by a single
	// pass that happens to fit.
	s.outboxScrubRowsOverride = 3
	deleted, bystander := scrubTestUsers(t, s)
	ws := createTestWorkspace(t, s, "ScrubBatching")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Commented", "")
	clearOutbox(t, s)

	const n = 10
	mine := make([]string, 0, n)
	emitInTx(t, s, func(tx *sql.Tx) error {
		for i := 0; i < n; i++ {
			c := &models.Comment{ID: newID(), ItemID: item.ID, WorkspaceID: ws.ID,
				Author: "Scrub Me", UserID: deleted.ID, Body: fmt.Sprintf("body %d", i)}
			mine = append(mine, c.ID)
			if err := s.emitCommentEventTx(tx, kernelevents.CommentCreated, c); err != nil {
				return err
			}
		}
		other := &models.Comment{ID: newID(), ItemID: item.ID, WorkspaceID: ws.ID,
			Author: "Bystander", UserID: bystander.ID, Body: "untouched"}
		return s.emitCommentEventTx(tx, kernelevents.CommentCreated, other)
	})

	scrubInTx(t, s, deleted.ID)

	// EVERY row, not just the first batch. A cursor that failed to advance
	// would loop forever; one that advanced too far would leave the tail
	// carrying the deleted id, which is the silent half and the reason this
	// counts rather than spot-checking.
	for _, id := range mine {
		got := outboxPayloadFor(t, s, id, kernelevents.CommentCreated)
		if _, present := got["user_id"]; present {
			t.Fatalf("comment %s kept user_id after a batched scrub: %v", id, got["user_id"])
		}
	}
}

func TestScrubOutboxUserRefsTerminatesOnLikeFalsePositives(t *testing.T) {
	s := testStore(t)
	s.outboxScrubRowsOverride = 3
	deleted, _ := scrubTestUsers(t, s)
	ws := createTestWorkspace(t, s, "ScrubCursor")
	clearOutbox(t, s)

	// LIKE FALSE POSITIVES: the id appears in `body`, which is not a user-ref
	// key, so scrubUserRefsFromPayload rewrites nothing and the rows keep
	// matching the candidate query on every pass. They are the population the
	// cursor rule exists for — "advance past the last row READ, not the last
	// row rewritten" — and without them a cursor bug is invisible, because
	// every genuinely scrubbed row drops out of the candidate set by itself.
	for i := 0; i < 7; i++ {
		insertRawOutboxRow(t, s, fmt.Sprintf("fp-%02d", i), ws.ID,
			fmt.Sprintf("2026-01-01T00:00:%02dZ", i),
			[]byte(fmt.Sprintf(`{"body":"mentions %s in prose"}`, deleted.ID)))
	}

	// Bounded so a non-advancing cursor FAILS rather than hanging the package
	// until the suite-wide timeout, which would report as an unrelated
	// casualty in whatever test happened to be running.
	done := make(chan error, 1)
	go func() { done <- scrubInTxErr(s, deleted.ID) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("scrub: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("scrub did not terminate: rows that LIKE-match but are never rewritten must still " +
			"advance the batch cursor")
	}

	// And the false positives are left byte-identical — consumed by the
	// cursor, not damaged by it.
	var body string
	if err := s.db.QueryRow(s.q(`SELECT CAST(payload AS TEXT) FROM event_outbox WHERE id = ?`), "fp-00").Scan(&body); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !strings.Contains(body, deleted.ID) {
		t.Errorf("a LIKE false positive was rewritten: %s", body)
	}
}

// scrubInTxErr runs the scrub on its own transaction and RETURNS the error,
// unlike the shared scrubInTx helper which fails the test from whatever
// goroutine calls it — not safe from the watchdog goroutine above.
func scrubInTxErr(s *Store, userID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.ScrubOutboxUserRefsTx(tx, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func TestARowWrittenAtTheCapIsStillClaimable(t *testing.T) {
	s := testStore(t)
	s.outboxRowCapOverride = 4096
	ws := createTestWorkspace(t, s, "OutboxCapRoundTrip")

	// THE POSTGRES ROUND TRIP, and the reason the claim's ceiling is not the
	// write cap. A payload accepted at exactly the cap is stored as JSONB
	// there and read back one byte larger, so a claim thresholded at the same
	// number would exclude a row its own guard had just admitted: written,
	// never delivered, never reported, reaped seven days later. This passes
	// trivially on SQLite and is the whole point of running it on both.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID:   ws.ID,
		EventType:     kernelevents.ItemCreated,
		SubjectID:     "at-cap",
		Payload:       jsonPayloadOfSize(t, s.outboxRowCap()),
		PayloadFamily: kernelevents.PayloadItemSnapshot,
	}); err != nil {
		tx.Rollback()
		t.Fatalf("write at the cap was refused: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	events, err := s.ClaimPendingOutboxEvents("drainer", 10, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("claimed %d rows, want 1 — a row this binary wrote must always be claimable by it", len(events))
	}
	if skipped, err := s.OversizedPendingOutbox(5); err != nil {
		t.Fatalf("OversizedPendingOutbox: %v", err)
	} else if len(skipped) != 0 {
		t.Fatalf("a row written at the cap was reported oversized: %+v", skipped)
	}
}

func TestOutboxClaimStopsAtTheRowCap(t *testing.T) {
	s := testStore(t)
	s.outboxRowCapOverride = 8192
	s.outboxClaimRowsOverride = 3
	ws := createTestWorkspace(t, s, "OutboxClaimRows")

	// Tiny payloads, so the BYTE budget is nowhere near spent and only the row
	// cap can stop the pass. All in one batch, because the sibling scan is the
	// path that collects past the row limit by design and is therefore the one
	// a byte-only budget leaves unbounded.
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("tiny-%02d", i)
		insertRawOutboxRow(t, s, id, ws.ID, fmt.Sprintf("2026-01-01T00:00:%02dZ", i), jsonPayloadOfSize(t, 40))
		if _, err := s.db.Exec(s.q(`UPDATE event_outbox SET batch_id = ? WHERE id = ?`), "batch-1", id); err != nil {
			t.Fatalf("set batch_id: %v", err)
		}
	}

	events, err := s.ClaimPendingOutboxEvents("drainer", 1, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(events) > s.outboxClaimRows() {
		t.Fatalf("claimed %d rows, over the %d row cap — a byte budget alone does not bound the "+
			"per-row overhead of a large batch", len(events), s.outboxClaimRows())
	}
	if len(events) == 0 {
		t.Fatalf("claimed nothing")
	}
}

func TestBulkEventIsRefusedBeforeItIsMarshalled(t *testing.T) {
	s := testStore(t)
	s.outboxRowCapOverride = 4096
	ws := createTestWorkspace(t, s, "BulkPreMarshal")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Member bodies alone exceed the cap, so the refusal is owed before
	// json.Marshal builds the payload — building a payload only to reject it
	// is the allocation the cap exists to avoid.
	members := []*models.Item{}
	for i := 0; i < 4; i++ {
		members = append(members, &models.Item{
			ID:           newID(),
			WorkspaceID:  ws.ID,
			CollectionID: col.ID,
			Title:        fmt.Sprintf("Member %d", i),
			Content:      strings.Repeat("x", 2000),
		})
	}

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	err = s.emitBulkItemEventTx(tx, ws.ID, members, map[string]any{"kind": "field_option_renamed"})
	var typed *OversizedOutboxPayloadError
	if !errors.As(err, &typed) {
		t.Fatalf("emitBulkItemEventTx returned %v (%T), want *OversizedOutboxPayloadError", err, err)
	}
	if typed.Limit != s.outboxRowCap() {
		t.Errorf("Limit = %d, want %d", typed.Limit, s.outboxRowCap())
	}
	// EXACTLY the projection, which is what makes this test discriminate.
	// Without the early-out the mutation still fails — writeOutboxTx refuses
	// the marshalled payload with the same error type — so any assertion that
	// only checks the type passes against the code this exists to guard. The
	// marshalled size is strictly larger (quoting, escaping, key names, the
	// envelope), so equality with the projection can only come from the
	// pre-marshal check.
	if want := projectedBulkPayloadBytes(members); typed.Bytes != want {
		t.Errorf("Bytes = %d, want the projected %d — the refusal came from writeOutboxTx after "+
			"marshalling, not from the early-out before it", typed.Bytes, want)
	}
}

func TestEverythingWrittenIsClaimable(t *testing.T) {
	s := testStore(t)
	s.outboxRowCapOverride = 4096 // claimable ceiling 8192
	ws := createTestWorkspace(t, s, "OutboxWriteReadAgreement")

	// A PAYLOAD THAT IS SMALL IN GO AND HUGE AS STORED. Postgres reparses JSON
	// numbers as numeric and prints them positionally, so 1e-3000 is 7 bytes
	// on the way in and about 3,001 on the way out — measured, and unbounded
	// as the exponent grows. SQLite stores the text verbatim and expands
	// nothing.
	//
	// ASSERTED AS A PROPERTY, not as a dialect-specific outcome, because the
	// two dialects legitimately differ on whether this is refused: what must
	// hold on both is that a row the write accepted is a row the claim will
	// read. Without the stored-size check in writeOutboxTx this row is
	// accepted on Postgres and then excluded from every claim for the rest of
	// its retention window — written, undeliverable, and reported only as an
	// oversized-row log line.
	var b strings.Builder
	b.WriteString(`{"n":[`)
	for i := 0; i < 100; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("1e-3000")
	}
	b.WriteString(`]}`)
	payload := []byte(b.String())
	if len(payload) > s.outboxRowCap() {
		t.Fatalf("fixture is %d bytes, over the %d write cap — it must be refusable only by the "+
			"STORED size, or it discriminates nothing", len(payload), s.outboxRowCap())
	}

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	writeErr := writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID:   ws.ID,
		EventType:     kernelevents.ItemCreated,
		SubjectID:     "numeric",
		Payload:       payload,
		PayloadFamily: kernelevents.PayloadItemSnapshot,
	})
	if writeErr != nil {
		var typed *OversizedOutboxPayloadError
		if !errors.As(writeErr, &typed) {
			tx.Rollback()
			t.Fatalf("write failed for an unexpected reason: %v", writeErr)
		}
		// Refused on the stored size. The mutation fails, nothing is
		// committed, and the invariant holds vacuously.
		tx.Rollback()
		if typed.Bytes <= len(payload) {
			t.Errorf("refusal reported %d bytes, not more than the %d written — it was charged "+
				"against the Go payload, not the stored row", typed.Bytes, len(payload))
		}
		return
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	events, err := s.ClaimPendingOutboxEvents("drainer", 10, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("the write was accepted but the claim returned %d rows: a row this binary wrote "+
			"must be a row this binary can read back", len(events))
	}
}

func TestBatchSiblingQueryIsBoundedInSQL(t *testing.T) {
	s := testStore(t)
	s.outboxRowCapOverride = 8192
	s.outboxClaimRowsOverride = 3
	ws := createTestWorkspace(t, s, "OutboxSiblingSQLBound")

	for i := 0; i < 9; i++ {
		id := fmt.Sprintf("sq-%02d", i)
		insertRawOutboxRow(t, s, id, ws.ID, fmt.Sprintf("2026-01-01T00:00:%02dZ", i), jsonPayloadOfSize(t, 40))
		if _, err := s.db.Exec(s.q(`UPDATE event_outbox SET batch_id = ? WHERE id = ?`), "batch-1", id); err != nil {
			t.Fatalf("set batch_id: %v", err)
		}
	}

	// DRIVEN DIRECTLY, because the caller's row cap would hide the defect this
	// is about: a sibling query that materialised the whole batch and let the
	// caller truncate afterwards passes every assertion on the claim's return
	// value while keeping exactly the unbounded allocation the cap was added
	// to remove (codex round 2).
	siblings, err := s.claimableBatchSiblings("batch-1", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("claimableBatchSiblings: %v", err)
	}
	if len(siblings) > s.outboxClaimRows() {
		t.Fatalf("the sibling query returned %d rows for a 9-row batch with a cap of %d; it is "+
			"bounded by the caller, not by SQL", len(siblings), s.outboxClaimRows())
	}
}

func TestScrubSpendsItsByteBudgetNotJustItsRowLimit(t *testing.T) {
	s := testStore(t)
	// Row limit high enough that only the BYTE budget can split the work.
	s.outboxScrubRowsOverride = 100
	s.outboxClaimBudgetOverride = 4096
	deleted, _ := scrubTestUsers(t, s)
	ws := createTestWorkspace(t, s, "ScrubByteBudget")
	clearOutbox(t, s)

	var batches []int
	s.afterOutboxScrubBatch = func(rows int) { batches = append(batches, rows) }

	// Eight rows of ~2 KiB each against a 4 KiB budget: a correct loop runs
	// several batches, a byte-blind one runs exactly one.
	const n = 8
	for i := 0; i < n; i++ {
		insertRawOutboxRow(t, s, fmt.Sprintf("big-%02d", i), ws.ID,
			fmt.Sprintf("2026-01-01T00:00:%02dZ", i),
			[]byte(fmt.Sprintf(`{"body":"%s mentions %s"}`, strings.Repeat("x", 2000), deleted.ID)))
	}

	if err := scrubInTxErr(s, deleted.ID); err != nil {
		t.Fatalf("scrub: %v", err)
	}
	if len(batches) < 2 {
		t.Fatalf("the scrub ran %d batch(es) over %d rows of ~2 KiB against a %d-byte budget; the "+
			"byte budget is not being spent, so peak memory is still the whole match set",
			len(batches), n, s.outboxScrubBytes())
	}
	for i, rows := range batches {
		if rows > s.outboxScrubRows() {
			t.Errorf("batch %d carried %d rows, over the %d row limit", i, rows, s.outboxScrubRows())
		}
	}
}
