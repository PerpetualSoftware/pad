package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// Tests for the account-deletion outbox scrub (TASK-2719).
//
// The CONVE-18 shape here: one test per payload family that carries a user id,
// because a scrub that handles four of the five shapes looks exactly like one
// that handles all five from any single test's point of view. Every positive
// case is paired with a negative control — a payload naming a DIFFERENT user
// that must come back byte-identical — because without it, "scrub everything"
// passes every positive assertion.

// rawOutboxPayload returns the single stored payload for a subject+type,
// as raw bytes for byte-identity assertions.
func rawOutboxPayload(t *testing.T, s *Store, subjectID, eventType string) string {
	t.Helper()
	rows, err := s.db.Query(s.q(`SELECT payload FROM event_outbox WHERE subject_id = ? AND event_type = ?`), subjectID, eventType)
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
		t.Fatalf("outbox rows for subject %s type %s = %d, want exactly 1", subjectID, eventType, len(payloads))
	}
	return payloads[0]
}

// scrubInTx runs the scrub inside its own committed transaction, the way
// DeleteAccountAtomic does.
func scrubInTx(t *testing.T, s *Store, userID string) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if err := s.ScrubOutboxUserRefsTx(tx, userID); err != nil {
		t.Fatalf("ScrubOutboxUserRefsTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// emitInTx runs one production emitter inside a committed transaction.
func emitInTx(t *testing.T, s *Store, emit func(tx *sql.Tx) error) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if err := emit(tx); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// scrubTestUsers creates the deleted-account user and the bystander whose
// data is the negative control.
func scrubTestUsers(t *testing.T, s *Store) (deleted, bystander *models.User) {
	t.Helper()
	deleted = createTestUser(t, s, "scrub-me@test.com", "Scrub Me", "correct-horse-battery-staple")
	bystander = createTestUser(t, s, "bystander@test.com", "Bystander", "correct-horse-battery-staple")
	return deleted, bystander
}

// assignItemSnapshot reads the item back through getItemTx — the same query
// every production emitter uses — after assigning it to the given user, so
// the emitted payload is the production shape rather than a hand-built one.
func assignItemSnapshot(t *testing.T, s *Store, itemID, userID string) *models.Item {
	t.Helper()
	if _, err := s.db.Exec(s.q(`UPDATE items SET assigned_user_id = ? WHERE id = ?`), userID, itemID); err != nil {
		t.Fatalf("assign item: %v", err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	item, err := s.getItemTx(tx, itemID)
	if err != nil {
		t.Fatalf("getItemTx: %v", err)
	}
	if item == nil {
		t.Fatalf("item %s not found", itemID)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return item
}

func TestScrubOutboxUserRefs_ItemSnapshot(t *testing.T) {
	s := testStore(t)
	deleted, bystander := scrubTestUsers(t, s)
	ws := createTestWorkspace(t, s, "Scrub item")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	mine := createTestItem(t, s, ws.ID, col.ID, "Assigned to deleted", "body")
	theirs := createTestItem(t, s, ws.ID, col.ID, "Assigned to bystander", "body")
	clearOutbox(t, s)

	mineSnap := assignItemSnapshot(t, s, mine.ID, deleted.ID)
	theirsSnap := assignItemSnapshot(t, s, theirs.ID, bystander.ID)
	emitInTx(t, s, func(tx *sql.Tx) error {
		if err := s.emitItemEventTx(tx, kernelevents.ItemUpdated, mineSnap, nil, ""); err != nil {
			return err
		}
		return s.emitItemEventTx(tx, kernelevents.ItemUpdated, theirsSnap, nil, "")
	})
	theirsBefore := rawOutboxPayload(t, s, theirs.ID, kernelevents.ItemUpdated)

	scrubInTx(t, s, deleted.ID)

	got := outboxPayloadFor(t, s, mine.ID, kernelevents.ItemUpdated)
	if _, present := got["assigned_user_id"]; present {
		t.Fatalf("assigned_user_id survived the scrub: %v", got["assigned_user_id"])
	}
	// The rest of the snapshot must survive: same id, title, and fields blob.
	if got["id"] != mine.ID || got["title"] != "Assigned to deleted" {
		t.Fatalf("scrub damaged non-target fields: id=%v title=%v", got["id"], got["title"])
	}
	if got["fields"] != mineSnap.Fields {
		t.Fatalf("scrub rewrote the fields blob: %v != %v", got["fields"], mineSnap.Fields)
	}

	// NEGATIVE CONTROL: the bystander's payload is byte-identical. Without
	// this, a scrub that strips assigned_user_id from every row passes the
	// positive half.
	if after := rawOutboxPayload(t, s, theirs.ID, kernelevents.ItemUpdated); after != theirsBefore {
		t.Fatalf("bystander payload changed:\nbefore %s\nafter  %s", theirsBefore, after)
	}
}

func TestScrubOutboxUserRefs_BulkMembers(t *testing.T) {
	s := testStore(t)
	deleted, bystander := scrubTestUsers(t, s)
	ws := createTestWorkspace(t, s, "Scrub bulk")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	mine := createTestItem(t, s, ws.ID, col.ID, "Bulk mine", "")
	theirs := createTestItem(t, s, ws.ID, col.ID, "Bulk theirs", "")
	clearOutbox(t, s)

	mineSnap := assignItemSnapshot(t, s, mine.ID, deleted.ID)
	theirsSnap := assignItemSnapshot(t, s, theirs.ID, bystander.ID)
	emitInTx(t, s, func(tx *sql.Tx) error {
		return s.emitBulkItemEventTx(tx, ws.ID, []*models.Item{scrubItemPII(mineSnap), scrubItemPII(theirsSnap)}, map[string]any{"kind": "test"})
	})

	scrubInTx(t, s, deleted.ID)

	var payload string
	if err := s.db.QueryRow(s.q(`SELECT payload FROM event_outbox WHERE event_type = ?`), kernelevents.ItemBulkUpdated).Scan(&payload); err != nil {
		t.Fatalf("read bulk payload: %v", err)
	}
	var decoded struct {
		Members []map[string]any `json:"members"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("decode bulk payload: %v", err)
	}
	if len(decoded.Members) != 2 {
		t.Fatalf("members = %d, want 2", len(decoded.Members))
	}
	// The scrub reaches NESTED member snapshots, and only the deleted
	// user's — the bystander's assignment survives inside the same payload,
	// which is the in-payload negative control a whole-row rewrite would fail.
	for _, m := range decoded.Members {
		assigned, present := m["assigned_user_id"]
		switch m["id"] {
		case mine.ID:
			if present {
				t.Fatalf("deleted user's assigned_user_id survived in bulk member: %v", assigned)
			}
		case theirs.ID:
			if !present || assigned != bystander.ID {
				t.Fatalf("bystander's assigned_user_id damaged in bulk member: %v", assigned)
			}
		default:
			t.Fatalf("unexpected member id %v", m["id"])
		}
	}
}

func TestScrubOutboxUserRefs_CommentSnapshot(t *testing.T) {
	s := testStore(t)
	deleted, bystander := scrubTestUsers(t, s)
	ws := createTestWorkspace(t, s, "Scrub comment")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Commented", "")
	clearOutbox(t, s)

	mine := &models.Comment{ID: newID(), ItemID: item.ID, WorkspaceID: ws.ID, Author: "Scrub Me", UserID: deleted.ID, Body: "frozen body"}
	theirs := &models.Comment{ID: newID(), ItemID: item.ID, WorkspaceID: ws.ID, Author: "Bystander", UserID: bystander.ID, Body: "other body"}
	emitInTx(t, s, func(tx *sql.Tx) error {
		if err := s.emitCommentEventTx(tx, kernelevents.CommentCreated, mine); err != nil {
			return err
		}
		return s.emitCommentEventTx(tx, kernelevents.CommentCreated, theirs)
	})
	theirsBefore := rawOutboxPayload(t, s, theirs.ID, kernelevents.CommentCreated)

	scrubInTx(t, s, deleted.ID)

	got := outboxPayloadFor(t, s, mine.ID, kernelevents.CommentCreated)
	if _, present := got["user_id"]; present {
		t.Fatalf("comment user_id survived the scrub: %v", got["user_id"])
	}
	// The author DISPLAY STRING stays: live-row de-identification nulls
	// comments.user_id only, and the scrub matches that posture exactly. The
	// wider live-rows question is with Dave (TASK-2719 trail), not decided
	// silently here.
	if got["author"] != "Scrub Me" || got["body"] != "frozen body" {
		t.Fatalf("scrub damaged non-target comment fields: author=%v body=%v", got["author"], got["body"])
	}
	if after := rawOutboxPayload(t, s, theirs.ID, kernelevents.CommentCreated); after != theirsBefore {
		t.Fatalf("bystander comment payload changed:\nbefore %s\nafter  %s", theirsBefore, after)
	}
}

func TestScrubOutboxUserRefs_AttachmentSnapshot(t *testing.T) {
	s := testStore(t)
	deleted, bystander := scrubTestUsers(t, s)
	ws := createTestWorkspace(t, s, "Scrub attachment")
	clearOutbox(t, s)

	mine := &models.Attachment{ID: newID(), WorkspaceID: ws.ID, UploadedBy: deleted.ID, StorageKey: "fs:aaa", ContentHash: "aaa", MimeType: "image/png", Filename: "a.png"}
	theirs := &models.Attachment{ID: newID(), WorkspaceID: ws.ID, UploadedBy: bystander.ID, StorageKey: "fs:bbb", ContentHash: "bbb", MimeType: "image/png", Filename: "b.png"}
	emitInTx(t, s, func(tx *sql.Tx) error {
		if err := s.emitAttachmentEventTx(tx, kernelevents.AttachmentAdded, mine); err != nil {
			return err
		}
		return s.emitAttachmentEventTx(tx, kernelevents.AttachmentAdded, theirs)
	})
	theirsBefore := rawOutboxPayload(t, s, theirs.ID, kernelevents.AttachmentAdded)

	scrubInTx(t, s, deleted.ID)

	got := outboxPayloadFor(t, s, mine.ID, kernelevents.AttachmentAdded)
	if _, present := got["uploaded_by"]; present {
		t.Fatalf("uploaded_by survived the scrub: %v", got["uploaded_by"])
	}
	if got["filename"] != "a.png" || got["storage_key"] != "fs:aaa" {
		t.Fatalf("scrub damaged non-target attachment fields: %v", got)
	}
	if after := rawOutboxPayload(t, s, theirs.ID, kernelevents.AttachmentAdded); after != theirsBefore {
		t.Fatalf("bystander attachment payload changed:\nbefore %s\nafter  %s", theirsBefore, after)
	}
}

func TestScrubOutboxUserRefs_MemberRowAndSubjectColumn(t *testing.T) {
	s := testStore(t)
	deleted, bystander := scrubTestUsers(t, s)
	ws := createTestWorkspace(t, s, "Scrub member")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Subject control", "")
	clearOutbox(t, s)

	emitInTx(t, s, func(tx *sql.Tx) error {
		if err := s.emitMemberEventTx(tx, kernelevents.MemberJoined, ws.ID, deleted.ID, "editor", now()); err != nil {
			return err
		}
		return s.emitMemberEventTx(tx, kernelevents.MemberJoined, ws.ID, bystander.ID, "viewer", now())
	})
	// An item-subject row whose subject must NOT be nulled — the column half
	// of the scrub is value-matched, not kind-wide.
	snap := assignItemSnapshot(t, s, item.ID, deleted.ID)
	emitInTx(t, s, func(tx *sql.Tx) error {
		return s.emitItemEventTx(tx, kernelevents.ItemUpdated, snap, nil, "")
	})

	scrubInTx(t, s, deleted.ID)

	// The deleted user's member row: subject_id NULL, payload user_id gone,
	// the rest intact — a resync signal, not a broken row (lead ruling).
	var subjectNull int
	if err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM event_outbox WHERE event_type = ? AND subject_id IS NULL
	`), kernelevents.MemberJoined).Scan(&subjectNull); err != nil {
		t.Fatalf("count nulled member subjects: %v", err)
	}
	if subjectNull != 1 {
		t.Fatalf("member rows with NULL subject = %d, want exactly 1 (deleted user's only)", subjectNull)
	}

	var payload string
	if err := s.db.QueryRow(s.q(`
		SELECT payload FROM event_outbox WHERE event_type = ? AND subject_id IS NULL
	`), kernelevents.MemberJoined).Scan(&payload); err != nil {
		t.Fatalf("read scrubbed member payload: %v", err)
	}
	// The parse condition from the lead's ruling: the scrubbed row must
	// unmarshal under the member payload shape. It does — user_id is simply
	// absent — which is what closed the tombstone branch.
	var member memberEventPayload
	if err := json.Unmarshal([]byte(payload), &member); err != nil {
		t.Fatalf("scrubbed member payload does not parse: %v", err)
	}
	if member.UserID != "" {
		t.Fatalf("member user_id survived the scrub: %q", member.UserID)
	}
	if member.WorkspaceID != ws.ID || member.Role != "editor" {
		t.Fatalf("scrub damaged non-target member fields: %+v", member)
	}
	if strings.Contains(payload, deleted.ID) {
		t.Fatalf("deleted user id still legible in member payload: %s", payload)
	}

	// The bystander's member row keeps its subject and payload.
	byPayload := rawOutboxPayload(t, s, bystander.ID, kernelevents.MemberJoined)
	if !strings.Contains(byPayload, bystander.ID) {
		t.Fatalf("bystander member payload damaged: %s", byPayload)
	}

	// The item-subject row keeps its subject id: the column scrub is value
	// equality on the USER id, and an item's subject is the item.
	if got := rawOutboxPayload(t, s, item.ID, kernelevents.ItemUpdated); strings.Contains(got, deleted.ID) {
		t.Fatalf("deleted user id still legible in item payload: %s", got)
	}
}

func TestScrubOutboxUserRefs_DispatchedRowsScrubbedNotDeleted(t *testing.T) {
	s := testStore(t)
	deleted, _ := scrubTestUsers(t, s)
	ws := createTestWorkspace(t, s, "Scrub dispatched")
	clearOutbox(t, s)

	emitInTx(t, s, func(tx *sql.Tx) error {
		return s.emitMemberEventTx(tx, kernelevents.MemberJoined, ws.ID, deleted.ID, "editor", now())
	})
	// Mark it dispatched the way the drain would leave it.
	if _, err := s.db.Exec(s.q(`UPDATE event_outbox SET dispatched_at = ?`), now()); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}

	scrubInTx(t, s, deleted.ID)

	// UNIFORM, DELETE NOTHING (lead ruling): the dispatched-retained row
	// survives — lifecycle is retention's job — but its id is gone.
	var count int
	var payload string
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM event_outbox`)).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("outbox rows = %d, want 1 (scrub must not delete)", count)
	}
	if err := s.db.QueryRow(s.q(`SELECT payload FROM event_outbox`)).Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if strings.Contains(payload, deleted.ID) {
		t.Fatalf("deleted user id survived in dispatched row: %s", payload)
	}
}

// TestScrubOutboxUserRefs_KeyScopedAndNumberSafe pins two properties a looser
// implementation passes every other test without (codex round 1): the walk is
// KEY-scoped, not value-scoped — a payload field that merely CONTAINS the
// deleted user's id under an unrelated key survives — and numeric literals
// cross the rewrite verbatim, so an int64 past float64's 2^53 integer range
// cannot be corrupted by the re-marshal.
func TestScrubOutboxUserRefs_KeyScopedAndNumberSafe(t *testing.T) {
	s := testStore(t)
	deleted, _ := scrubTestUsers(t, s)
	ws := createTestWorkspace(t, s, "Scrub scoped")
	clearOutbox(t, s)

	// A hand-rolled item snapshot: the target key, a decoy string field whose
	// VALUE is the deleted id under a non-target key, and a seq that float64
	// cannot represent (2^53 + 1).
	payload := `{"id":"` + newID() + `","workspace_id":"` + ws.ID + `","title":"decoy","content":"` + deleted.ID + `","assigned_user_id":"` + deleted.ID + `","seq":9007199254740993}`
	emitInTx(t, s, func(tx *sql.Tx) error {
		return writeOutboxTx(tx, s, OutboxEvent{
			WorkspaceID:   ws.ID,
			EventType:     kernelevents.ItemUpdated,
			SubjectID:     newID(),
			Payload:       []byte(payload),
			PayloadFamily: kernelevents.PayloadItemSnapshot,
		})
	})

	scrubInTx(t, s, deleted.ID)

	var after string
	if err := s.db.QueryRow(s.q(`SELECT payload FROM event_outbox`)).Scan(&after); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(after), &decoded); err != nil {
		t.Fatalf("decode scrubbed payload: %v", err)
	}
	if _, present := decoded["assigned_user_id"]; present {
		t.Fatalf("assigned_user_id survived: %v", decoded["assigned_user_id"])
	}
	if decoded["content"] != deleted.ID {
		t.Fatalf("content damaged — the scrub is not key-scoped: %v", decoded["content"])
	}
	if !strings.Contains(after, `9007199254740993`) {
		t.Fatalf("seq literal corrupted by the rewrite: %s", after)
	}
}

// TestScrubOutboxRow_StaleReadRetriesAgainstFreshPayload drives the
// compare-and-swap's retry leg directly — the concurrent-deletion race it
// exists for needs two overlapping Postgres transactions to reproduce, but
// the leg itself is reachable deterministically by handing the helper a STALE
// copy of the payload: the conditional UPDATE must match zero rows, and the
// re-read must scrub what is actually stored rather than reintroduce the
// stale bytes.
func TestScrubOutboxRow_StaleReadRetriesAgainstFreshPayload(t *testing.T) {
	s := testStore(t)
	deleted, bystander := scrubTestUsers(t, s)
	ws := createTestWorkspace(t, s, "Scrub CAS")
	clearOutbox(t, s)

	// Stored reality: the payload another deletion already scrubbed — the
	// bystander's id is gone, the deleted user's remains.
	stored := `{"id":"` + newID() + `","workspace_id":"` + ws.ID + `","title":"cas","assigned_user_id":"` + deleted.ID + `"}`
	// Our stale read: the pre-scrub original still naming BOTH users.
	stale := `{"id":"` + newID() + `","workspace_id":"` + ws.ID + `","title":"cas","assigned_user_id":"` + deleted.ID + `","user_id":"` + bystander.ID + `"}`

	var rowID string
	emitInTx(t, s, func(tx *sql.Tx) error {
		ev := OutboxEvent{
			WorkspaceID:   ws.ID,
			EventType:     kernelevents.ItemUpdated,
			SubjectID:     newID(),
			Payload:       []byte(stored),
			PayloadFamily: kernelevents.PayloadItemSnapshot,
		}
		if err := writeOutboxTx(tx, s, ev); err != nil {
			return err
		}
		return tx.QueryRow(s.q(`SELECT id FROM event_outbox`)).Scan(&rowID)
	})

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if err := s.scrubOutboxRowTx(tx, rowID, stale, deleted.ID); err != nil {
		t.Fatalf("scrubOutboxRowTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var after string
	if err := s.db.QueryRow(s.q(`SELECT payload FROM event_outbox WHERE id = ?`), rowID).Scan(&after); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if strings.Contains(after, deleted.ID) {
		t.Fatalf("deleted user id survived the retry: %s", after)
	}
	// The stale copy's extra key must NOT be reintroduced: the retry scrubbed
	// the STORED bytes, not our stale ones — this is the lost-update the CAS
	// prevents, asserted from its observable end.
	if strings.Contains(after, bystander.ID) {
		t.Fatalf("stale payload reintroduced the other deletion's id: %s", after)
	}
}

func TestScrubOutboxUserRefs_RefusesEmptyUserID(t *testing.T) {
	s := testStore(t)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if err := s.ScrubOutboxUserRefsTx(tx, ""); err == nil {
		t.Fatalf("ScrubOutboxUserRefsTx accepted an empty user id; it would LIKE-match every row")
	}
}

// TestDeleteAccountAtomic_ScrubsOutbox is the WIRING test (CONVE-19): the
// scrub is exercised through the public deletion entry point, not by calling
// the helper directly — a helper that exists but is never called from
// DeleteAccountAtomic passes every test above and still ships the bug this
// task is about.
func TestDeleteAccountAtomic_ScrubsOutbox(t *testing.T) {
	s := testStore(t)
	deleted, bystander := scrubTestUsers(t, s)
	ws := createTestWorkspace(t, s, "Deletion scrub wiring")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Wired", "")
	clearOutbox(t, s)

	snap := assignItemSnapshot(t, s, item.ID, deleted.ID)
	emitInTx(t, s, func(tx *sql.Tx) error {
		if err := s.emitItemEventTx(tx, kernelevents.ItemUpdated, snap, nil, ""); err != nil {
			return err
		}
		if err := s.emitMemberEventTx(tx, kernelevents.MemberJoined, ws.ID, deleted.ID, "editor", now()); err != nil {
			return err
		}
		return s.emitMemberEventTx(tx, kernelevents.MemberJoined, ws.ID, bystander.ID, "viewer", now())
	})

	if err := s.DeleteAccountAtomic(deleted.ID, nil); err != nil {
		t.Fatalf("DeleteAccountAtomic: %v", err)
	}

	// No payload and no subject_id anywhere in the outbox still names the
	// deleted user — the deletion contract's erasure claim, asserted as the
	// absence of the id in the BYTES rather than via any helper.
	rows, err := s.db.Query(s.q(`SELECT COALESCE(subject_id, ''), payload FROM event_outbox`))
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()
	total := 0
	for rows.Next() {
		var subject, payload string
		if err := rows.Scan(&subject, &payload); err != nil {
			t.Fatalf("scan: %v", err)
		}
		total++
		if subject == deleted.ID {
			t.Fatalf("outbox subject_id still names the deleted user")
		}
		if strings.Contains(payload, deleted.ID) {
			t.Fatalf("outbox payload still names the deleted user: %s", payload)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if total != 3 {
		t.Fatalf("outbox rows = %d, want 3 (deletion must scrub, never delete rows)", total)
	}
	// Bystander's member row untouched.
	if byPayload := rawOutboxPayload(t, s, bystander.ID, kernelevents.MemberJoined); !strings.Contains(byPayload, bystander.ID) {
		t.Fatalf("bystander member payload damaged: %s", byPayload)
	}
}
