package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3390: via_app is part of the writer identity wherever writes are
// consolidated (DOC-3371 §4), so two installs, or an install and a human,
// never fold into one activity or one version.

func TestTASK3390_DebounceKeepsInstallsApart(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Identity")
	col := createTestCollection(t, s, ws.ID, "Tickets")
	item := createTestItem(t, s, ws.ID, col.ID, "Shared", "")
	appA := insertTestInstall(t, s, ws.ID, "active", 1)
	appB := insertTestInstall(t, s, ws.ID, "active", 1)

	write := func(via string) {
		t.Helper()
		if _, err := s.CreateActivityDebounced(models.Activity{
			WorkspaceID: ws.ID, DocumentID: item.ID, Action: "updated", Actor: "user", Source: "app",
			Metadata: `{"changes":"x"}`, ViaApp: via,
		}); err != nil {
			t.Fatal(err)
		}
	}
	write(appA)
	write(appA) // same writer: merges
	write(appB) // another install: its own row
	write("")   // no install: its own row
	rows := func(via string) int {
		var n int
		q := `SELECT COUNT(*) FROM activities WHERE document_id = ? AND action = 'updated' AND COALESCE(via_app, '') = ?`
		if err := s.db.QueryRow(s.q(q), item.ID, via).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if a, b, none := rows(appA), rows(appB), rows(""); a != 1 || b != 1 || none != 1 {
		t.Fatalf("activities per writer: A=%d B=%d none=%d, want 1 each", a, b, none)
	}
}

func TestTASK3390_VersionThrottleKeepsInstallsApart(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Versions")
	col := createTestCollection(t, s, ws.ID, "Tickets")
	item := createTestItem(t, s, ws.ID, col.ID, "Versioned", "first body")
	app := insertTestInstall(t, s, ws.ID, "active", 1)
	// The newest version is an app's, with the same created_by and source a
	// human edit would carry.
	if _, err := s.db.Exec(s.q(`UPDATE item_versions SET via_app = ?, created_by = 'user', source = 'web' WHERE item_id = ?`), app, item.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.shouldCreateItemVersion(s.db, item.ID, "user", "web", ""); err != nil || !ok {
		t.Errorf("a human edit after an app's version must start its own version: %v %v", ok, err)
	}
	if ok, err := s.shouldCreateItemVersion(s.db, item.ID, "user", "web", app); err != nil || ok {
		t.Errorf("the same install inside the window is throttled: %v %v", ok, err)
	}
}
