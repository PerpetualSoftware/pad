package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2896, the collection create and update doors. Traits sent in the STRING
// wire form were stored as the caller spelled them, so a repeated member was
// read FIRST by SQLite's unique index (json_extract) and LAST by the Go
// resolver: measured as a 409 from the database on a write the pre-check had
// passed. They are now stored as the parsed declaration's own encoding, and the
// response names the repeated member in warnings.collapsed_duplicate_keys.
// A top-level and a nested repeat, at each door.
func TestCollectionTraitsWithRepeatedMembersAreStoredCollapsed(t *testing.T) {
	cases := []struct {
		name, traits, member, wantKind string
	}{
		{"top-level repeat", `{"artifact_kind":{"kind":"convention"},"artifact_kind":{"kind":"playbook"}}`, "artifact_kind", "playbook"},
		{"nested repeat", `{"artifact_kind":{"kind":"convention","kind":"playbook"}}`, "kind", "playbook"},
	}
	for _, door := range []string{"create", "update"} {
		for _, tc := range cases {
			t.Run(door+"/"+tc.name, func(t *testing.T) {
				srv := testServer(t)
				// A workspace with NO collections, so no kind is taken and the
				// raw blob would otherwise be stored rather than refused.
				ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Empty " + door})
				if err != nil {
					t.Fatal(err)
				}
				// PREMISE: the two readers disagree about the fixture.
				var first string
				if err := srv.store.DB().QueryRow(`SELECT json_extract(?, '$.artifact_kind.kind')`, tc.traits).Scan(&first); err == nil && first == tc.wantKind {
					t.Fatalf("premise: json_extract already reads %q, so the fixture shows no disagreement", first)
				}

				base := "/api/v1/workspaces/" + ws.Slug + "/collections"
				body := `{"name":"Things","traits":` + strconv.Quote(tc.traits) + `}`
				method, path, want := "POST", base, http.StatusCreated
				if door == "update" {
					if rr := rawJSONRequest(srv, "POST", base, `{"name":"Things"}`); rr.Code != http.StatusCreated {
						t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
					}
					body = `{"traits":` + strconv.Quote(tc.traits) + `}`
					method, path, want = "PATCH", base+"/things", http.StatusOK
				}
				rr := rawJSONRequest(srv, method, path, body)
				if rr.Code != want {
					t.Fatalf("%s: %d %s", door, rr.Code, rr.Body.String())
				}
				var resp struct {
					Warnings *struct {
						CollapsedDuplicateKeys []string `json:"collapsed_duplicate_keys"`
					} `json:"warnings"`
				}
				_ = json.Unmarshal(rr.Body.Bytes(), &resp)
				if resp.Warnings == nil || len(resp.Warnings.CollapsedDuplicateKeys) != 1 || resp.Warnings.CollapsedDuplicateKeys[0] != tc.member {
					t.Errorf("warnings.collapsed_duplicate_keys: got %+v, want [%s]", resp.Warnings, tc.member)
				}

				coll, err := srv.store.GetCollectionBySlug(ws.ID, "things")
				if err != nil || coll == nil {
					t.Fatalf("stored collection: %v", err)
				}
				var sqlKind string
				if err := srv.store.DB().QueryRow(`SELECT json_extract(traits, '$.artifact_kind.kind') FROM collections WHERE id = ?`, coll.ID).Scan(&sqlKind); err != nil {
					t.Fatal(err)
				}
				parsed, _ := models.ParseCollectionTraits(coll.Traits)
				if sqlKind != tc.wantKind || parsed.ArtifactKind == nil || parsed.ArtifactKind.Kind != tc.wantKind {
					t.Errorf("SQL reads %q and the resolver reads %+v; both must read %s. Stored: %s", sqlKind, parsed.ArtifactKind, tc.wantKind, coll.Traits)
				}
			})
		}
	}
}

// The control direction: traits with no repeat are stored exactly as sent and
// the response carries no warnings key at all.
func TestCollectionTraitsWithoutRepeatsAreStoredVerbatim(t *testing.T) {
	srv := testServer(t)
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Clean"})
	if err != nil {
		t.Fatal(err)
	}
	const traits = `{ "artifact_kind" : { "kind" : "playbook" } }`
	rr := rawJSONRequest(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections", `{"name":"Things","traits":`+strconv.Quote(traits)+`}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rr.Body.Bytes(), &raw)
	if _, ok := raw["warnings"]; ok {
		t.Errorf("a clean write carries a warnings key: %s", raw["warnings"])
	}
	coll, _ := srv.store.GetCollectionBySlug(ws.ID, "things")
	if coll == nil || coll.Traits != traits {
		t.Errorf("clean traits were rewritten: stored %q, sent %q", coll.Traits, traits)
	}
}

// The import door's surface: the response header counts the blobs the import
// stored collapsed, and is "0" on a clean import so a client can tell that
// apart from a server that predates it. The store test pins what is stored.
func TestWorkspaceImportReportsCollapsedDuplicateKeys(t *testing.T) {
	const ts = "2026-09-25T12:00:00Z"
	for _, tc := range []struct {
		name, fields, want string
	}{
		{"repeat", `{"meta":{"k":1,"k":2}}`, "1"},
		{"clean", `{"meta":{"k":1}}`, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t)
			bundle := models.WorkspaceExport{
				Version:   1,
				Workspace: models.WorkspaceExportMeta{Name: "Imp " + tc.name, Slug: "imp-" + tc.name, Settings: "{}"},
				Collections: []models.CollectionExport{
					{ID: "c", Name: "Docs", Slug: "docs", Prefix: "DOC", Schema: `{"fields":[]}`, Settings: "{}", CreatedAt: ts, UpdatedAt: ts},
				},
				Items: []models.ItemExport{
					{ID: "i", CollectionID: "c", Title: "One", Slug: "one", Fields: tc.fields, Tags: "[]", ItemNumber: 1, CreatedAt: ts, UpdatedAt: ts},
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
			if got := rr.Header().Get(ImportCollapsedDuplicateKeysHeader); got != tc.want {
				t.Errorf("%s = %q, want %q", ImportCollapsedDuplicateKeysHeader, got, tc.want)
			}
		})
	}
}
