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
// the outbox after the account was deleted for as long as the row survived.
// TASK-2714 bounded that window with the drain and its two prunes; bounded is
// not zero, so the scrub is still what does the work.
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

// TestCanonicalEventsAreFullyDeclared pins the events/1 contract surface as an
// INDEPENDENT COPY: every canonical name, its subject kind and its payload
// family, written out here as literals.
//
// The previous version of this test iterated kernelevents.Canonical() and
// asserted each entry resolved SOMETHING non-empty. That check cannot fail for
// any table the compiler accepts — eventSpec requires both fields, so a
// corrupted table (a name deleted, a name added, item.deleted quietly rebased
// onto the ref-only payload) passed its own validation. A test that agrees with
// whatever the table says is not a test of the table.
//
// So the literals below must DISAGREE with the table when the table moves. That
// is the point of the duplication, and the duplication is deliberate: this is a
// PUBLIC contract (SPEC-3 §Taxonomy) where a rename is as breaking as an HTTP
// route change, and the cost of restating sixteen triples is one edit per
// intentional contract change — paid at exactly the moment a version note is
// owed anyway.
//
// TASK-2714 edits this table (the handler-path bulk mapping), which is why the
// independent copy lands as this unit's first commit.
func TestCanonicalEventsAreFullyDeclared(t *testing.T) {
	// The events/1 set at SPEC-3 v1.4. Adding, removing or re-homing an entry
	// here is a CONTRACT CHANGE: update the spec version and the taxonomy's
	// doc comment in the same commit.
	want := map[string]struct {
		subject string
		family  []string
		sse     string
	}{
		"item.created":        {kernelevents.SubjectItem, []string{kernelevents.PayloadItemSnapshot}, "item_created"},
		"item.updated":        {kernelevents.SubjectItem, []string{kernelevents.PayloadItemSnapshot}, "item_updated"},
		"item.status_changed": {kernelevents.SubjectItem, []string{kernelevents.PayloadItemSnapshot}, "item_updated"},
		"item.moved":          {kernelevents.SubjectItem, []string{kernelevents.PayloadItemSnapshot}, "item_updated"},
		"item.deleted":        {kernelevents.SubjectItem, []string{kernelevents.PayloadItemSnapshot}, "item_archived"},
		"item.restored":       {kernelevents.SubjectItem, []string{kernelevents.PayloadItemSnapshot}, "item_restored"},
		"item.bulk_updated":   {kernelevents.SubjectItemBatch, []string{kernelevents.PayloadItemBatch, kernelevents.PayloadItemBatchHeader}, "items_bulk_updated"},
		"comment.created":     {kernelevents.SubjectComment, []string{kernelevents.PayloadCommentSnapshot}, "comment_created"},
		"comment.updated":     {kernelevents.SubjectComment, []string{kernelevents.PayloadCommentSnapshot}, "comment_updated"},
		"comment.deleted":     {kernelevents.SubjectComment, []string{kernelevents.PayloadRefOnly}, "comment_deleted"},
		"attachment.added":    {kernelevents.SubjectAttachment, []string{kernelevents.PayloadAttachmentSnapshot}, ""},
		"attachment.removed":  {kernelevents.SubjectAttachment, []string{kernelevents.PayloadRefOnly}, ""},
		"member.joined":       {kernelevents.SubjectMember, []string{kernelevents.PayloadMember}, ""},
		"pack.installed":      {kernelevents.SubjectPack, []string{kernelevents.PayloadPack}, ""},
		"pack.upgraded":       {kernelevents.SubjectPack, []string{kernelevents.PayloadPack}, ""},
		"pack.disabled":       {kernelevents.SubjectPack, []string{kernelevents.PayloadPack}, ""},
	}

	// The name constants are pinned to their wire strings separately, because
	// the map above is keyed on literals: a renamed constant would otherwise
	// slip through as long as the table and the constant moved together.
	for constant, wire := range map[string]string{
		kernelevents.ItemCreated:       "item.created",
		kernelevents.ItemUpdated:       "item.updated",
		kernelevents.ItemStatusChanged: "item.status_changed",
		kernelevents.ItemMoved:         "item.moved",
		kernelevents.ItemDeleted:       "item.deleted",
		kernelevents.ItemRestored:      "item.restored",
		kernelevents.ItemBulkUpdated:   "item.bulk_updated",
		kernelevents.CommentCreated:    "comment.created",
		kernelevents.CommentUpdated:    "comment.updated",
		kernelevents.CommentDeleted:    "comment.deleted",
		kernelevents.AttachmentAdded:   "attachment.added",
		kernelevents.AttachmentRemoved: "attachment.removed",
		kernelevents.MemberJoined:      "member.joined",
		kernelevents.PackInstalled:     "pack.installed",
		kernelevents.PackUpgraded:      "pack.upgraded",
		kernelevents.PackDisabled:      "pack.disabled",
	} {
		if constant != wire {
			t.Errorf("event name constant = %q, want %q on the wire", constant, wire)
		}
	}

	got := kernelevents.Canonical()
	if len(got) != len(want) {
		t.Errorf("Canonical() has %d events, want %d — the contract set changed", len(got), len(want))
	}

	seen := make(map[string]bool, len(got))
	for _, name := range got {
		seen[name] = true
		expected, ok := want[name]
		if !ok {
			t.Errorf("Canonical() carries %q, which the events/1 contract does not declare", name)
			continue
		}
		kind, kindOK := kernelevents.SubjectKind(name)
		if !kindOK || kind != expected.subject {
			t.Errorf("SubjectKind(%q) = (%q, %v), want (%q, true)", name, kind, kindOK, expected.subject)
		}
		families, familiesOK := kernelevents.PayloadFamilies(name)
		if !familiesOK || !sameStrings(families, expected.family) {
			t.Errorf("PayloadFamilies(%q) = (%v, %v), want (%v, true)", name, families, familiesOK, expected.family)
		}
		for _, family := range expected.family {
			if !kernelevents.AllowsPayload(name, family) {
				t.Errorf("AllowsPayload(%q, %q) = false for a declared shape", name, family)
			}
		}
		if kernelevents.AllowsPayload(name, "") {
			t.Errorf("AllowsPayload(%q, \"\") = true — a caller declaring nothing must not match", name)
		}
		if kernelevents.AllowsPayload(name, "not_a_family") {
			t.Errorf("AllowsPayload(%q, \"not_a_family\") = true", name)
		}
		if !kernelevents.IsCanonical(name) {
			t.Errorf("IsCanonical(%q) = false for an event Canonical() returned", name)
		}
		// The SSE surface name, including the events that deliberately have
		// none. An empty want.sse asserts SILENCE — SurfaceSSE must report
		// false rather than handing back a name a caller would publish under.
		sse, sseOK := kernelevents.SurfaceSSE(name)
		if expected.sse == "" {
			if sseOK || sse != "" {
				t.Errorf("SurfaceSSE(%q) = (%q, %v), want (\"\", false) — this event has no SSE surface", name, sse, sseOK)
			}
		} else if !sseOK || sse != expected.sse {
			t.Errorf("SurfaceSSE(%q) = (%q, %v), want (%q, true)", name, sse, sseOK, expected.sse)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("events/1 declares %q but Canonical() does not return it", name)
		}
	}

	// Non-canonical names must report ok=false rather than an empty string a
	// caller declaring nothing would match — the fail-open shape Codex round 11
	// found in the two-map version.
	for _, name := range []string{"", "item.frobnicated", "comment.deleted.v2"} {
		if _, ok := kernelevents.PayloadFamilies(name); ok {
			t.Errorf("PayloadFamilies(%q) reported ok for a non-canonical name", name)
		}
		if kernelevents.AllowsPayload(name, kernelevents.PayloadItemSnapshot) {
			t.Errorf("AllowsPayload(%q, ...) = true for a non-canonical name", name)
		}
		if _, ok := kernelevents.SubjectKind(name); ok {
			t.Errorf("SubjectKind(%q) reported ok for a non-canonical name", name)
		}
		if kernelevents.IsCanonical(name) {
			t.Errorf("IsCanonical(%q) = true", name)
		}
		if _, ok := kernelevents.SurfaceSSE(name); ok {
			t.Errorf("SurfaceSSE(%q) reported ok for a non-canonical name", name)
		}
	}
}

// insertOutboxRow writes one outbox row directly, so a retention test can pin
// occurred_at / dispatched_at to values a real emit would never produce on
// demand.
func insertOutboxRow(t *testing.T, s *Store, id, workspaceID, occurredAt string, dispatchedAt *string) {
	t.Helper()
	insertOutboxRowBatch(t, s, id, workspaceID, occurredAt, dispatchedAt, "")
}

func insertOutboxRowBatch(t *testing.T, s *Store, id, workspaceID, occurredAt string, dispatchedAt *string, batchID string) {
	t.Helper()
	if _, err := s.db.Exec(s.q(`
		INSERT INTO event_outbox (id, workspace_id, event_type, subject_kind, subject_id, payload, hop, occurred_at, dispatched_at, batch_id)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, NULLIF(?, ''))
	`), id, workspaceID, kernelevents.ItemCreated, kernelevents.SubjectItem, "subj-"+id,
		`{"id":"subj-`+id+`","title":"frozen"}`, occurredAt, dispatchedAt, batchID); err != nil {
		t.Fatalf("insert outbox row %s: %v", id, err)
	}
}

func outboxRowIDs(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(s.q(`SELECT id FROM event_outbox ORDER BY id`))
	if err != nil {
		t.Fatalf("query outbox ids: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox ids: %v", err)
	}
	return out
}

// TestOutbox_PruneUndispatchedBoundsTheFrozenPayloadWindow pins the privacy
// half of retention (TASK-2714 requirement 3).
//
// PruneDispatchedOutbox filters on dispatched_at IS NOT NULL, so a row that
// can NEVER be delivered is unreachable by it and keeps its frozen payload
// forever. Both legs below discriminate a plausible wrong implementation: a
// prune that dropped the dispatched_at IS NULL clause would take the dispatched
// row too (and hand that table's retention two owners with different windows),
// and one that ignored the cutoff would take the young pending row that a
// retry is still owed.
func TestOutbox_PruneUndispatchedBoundsTheFrozenPayloadWindow(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox retention")
	clearOutbox(t, s)

	old := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339)
	recent := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	dispatchedAt := time.Now().UTC().Add(-71 * time.Hour).Format(time.RFC3339)

	insertOutboxRow(t, s, "a-old-pending", ws.ID, old, nil)
	insertOutboxRow(t, s, "b-old-dispatched", ws.ID, old, &dispatchedAt)
	insertOutboxRow(t, s, "c-recent-pending", ws.ID, recent, nil)

	// The test asserts its own premise: without all three rows present, the
	// survivor checks below would pass for a reason unrelated to the prune.
	if got := outboxRowIDs(t, s); len(got) != 3 {
		t.Fatalf("seeded rows = %v, want 3", got)
	}

	cutoff := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	liveClaim := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	n, err := s.PruneUndispatchedOutbox(cutoff, liveClaim)
	if err != nil {
		t.Fatalf("PruneUndispatchedOutbox: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want 1 (only the aged pending row)", n)
	}

	got := outboxRowIDs(t, s)
	want := []string{"b-old-dispatched", "c-recent-pending"}
	if len(got) != len(want) {
		t.Fatalf("survivors = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("survivors = %v, want %v", got, want)
		}
	}
}

// outboxBatchIDsFor returns the batch_id of every event recorded for an item.
func outboxBatchIDsFor(t *testing.T, s *Store, itemID string) []string {
	t.Helper()
	rows, err := s.db.Query(s.q(`SELECT COALESCE(batch_id, '') FROM event_outbox WHERE subject_id = ? ORDER BY occurred_at, id`), itemID)
	if err != nil {
		t.Fatalf("query batch ids: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatalf("scan batch_id: %v", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate batch ids: %v", err)
	}
	return out
}

// TestOutbox_WithEventBatchStampsEveryMutationPath drives ALL FIVE store entry
// points the bulk handler reaches, because the failure this guards against is
// per-method: a signature that accepts the option and never threads it to the
// emit compiles, passes every other test, and silently un-batches whichever
// bulk verbs route through it.
//
// THIS TEST IS NOT SUFFICIENT ON ITS OWN, and saying so is the point: it
// proves the OPTION works, not that the handler passes it. Codex round 1 found
// three handler call sites that never did.
// TestBulkItems_EveryVerbStampsOneBatchID covers that layer.
//
// The population is enumerated rather than sampled (CONVE-18): archive
// (DeleteItem), restore (RestoreItem), move (MoveItemWithPreCheck), field
// update (UpdateItemWithPreCheck) and assign (UpdateItem) are the complete set
// of mutating store calls handlers_items_bulk.go makes. My own escalation said
// four; it was five, and restore was the one missing.
//
// Each leg asserts the unstamped control too. Without it the test would pass
// for an implementation that stamped every event ever written.
func TestOutbox_WithEventBatchStampsEveryMutationPath(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox batch")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	other := createTestCollection(t, s, ws.ID, "Done")

	const batch = "batch-42"

	assertBatch := func(t *testing.T, itemID, want string) {
		t.Helper()
		got := outboxBatchIDsFor(t, s, itemID)
		if len(got) == 0 {
			t.Fatalf("no outbox rows for %s — the mutation emitted nothing, so the batch assertion proves nothing", itemID)
		}
		for _, b := range got {
			if b != want {
				t.Errorf("batch ids = %v, want every row stamped %q", got, want)
				return
			}
		}
	}

	t.Run("DeleteItem", func(t *testing.T) {
		batched := createTestItem(t, s, ws.ID, col.ID, "Archived in a batch", "")
		clearOutbox(t, s)
		if err := s.DeleteItem(batched.ID, WithEventBatch(batch)); err != nil {
			t.Fatalf("DeleteItem: %v", err)
		}
		assertBatch(t, batched.ID, batch)

		solo := createTestItem(t, s, ws.ID, col.ID, "Archived alone", "")
		clearOutbox(t, s)
		if err := s.DeleteItem(solo.ID); err != nil {
			t.Fatalf("DeleteItem (control): %v", err)
		}
		assertBatch(t, solo.ID, "")
	})

	t.Run("RestoreItem", func(t *testing.T) {
		item := createTestItem(t, s, ws.ID, col.ID, "Restored in a batch", "")
		if err := s.DeleteItem(item.ID); err != nil {
			t.Fatalf("DeleteItem: %v", err)
		}
		clearOutbox(t, s)
		if _, err := s.RestoreItem(item.ID, WithEventBatch(batch)); err != nil {
			t.Fatalf("RestoreItem: %v", err)
		}
		assertBatch(t, item.ID, batch)
	})

	t.Run("MoveItemWithPreCheck", func(t *testing.T) {
		item := createTestItem(t, s, ws.ID, col.ID, "Moved in a batch", "")
		clearOutbox(t, s)
		if _, err := s.MoveItemWithPreCheck(item.ID, other.ID, "{}", nil, WithEventBatch(batch)); err != nil {
			t.Fatalf("MoveItemWithPreCheck: %v", err)
		}
		assertBatch(t, item.ID, batch)
	})

	t.Run("UpdateItemWithPreCheck", func(t *testing.T) {
		item := createTestItem(t, s, ws.ID, col.ID, "Field-updated in a batch", "")
		clearOutbox(t, s)
		title := "Field-updated in a batch, renamed"
		if _, err := s.UpdateItemWithPreCheck(item.ID, models.ItemUpdate{Title: &title}, nil, WithEventBatch(batch)); err != nil {
			t.Fatalf("UpdateItemWithPreCheck: %v", err)
		}
		assertBatch(t, item.ID, batch)
	})

	t.Run("UpdateItem", func(t *testing.T) {
		item := createTestItem(t, s, ws.ID, col.ID, "Assigned in a batch", "")
		clearOutbox(t, s)
		title := "Assigned in a batch, renamed"
		if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &title}, WithEventBatch(batch)); err != nil {
			t.Fatalf("UpdateItem: %v", err)
		}
		assertBatch(t, item.ID, batch)

		solo := createTestItem(t, s, ws.ID, col.ID, "Assigned alone", "")
		clearOutbox(t, s)
		soloTitle := "Assigned alone, renamed"
		if _, err := s.UpdateItem(solo.ID, models.ItemUpdate{Title: &soloTitle}); err != nil {
			t.Fatalf("UpdateItem (control): %v", err)
		}
		assertBatch(t, solo.ID, "")
	})
}

func claimedIDs(evs []OutboxEvent) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.ID)
	}
	sort.Strings(out)
	return out
}

// TestOutbox_ClaimIsExclusiveUntilTheLeaseExpires is the multi-instance guard.
// Every instance of a cloud deployment runs the drain, so without the claim
// each pending row is delivered once PER INSTANCE — by construction, not by
// accident.
func TestOutbox_ClaimIsExclusiveUntilTheLeaseExpires(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox claim")
	clearOutbox(t, s)

	occurred := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	insertOutboxRow(t, s, "row-a", ws.ID, occurred, nil)
	insertOutboxRow(t, s, "row-b", ws.ID, occurred, nil)

	live := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)

	first, err := s.ClaimPendingOutboxEvents("instance-1", 10, live)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if got := claimedIDs(first); len(got) != 2 {
		t.Fatalf("instance-1 claimed %v, want both rows", got)
	}

	second, err := s.ClaimPendingOutboxEvents("instance-2", 10, live)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("instance-2 claimed %v — every event would be delivered once per instance", claimedIDs(second))
	}

	// A drainer that died holding claims must not strand them: past the lease,
	// the rows are claimable again. At-least-once is what makes that safe.
	expired := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	third, err := s.ClaimPendingOutboxEvents("instance-2", 10, expired)
	if err != nil {
		t.Fatalf("post-lease claim: %v", err)
	}
	if got := claimedIDs(third); len(got) != 2 {
		t.Fatalf("post-lease claim got %v, want both rows — a dead instance stranded them", got)
	}
}

// TestOutbox_ClaimTakesWholeBatchesPastTheLimit pins the rule that keeps a
// folded wire event true: the limit is a throughput knob, and letting it split
// a batch would make one bulk operation arrive as two events each reporting a
// partial member count.
func TestOutbox_ClaimTakesWholeBatchesPastTheLimit(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox batch claim")
	clearOutbox(t, s)

	occurred := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	insertOutboxRowBatch(t, s, "b-1", ws.ID, occurred, nil, "batch-x")
	insertOutboxRowBatch(t, s, "b-2", ws.ID, occurred, nil, "batch-x")
	insertOutboxRowBatch(t, s, "b-3", ws.ID, occurred, nil, "batch-x")

	live := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	claimed, err := s.ClaimPendingOutboxEvents("instance-1", 1, live)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	got := claimedIDs(claimed)
	if len(got) != 3 {
		t.Fatalf("claimed %v with limit=1, want all three members of batch-x", got)
	}
	for _, ev := range claimed {
		if ev.BatchID != "batch-x" {
			t.Errorf("claimed row %s carries batch %q, want batch-x", ev.ID, ev.BatchID)
		}
	}

	// Control: an unbatched row is still bounded by the limit, so the
	// expansion above is batch-specific rather than the limit being ignored.
	clearOutbox(t, s)
	insertOutboxRow(t, s, "s-1", ws.ID, occurred, nil)
	insertOutboxRow(t, s, "s-2", ws.ID, occurred, nil)
	single, err := s.ClaimPendingOutboxEvents("instance-1", 1, live)
	if err != nil {
		t.Fatalf("control claim: %v", err)
	}
	if len(single) != 1 {
		t.Fatalf("control claimed %v with limit=1, want exactly one row", claimedIDs(single))
	}
}

// TestOutbox_FailedAttemptReleasesTheClaim: a transient failure means the
// event is still owed and nothing is in flight, so the next tick must be able
// to take it. Holding the claim until the lease expired would idle the row for
// the whole window — and on a single-instance deployment that is the only
// reason it would ever wait at all.
func TestOutbox_FailedAttemptReleasesTheClaim(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox claim release")
	clearOutbox(t, s)

	occurred := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	insertOutboxRow(t, s, "row-a", ws.ID, occurred, nil)
	live := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)

	claimed, err := s.ClaimPendingOutboxEvents("instance-1", 10, live)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %v, %v; want one row", claimedIDs(claimed), err)
	}
	// Premise: it really is unavailable while claimed, so the re-claim below
	// proves the release rather than the claim never having applied.
	if again, err := s.ClaimPendingOutboxEvents("instance-2", 10, live); err != nil || len(again) != 0 {
		t.Fatalf("row was claimable while claimed (%v, %v)", claimedIDs(again), err)
	}

	if err := s.MarkOutboxAttemptFailed(claimed[0].ClaimToken, "row-a", "endpoint timed out"); err != nil {
		t.Fatalf("MarkOutboxAttemptFailed: %v", err)
	}

	retry, err := s.ClaimPendingOutboxEvents("instance-2", 10, live)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if len(retry) != 1 {
		t.Fatalf("re-claim got %v, want the released row", claimedIDs(retry))
	}
	if retry[0].Attempts != 1 {
		t.Errorf("attempts = %d, want 1 — the failed attempt was not recorded", retry[0].Attempts)
	}
	if retry[0].LastError != "endpoint timed out" {
		t.Errorf("last_error = %q, want the recorded reason", retry[0].LastError)
	}
}

// TestOutbox_ClaimArbitratesTheRaceNotJustTheQuery drives the conditional
// UPDATE with a STALE candidate list — the case the public entry point cannot
// produce, because its own candidate query has already filtered held rows.
//
// Without this, the exclusivity test above passes for an implementation whose
// UPDATE has no availability predicate at all: the candidate filter alone
// produces the same end state single-threaded (CONVE-12 — name the other
// mechanism that produces the end state, then assert against IT). Under real
// concurrency that implementation double-claims every row two instances select
// at the same moment, which is precisely the multi-instance bug the claim
// exists to prevent.
func TestOutbox_ClaimArbitratesTheRaceNotJustTheQuery(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox claim race")
	clearOutbox(t, s)

	occurred := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	insertOutboxRow(t, s, "row-a", ws.ID, occurred, nil)
	live := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)

	// Instance 2 selects candidates FIRST — before instance 1 claims. This is
	// the window: both instances have the row in their candidate list.
	stale, err := s.pendingClaimCandidates(10, live)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("candidates = %v, want the one pending row", stale)
	}

	won, err := s.ClaimPendingOutboxEvents("instance-1", 10, live)
	if err != nil || len(won) != 1 {
		t.Fatalf("instance-1 claim = %v, %v; want the row", claimedIDs(won), err)
	}

	// Instance 2 now runs its UPDATE against the list it selected before the
	// claim landed. The predicate on each statement is the only thing that can
	// stop it.
	const loser = "instance-2:stale-token"
	if err := s.claimOutboxIDs(loser, stale, live); err != nil {
		t.Fatalf("loser claim: %v", err)
	}
	got, err := s.outboxEventsClaimedBy(loser)
	if err != nil {
		t.Fatalf("read loser claims: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("instance-2 also claimed %v — both instances would deliver the same event", claimedIDs(got))
	}
}

// sameStrings compares two payload-family lists order-independently: the
// taxonomy declares a SET, and pinning its order would make the test fail on a
// reordering that changes nothing.
func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

// TestFoldBulkHeader_KeepsOneSnapshotPerItem pins the fold's answer to the
// disjoint-delta rule: one bulk member can write several rows (a move that
// also changes status emits item.moved AND item.status_changed), and the
// folded payload reports `count` in ITEMS.
//
// Without dedup the members list disagrees with the count in the same
// payload — the wire event contradicting itself (codex round 1).
func TestFoldBulkHeader_KeepsOneSnapshotPerItem(t *testing.T) {
	header := []byte(`{"batch_id":"b1","op":"move","count":2,"item_ids":["i1","i2"]}`)
	members := [][]byte{
		[]byte(`{"id":"i1","status":"open"}`),
		[]byte(`{"id":"i2","status":"open"}`),
		[]byte(`{"id":"i1","status":"done"}`),
		[]byte(`{"id":"i2","status":"done"}`),
	}

	folded, err := FoldBulkHeader(header, members)
	if err != nil {
		t.Fatalf("FoldBulkHeader: %v", err)
	}
	var got struct {
		Count   int `json:"count"`
		Members []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"members"`
	}
	if err := json.Unmarshal(folded, &got); err != nil {
		t.Fatalf("decode folded: %v", err)
	}
	if len(got.Members) != got.Count {
		t.Fatalf("members = %d but count = %d — the payload contradicts itself", len(got.Members), got.Count)
	}
	for _, m := range got.Members {
		if m.Status != "done" {
			t.Errorf("member %s carries status %q, want the LAST snapshot (done)", m.ID, m.Status)
		}
	}

	// prior_status survives the collapse. A mixed update writes
	// item.status_changed (which carries it) AND item.updated (which does
	// not); plain last-wins keeps the later row and drops the transition —
	// the one field a "nonterminal -> terminal" binding needs, lost exactly
	// in the case that produces both rows.
	withPrior, err := FoldBulkHeader(
		[]byte(`{"batch_id":"b1","op":"move","count":1,"item_ids":["i1"]}`),
		[][]byte{
			[]byte(`{"id":"i1","status":"done","prior_status":"open"}`),
			[]byte(`{"id":"i1","status":"done","title":"later row, no prior_status"}`),
		})
	if err != nil {
		t.Fatalf("FoldBulkHeader (prior_status): %v", err)
	}
	var priorGot struct {
		Members []struct {
			Title       string `json:"title"`
			PriorStatus string `json:"prior_status"`
		} `json:"members"`
	}
	if err := json.Unmarshal(withPrior, &priorGot); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(priorGot.Members) != 1 {
		t.Fatalf("members = %d, want 1", len(priorGot.Members))
	}
	if priorGot.Members[0].PriorStatus != "open" {
		t.Errorf("prior_status = %q, want it carried forward from the status_changed row", priorGot.Members[0].PriorStatus)
	}
	if priorGot.Members[0].Title != "later row, no prior_status" {
		t.Errorf("member = %+v, want the LAST snapshot's fields kept", priorGot.Members[0])
	}

	// A snapshot the drain cannot read is kept, not dropped: it is real data,
	// and discarding it silently would be worse than a duplicate.
	odd, err := FoldBulkHeader(header, [][]byte{[]byte(`"not an object"`), []byte(`{"id":"i1"}`)})
	if err != nil {
		t.Fatalf("FoldBulkHeader (unreadable member): %v", err)
	}
	var oddGot struct {
		Members []json.RawMessage `json:"members"`
	}
	if err := json.Unmarshal(odd, &oddGot); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(oddGot.Members) != 2 {
		t.Errorf("members = %d, want both kept", len(oddGot.Members))
	}
}

// TestOutbox_StaleClaimCannotAckOrReleaseAnotherPass pins the lease's other
// half. Delivery can outrun the lease — a workspace with many slow endpoints
// is delivered sequentially — and the estimate behind the default is an
// estimate, not a bound.
//
// So the question is what a LATE pass may still do to rows a newer pass now
// owns. The answer has to be nothing: a late ack would stamp a row the new
// holder is mid-delivery on, and a late release would clear a live claim and
// hand the event to a third pass.
func TestOutbox_StaleClaimCannotAckOrReleaseAnotherPass(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox stale claim")
	clearOutbox(t, s)

	occurred := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	insertOutboxRow(t, s, "row-a", ws.ID, occurred, nil)

	live := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	first, err := s.ClaimPendingOutboxEvents("instance-1", 10, live)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim = %v, %v", claimedIDs(first), err)
	}
	staleToken := first[0].ClaimToken
	if staleToken == "" {
		t.Fatal("claim returned no token; ack and release have nothing to condition on")
	}

	// The lease expires and a second instance legitimately takes the row.
	expired := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	second, err := s.ClaimPendingOutboxEvents("instance-2", 10, expired)
	if err != nil || len(second) != 1 {
		t.Fatalf("second claim = %v, %v — the premise of this test is that the row moved on", claimedIDs(second), err)
	}
	if second[0].ClaimToken == staleToken {
		t.Fatal("both passes share a claim token; the tokens do not identify a pass")
	}

	// Instance 1 finally finishes and acks. It must reach nothing.
	if err := s.MarkOutboxDispatched(staleToken, []string{"row-a"}); err != nil {
		t.Fatalf("stale ack: %v", err)
	}
	pending, err := s.ListPendingOutboxEvents(10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("stale ack marked the row dispatched while instance-2 holds it; pending = %d", len(pending))
	}

	// ...and its release must not free instance-2's claim either.
	if err := s.MarkOutboxAttemptFailed(staleToken, "row-a", "late failure"); err != nil {
		t.Fatalf("stale release: %v", err)
	}
	third, err := s.ClaimPendingOutboxEvents("instance-3", 10, live)
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	if len(third) != 0 {
		t.Errorf("a stale release freed a live claim; instance-3 took %v", claimedIDs(third))
	}

	// Positive control: the CURRENT holder's ack does work, so the assertions
	// above are about staleness rather than about acking being broken.
	if err := s.MarkOutboxDispatched(second[0].ClaimToken, []string{"row-a"}); err != nil {
		t.Fatalf("live ack: %v", err)
	}
	if pending, err := s.ListPendingOutboxEvents(10); err != nil || len(pending) != 0 {
		t.Errorf("live ack left %d pending (err %v)", len(pending), err)
	}
}

// TestOutbox_PruneUndispatchedSparesAClaimedRow: every instance runs
// retention, so without a claim check one instance's prune deletes a row
// another is mid-delivery on — the delivery then succeeds while the ack
// matches zero rows, and a crash in that window loses an event the outbox had
// already committed (codex round 4).
func TestOutbox_PruneUndispatchedSparesAClaimedRow(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Outbox prune vs claim")
	clearOutbox(t, s)

	old := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339)
	insertOutboxRow(t, s, "a-claimed", ws.ID, old, nil)
	insertOutboxRow(t, s, "b-unclaimed", ws.ID, old, nil)

	live := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	// Claim only the first row, by asking for exactly one.
	claimed, err := s.ClaimPendingOutboxEvents("instance-1", 1, live)
	if err != nil || len(claimed) != 1 || claimed[0].ID != "a-claimed" {
		t.Fatalf("claim = %v, %v; want exactly a-claimed", claimedIDs(claimed), err)
	}

	cutoff := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	n, err := s.PruneUndispatchedOutbox(cutoff, live)
	if err != nil {
		t.Fatalf("PruneUndispatchedOutbox: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want 1 — the claimed row must survive and the unclaimed one must not", n)
	}
	got := outboxRowIDs(t, s)
	if len(got) != 1 || got[0] != "a-claimed" {
		t.Fatalf("survivors = %v, want [a-claimed]", got)
	}

	// An EXPIRED claim is fair game — that is what expiry means — so the
	// exemption is about live deliveries, not about claimed rows forever.
	expired := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	if n, err := s.PruneUndispatchedOutbox(cutoff, expired); err != nil || n != 1 {
		t.Errorf("prune with an expired lease removed %d rows (err %v), want 1", n, err)
	}
}
