package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3833: the legacy documents' version doors, the twin of BUG-3814's item
// doors. A document content write lands between the snapshot's body read and
// its version read; resolving vt must still use the body the version list was
// read with. Runs on SQLite and Postgres, where beginReadSnapshot differs.
func TestBUG3833_DocumentVersionSnapshotResolvesFromOneSnapshot(t *testing.T) {
	target := bug3814StoreLines(1, 20)
	current := bug3814StoreLines(1, 21)
	newer := "Line 0 of the plan.\n" + bug3814StoreLines(1, 21)

	reads := []struct {
		name string
		body func(t *testing.T, s *store.Store, docID string) string
	}{
		{"list", func(t *testing.T, s *store.Store, docID string) string {
			vs, err := s.ListVersionsResolvedSnapshot(context.Background(), docID)
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
		{"get", func(t *testing.T, s *store.Store, docID string) string {
			v, err := s.GetVersionResolvedSnapshot(context.Background(), "vt", docID)
			if err != nil || v == nil {
				t.Fatalf("get: %v %v", v, err)
			}
			return v.Content
		}},
	}

	for _, backend := range contentBackends() {
		for _, read := range reads {
			t.Run(backend.name+"/"+read.name, func(t *testing.T) {
				s := backend.open(t)
				ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "BUG-3833"})
				if err != nil {
					t.Fatal(err)
				}
				doc, err := s.CreateDocument(ws.ID, models.DocumentCreate{Title: "Doc", Content: current, CreatedBy: "user", Source: "web"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`DELETE FROM versions WHERE document_id = $1`, doc.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`INSERT INTO versions (id, document_id, content, change_summary, created_by, source, is_diff, created_at) VALUES ($1, $2, $3, '', 'user', 'web', $4, $5)`,
					"vt", doc.ID, diff.CreateReversePatch(target, current), true, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)); err != nil {
					t.Fatal(err)
				}

				// The write commits on its own connection while the snapshot
				// transaction is open. A different source than vt's defeats
				// the throttle, so the write adds a new reverse-patch row.
				fired := 0
				nested := false
				restore := s.SetBeforeVersionsReadHookForTesting(func(id string) {
					if id != doc.ID || nested || fired > 0 {
						return
					}
					fired++
					nested = true
					content := newer
					if _, err := s.UpdateDocument(doc.ID, models.DocumentUpdate{Content: &content, LastModifiedBy: "user", Source: "cli"}); err != nil {
						t.Errorf("concurrent write: %v", err)
					}
					nested = false
				})
				got := read.body(t, s, doc.ID)
				restore()

				if fired != 1 {
					t.Fatalf("instrument: the concurrent write ran %d times, want 1", fired)
				}
				after, err := s.GetDocument(doc.ID)
				if err != nil || after == nil || after.Content != newer {
					t.Fatalf("instrument: the concurrent write did not commit: %v", err)
				}
				vs, err := s.ListVersions(doc.ID)
				if err != nil || len(vs) != 2 {
					t.Fatalf("instrument: want the write to add a version (2 rows), got %d: %v", len(vs), err)
				}
				if got != target {
					t.Fatalf("vt resolved to %q, want the version it records %q", got, target)
				}
			})
		}
	}
}
