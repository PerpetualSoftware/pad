package store

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// Tests for the transactional event outbox (SPEC-3 §choke point, TASK-2658).
//
// The load-bearing case here is the DISJOINT-DELTA RULE (SPEC-3 v1.3):
// canonical events partition a mutation's delta rather than competing to
// describe it, and a mutation emits every event whose slice actually changed.
// The mixed update — status AND a non-status field in one call — is the case a
// naive "did the status change?" branch gets wrong, so it is tested explicitly
// rather than left to follow from the two single-slice cases.

// outboxEventsFor returns the canonical event types recorded for an item,
// sorted for stable comparison.
func outboxEventsFor(t *testing.T, s *Store, itemID string) []string {
	t.Helper()
	rows, err := s.db.Query(s.q(`SELECT event_type FROM event_outbox WHERE subject_id = ? ORDER BY occurred_at, id`), itemID)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var et string
		if err := rows.Scan(&et); err != nil {
			t.Fatalf("scan event_type: %v", err)
		}
		out = append(out, et)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox: %v", err)
	}
	sort.Strings(out)
	return out
}

// outboxPayloadFor returns the decoded payload of the single event of the
// given type for an item, failing if there is not exactly one.
func outboxPayloadFor(t *testing.T, s *Store, itemID, eventType string) map[string]any {
	t.Helper()
	rows, err := s.db.Query(s.q(`SELECT payload FROM event_outbox WHERE subject_id = ? AND event_type = ?`), itemID, eventType)
	if err != nil {
		t.Fatalf("query payload: %v", err)
	}
	defer rows.Close()

	var payloads []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan payload: %v", err)
		}
		payloads = append(payloads, p)
	}
	if len(payloads) != 1 {
		t.Fatalf("outbox rows for %s = %d, want exactly 1", eventType, len(payloads))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(payloads[0]), &m); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return m
}

func clearOutbox(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(s.q(`DELETE FROM event_outbox`)); err != nil {
		t.Fatalf("clear outbox: %v", err)
	}
}

func TestOutbox_CreateEmitsItemCreatedWithSnapshot(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox create")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item := createTestItem(t, s, ws.ID, col.ID, "Emitted on create", "body")

	if got := outboxEventsFor(t, s, item.ID); len(got) != 1 || got[0] != kernelevents.ItemCreated {
		t.Fatalf("events = %v, want exactly [%s]", got, kernelevents.ItemCreated)
	}

	// The payload must carry the SNAPSHOT with item fields at the top level
	// (embedded, not nested under an "item" key) — SPEC-3 §Bindings applies
	// query/1 #where fragments verbatim, and query/1 addresses item fields by
	// their own names.
	payload := outboxPayloadFor(t, s, item.ID, kernelevents.ItemCreated)
	if payload["id"] != item.ID {
		t.Fatalf("payload id = %v, want %s (snapshot is not embedded at the top level)", payload["id"], item.ID)
	}
	if payload["title"] != "Emitted on create" {
		t.Fatalf("payload title = %v, want the created title", payload["title"])
	}
	if _, present := payload["prior_status"]; present {
		t.Fatalf("prior_status present on item.created; it is meaningless there and must be omitted")
	}
}

func TestOutbox_BareStatusFlipEmitsStatusChangedOnly(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox status")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Status only", "body")
	clearOutbox(t, s)

	fields := `{"status":"in-progress"}`
	updated, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: &fields})
	if err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	if updated == nil {
		t.Fatalf("UpdateItem returned nil")
	}

	got := outboxEventsFor(t, s, item.ID)
	if len(got) != 1 || got[0] != kernelevents.ItemStatusChanged {
		t.Fatalf("events = %v, want exactly [%s] — a bare status flip must not also emit item.updated",
			got, kernelevents.ItemStatusChanged)
	}

	payload := outboxPayloadFor(t, s, item.ID, kernelevents.ItemStatusChanged)
	if payload["prior_status"] != "open" {
		t.Fatalf("prior_status = %v, want %q — the envelope pseudo-field is what makes nonterminal→terminal filterable",
			payload["prior_status"], "open")
	}
}

func TestOutbox_MixedUpdateEmitsBothSlices(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox mixed")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Mixed", "body")
	clearOutbox(t, s)

	// One update that moves TWO slices: the status field and the title.
	// Under the disjoint-delta rule each slice gets exactly one event.
	fields := `{"status":"done"}`
	title := "Mixed, renamed"
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: &fields, Title: &title}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}

	got := outboxEventsFor(t, s, item.ID)
	want := []string{kernelevents.ItemStatusChanged, kernelevents.ItemUpdated}
	sort.Strings(want)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("events = %v, want %v — a mixed update must emit BOTH slices; branching on "+
			"\"was this a status update\" drops the item.updated half silently", got, want)
	}
}

func TestOutbox_NonStatusUpdateEmitsUpdatedOnly(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox plain")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Plain", "body")
	clearOutbox(t, s)

	title := "Plain, renamed"
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}

	got := outboxEventsFor(t, s, item.ID)
	if len(got) != 1 || got[0] != kernelevents.ItemUpdated {
		t.Fatalf("events = %v, want exactly [%s]", got, kernelevents.ItemUpdated)
	}
}

func TestOutbox_NoOpUpdateEmitsNothing(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox noop")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Noop", "body")
	clearOutbox(t, s)

	// Rewrite the title to the value it already holds. The row is touched
	// (updated_at and seq both move), but no slice the taxonomy names has
	// changed — so the disjoint-delta rule emits nothing. This is the leg
	// that proves the rule diffs SLICES rather than detecting "an update
	// happened": the excluded bookkeeping columns must not manufacture a
	// change on their own.
	same := item.Title
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &same}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}

	if got := outboxEventsFor(t, s, item.ID); len(got) != 0 {
		t.Fatalf("events = %v, want none — updated_at/seq/last_modified_by move on every "+
			"mutation and must not be read as a delta", got)
	}
}

// TestItemUpdatedSliceChanged_IgnoresPerMutationBookkeeping drives the slice
// diff DIRECTLY, with hand-built snapshots, because the end-to-end no-op test
// above cannot see this.
//
// WHY IT CANNOT: store.now() formats RFC3339, which is SECOND granularity. A
// test that creates an item and updates it milliseconds later produces the
// SAME updated_at string on both snapshots, so the exclusion of updated_at is
// never exercised — removing "updated_at" from itemDeltaExcludedKeys leaves
// TestOutbox_NoOpUpdateEmitsNothing green (verified by mutation, not assumed).
// In production the two writes are seconds or hours apart and the exclusion is
// entirely load-bearing: without it, EVERY update would emit item.updated and
// the disjoint-delta rule would be decorative.
//
// So the exclusions get an instrument that does not depend on wall-clock luck:
// two snapshots differing ONLY in the per-mutation bookkeeping columns must
// compare equal, and one differing in a real field must not.
func TestItemUpdatedSliceChanged_IgnoresPerMutationBookkeeping(t *testing.T) {
	base := &models.Item{
		ID:          "item-1",
		WorkspaceID: "ws-1",
		Title:       "Same title",
		Fields:      `{"status":"open","priority":"high"}`,
		Seq:         10,
		UpdatedAt:   time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC),
	}

	// Only bookkeeping moved: a touched-but-unchanged row.
	touched := *base
	touched.Seq = 11
	touched.UpdatedAt = time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	touched.LastModifiedBy = "someone-else"

	changed, err := itemUpdatedSliceChanged(base, &touched, "status")
	if err != nil {
		t.Fatalf("itemUpdatedSliceChanged: %v", err)
	}
	if changed {
		t.Fatalf("a row whose only movement was seq/updated_at/last_modified_by reported a delta; " +
			"every update would emit item.updated and the disjoint-delta rule would be decorative")
	}

	// The status field alone moved: owned by item.status_changed, so the
	// item.updated slice must NOT report a change.
	statusOnly := *base
	statusOnly.Fields = `{"status":"done","priority":"high"}`
	changed, err = itemUpdatedSliceChanged(base, &statusOnly, "status")
	if err != nil {
		t.Fatalf("itemUpdatedSliceChanged: %v", err)
	}
	if changed {
		t.Fatalf("a bare status change reported an item.updated delta; status is status_changed's " +
			"slice and would be described twice")
	}

	// A non-status field inside the same blob moved: that IS item.updated's
	// slice. This is the leg that proves the status mask is surgical rather
	// than excluding the whole fields blob.
	priorityOnly := *base
	priorityOnly.Fields = `{"status":"open","priority":"low"}`
	changed, err = itemUpdatedSliceChanged(base, &priorityOnly, "status")
	if err != nil {
		t.Fatalf("itemUpdatedSliceChanged: %v", err)
	}
	if !changed {
		t.Fatalf("a priority change inside the fields blob reported NO delta; masking the status " +
			"key must not mask the whole blob")
	}

	// Key order in the blob is not semantic.
	reordered := *base
	reordered.Fields = `{"priority":"high","status":"open"}`
	changed, err = itemUpdatedSliceChanged(base, &reordered, "status")
	if err != nil {
		t.Fatalf("itemUpdatedSliceChanged: %v", err)
	}
	if changed {
		t.Fatalf("re-serializing the fields blob in a different key order reported a delta")
	}
}

func TestWriteOutboxTx_RejectsNonCanonicalEvent(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox reject")

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	err = writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID: ws.ID,
		EventType:   "item.frobnicated",
		SubjectKind: kernelevents.SubjectItem,
		Payload:     []byte(`{"id":"x"}`),
	})
	if err == nil {
		t.Fatalf("writeOutboxTx accepted a non-canonical event name; the events/1 set is closed, " +
			"and an unknown name would reach the PUBLIC webhook surface")
	}
}

func TestWriteOutboxTx_RejectsEmptyPayload(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox empty")

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	err = writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID: ws.ID,
		EventType:   kernelevents.ItemCreated,
		SubjectKind: kernelevents.SubjectItem,
	})
	if err == nil {
		t.Fatalf("writeOutboxTx accepted an empty payload; binding predicates evaluate against " +
			"the payload snapshot, so such an event is undeliverable by construction")
	}
}

func TestWriteOutboxTx_DropsPastCascadeBound(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox hop")

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	// At the bound: written. Past it: dropped, and NOT an error — the
	// mutation itself was legitimate, only the cascade it would extend is
	// not (SPEC-3 §L5).
	atBound := OutboxEvent{
		WorkspaceID: ws.ID,
		EventType:   kernelevents.ItemCreated,
		SubjectKind: kernelevents.SubjectItem,
		SubjectID:   "at-bound",
		Payload:     []byte(`{"id":"at-bound"}`),
		Hop:         maxOutboxHop,
	}
	if err := writeOutboxTx(tx, s, atBound); err != nil {
		t.Fatalf("writeOutboxTx at hop %d: %v", maxOutboxHop, err)
	}

	past := atBound
	past.ID = ""
	past.SubjectID = "past-bound"
	past.Hop = maxOutboxHop + 1
	if err := writeOutboxTx(tx, s, past); err != nil {
		t.Fatalf("writeOutboxTx past the cascade bound returned an error; dropping is the bound "+
			"working and must not fail the mutation: %v", err)
	}

	var n int
	if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM event_outbox WHERE subject_id = ?`), "past-bound").Scan(&n); err != nil {
		t.Fatalf("count past-bound: %v", err)
	}
	if n != 0 {
		t.Fatalf("past-bound rows = %d, want 0 — the cascade bound did not drop the event", n)
	}

	if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM event_outbox WHERE subject_id = ?`), "at-bound").Scan(&n); err != nil {
		t.Fatalf("count at-bound: %v", err)
	}
	if n != 1 {
		t.Fatalf("at-bound rows = %d, want 1 — the bound is off by one and is dropping events "+
			"that should be written", n)
	}
}

func TestOutbox_DeleteEmitsPreArchiveSnapshot(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox delete")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "To be archived", "body")
	clearOutbox(t, s)

	if err := s.DeleteItem(item.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	if got := outboxEventsFor(t, s, item.ID); len(got) != 1 || got[0] != kernelevents.ItemDeleted {
		t.Fatalf("events = %v, want exactly [%s]", got, kernelevents.ItemDeleted)
	}

	// SPEC-3 §Bindings: the payload must be the FINAL PRE-ARCHIVE state. It is
	// the only thing keeping a deleted item addressable to predicates, which
	// never consult the live store — so an empty or post-archive snapshot here
	// makes every item.deleted binding unevaluable.
	payload := outboxPayloadFor(t, s, item.ID, kernelevents.ItemDeleted)
	if payload["title"] != "To be archived" {
		t.Fatalf("payload title = %v, want the pre-archive title", payload["title"])
	}
	if payload["id"] != item.ID {
		t.Fatalf("payload id = %v, want %s", payload["id"], item.ID)
	}
}

func TestOutbox_RedeleteEmitsNothing(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox redelete")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Archived once", "body")

	if err := s.DeleteItem(item.ID); err != nil {
		t.Fatalf("first DeleteItem: %v", err)
	}
	clearOutbox(t, s)

	// The second delete affects zero rows: nothing was archived, so nothing
	// may be announced. An event here would describe a mutation that did not
	// happen — the exact class of lie the outbox exists to make impossible.
	if err := s.DeleteItem(item.ID); err == nil {
		t.Fatalf("re-deleting an archived item unexpectedly succeeded")
	}
	if got := outboxEventsFor(t, s, item.ID); len(got) != 0 {
		t.Fatalf("events = %v, want none — a no-op delete emitted an event", got)
	}
}

func TestOutbox_RestoreEmitsItemRestored(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox restore")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Round trip", "body")
	if err := s.DeleteItem(item.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	clearOutbox(t, s)

	if _, err := s.RestoreItem(item.ID); err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}

	if got := outboxEventsFor(t, s, item.ID); len(got) != 1 || got[0] != kernelevents.ItemRestored {
		t.Fatalf("events = %v, want exactly [%s] — restore was silent before SPEC-3 v1.1 and an "+
			"item could reappear with no observable event", got, kernelevents.ItemRestored)
	}
}

func TestOutbox_MoveEmitsMovedOnlyWhenNothingElseChanged(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox move")
	from := createTestCollection(t, s, ws.ID, "Tasks")
	to := createTestCollection(t, s, ws.ID, "Bugs")
	item := createTestItem(t, s, ws.ID, from.ID, "Relocating", "body")
	clearOutbox(t, s)

	if _, err := s.MoveItem(item.ID, to.ID, item.Fields); err != nil {
		t.Fatalf("MoveItem: %v", err)
	}

	if got := outboxEventsFor(t, s, item.ID); len(got) != 1 || got[0] != kernelevents.ItemMoved {
		t.Fatalf("events = %v, want exactly [%s] — a pure relocation must not leak into "+
			"item.updated's slice", got, kernelevents.ItemMoved)
	}
}

func TestOutbox_CommentCreateAndUpdateEmit(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox comments")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Commented", "body")
	clearOutbox(t, s)

	c, err := s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Body: "first"})
	if err != nil {
		t.Fatalf("CreateComment: %v", err)
	}

	if got := outboxEventsFor(t, s, c.ID); len(got) != 1 || got[0] != kernelevents.CommentCreated {
		t.Fatalf("events = %v, want exactly [%s]", got, kernelevents.CommentCreated)
	}

	// The SUBJECT is the comment, not the item it hangs off — a binding keyed
	// on the item as subject could not tell a comment from an edit to the item.
	payload := outboxPayloadFor(t, s, c.ID, kernelevents.CommentCreated)
	if payload["id"] != c.ID {
		t.Fatalf("payload id = %v, want the comment id %s", payload["id"], c.ID)
	}
	// The payload must be the STORED row: a snapshot assembled from caller
	// input would carry the body but not the values the write path filled in.
	if payload["body"] != "first" {
		t.Fatalf("payload body = %v, want %q", payload["body"], "first")
	}
	if payload["item_id"] != item.ID {
		t.Fatalf("payload item_id = %v, want %s — a comment binding filters on this", payload["item_id"], item.ID)
	}

	clearOutbox(t, s)
	if _, err := s.UpdateComment(c.ID, "edited"); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
	if got := outboxEventsFor(t, s, c.ID); len(got) != 1 || got[0] != kernelevents.CommentUpdated {
		t.Fatalf("events = %v, want exactly [%s]", got, kernelevents.CommentUpdated)
	}
	updatedPayload := outboxPayloadFor(t, s, c.ID, kernelevents.CommentUpdated)
	if updatedPayload["body"] != "edited" {
		t.Fatalf("payload body = %v, want the POST-update body; an in-tx read that took the pool "+
			"connection instead of the tx would return the pre-write row here", updatedPayload["body"])
	}
}

func TestOutbox_AttachmentAddedSkipsVariants(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox attachments")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Has attachments", "body")
	clearOutbox(t, s)

	original := &models.Attachment{
		WorkspaceID: ws.ID,
		ItemID:      &item.ID,
		StorageKey:  "blob/original",
		Filename:    "photo.jpg",
		MimeType:    "image/jpeg",
		SizeBytes:   1234,
	}
	if err := s.CreateAttachmentForLiveItem(original); err != nil {
		t.Fatalf("CreateAttachmentForLiveItem(original): %v", err)
	}
	if got := outboxEventsFor(t, s, original.ID); len(got) != 1 || got[0] != kernelevents.AttachmentAdded {
		t.Fatalf("events = %v, want exactly [%s]", got, kernelevents.AttachmentAdded)
	}

	// A THUMBNAIL IS AN ATTACHMENT ROW TOO. Without the gate, one image
	// upload announces three attachment.added events — two of them for files
	// no user added. This is the leg that proves the gate exists; deleting it
	// makes this test fail rather than making it pass more easily.
	variant := models.AttachmentVariantThumbMd
	thumb := &models.Attachment{
		WorkspaceID: ws.ID,
		ItemID:      &item.ID,
		ParentID:    &original.ID,
		Variant:     &variant,
		StorageKey:  "blob/thumb-md",
		Filename:    "photo-thumb.jpg",
		MimeType:    "image/jpeg",
		SizeBytes:   234,
	}
	if err := s.CreateAttachmentForLiveItem(thumb); err != nil {
		t.Fatalf("CreateAttachmentForLiveItem(thumb): %v", err)
	}
	if got := outboxEventsFor(t, s, thumb.ID); len(got) != 0 {
		t.Fatalf("events for a %s variant = %v, want none — variants are derived rows, not "+
			"attachments a user added", variant, got)
	}
}

func TestOutbox_MemberJoinedEmits(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox members")
	user := createTestUser(t, s, "joiner@example.com", "Joiner", "pw-joiner-123")
	clearOutbox(t, s)

	if err := s.AddWorkspaceMember(ws.ID, user.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}

	if got := outboxEventsFor(t, s, user.ID); len(got) != 1 || got[0] != kernelevents.MemberJoined {
		t.Fatalf("events = %v, want exactly [%s]", got, kernelevents.MemberJoined)
	}
	payload := outboxPayloadFor(t, s, user.ID, kernelevents.MemberJoined)
	if payload["role"] != "editor" {
		t.Fatalf("payload role = %v, want %q", payload["role"], "editor")
	}
	if payload["workspace_id"] != ws.ID {
		t.Fatalf("payload workspace_id = %v, want %s", payload["workspace_id"], ws.ID)
	}
}

// TestOutbox_CrossWorkspaceMoveEmitsSourceArchive pins the gap that made this
// test exist.
//
// archiveItemForCopyTx deliberately REPRODUCES DeleteItem's UPDATE inside the
// copy's own transaction rather than calling it, so that the archive is atomic
// with the destination write. The consequence nobody had to think about before
// TASK-2658 is that it does not inherit DeleteItem's emit either: without an
// explicit one, a cross-workspace MOVE archives the source silently while an
// ordinary archive of the same item announces itself. Invisible until something
// consumes the outbox, at which point moves just stop being observable.
func TestOutbox_CrossWorkspaceMoveEmitsSourceArchive(t *testing.T) {
	f := newCopyFixture(t)
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Moving out", "body")
	clearOutbox(t, f.s)

	req := f.req()
	req.SourceItemID = src.ID
	req.ArchiveSource = true
	res := f.copy(t, req)

	if got := outboxEventsFor(t, f.s, src.ID); len(got) != 1 || got[0] != kernelevents.ItemDeleted {
		t.Fatalf("source events = %v, want exactly [%s] — the move archived the source with no event",
			got, kernelevents.ItemDeleted)
	}

	// The pre-archive snapshot must be the SOURCE's state, not the clone's.
	payload := outboxPayloadFor(t, f.s, src.ID, kernelevents.ItemDeleted)
	if payload["workspace_id"] != f.wsA.ID {
		t.Fatalf("payload workspace_id = %v, want the SOURCE workspace %s", payload["workspace_id"], f.wsA.ID)
	}

	// And the destination item still emits its own creation, from the same
	// transaction — the move is two canonical events, not one.
	if got := outboxEventsFor(t, f.s, res.Item.ID); len(got) != 1 || got[0] != kernelevents.ItemCreated {
		t.Fatalf("destination events = %v, want exactly [%s]", got, kernelevents.ItemCreated)
	}
}
