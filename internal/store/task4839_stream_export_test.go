package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestStreamWorkspaceExport_VersionsMatchSlice(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Stream Export Versions")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item1 := createTestItem(t, s, ws.ID, col.ID, "First", "body v1")
	item2 := createTestItem(t, s, ws.ID, col.ID, "Second", "other v1")

	for _, body := range []string{"body v2", "body v3", "body v4"} {
		b := body
		if _, err := s.UpdateItem(item1.ID, models.ItemUpdate{Content: &b}); err != nil {
			t.Fatalf("update item1: %v", err)
		}
	}
	b2 := "other v2"
	if _, err := s.UpdateItem(item2.ID, models.ItemUpdate{Content: &b2}); err != nil {
		t.Fatalf("update item2: %v", err)
	}

	want, err := s.ExportWorkspaceQ(s.db, ws.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspaceQ: %v", err)
	}
	if len(want.ItemVersions) == 0 {
		t.Fatal("precondition: expected exported workspace to carry item versions")
	}

	var gotBase *models.WorkspaceExport
	var gotVersions []models.ItemVersionExport
	err = s.StreamWorkspaceExport(context.Background(), ws.Slug, ExportOptions{
		WithAttachments: false,
		IncludeVersions: true,
	}, func(x *models.WorkspaceExport, atts []models.Attachment, versions VersionIter) error {
		gotBase = x
		if len(x.ItemVersions) != 0 {
			t.Errorf("expected base export to have no preloaded versions, got %d", len(x.ItemVersions))
		}
		return versions(func(v *models.ItemVersionExport) error {
			gotVersions = append(gotVersions, *v)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("StreamWorkspaceExport(include_versions=true): %v", err)
	}
	if !reflect.DeepEqual(gotVersions, want.ItemVersions) {
		t.Fatalf("streamed versions differ from ExportWorkspaceQ:\n got: %+v\nwant: %+v", gotVersions, want.ItemVersions)
	}

	wantCopy := *want
	wantCopy.ExportedAt = ""
	wantCopy.ItemVersions = nil
	gotCopy := *gotBase
	gotCopy.ExportedAt = ""
	gotCopy.ItemVersions = nil
	if !reflect.DeepEqual(gotCopy, wantCopy) {
		t.Fatalf("base export differs:\n got: %+v\nwant: %+v", gotCopy, wantCopy)
	}

	// Control: IncludeVersions=false yields no versions.
	var omittedCount int
	err = s.StreamWorkspaceExport(context.Background(), ws.Slug, ExportOptions{
		WithAttachments: false,
		IncludeVersions: false,
	}, func(x *models.WorkspaceExport, atts []models.Attachment, versions VersionIter) error {
		return versions(func(v *models.ItemVersionExport) error {
			omittedCount++
			return nil
		})
	})
	if err != nil {
		t.Fatalf("StreamWorkspaceExport(include_versions=false): %v", err)
	}
	if omittedCount != 0 {
		t.Fatalf("expected 0 versions when IncludeVersions=false, got %d", omittedCount)
	}
}
