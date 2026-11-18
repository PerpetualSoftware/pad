package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const awaitsHumanSchema = `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","needs-review","done"],"terminal_options":["done"],"awaits_human_options":["needs-review"]}]}`

// TASK-4643: a collection's awaits_human_options survive export and import.
func TestImportWorkspace_AwaitsHumanOptionsRoundTrip(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	owner := createTestUser(t, s, "awaits-import@test.com", "Owner", "password123")
	src := createTestWorkspace(t, s, "Awaits Import Source")
	if _, err := s.CreateCollection(src.ID, models.CollectionCreate{Name: "Reviews", Slug: "reviews", Prefix: "REV", Schema: awaitsHumanSchema}); err != nil {
		t.Fatal(err)
	}
	exp, err := s.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.ImportWorkspace(exp, "awaits-ok", owner.ID, "")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	coll, err := s.GetCollectionBySlug(ws.ID, "reviews")
	if err != nil || coll == nil {
		t.Fatalf("imported collection: %v", err)
	}
	var schema models.CollectionSchema
	if err := models.UnmarshalItemFieldSchema([]byte(coll.Schema), &schema); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(schema.Fields[0].AwaitsHumanOptions, ","); got != "needs-review" {
		t.Fatalf("imported awaits_human_options = %q, want needs-review", got)
	}
}

// TASK-4643: import holds awaits_human_options to the create/update rules.
func TestImportWorkspace_RefusesInvalidAwaitsHumanOptions(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ awaits, want string }{
		"not an option":     {`["waiting"]`, `"waiting" is not one of its options`},
		"a terminal option": {`["done"]`, `"done" is one of its terminal_options`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := testStore(t)
			owner := createTestUser(t, s, "awaits-bad@test.com", "Owner", "password123")
			exp := &models.WorkspaceExport{
				Version:   1,
				Workspace: models.WorkspaceExportMeta{Name: "Awaits Bad", Slug: "awaits-bad"},
				Collections: []models.CollectionExport{{
					ID: "old-coll", Name: "Reviews", Slug: "reviews", Prefix: "REV",
					Schema:    `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"],"awaits_human_options":` + tc.awaits + `}]}`,
					CreatedAt: "2026-11-18T00:00:00Z", UpdatedAt: "2026-11-18T00:00:00Z",
				}},
			}
			_, err := s.ImportWorkspace(exp, "awaits-bad-target", owner.ID, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "awaits_human_options") {
				t.Fatalf("want an import refusal naming %s, got %v", tc.want, err)
			}
		})
	}
}
