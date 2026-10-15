package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3825, both import doors: an archive carrying one invocation_slug on two
// items of a collection used to fail the whole import on the unique index
// (idx_items_invocation_slug_per_collection) with a 500 import_failed. It now
// imports, the first item in archive order keeps the slug, and the 201 names
// the drop as (item, field, value, holder). The store tests pin what is
// stored; this pins the status and the response.
func TestBUG3825_ImportDropsADuplicateInvocationSlugAndSaysSo(t *testing.T) {
	const ts = "2026-10-15T00:00:00Z"
	bundle := models.WorkspaceExport{
		Version:   1,
		Workspace: models.WorkspaceExportMeta{Name: "Dup Slug", Slug: "dup-slug", Settings: "{}"},
		Collections: []models.CollectionExport{{
			ID: "c", Name: "Playbooks", Slug: "playbooks", Prefix: "PLAYB", Settings: "{}", CreatedAt: ts, UpdatedAt: ts,
			Schema: `{"fields":[{"key":"invocation_slug","type":"text"}]}`,
		}},
		Items: []models.ItemExport{
			{ID: "11111111-1111-4111-8111-111111111111", CollectionID: "c", Title: "One", Slug: "one",
				Fields: `{"invocation_slug":"ship"}`, Tags: "[]", ItemNumber: 1, CreatedAt: ts, UpdatedAt: ts},
			{ID: "22222222-2222-4222-8222-222222222222", CollectionID: "c", Title: "Two", Slug: "two",
				Fields: `{"invocation_slug":"ship"}`, Tags: "[]", ItemNumber: 2, CreatedAt: ts, UpdatedAt: ts},
		},
	}
	export, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	want := []models.ImportUniqueDrop{{Item: "PLAYB-2", Key: "invocation_slug", Value: "ship", Holder: "PLAYB-1"}}

	for _, door := range []struct {
		name   string
		server func(*testing.T) *Server
		post   func(*Server) *httptest.ResponseRecorder
	}{
		{"json", testServer, func(srv *Server) *httptest.ResponseRecorder {
			return rawJSONRequest(srv, "POST", "/api/v1/workspaces/import", string(export))
		}},
		// The bundle door needs attachment storage configured.
		{"bundle", func(t *testing.T) *Server {
			srv, _ := testServerWithAttachments(t)
			return srv
		}, func(srv *Server) *httptest.ResponseRecorder {
			return postBundle(srv, "DupSlugBundle", gzipTar(t, []bundleEntry{{"pad-export.json", export}}))
		}},
	} {
		t.Run(door.name, func(t *testing.T) {
			srv := door.server(t)
			rr := door.post(srv)
			if rr.Code != http.StatusCreated {
				t.Fatalf("import: %d %s, want 201", rr.Code, rr.Body.String())
			}
			var got struct {
				Slug                string                    `json:"slug"`
				DroppedUniqueValues []models.ImportUniqueDrop `json:"dropped_unique_values"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v (%s)", err, rr.Body.String())
			}
			if got.Slug == "" {
				t.Errorf("the workspace is no longer in the 201: %s", rr.Body.String())
			}
			if !reflect.DeepEqual(got.DroppedUniqueValues, want) {
				t.Errorf("dropped_unique_values = %+v, want %+v", got.DroppedUniqueValues, want)
			}
		})
	}
}
