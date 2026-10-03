package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3389 (SPEC-6 U3): the app-projection block frozen into outbox payloads.

const task3389Schema = `{"fields":[
 {"key":"status","label":"Status","type":"select","options":["open","done"]},
 {"key":"size","label":"Size","type":"text"},
 {"key":"owner","label":"Owner","type":"relation","collection":"people"}
]}`

type pendingEvent struct {
	eventType, subjectID string
	doc                  map[string]any
}

func task3389Pending(t *testing.T, s *Store) []pendingEvent {
	t.Helper()
	evs, err := s.ListPendingOutboxEvents(1000)
	if err != nil {
		t.Fatalf("ListPendingOutboxEvents: %v", err)
	}
	var out []pendingEvent
	for _, ev := range evs {
		var doc map[string]any
		if err := json.Unmarshal(ev.Payload, &doc); err != nil {
			t.Fatalf("decode %s payload: %v", ev.EventType, err)
		}
		out = append(out, pendingEvent{ev.EventType, ev.SubjectID, doc})
	}
	return out
}

func task3389Block(t *testing.T, ev pendingEvent) map[string]any {
	t.Helper()
	b, ok := ev.doc["app_projection"].(map[string]any)
	if !ok {
		t.Fatalf("%s (%s) has no app_projection block: %v", ev.eventType, ev.subjectID, ev.doc)
	}
	return b
}

func task3389Fixture(t *testing.T) (*Store, *models.Workspace, *models.Collection, *models.User) {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Projection")
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Requests", Schema: task3389Schema})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	u := createTestUser(t, s, "ada-3389@example.com", "Ada Projection", "password123")
	return s, ws, coll, u
}

// Census: all eight retained event types carry a complete block.
func TestTask3389_AppProjectionCensus(t *testing.T) {
	s, ws, coll, u := task3389Fixture(t)
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{
		ActorUserID: u.ID, Title: "Login broken",
		Fields: `{"status":"open","size":"L","owner":"11111111-1111-1111-1111-111111111111","ghost":"undeclared"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{FieldsPatch: map[string]interface{}{"size": "M"}}); err != nil {
		t.Fatalf("UpdateItem size: %v", err)
	}
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{FieldsPatch: map[string]interface{}{"status": "done"}}); err != nil {
		t.Fatalf("UpdateItem status: %v", err)
	}
	c, err := s.CreateComment(ws.ID, item.ID, u.ID, models.CommentCreate{Body: "first", Author: "Ada Projection", CreatedBy: "user"})
	if err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	if _, err := s.UpdateComment(c.ID, "edited"); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
	if err := s.DeleteComment(c.ID); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}
	if err := s.DeleteItem(item.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	if _, err := s.RestoreItem(item.ID); err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}

	want := map[string]bool{
		kernelevents.ItemCreated: false, kernelevents.ItemUpdated: false, kernelevents.ItemStatusChanged: false,
		kernelevents.ItemDeleted: false, kernelevents.ItemRestored: false,
		kernelevents.CommentCreated: false, kernelevents.CommentUpdated: false, kernelevents.CommentDeleted: false,
	}
	for _, ev := range task3389Pending(t, s) {
		if _, retained := want[ev.eventType]; !retained {
			continue
		}
		want[ev.eventType] = true
		b := task3389Block(t, ev)
		if b["v"] != float64(1) || b["collection_id"] != coll.ID {
			t.Errorf("%s: v/collection_id = %v/%v", ev.eventType, b["v"], b["collection_id"])
		}
		creator, _ := b["creator"].(map[string]any)
		if creator["user_id"] != u.ID || creator["display"] != "Ada Projection" || creator["kind"] == "" || creator["kind"] == nil {
			t.Errorf("%s: creator = %v, want user_id %s, display Ada Projection, a kind", ev.eventType, creator, u.ID)
		}
		if strings.HasPrefix(ev.eventType, "item.") {
			fields, ok := b["fields"].(map[string]any)
			if !ok {
				t.Errorf("%s: no fields map", ev.eventType)
				continue
			}
			if _, ok := fields["status"]; !ok {
				t.Errorf("%s: declared field status missing: %v", ev.eventType, fields)
			}
			if _, ok := fields["owner"]; ok {
				t.Errorf("%s: relation field owner leaked: %v", ev.eventType, fields)
			}
			if _, ok := fields["ghost"]; ok {
				t.Errorf("%s: undeclared key ghost leaked: %v", ev.eventType, fields)
			}
		} else if b["item_id"] != item.ID {
			t.Errorf("%s: item_id = %v, want %s", ev.eventType, b["item_id"], item.ID)
		}
	}
	for typ, seen := range want {
		if !seen {
			t.Errorf("no %s event was emitted, so the census did not cover it", typ)
		}
	}
}

// The block is frozen at event time: a schema change afterwards does not alter
// a stored block, and a NEW event follows the new schema, so the projection is
// schema-driven rather than a constant.
func TestTask3389_SchemaChangeAfterEmit(t *testing.T) {
	s, ws, coll, u := task3389Fixture(t)
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{ActorUserID: u.ID, Title: "Freeze", Fields: `{"status":"open","size":"L","ghost":"x"}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	// Retype size to a relation and declare ghost.
	newSchema := strings.Replace(task3389Schema, `{"key":"size","label":"Size","type":"text"}`, `{"key":"size","label":"Size","type":"relation","collection":"people"},{"key":"ghost","label":"Ghost","type":"text"}`, 1)
	if _, err := s.UpdateCollection(coll.ID, models.CollectionUpdate{Schema: &newSchema}); err != nil {
		t.Fatalf("UpdateCollection: %v", err)
	}
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{FieldsPatch: map[string]interface{}{"status": "done"}}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	var created, later map[string]any
	for _, ev := range task3389Pending(t, s) {
		if ev.subjectID != item.ID {
			continue
		}
		f, _ := task3389Block(t, ev)["fields"].(map[string]any)
		switch ev.eventType {
		case kernelevents.ItemCreated:
			created = f
		case kernelevents.ItemStatusChanged:
			later = f
		}
	}
	if created == nil || later == nil {
		t.Fatalf("missing events: created=%v later=%v", created, later)
	}
	if created["size"] != "L" {
		t.Errorf("stored item.created block changed after the schema change: %v", created)
	}
	if _, ok := created["ghost"]; ok {
		t.Errorf("stored item.created block gained ghost, declared only later: %v", created)
	}
	if _, ok := later["size"]; ok {
		t.Errorf("new event after the schema change still projects size, now a relation: %v", later)
	}
	if later["ghost"] != "x" {
		t.Errorf("new event after the schema change does not project newly declared ghost: %v", later)
	}
}

// comment.deleted's parent check runs BEFORE the reap removes the parent.
func TestTask3389_CommentDeletedFreezesParentBeforeReap(t *testing.T) {
	s, ws, coll, u := task3389Fixture(t)
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{ActorUserID: u.ID, Title: "Thread"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	parent, err := s.CreateComment(ws.ID, item.ID, u.ID, models.CommentCreate{Body: "parent", Author: "Ada Projection"})
	if err != nil {
		t.Fatalf("CreateComment parent: %v", err)
	}
	reply, err := s.CreateComment(ws.ID, item.ID, u.ID, models.CommentCreate{Body: "reply", Author: "Ada Projection", ParentID: parent.ID})
	if err != nil {
		t.Fatalf("CreateComment reply: %v", err)
	}
	if err := s.DeleteComment(parent.ID); err != nil { // tombstone: it has a reply
		t.Fatalf("DeleteComment parent: %v", err)
	}
	if err := s.DeleteComment(reply.ID); err != nil { // last reply: the reap removes the tombstoned parent
		t.Fatalf("DeleteComment reply: %v", err)
	}
	var gone int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM comments WHERE id = ?`), parent.ID).Scan(&gone); err != nil || gone != 0 {
		t.Fatalf("precondition: the reap did not remove the parent (rows=%d err=%v)", gone, err)
	}
	found := false
	for _, ev := range task3389Pending(t, s) {
		if ev.eventType == kernelevents.CommentDeleted && ev.subjectID == reply.ID {
			found = true
			if p := task3389Block(t, ev)["parent_comment_id"]; p != parent.ID {
				t.Errorf("comment.deleted block parent_comment_id = %v, want %s (checked before the reap)", p, parent.ID)
			}
		}
	}
	if !found {
		t.Fatal("no comment.deleted event for the reply")
	}

	// Control: a legacy cross-item parent projects to absent.
	other, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{ActorUserID: u.ID, Title: "Other"})
	if err != nil {
		t.Fatalf("CreateItem other: %v", err)
	}
	foreign, err := s.CreateComment(ws.ID, other.ID, u.ID, models.CommentCreate{Body: "elsewhere"})
	if err != nil {
		t.Fatalf("CreateComment foreign: %v", err)
	}
	legacy, err := s.CreateComment(ws.ID, item.ID, u.ID, models.CommentCreate{Body: "legacy"})
	if err != nil {
		t.Fatalf("CreateComment legacy: %v", err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE comments SET parent_id = ? WHERE id = ?`), foreign.ID, legacy.ID); err != nil {
		t.Fatalf("plant cross-item parent: %v", err)
	}
	if _, err := s.UpdateComment(legacy.ID, "legacy edited"); err != nil {
		t.Fatalf("UpdateComment legacy: %v", err)
	}
	for _, ev := range task3389Pending(t, s) {
		if ev.eventType == kernelevents.CommentUpdated && ev.subjectID == legacy.ID {
			if p, ok := task3389Block(t, ev)["parent_comment_id"]; ok {
				t.Errorf("cross-item parent projected as %v, want absent", p)
			}
		}
	}
}

// Account deletion erases the deleted user's identity from the block: the id,
// and the display beside it, which no generic key match could tie to them.
func TestTask3389_AccountDeletionScrubsCreator(t *testing.T) {
	s, ws, coll, _ := task3389Fixture(t)
	gone := createTestUser(t, s, "gone-3389@example.com", "Zxq Departing Person", "password123")
	if err := s.AddWorkspaceMember(ws.ID, gone.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{ActorUserID: gone.ID, Title: "By the departing user"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := s.CreateComment(ws.ID, item.ID, gone.ID, models.CommentCreate{Body: "hello", Author: "Zxq Departing Person"}); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	// Precondition: the blocks name them before the deletion.
	named := 0
	for _, ev := range task3389Pending(t, s) {
		if b, ok := ev.doc["app_projection"].(map[string]any); ok {
			if c, _ := b["creator"].(map[string]any); c["display"] == "Zxq Departing Person" {
				named++
			}
		}
	}
	if named < 2 {
		t.Fatalf("precondition: %d blocks name the user before deletion, want at least 2", named)
	}
	if err := s.DeleteAccountAtomic(gone.ID); err != nil {
		t.Fatalf("DeleteAccountAtomic: %v", err)
	}
	for _, ev := range task3389Pending(t, s) {
		b, ok := ev.doc["app_projection"].(map[string]any)
		if !ok {
			continue
		}
		raw, _ := json.Marshal(b)
		if strings.Contains(string(raw), gone.ID) || strings.Contains(string(raw), "Zxq Departing Person") {
			t.Errorf("%s: app_projection still identifies the deleted user: %s", ev.eventType, raw)
		}
	}
}

// Codex round 1 [P2]: the block needs only the collection's schema. Reading the
// whole row broke item mutations on a collection with a legal NULL column.
func TestTask3389_NullCollectionColumnsDoNotBlockMutations(t *testing.T) {
	s, ws, coll, u := task3389Fixture(t)
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{ActorUserID: u.ID, Title: "Null description"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE collections SET description = NULL WHERE id = ?`), coll.ID); err != nil {
		t.Fatalf("null the description: %v", err)
	}
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{FieldsPatch: map[string]interface{}{"size": "S"}}); err != nil {
		t.Errorf("UpdateItem on a collection with a NULL description: %v", err)
	}
	if err := s.DeleteItem(item.ID); err != nil {
		t.Errorf("DeleteItem on a collection with a NULL description: %v", err)
	}
}

// Codex round 1 [P2]: the account scrub erases the deleted user's identity in
// the block's CREATOR, and must not touch projected FIELD values, which are the
// item's own data (a declared text field may well be called user_id).
func TestTask3389_ScrubLeavesProjectedFieldsAlone(t *testing.T) {
	s, ws, _, _ := task3389Fixture(t)
	schema := `{"fields":[{"key":"user_id","label":"User id","type":"text"}]}`
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Accounts", Schema: schema})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	gone := createTestUser(t, s, "gone2-3389@example.com", "Zxq Field Person", "password123")
	if err := s.AddWorkspaceMember(ws.ID, gone.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if _, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{ActorUserID: gone.ID, Title: "Has a user_id field", Fields: `{"user_id":"` + gone.ID + `"}`}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if err := s.DeleteAccountAtomic(gone.ID); err != nil {
		t.Fatalf("DeleteAccountAtomic: %v", err)
	}
	found := false
	for _, ev := range task3389Pending(t, s) {
		if ev.eventType != kernelevents.ItemCreated {
			continue
		}
		b, ok := ev.doc["app_projection"].(map[string]any)
		if !ok {
			continue
		}
		f, _ := b["fields"].(map[string]any)
		if _, has := f["user_id"]; !has {
			continue
		}
		found = true
		if f["user_id"] != gone.ID {
			t.Errorf("projected field user_id changed by the scrub: %v", f["user_id"])
		}
		if c, _ := b["creator"].(map[string]any); c["user_id"] != nil || c["display"] != nil {
			t.Errorf("creator still identifies the deleted user: %v", c)
		}
	}
	if !found {
		t.Fatal("the projected field user_id was removed by the scrub (no item.created block still carries it)")
	}
}

// A stored fields blob that is not a JSON object (the legacy corruption BUG-3163
// repairs) projects to no fields and never fails the write: the repair of such
// a blob goes through the same emit path. Found by the full suite, not review.
func TestTask3389_NonObjectFieldsBlobDoesNotBlockWrites(t *testing.T) {
	s, ws, coll, u := task3389Fixture(t)
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{ActorUserID: u.ID, Title: "Corrupt blob"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE items SET fields = ? WHERE id = ?`), `["not","an","object"]`, item.ID); err != nil {
		t.Fatalf("plant non-object blob: %v", err)
	}
	title := "Corrupt blob, renamed"
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatalf("UpdateItem on a non-object fields blob: %v", err)
	}
	for _, ev := range task3389Pending(t, s) {
		if ev.eventType == kernelevents.ItemUpdated && ev.subjectID == item.ID {
			b := task3389Block(t, ev)
			if _, has := b["fields"]; has {
				t.Errorf("non-object blob projected fields %v, want them omitted", b["fields"])
			}
			if b["partial"] != true {
				t.Errorf("non-object blob: partial = %v, want true", b["partial"])
			}
			return
		}
	}
	t.Fatal("no item.updated event for the renamed item")
}

// Lead ruling on #1763: data-shape problems must NEVER fail a human write. A
// malformed schema or an unknown field type degrades the block (fields
// omitted, partial: true) and the edit goes through. Only DB errors propagate.
func TestTask3389_DataShapeProblemsDegradeNotFail(t *testing.T) {
	for _, tc := range []struct{ name, schema string }{
		{"unparseable schema", `{"fields":"not-a-list"}`},
		{"unknown field type", `{"fields":[{"key":"status","label":"Status","type":"select","options":["open"]},{"key":"odd","label":"Odd","type":"mystery"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ws, coll, u := task3389Fixture(t)
			item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{ActorUserID: u.ID, Title: "Shape", Fields: `{"status":"open","odd":"x"}`})
			if err != nil {
				t.Fatalf("CreateItem: %v", err)
			}
			if _, err := s.db.Exec(s.q(`UPDATE collections SET schema = ? WHERE id = ?`), tc.schema, coll.ID); err != nil {
				t.Fatalf("plant schema: %v", err)
			}
			title := "Shape, edited by a human"
			if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &title}); err != nil {
				t.Fatalf("a human edit failed on a %s: %v", tc.name, err)
			}
			for _, ev := range task3389Pending(t, s) {
				if ev.eventType == kernelevents.ItemUpdated && ev.subjectID == item.ID {
					b := task3389Block(t, ev)
					if b["partial"] != true {
						t.Errorf("partial = %v, want true", b["partial"])
					}
					if _, has := b["fields"]; has {
						t.Errorf("fields %v, want omitted on a partial block", b["fields"])
					}
					if b["collection_id"] != coll.ID {
						t.Errorf("collection_id = %v, want it kept on a partial block", b["collection_id"])
					}
					return
				}
			}
			t.Fatal("no item.updated event")
		})
	}
}

// A clean block with nothing to project keeps "fields": {}, so only a partial
// block lacks the key.
func TestTask3389_CleanBlockWithNoFieldsKeepsFieldsKey(t *testing.T) {
	s, ws, coll, u := task3389Fixture(t)
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{ActorUserID: u.ID, Title: "Bare", Fields: `{}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	for _, ev := range task3389Pending(t, s) {
		if ev.eventType == kernelevents.ItemCreated && ev.subjectID == item.ID {
			b := task3389Block(t, ev)
			if _, has := b["fields"]; !has {
				t.Error("clean block omitted fields; only a partial block may")
			}
			if _, has := b["partial"]; has {
				t.Errorf("clean block carries partial = %v", b["partial"])
			}
			return
		}
	}
	t.Fatal("no item.created event")
}

// A partial block is logged once per collection, not once per write.
func TestTask3389_PartialLoggedOncePerCollection(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s, ws, coll, u := task3389Fixture(t)
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{ActorUserID: u.ID, Title: "Log", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE collections SET schema = ? WHERE id = ?`), `{"fields":"nope"}`, coll.ID); err != nil {
		t.Fatalf("plant schema: %v", err)
	}
	for i := 0; i < 3; i++ {
		title := fmt.Sprintf("Log %d", i)
		if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &title}); err != nil {
			t.Fatalf("UpdateItem %d: %v", i, err)
		}
	}
	n := 0
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "app projection: partial block") && strings.Contains(line, coll.ID) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("partial logged %d times for one collection, want 1:\n%s", n, buf.String())
	}
}
