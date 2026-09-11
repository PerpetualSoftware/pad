package store

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestRelationReindexCost measures ReindexCollectionRelationLinks at scale.
//
// The lead asked for this number on TASK-2997's trail before accepting the
// reindex-on-schema-change ruling, and the reason is worth keeping: "a rare
// owner-only operation" is a claim about FREQUENCY, and it says nothing about
// what the operation costs when it does run. The reindex holds the
// schema-update transaction open for its whole duration, so its cost is a
// user-visible latency on an API call, not background work.
//
// Skipped unless RELATION_COST=1 — it builds 10k items, which is minutes of
// setup nobody wants on every `go test ./...`.
func TestRelationReindexCost(t *testing.T) {
	if os.Getenv("RELATION_COST") != "1" {
		t.Skip("set RELATION_COST=1 to run the 10k reindex measurement")
	}
	s := testStore(t)
	ws, colors, cars, red := relationFixture(t, s)
	_ = colors

	const n = 10000
	start := time.Now()
	for i := 0; i < n; i++ {
		blob, _ := json.Marshal(map[string]any{"status": "open", "color": red.ID})
		if _, err := s.CreateItem(ws.ID, cars.ID, models.ItemCreate{
			Title:  fmt.Sprintf("Car %d", i),
			Fields: string(blob),
		}); err != nil {
			t.Fatalf("CreateItem %d: %v", i, err)
		}
	}
	t.Logf("setup: created %d items in %s", n, time.Since(start).Round(time.Millisecond))

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	reindexStart := time.Now()
	if err := s.ReindexCollectionRelationLinks(tx, cars.ID, ws.ID); err != nil {
		tx.Rollback() //nolint:errcheck
		t.Fatalf("reindex: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	elapsed := time.Since(reindexStart)

	count, err := s.CountRelationBacklinks(red.ID, ws.ID, unrestricted)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != n {
		t.Fatalf("reindex produced %d edges, want %d — the measurement below would describe the wrong work", count, n)
	}
	t.Logf("REINDEX COST: %d items, %d edges, %s", n, count, elapsed.Round(time.Millisecond))
}
