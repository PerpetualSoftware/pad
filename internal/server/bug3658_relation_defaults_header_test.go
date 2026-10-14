package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3658, the import door's surface: the response header counts the
// relation defaults the import dropped because they named no imported item,
// and is "0" when none were. The store test pins what is stored.
func TestWorkspaceImportReportsDroppedRelationDefaults(t *testing.T) {
	const ts = "2026-09-25T12:00:00Z"
	const item = "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct{ name, def, want string }{
		{"dangling", "33333333-3333-4333-8333-333333333333", "1"},
		{"imported", item, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t)
			bundle := models.WorkspaceExport{
				Version:   1,
				Workspace: models.WorkspaceExportMeta{Name: "Def " + tc.name, Slug: "def-" + tc.name, Settings: "{}"},
				Collections: []models.CollectionExport{{
					ID: "c", Name: "Docs", Slug: "docs", Prefix: "DOC", Settings: "{}", CreatedAt: ts, UpdatedAt: ts,
					Schema: `{"fields":[{"key":"see","type":"relation","collection":"docs","default":"` + tc.def + `"}]}`,
				}},
				Items: []models.ItemExport{
					{ID: item, CollectionID: "c", Title: "One", Slug: "one", Fields: "{}", Tags: "[]", ItemNumber: 1, CreatedAt: ts, UpdatedAt: ts},
				},
			}
			b, err := json.Marshal(bundle)
			if err != nil {
				t.Fatal(err)
			}
			rr := rawJSONRequest(srv, "POST", "/api/v1/workspaces/import", string(b))
			if rr.Code != http.StatusCreated {
				t.Fatalf("import: %d %s", rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get(ImportDroppedRelationDefaultsHeader); got != tc.want {
				t.Errorf("%s = %q, want %q", ImportDroppedRelationDefaultsHeader, got, tc.want)
			}
		})
	}
}
