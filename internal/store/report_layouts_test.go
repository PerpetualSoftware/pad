package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestReportLayout_SaveGetUpsert(t *testing.T) {
	s := testStore(t)
	u, err := s.CreateUser(models.UserCreate{Name: "Lay", Email: "lay@example.com"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "L", Slug: "l", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	// No layout yet → nil.
	got, err := s.GetReportLayout(u.ID, ws.ID)
	if err != nil {
		t.Fatalf("get (empty): %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil layout before save, got %+v", got)
	}

	// Save.
	in := models.ReportLayout{
		HiddenCards:        []string{"wip", "status_distribution"},
		DefaultWindow:      "month",
		DefaultCollections: []string{"tasks"},
	}
	if err := s.SaveReportLayout(u.ID, ws.ID, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err = s.GetReportLayout(u.ID, ws.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.DefaultWindow != "month" || len(got.HiddenCards) != 2 || len(got.DefaultCollections) != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Upsert: second save replaces, doesn't duplicate.
	in2 := models.ReportLayout{HiddenCards: []string{}, DefaultWindow: "week", DefaultCollections: []string{}}
	if err := s.SaveReportLayout(u.ID, ws.ID, in2); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	got, err = s.GetReportLayout(u.ID, ws.ID)
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if got.DefaultWindow != "week" || len(got.HiddenCards) != 0 {
		t.Fatalf("upsert didn't replace: %+v", got)
	}
	var count int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM user_report_layouts WHERE user_id = ? AND workspace_id = ?`), u.ID, ws.ID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row after upsert, got %d", count)
	}
}

func TestReportLayout_IsolatedPerUserAndWorkspace(t *testing.T) {
	s := testStore(t)
	a, _ := s.CreateUser(models.UserCreate{Name: "A", Email: "a@example.com"})
	b, _ := s.CreateUser(models.UserCreate{Name: "B", Email: "b@example.com"})
	ws, _ := s.CreateWorkspace(models.WorkspaceCreate{Name: "W", Slug: "w", OwnerID: a.ID})

	if err := s.SaveReportLayout(a.ID, ws.ID, models.ReportLayout{DefaultWindow: "day"}); err != nil {
		t.Fatalf("save a: %v", err)
	}
	// B has no layout in the same workspace.
	got, err := s.GetReportLayout(b.ID, ws.ID)
	if err != nil {
		t.Fatalf("get b: %v", err)
	}
	if got != nil {
		t.Fatalf("user B should have no layout, got %+v", got)
	}
}
