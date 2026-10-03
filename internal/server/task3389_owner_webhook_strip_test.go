package server

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TestTask3389_OwnerWebhooksNeverCarryTheAppProjection: the app-projection
// block is written into every item and comment outbox payload (TASK-3389),
// but it is for app delivery only. It carries the creator's display name,
// which the item snapshot deliberately does not (scrubItemPII). Owner webhooks
// receive stored payloads, so the drain strips the block at its one delivery
// point. Covered: a single item event, a comment event, and a folded bulk
// batch whose members each carry the block.
//
// Precondition legs prove the stored payloads DO carry the block, so the
// delivered-body absence is the strip working, not the block missing.
func TestTask3389_OwnerWebhooksNeverCarryTheAppProjection(t *testing.T) {
	srv, sink, ws := drainFixture(t)
	// The block is written only once the workspace has an installed app
	// (TASK-3392).
	if _, err := srv.store.DB().Exec(`INSERT INTO app_installs (id, workspace_id, origin, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"inst-drain", ws.ID, "https://portal.example", "2026-10-03T00:00:00Z", "2026-10-03T00:00:00Z"); err != nil {
		t.Fatalf("plant install: %v", err)
	}
	col := createDrainCollection(t, srv, ws.ID)

	item := createDrainItem(t, srv, ws.ID, col.ID, "Single")
	if _, err := srv.store.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Body: "a comment", Author: "Someone"}); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}

	const batch = "batch-3389"
	var ids []string
	for _, title := range []string{"B1", "B2"} {
		ids = append(ids, createDrainItem(t, srv, ws.ID, col.ID, title).ID)
	}
	for _, id := range ids {
		if err := srv.store.DeleteItem(id, store.WithEventBatch(batch)); err != nil {
			t.Fatalf("archive %s: %v", id, err)
		}
	}
	if err := srv.store.EmitBulkHeaderEvent(ws.ID, batch, "archive", ids, nil); err != nil {
		t.Fatalf("emit header: %v", err)
	}

	pending, err := srv.store.ListPendingOutboxEvents(1000)
	if err != nil {
		t.Fatalf("ListPendingOutboxEvents: %v", err)
	}
	withBlock := 0
	for _, ev := range pending {
		if strings.Contains(string(ev.Payload), `"app_projection"`) {
			withBlock++
		}
	}
	// item.created x3, comment.created, item.deleted x2 (batch members).
	if withBlock < 6 {
		t.Fatalf("precondition: only %d stored payloads carry app_projection, want at least 6", withBlock)
	}

	srv.runOutboxDrainTick()

	got := sink.deliveries()
	if len(got) == 0 {
		t.Fatal("no deliveries: the drain delivered nothing, so the strip was not exercised")
	}
	sawBulk := false
	for _, d := range got {
		if d.Event == "item.bulk_updated" {
			sawBulk = true
		}
		if strings.Contains(string(d.Data), "app_projection") {
			t.Errorf("owner webhook delivery %s carries app_projection: %s", d.Event, d.Data)
		}
	}
	if !sawBulk {
		t.Error("the folded bulk delivery was not exercised")
	}
}
