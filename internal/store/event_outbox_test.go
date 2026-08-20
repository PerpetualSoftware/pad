package store

import (
	"encoding/json"
	"sort"
	"strings"
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
		WorkspaceID:   ws.ID,
		EventType:     "item.frobnicated",
		SubjectKind:   kernelevents.SubjectItem,
		Payload:       []byte(`{"id":"x"}`),
		PayloadFamily: kernelevents.PayloadItemSnapshot,
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
		WorkspaceID:   ws.ID,
		EventType:     kernelevents.ItemCreated,
		SubjectKind:   kernelevents.SubjectItem,
		PayloadFamily: kernelevents.PayloadItemSnapshot,
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

	// A caller-supplied occurred_at is IGNORED — the write stamps it. SPEC-3
	// pins time-relative predicates to this value, so accepting one would let
	// a caller silently change how a predicate evaluates.
	if err := writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID:   ws.ID,
		EventType:     kernelevents.ItemCreated,
		SubjectID:     "stamped",
		Payload:       []byte(`{"id":"stamped"}`),
		OccurredAt:    "1999-01-01T00:00:00Z",
		PayloadFamily: kernelevents.PayloadItemSnapshot,
	}); err != nil {
		t.Fatalf("writeOutboxTx: %v", err)
	}
	var stamped string
	if err := tx.QueryRow(s.q(`SELECT occurred_at FROM event_outbox WHERE subject_id = ?`), "stamped").Scan(&stamped); err != nil {
		t.Fatalf("read occurred_at: %v", err)
	}
	if stamped == "1999-01-01T00:00:00Z" {
		t.Fatalf("occurred_at was taken from the caller; a supplied timestamp silently changes " +
			"how `within` predicates evaluate")
	}

	// At the bound: written. Past it: dropped, and NOT an error — the
	// mutation itself was legitimate, only the cascade it would extend is
	// not (SPEC-3 §L5).
	atBound := OutboxEvent{
		WorkspaceID:   ws.ID,
		EventType:     kernelevents.ItemCreated,
		SubjectKind:   kernelevents.SubjectItem,
		SubjectID:     "at-bound",
		Payload:       []byte(`{"id":"at-bound"}`),
		Hop:           maxOutboxHop,
		PayloadFamily: kernelevents.PayloadItemSnapshot,
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

// outboxBulkPayload returns the single item.bulk_updated payload in the
// outbox, failing unless there is exactly one.
func outboxBulkPayload(t *testing.T, s *Store) map[string]any {
	t.Helper()
	rows, err := s.db.Query(s.q(`SELECT payload FROM event_outbox WHERE event_type = ?`), kernelevents.ItemBulkUpdated)
	if err != nil {
		t.Fatalf("query bulk payloads: %v", err)
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
		t.Fatalf("item.bulk_updated rows = %d, want exactly 1 — a bulk mutation must emit ONE "+
			"wire event, not per-row fan-out", len(payloads))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(payloads[0]), &m); err != nil {
		t.Fatalf("decode bulk payload: %v", err)
	}
	return m
}

func TestOutbox_CollectionOptionRenameEmitsOneBulkEvent(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox option rename")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	a := createTestItem(t, s, ws.ID, col.ID, "One", "body")
	b := createTestItem(t, s, ws.ID, col.ID, "Two", "body")
	wip := `{"status":"in-progress"}`
	for _, it := range []*models.Item{a, b} {
		if _, err := s.UpdateItem(it.ID, models.ItemUpdate{Fields: &wip}); err != nil {
			t.Fatalf("seed status: %v", err)
		}
	}
	clearOutbox(t, s)

	n, err := s.MigrateItemFieldValues(col.ID, []models.FieldMigration{
		{Field: "status", RenameOptions: map[string]string{"in-progress": "doing"}},
	})
	if err != nil {
		t.Fatalf("MigrateItemFieldValues: %v", err)
	}
	if n != 2 {
		t.Fatalf("migrated rows = %d, want 2 — the test is not exercising a bulk mutation", n)
	}

	payload := outboxBulkPayload(t, s)
	if got := payload["member_count"]; got != float64(2) {
		t.Fatalf("member_count = %v, want 2", got)
	}
	members, _ := payload["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2 — per-member snapshots are what keep item-level bindings "+
			"evaluable across a batched delivery", len(members))
	}
	// MEMBER IDENTITY, not just count (Codex round 3): a reader that returned
	// the same member twice, or the wrong items entirely, would satisfy a
	// count-only assertion while making per-member binding evaluation fire on
	// the wrong set.
	gotIDs := map[string]bool{}
	for _, raw := range members {
		m, _ := raw.(map[string]any)
		id, _ := m["id"].(string)
		if gotIDs[id] {
			t.Fatalf("member %s appears twice; a duplicate makes per-member evaluation fire "+
				"twice on one item", id)
		}
		gotIDs[id] = true
		// The snapshots must be POST-migration: a pre-migration snapshot would
		// carry the value the mutation just removed.
		fields, _ := m["fields"].(string)
		if !strings.Contains(fields, "doing") {
			t.Fatalf("member snapshot fields = %q, want the POST-migration value", fields)
		}
	}
	if !gotIDs[a.ID] || !gotIDs[b.ID] {
		t.Fatalf("members = %v, want exactly the two migrated items %s and %s", gotIDs, a.ID, b.ID)
	}

	// THE DELTA, which is the only part of the payload that says what the one
	// mutation actually did. An emitter that dropped it would leave consumers
	// with two changed items and no explanation.
	delta, _ := payload["delta"].(map[string]any)
	if delta["kind"] != "field_option_renamed" {
		t.Fatalf("delta kind = %v, want %q", delta["kind"], "field_option_renamed")
	}
	renames, _ := delta["renames"].([]any)
	if len(renames) != 1 {
		t.Fatalf("delta renames = %v, want one entry describing status in-progress→doing", renames)
	}
	r0, _ := renames[0].(map[string]any)
	if r0["field"] != "status" || r0["from"] != "in-progress" || r0["to"] != "doing" {
		t.Fatalf("delta rename = %v, want {field:status from:in-progress to:doing}", r0)
	}
}

func TestOutbox_WikiTitleCascadeEmitsOneBulkEvent(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox cascade")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Old Title", "the target")
	linker := createTestItem(t, s, ws.ID, col.ID, "Linker", "points at [[Old Title]] here")
	clearOutbox(t, s)

	newTitle := "New Title"
	if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// The renamed item emits its own item.updated for its own slice...
	if got := outboxEventsFor(t, s, target.ID); len(got) != 1 || got[0] != kernelevents.ItemUpdated {
		t.Fatalf("renamed item events = %v, want exactly [%s]", got, kernelevents.ItemUpdated)
	}

	// ...and the cascade's rewritten backlinks arrive as ONE batch event, not
	// as per-backlink fan-out.
	payload := outboxBulkPayload(t, s)
	if payload["member_count"] != float64(1) {
		t.Fatalf("member_count = %v, want 1", payload["member_count"])
	}
	members, _ := payload["members"].([]any)
	m, _ := members[0].(map[string]any)
	if m["id"] != linker.ID {
		t.Fatalf("member id = %v, want the backlink item %s", m["id"], linker.ID)
	}
	if content, _ := m["content"].(string); !strings.Contains(content, "[[New Title]]") {
		t.Fatalf("member content = %q, want the REWRITTEN content — the cascade's whole delta is "+
			"content, so a pre-rewrite snapshot makes the event useless", content)
	}
}

// TestItemUpdatedSliceChanged_NonDefaultAndNonStringDoneKey covers two gaps
// Codex round 3 found in the classification tests above: every other test uses
// the default "status" key, so a classifier hard-coded to mask "status" would
// pass them all; and none of them exercise a done-key value the status
// machinery cannot read.
func TestItemUpdatedSliceChanged_NonDefaultAndNonStringDoneKey(t *testing.T) {
	// A CUSTOM done-field key must behave exactly as "status" does. A
	// classifier that masks the literal "status" instead of the collection's
	// declared key fails here and nowhere else.
	base := &models.Item{ID: "i", WorkspaceID: "w", Fields: `{"stage":"open","note":"a"}`}
	stageOnly := *base
	stageOnly.Fields = `{"stage":"done","note":"a"}`
	changed, err := itemUpdatedSliceChanged(base, &stageOnly, "stage")
	if err != nil {
		t.Fatalf("itemUpdatedSliceChanged: %v", err)
	}
	if changed {
		t.Fatalf("a bare change to the CUSTOM done field %q reported an item.updated delta; "+
			"the mask must follow the collection's declared key, not the literal \"status\"", "stage")
	}

	// The same custom key, but the value is a NUMBER. extractFieldValue only
	// reads JSON strings, so status_changed will never report this — and if
	// the mask deleted the key anyway, the remainder would compare equal and
	// the mutation would emit NOTHING AT ALL. A real field change has to land
	// in item.updated's slice when no other event can describe it.
	numBefore := &models.Item{ID: "i", WorkspaceID: "w", Fields: `{"stage":1}`}
	numAfter := &models.Item{ID: "i", WorkspaceID: "w", Fields: `{"stage":2}`}
	changed, err = itemUpdatedSliceChanged(numBefore, numAfter, "stage")
	if err != nil {
		t.Fatalf("itemUpdatedSliceChanged: %v", err)
	}
	if !changed {
		t.Fatalf("a numeric done-field change reported NO delta; status_changed cannot see a " +
			"non-string value either, so this mutation would be silently unobservable")
	}

	// A non-object blob cannot have anything masked out of it. It still has to
	// yield a correct changed/did-not-change answer.
	arrBefore := &models.Item{ID: "i", WorkspaceID: "w", Fields: `[{"status":"open"}]`}
	arrAfter := &models.Item{ID: "i", WorkspaceID: "w", Fields: `[{"status":"done"}]`}
	changed, err = itemUpdatedSliceChanged(arrBefore, arrAfter, "status")
	if err != nil {
		t.Fatalf("itemUpdatedSliceChanged: %v", err)
	}
	if !changed {
		t.Fatalf("a change inside a non-object fields blob reported NO delta")
	}
	changed, err = itemUpdatedSliceChanged(arrBefore, arrBefore, "status")
	if err != nil {
		t.Fatalf("itemUpdatedSliceChanged: %v", err)
	}
	if changed {
		t.Fatalf("an unchanged non-object fields blob reported a delta")
	}
}

func TestWriteOutboxTx_RejectsMalformedJSONPayload(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox malformed")

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	// Postgres would reject this at the INSERT (JSONB); SQLite's TEXT column
	// would accept it. Validating in Go makes both backends fail identically
	// rather than one silently persisting an undeliverable event.
	err = writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID:   ws.ID,
		EventType:     kernelevents.ItemCreated,
		SubjectKind:   kernelevents.SubjectItem,
		SubjectID:     "x",
		Payload:       []byte(`{"id":"x"`),
		PayloadFamily: kernelevents.PayloadItemSnapshot,
	})
	if err == nil {
		t.Fatalf("writeOutboxTx accepted a malformed JSON payload; on SQLite this persists an " +
			"event no consumer can parse, while Postgres refuses the same write")
	}
}

func TestOutbox_NoOpCommentEditEmitsNothing(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox comment noop")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Commented", "body")

	c, err := s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Body: "same"})
	if err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	clearOutbox(t, s)

	// Re-saving an identical body still MATCHES the row — the UPDATE keys on
	// id alone and updated_at moves — so the row-count check cannot suppress
	// this. Comparing the body is what does.
	if _, err := s.UpdateComment(c.ID, "same"); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
	if got := outboxEventsFor(t, s, c.ID); len(got) != 0 {
		t.Fatalf("events = %v, want none — a no-op comment edit must not announce a change", got)
	}

	// ...and a real edit still does.
	if _, err := s.UpdateComment(c.ID, "different"); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
	if got := outboxEventsFor(t, s, c.ID); len(got) != 1 || got[0] != kernelevents.CommentUpdated {
		t.Fatalf("events = %v, want exactly [%s] — the no-op gate must not suppress real edits",
			got, kernelevents.CommentUpdated)
	}
}

// TestEmitBulkItemEventTx_PartitionsByMemberWorkspace pins the multi-tenancy
// property: a member snapshot must never be published under a workspace that
// is not its own.
//
// The wiki-title cascade's source query selects on target_item_id alone and
// carries each source row's workspace_id per-row, so a cross-workspace member
// is not excluded by construction. Publishing the whole member set under the
// caller's workspace would put one workspace's item content on another
// workspace's webhook — so the emitter partitions rather than trusting that
// today's queries happen never to produce one.
func TestEmitBulkItemEventTx_PartitionsByMemberWorkspace(t *testing.T) {
	s := testStore(t)
	wsA := createTestWorkspace(t, s, "Partition A")
	wsB := createTestWorkspace(t, s, "Partition B")

	members := []*models.Item{
		{ID: "a1", WorkspaceID: wsA.ID, Fields: "{}"},
		{ID: "b1", WorkspaceID: wsB.ID, Fields: "{}"},
		{ID: "a2", WorkspaceID: wsA.ID, Fields: "{}"},
	}

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Caller passes workspace A, as the cascade would when A's item was renamed.
	if err := s.emitBulkItemEventTx(tx, wsA.ID, members, map[string]any{"kind": "test"}); err != nil {
		tx.Rollback()
		t.Fatalf("emitBulkItemEventTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	rows, err := s.db.Query(s.q(`SELECT workspace_id, payload FROM event_outbox WHERE event_type = ? ORDER BY workspace_id`),
		kernelevents.ItemBulkUpdated)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var ws, payload string
		if err := rows.Scan(&ws, &payload); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		ms, _ := m["members"].([]any)
		counts[ws] = len(ms)
		// Every member in this row must belong to the row's workspace.
		for _, raw := range ms {
			mm, _ := raw.(map[string]any)
			if mm["workspace_id"] != ws {
				t.Fatalf("event for workspace %s carries a member from %v — one workspace's item "+
					"content would reach another workspace's webhook", ws, mm["workspace_id"])
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	if len(counts) != 2 {
		t.Fatalf("bulk events = %d, want 2 (one per member workspace); a single event means the "+
			"member set was published under the caller's workspace", len(counts))
	}
	if counts[wsA.ID] != 2 || counts[wsB.ID] != 1 {
		t.Fatalf("member counts = %v, want {A:2, B:1}", counts)
	}
}

// TestOutbox_StatusChangeFromEmptyCarriesEmptyPriorStatus pins the SPEC-3
// conformance point Codex round 6 found: prior_status must be PRESENT on
// item.status_changed even when the prior status was empty.
//
// An item can transition from no status at all, and that is a real status
// change. With `omitempty` on a plain string the key vanished, leaving a
// binding predicate unable to distinguish "the prior status was empty" from
// "this event carries no prior status" — which is exactly the distinction the
// envelope pseudo-field exists to make.
func TestOutbox_StatusChangeFromEmptyCarriesEmptyPriorStatus(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox empty prior")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "No status yet", "body")

	// Clear the status first, so the next write is a genuine ""→value change.
	cleared := `{"status":""}`
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: &cleared}); err != nil {
		t.Fatalf("clear status: %v", err)
	}
	clearOutbox(t, s)

	set := `{"status":"open"}`
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: &set}); err != nil {
		t.Fatalf("set status: %v", err)
	}

	if got := outboxEventsFor(t, s, item.ID); len(got) != 1 || got[0] != kernelevents.ItemStatusChanged {
		t.Fatalf("events = %v, want exactly [%s]", got, kernelevents.ItemStatusChanged)
	}
	payload := outboxPayloadFor(t, s, item.ID, kernelevents.ItemStatusChanged)
	raw, present := payload["prior_status"]
	if !present {
		t.Fatalf("prior_status is ABSENT on a \"\"→open transition; a predicate cannot tell an "+
			"empty prior status from an event that has none. payload keys: %v", payload)
	}
	if raw != "" {
		t.Fatalf("prior_status = %v, want the empty string", raw)
	}

	// And it must still be absent where it is genuinely meaningless.
	clearOutbox(t, s)
	title := "Renamed"
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	updatedPayload := outboxPayloadFor(t, s, item.ID, kernelevents.ItemUpdated)
	if _, present := updatedPayload["prior_status"]; present {
		t.Fatalf("prior_status present on item.updated, where there is no prior status to report")
	}
}

// TestOutbox_PayloadsOmitAssigneeIdentity pins the privacy-lifecycle property.
//
// An outbox payload is a frozen snapshot that outlives its subject by design.
// Account deletion's de-identify pass nulls user identity on LIVE rows so a
// departed user stops being legible; it cannot reach a frozen payload. If the
// payload captured the assignee's name and email, those would stay readable in
// the outbox after the account was deleted — and today nothing drains or prunes
// the table.
func TestOutbox_PayloadsOmitAssigneeIdentity(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox PII")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	user := createTestUser(t, s, "assignee@example.com", "Assignee Person", "pw-assignee-123")
	if err := s.AddWorkspaceMember(ws.ID, user.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	item := createTestItem(t, s, ws.ID, col.ID, "Assigned", "body")
	clearOutbox(t, s)

	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{AssignedUserID: &user.ID}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	payload := outboxPayloadFor(t, s, item.ID, kernelevents.ItemUpdated)

	// The item's OWN column stays — a predicate filters on it and it is an
	// opaque id, not personal data.
	if payload["assigned_user_id"] != user.ID {
		t.Fatalf("assigned_user_id = %v, want %s — the item's own column must survive the scrub",
			payload["assigned_user_id"], user.ID)
	}
	// The join-populated identity must not.
	for _, key := range []string{"assigned_user_name", "assigned_user_email"} {
		if v, present := payload[key]; present {
			t.Fatalf("payload carries %s = %v; a frozen snapshot keeps it legible after the "+
				"account is deleted, which the de-identify pass cannot reach", key, v)
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "assignee@example.com") {
		t.Fatalf("payload contains the assignee's email address anywhere: %s", raw)
	}
}

func TestOutbox_CommentDeleteEmitsRefOnly(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox comment delete")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Commented", "body")

	const secret = "the body a user asked to remove"
	c, err := s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Body: secret})
	if err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	clearOutbox(t, s)

	if err := s.DeleteComment(c.ID); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}

	if got := outboxEventsFor(t, s, c.ID); len(got) != 1 || got[0] != kernelevents.CommentDeleted {
		t.Fatalf("events = %v, want exactly [%s] — without a delete marker a hard-deleted "+
			"comment's created event is the only record it existed", got, kernelevents.CommentDeleted)
	}

	payload := outboxPayloadFor(t, s, c.ID, kernelevents.CommentDeleted)
	if payload["id"] != c.ID || payload["item_id"] != item.ID || payload["workspace_id"] != ws.ID {
		t.Fatalf("payload = %v, want the ids a consumer needs to reconcile", payload)
	}
	// REF-ONLY is the whole contract (SPEC-3 v1.4): a deletion event must not
	// re-ship what it deletes.
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("comment.deleted payload contains the deleted body: %s", raw)
	}
	if _, present := payload["body"]; present {
		t.Fatalf("comment.deleted payload carries a body key: %v", payload)
	}
}

func TestOutbox_SoftDeletedAttachmentClaimEmitsRefOnly(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox attachment removed")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Has attachment", "body")

	const storageKey = "blob/secret-locator"
	att := &models.Attachment{
		WorkspaceID: ws.ID,
		ItemID:      &item.ID,
		StorageKey:  storageKey,
		Filename:    "private-notes.pdf",
		MimeType:    "application/pdf",
		SizeBytes:   99,
	}
	if err := s.CreateAttachmentForLiveItem(att); err != nil {
		t.Fatalf("CreateAttachmentForLiveItem: %v", err)
	}
	if err := s.SoftDeleteAttachment(att.ID); err != nil {
		t.Fatalf("SoftDeleteAttachment: %v", err)
	}
	clearOutbox(t, s)

	claimed, err := s.ClaimSoftDeletedAttachment(att.ID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ClaimSoftDeletedAttachment: %v", err)
	}
	if !claimed {
		t.Fatalf("claim did not take the row; the test is not exercising the emit path")
	}

	if got := outboxEventsFor(t, s, att.ID); len(got) != 1 || got[0] != kernelevents.AttachmentRemoved {
		t.Fatalf("events = %v, want exactly [%s]", got, kernelevents.AttachmentRemoved)
	}
	payload := outboxPayloadFor(t, s, att.ID, kernelevents.AttachmentRemoved)
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The sharp edge: an attachment payload carries the STORAGE KEY, so a
	// full-snapshot removal event would hand out a locator for bytes the
	// system just reclaimed.
	if strings.Contains(string(raw), storageKey) {
		t.Fatalf("attachment.removed payload contains the storage key: %s", raw)
	}
	if strings.Contains(string(raw), "private-notes.pdf") {
		t.Fatalf("attachment.removed payload contains the filename: %s", raw)
	}
}

func TestOutbox_NeverAttachedClaimEmitsNothing(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox never attached")

	// An orphan: never attached to an item, so it never emitted
	// attachment.added either.
	orphan := &models.Attachment{
		WorkspaceID: ws.ID,
		StorageKey:  "blob/orphan",
		Filename:    "stray.bin",
		MimeType:    "application/octet-stream",
		SizeBytes:   10,
	}
	if err := s.CreateAttachment(orphan); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	clearOutbox(t, s)

	claimed, err := s.ClaimNeverAttachedAttachment(orphan.ID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ClaimNeverAttachedAttachment: %v", err)
	}
	if !claimed {
		t.Fatalf("claim did not take the row; the test is not exercising the path")
	}

	// Announcing this removal would hand a consumer a deletion for an id it
	// has never seen. The asymmetry with the soft-deleted claim is the point.
	if got := outboxEventsFor(t, s, orphan.ID); len(got) != 0 {
		t.Fatalf("events = %v, want none — this row never emitted attachment.added", got)
	}
}

// TestOutbox_VariantClaimEmitsNothing pins the symmetry Codex round 8 found
// missing: a row that could not have emitted attachment.added must not emit
// attachment.removed.
//
// Variants are the systematic case. A thumbnail is written silently (it carries
// a parent, so the add gate excludes it), then tombstoned by its original's
// cascade, and arrives at ClaimSoftDeletedAttachment like any other soft-deleted
// row. Before the gates were made symmetric it announced a removal for a subject
// no consumer had ever been told about.
func TestOutbox_VariantClaimEmitsNothing(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox variant removal")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Has thumbnail", "body")

	original := &models.Attachment{
		WorkspaceID: ws.ID,
		ItemID:      &item.ID,
		StorageKey:  "blob/orig",
		Filename:    "photo.jpg",
		MimeType:    "image/jpeg",
		SizeBytes:   100,
	}
	if err := s.CreateAttachmentForLiveItem(original); err != nil {
		t.Fatalf("create original: %v", err)
	}
	variant := models.AttachmentVariantThumbMd
	thumb := &models.Attachment{
		WorkspaceID: ws.ID,
		ItemID:      &item.ID,
		ParentID:    &original.ID,
		Variant:     &variant,
		StorageKey:  "blob/thumb",
		Filename:    "photo-thumb.jpg",
		MimeType:    "image/jpeg",
		SizeBytes:   10,
	}
	if err := s.CreateAttachmentForLiveItem(thumb); err != nil {
		t.Fatalf("create thumb: %v", err)
	}
	// Confirm the premise rather than assuming it: the variant never announced
	// its arrival. Without this leg the test could pass because nothing was
	// ever emitted for the wrong reason.
	if got := outboxEventsFor(t, s, thumb.ID); len(got) != 0 {
		t.Fatalf("variant emitted %v on creation; the premise of this test is wrong", got)
	}

	if err := s.SoftDeleteAttachment(thumb.ID); err != nil {
		t.Fatalf("SoftDeleteAttachment: %v", err)
	}
	clearOutbox(t, s)

	claimed, err := s.ClaimSoftDeletedAttachment(thumb.ID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ClaimSoftDeletedAttachment: %v", err)
	}
	if !claimed {
		t.Fatalf("claim did not take the variant row; the test is not exercising the path")
	}

	if got := outboxEventsFor(t, s, thumb.ID); len(got) != 0 {
		t.Fatalf("events = %v, want none — a row that could not announce its arrival must not "+
			"announce its removal", got)
	}
}

// TestWriteOutboxTx_SubjectKindIsDerivedNotTrusted pins the round-9 finding.
//
// subject_kind is a pure function of the event name, so a caller-supplied
// value can only agree with the taxonomy or be wrong — and a wrong one used to
// persist silently and would misroute the event at drain time. Every earlier
// test passed either the correct value or none, which is precisely why this
// survived eight rounds of review.
func TestWriteOutboxTx_SubjectKindIsDerivedNotTrusted(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox subject kind")

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	// A mismatched kind is refused rather than stored.
	err = writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID:   ws.ID,
		EventType:     kernelevents.ItemCreated,
		SubjectKind:   kernelevents.SubjectComment,
		SubjectID:     "mismatched",
		Payload:       []byte(`{"id":"mismatched"}`),
		PayloadFamily: kernelevents.PayloadItemSnapshot,
	})
	if err == nil {
		t.Fatalf("writeOutboxTx stored item.created under subject kind %q; the drain would "+
			"route it as a comment", kernelevents.SubjectComment)
	}

	// An omitted kind is filled in from the taxonomy.
	if err := writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID:   ws.ID,
		EventType:     kernelevents.ItemCreated,
		SubjectID:     "derived",
		Payload:       []byte(`{"id":"derived"}`),
		PayloadFamily: kernelevents.PayloadItemSnapshot,
	}); err != nil {
		t.Fatalf("writeOutboxTx with no subject kind: %v", err)
	}
	var kind string
	if err := tx.QueryRow(s.q(`SELECT subject_kind FROM event_outbox WHERE subject_id = ?`), "derived").Scan(&kind); err != nil {
		t.Fatalf("read subject kind: %v", err)
	}
	if kind != kernelevents.SubjectItem {
		t.Fatalf("subject_kind = %q, want %q", kind, kernelevents.SubjectItem)
	}
}

// TestWriteOutboxTx_RejectsMismatchedPayloadFamily pins Codex round 10's
// finding: canonical membership validates the NAME, and said nothing about
// whether the bytes attached to it were the right shape.
func TestWriteOutboxTx_RejectsMismatchedPayloadFamily(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox family")

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	// item.created paired with a REF-ONLY deletion payload: both halves are
	// individually valid — a canonical name and well-formed JSON — and they
	// were not meant for each other.
	err = writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID:   ws.ID,
		EventType:     kernelevents.ItemCreated,
		SubjectID:     "mixed",
		Payload:       []byte(`{"id":"mixed","workspace_id":"w"}`),
		PayloadFamily: kernelevents.PayloadRefOnly,
	})
	if err == nil {
		t.Fatalf("writeOutboxTx accepted a %s payload under %s; a consumer would parse the "+
			"bytes as an item snapshot and find nothing it expected",
			kernelevents.PayloadRefOnly, kernelevents.ItemCreated)
	}

	// And an undeclared family is refused too — silence must not pass for
	// agreement.
	err = writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID: ws.ID,
		EventType:   kernelevents.ItemCreated,
		SubjectID:   "undeclared",
		Payload:     []byte(`{"id":"undeclared"}`),
	})
	if err == nil {
		t.Fatalf("writeOutboxTx accepted an event with no declared payload family")
	}
}

// TestPayloadFamily_CoversEveryCanonicalEvent keeps the two taxonomy maps from
// drifting: a canonical name with no declared family would be unwritable, and
// the failure would surface as a confusing runtime rejection rather than here.
func TestPayloadFamily_CoversEveryCanonicalEvent(t *testing.T) {
	for _, name := range kernelevents.Canonical() {
		if family, ok := kernelevents.PayloadFamily(name); !ok || family == "" {
			t.Errorf("canonical event %q has no payload family", name)
		}
	}
}
