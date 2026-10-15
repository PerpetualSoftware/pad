package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3814, store leg: the server test (internal/server) runs on SQLite only,
// so this is the one that proves the snapshot holds on Postgres, where
// beginReadSnapshot is REPEATABLE READ rather than a deferred transaction.
// A content write lands between the snapshot's body read and its version
// read; resolving vt must still use the body the version list was read with.

func bug3814StoreLines(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "Line %d of the plan.\n", i)
	}
	return b.String()
}

func TestBUG3814_SnapshotResolvesFromOneSnapshot(t *testing.T) {
	target := bug3814StoreLines(1, 20)
	current := bug3814StoreLines(1, 21)
	newer := "Line 0 of the plan.\n" + bug3814StoreLines(1, 21)

	reads := []struct {
		name string
		// body is what the snapshot read reports for vt.
		body func(t *testing.T, s *store.Store, itemID string) string
	}{
		{"list", func(t *testing.T, s *store.Store, itemID string) string {
			_, vs, err := s.ListItemVersionsResolvedSnapshot(context.Background(), itemID, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range vs {
				if v.ID == "vt" {
					return v.Content
				}
			}
			t.Fatal("vt missing from the snapshot list")
			return ""
		}},
		{"get", func(t *testing.T, s *store.Store, itemID string) string {
			v, err := s.GetItemVersionResolvedSnapshot(context.Background(), itemID, "vt")
			if err != nil || v == nil {
				t.Fatalf("get: %v %v", v, err)
			}
			return v.Content
		}},
		{"diff", func(t *testing.T, s *store.Store, itemID string) string {
			d, err := s.GetItemVersionDiffSnapshot(context.Background(), itemID, "vt")
			if err != nil || d == nil {
				t.Fatalf("diff: %v %v", d, err)
			}
			return d.Before
		}},
	}

	for _, backend := range contentBackends() {
		for _, read := range reads {
			for _, force := range []bool{true, false} {
				name := backend.name + "/" + read.name + "/rebased"
				if force {
					name = backend.name + "/" + read.name + "/new_version"
				}
				t.Run(name, func(t *testing.T) {
					s := backend.open(t)
					ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "BUG-3814"})
					if err != nil {
						t.Fatal(err)
					}
					col, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Docs", Schema: `{"fields":[]}`})
					if err != nil {
						t.Fatal(err)
					}
					item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Doc", Content: current, Fields: `{}`})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := s.DB().Exec(`DELETE FROM item_versions WHERE item_id = $1`, item.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := s.DB().Exec(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq) VALUES ($1, $2, $3, $4, $5, 1)`,
						"vt", item.ID, diff.CreateReversePatch(target, current), true, time.Now().UTC().Format(time.RFC3339)); err != nil {
						t.Fatal(err)
					}

					// The write commits on its own connection while the
					// snapshot transaction is open. force mints a new
					// version; without it the write rebases vt (BUG-3745).
					fired := 0
					nested := false
					restore := s.SetBeforeItemVersionsReadHookForTesting(func(id string) {
						if id != item.ID || nested || fired > 0 {
							return
						}
						fired++
						nested = true
						content := newer
						if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &content, ForceVersion: force}); err != nil {
							t.Errorf("concurrent write: %v", err)
						}
						nested = false
					})
					got := read.body(t, s, item.ID)
					restore()

					if fired != 1 {
						t.Fatalf("instrument: the concurrent write ran %d times, want 1", fired)
					}
					after, err := s.GetItem(item.ID)
					if err != nil || after == nil || after.Content != newer {
						t.Fatalf("instrument: the concurrent write did not commit: %v", err)
					}
					if got != target {
						t.Fatalf("vt resolved to %q, want the version it records %q", got, target)
					}
				})
			}
		}
	}
}
