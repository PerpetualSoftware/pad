package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3411 (SPEC-6 U10d): the app-projection block names the install whose
// fenced write caused the event (actor_via_app), and the app DTO's envelope
// carries it on every event. The human half of the census row is in
// TestTask3389_*; this is the fenced half, plus the portal echo: an app's
// own PATCH reaches it as item.updated with via_app = itself, and a
// person's edit to the same item does not.
func TestTask3411_FencedWritesNameTheirInstall(t *testing.T) {
	f := task3408Fixture(t, "inst-actor")
	var epoch int64
	if err := f.s.db.QueryRow(f.s.q(`SELECT auth_epoch FROM app_installs WHERE id = ?`), f.installID).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	actor := FencedActor{Kind: "user", UserID: f.bot.ID}
	fenced := func(fn func(*FencedTx)) {
		t.Helper()
		ft, err := f.s.BeginFenced(context.Background(), FenceSpec{InstallID: f.installID, WorkspaceID: f.ws.ID, Epoch: epoch, Companions: []string{f.companion.ID}})
		if err != nil {
			t.Fatal(err)
		}
		fn(ft)
		if err := ft.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	// The support portal's own field, which it PATCHes (the echo case).
	if _, err := f.s.db.Exec(f.s.q(`UPDATE collections SET schema = ? WHERE id = ?`),
		`{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open","required":true},{"key":"public_status","label":"Public status","type":"text"}]}`,
		f.companion.ID); err != nil {
		t.Fatal(err)
	}
	// Drop what the fixture emitted, so only this test's events are read.
	if _, err := f.s.db.Exec(`DELETE FROM event_outbox`); err != nil {
		t.Fatal(err)
	}

	var item *models.Item
	var commentID string
	fenced(func(ft *FencedTx) {
		var err error
		if item, err = ft.CreateItem(FencedItemCreate{CollectionID: f.companion.ID, Title: "App ticket", Fields: `{"status":"open"}`, Actor: actor}); err != nil {
			t.Fatal(err)
		}
	})
	fenced(func(ft *FencedTx) {
		key := []byte("etag-key")
		if _, err := ft.UpdateItemFields(item.ID, map[string]any{"status": "done"}, AppItemETag(key, f.installID, item.ID, item.Seq), key, actor); err != nil {
			t.Fatal(err)
		}
	})
	fenced(func(ft *FencedTx) {
		cur, err := ft.Item(item.ID)
		if err != nil {
			t.Fatal(err)
		}
		key := []byte("etag-key")
		if _, err := ft.UpdateItemFields(item.ID, map[string]any{"public_status": "replied"}, AppItemETag(key, f.installID, item.ID, cur.Seq), key, actor); err != nil {
			t.Fatal(err)
		}
	})
	fenced(func(ft *FencedTx) {
		c, err := ft.CreateComment(FencedCommentCreate{ItemID: item.ID, Body: "from the app", Actor: actor})
		if err != nil {
			t.Fatal(err)
		}
		commentID = c.ID
	})
	fenced(func(ft *FencedTx) {
		if _, err := ft.UpdateComment(item.ID, commentID, "edited by the app", actor); err != nil {
			t.Fatal(err)
		}
	})
	fenced(func(ft *FencedTx) {
		if err := ft.DeleteComment(item.ID, commentID, actor); err != nil {
			t.Fatal(err)
		}
	})
	// A person edits the same item.
	title := "Human edit"
	if _, err := f.s.UpdateItem(item.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}

	seen := map[string]int{}
	var humanUpdate, appUpdate []byte
	for _, ev := range task3389Pending(t, f.s) {
		b := task3389Block(t, ev)
		human := ev.doc["title"] == "Human edit"
		got, _ := b["actor_via_app"].(string)
		switch {
		case human && got != "":
			t.Errorf("%s by a person names install %q", ev.eventType, got)
		case !human && got != f.installID:
			t.Errorf("%s by the app: actor_via_app %q, want %s", ev.eventType, got, f.installID)
		}
		if !human {
			seen[ev.eventType]++
		}
		raw, _ := json.Marshal(ev.doc)
		if ev.eventType == kernelevents.ItemUpdated {
			if human {
				humanUpdate = raw
			} else {
				appUpdate = raw
			}
		}
	}
	for _, typ := range []string{kernelevents.ItemCreated, kernelevents.ItemStatusChanged, kernelevents.ItemUpdated, kernelevents.CommentCreated, kernelevents.CommentUpdated, kernelevents.CommentDeleted} {
		if seen[typ] == 0 {
			t.Errorf("no fenced %s event was emitted, so it was not checked", typ)
		}
	}

	// The portal echo, through the app DTO.
	envelope := func(raw []byte) string {
		t.Helper()
		if raw == nil {
			t.Fatal("precondition: an item.updated event is missing")
		}
		b, _, err := BuildAppEventDTO(kernelevents.ItemUpdated, "e", "t", raw)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		v, _ := m["via_app"].(string)
		return v
	}
	if got := envelope(appUpdate); got != f.installID {
		t.Errorf("the app's own item.updated: via_app %q, want %s (echo suppression needs it)", got, f.installID)
	}
	if got := envelope(humanUpdate); got != "" {
		t.Errorf("a person's item.updated to the app's item: via_app %q, want none", got)
	}
}

// A v1 block (written before v2, still pending) keeps the created-only rule.
func TestTask3411_V1BlocksKeepTheCreateOnlyRule(t *testing.T) {
	v1 := func(event string) string {
		payload := `{"id":"i1","title":"T","content":"C","app_projection":{"v":1,"collection_id":"c1","creator":{"via_app":"inst-1"},"fields":{}}}`
		b, _, err := BuildAppEventDTO(event, "e", "t", []byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		v, _ := m["via_app"].(string)
		return v
	}
	if got := v1(kernelevents.ItemCreated); got != "inst-1" {
		t.Errorf("v1 item.created: %q", got)
	}
	if got := v1(kernelevents.ItemUpdated); got != "" {
		t.Errorf("v1 item.updated: %q, want none (the creator is not the writer)", got)
	}
}
