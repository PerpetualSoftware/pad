package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3833: the legacy document version doors read the document's body for
// the workspace check, then resolved the version list from that body in a
// second read: BUG-3814's race on the documents table. A content write
// landing between the two adds a reverse patch against the NEW body while the
// walk starts from the OLD one. Reuses BUG-3814's three bodies.
func TestBUG3833_DocumentVersionDoorsResolveFromOneSnapshot(t *testing.T) {
	if got, err := diff.ApplyPatch(bug3814Current, diff.CreateReversePatch(bug3814Current, bug3814Newer)); err == nil && got == bug3814Current {
		t.Fatal("instrument: a new version's patch resolves correctly from the stale body")
	}

	doors := []struct {
		name string
		path string
		body func(t *testing.T, raw []byte) string
	}{
		{"list", "", func(t *testing.T, raw []byte) string {
			var vs []models.Version
			if err := json.Unmarshal(raw, &vs); err != nil {
				t.Fatalf("decode list: %v", err)
			}
			for _, v := range vs {
				if v.ID == "vt" {
					return v.Content
				}
			}
			t.Fatalf("vt missing from the list: %s", raw)
			return ""
		}},
		{"get", "/vt", func(t *testing.T, raw []byte) string {
			var v models.Version
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatal(err)
			}
			return v.Content
		}},
	}
	for _, door := range doors {
		t.Run(door.name, func(t *testing.T) {
			srv := testServer(t)
			ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "BUG-3833"})
			if err != nil {
				t.Fatal(err)
			}
			doc, err := srv.store.CreateDocument(ws.ID, models.DocumentCreate{Title: "Doc", Content: bug3814Current, CreatedBy: "user", Source: "web"})
			if err != nil {
				t.Fatal(err)
			}
			db := srv.store.DB()
			if _, err := db.Exec(`DELETE FROM versions WHERE document_id = ?`, doc.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO versions (id, document_id, content, change_summary, created_by, source, is_diff, created_at) VALUES (?, ?, ?, '', 'user', 'web', 1, ?)`,
				"vt", doc.ID, diff.CreateReversePatch(bug3814Target, bug3814Current), time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)); err != nil {
				t.Fatal(err)
			}

			// A different source than vt's defeats the throttle, so the
			// write adds a new reverse-patch row against the new body.
			fired := 0
			nested := false
			restore := srv.store.SetBeforeVersionsReadHookForTesting(func(id string) {
				if id != doc.ID || nested || fired > 0 {
					return
				}
				fired++
				nested = true
				content := bug3814Newer
				if _, err := srv.store.UpdateDocument(doc.ID, models.DocumentUpdate{Content: &content, LastModifiedBy: "user", Source: "cli"}); err != nil {
					t.Errorf("concurrent write: %v", err)
				}
				nested = false
			})
			rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/documents/"+doc.ID+"/versions"+door.path, nil)
			restore()
			if rr.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", door.name, rr.Code, rr.Body.String())
			}
			got := door.body(t, rr.Body.Bytes())

			if fired != 1 {
				t.Fatalf("instrument: the concurrent write ran %d times, want 1", fired)
			}
			if vs, err := srv.store.ListVersions(doc.ID); err != nil || len(vs) != 2 {
				t.Fatalf("instrument: want the write to add a version (2 rows), got %d: %v", len(vs), err)
			}
			if got != bug3814Target {
				t.Fatalf("vt resolved to %q, want the version it records %q", got, bug3814Target)
			}
		})
	}
}
