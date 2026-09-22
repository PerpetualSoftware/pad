package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// The artifact-import door is the ONE `relationsCarry` caller
// (handlers_artifact_import.go), and BUG-3015 filed it as silently discarding a
// relation value it could not resolve. It does not: TASK-2878 (#1246) made the
// carrying door run to the end of resolveRelationsForWrite and return its
// caller issues as a report. Nothing pinned that, which is why the bug could be
// filed six days later against behaviour that had already changed.
//
// THE REACHABILITY, because BUG-3015's own body says this door cannot express
// the case and that is half wrong. An artifact's frontmatter carries a fixed
// allow-list of keys per kind, so an invented key like `owner_ref` is dropped at
// decode — which is all TestArtifactImport_DropsFieldKeysTheKindDoesNotDeclare
// probes. But `conventionFieldKeys` is {status, trigger, scope, priority, role},
// and the DESTINATION SCHEMA decides what those keys MEAN. Retype the declared
// `role` as a relation and a relation value rides straight in. Same shape as
// BUG-3079: the artifact format picks which keys travel, the schema picks the
// type they are judged against.
//
// EVERY CASE ASSERTS BOTH HALVES — what landed in the blob and what the response
// said about it — because "carried" and "reported" are two claims and this door
// is required to make both. A test reading only the warnings would pass against
// a door that reports a drop it also performs, which is the failure BUG-3015
// describes.
func TestImportArtifactCarriesAndReportsUnresolvableRelations(t *testing.T) {
	const (
		carriedAsSent = "carried"
		droppedKey    = "dropped"
	)
	cases := []struct {
		name string
		// field replaces the conventions collection's declared `role` field.
		field map[string]any
		// role is the artifact frontmatter's `role:` value; "" omits the key.
		role string
		// want is what the stored blob must hold for `role`.
		want     string
		wantKind string
		// warning is a substring the response's warnings array must carry;
		// "" means no warning may mention the key at all.
		warning string
	}{
		{
			name:     "names nothing",
			field:    relationField(nil),
			role:     "nobody-at-all",
			want:     "nobody-at-all",
			wantKind: carriedAsSent,
			warning:  "does not name an item in this workspace",
		},
		{
			name:     "names a live item in the wrong collection",
			field:    relationField(nil),
			role:     "__DECOY_REF__",
			want:     "__DECOY_REF__",
			wantKind: carriedAsSent,
			warning:  "does not name an item in this workspace",
		},
		{
			name:     "the field declares no target collection",
			field:    map[string]any{"key": "role", "label": "Role", "type": "relation"},
			role:     "nobody-at-all",
			want:     "nobody-at-all",
			wantKind: carriedAsSent,
			warning:  "does not name an item in this workspace",
		},
		{
			// Carried rather than refused even though the field is REQUIRED:
			// the value is kept, so the field is not absent and the required
			// check has nothing to object to. The contrast that makes this
			// meaningful is the row below, where the same field is left to a
			// default and the write DOES lose it.
			name:     "required field, unresolvable value",
			field:    relationField(map[string]any{"required": true}),
			role:     "nobody-at-all",
			want:     "nobody-at-all",
			wantKind: carriedAsSent,
			warning:  "does not name an item in this workspace",
		},
		{
			// The one row that legitimately drops. Nobody in the request typed
			// this value — the destination schema injected it — so it is
			// discarded rather than stored, and reported through the other
			// half of the same warnings struct.
			name:     "destination default that resolves to nothing",
			field:    relationField(map[string]any{"default": "ghost-nobody"}),
			role:     "",
			wantKind: droppedKey,
			warning:  "declares a default for it that is not a valid reference",
		},
		{
			// BUG-3028: an import CARRIES values nobody typed in this request,
			// so a blank on a REQUIRED relation is normalised to key-absent,
			// not refused. Nothing was lost, so nothing is reported.
			name:     "required field, whitespace-only value",
			field:    relationField(map[string]any{"required": true}),
			role:     `"   "`,
			wantKind: droppedKey,
			warning:  "",
		},
		{
			// CONTROL. Without it every assertion above is satisfied by a door
			// that resolves nothing at all and reports everything.
			name:     "control: a value that resolves",
			field:    relationField(nil),
			role:     "Real Owner",
			want:     "__TARGET_ID__",
			wantKind: carriedAsSent,
			warning:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t)
			wsSlug := createWSForTest(t, srv)
			ws, err := srv.store.GetWorkspaceBySlug(wsSlug)
			if err != nil || ws == nil {
				t.Fatalf("GetWorkspaceBySlug(%s): %v", wsSlug, err)
			}
			secrets := mustSchemaCollection(t, srv, ws.ID, "Secrets", `{"fields":[]}`)
			target, err := srv.store.CreateItem(ws.ID, secrets.ID, models.ItemCreate{Title: "Real Owner"})
			if err != nil {
				t.Fatalf("CreateItem(target): %v", err)
			}
			decoy := createDecoyInAnotherCollection(t, srv, ws.ID)

			retypeConventionRole(t, srv, ws.ID, tc.field)

			role := strings.ReplaceAll(tc.role, "__DECOY_REF__", decoy.Ref)
			want := strings.ReplaceAll(tc.want, "__DECOY_REF__", decoy.Ref)
			want = strings.ReplaceAll(want, "__TARGET_ID__", target.ID)

			frontmatter := ""
			if role != "" {
				frontmatter = "role: " + role + "\n"
			}
			data := []byte("---\n" +
				"pad_artifact: convention\n" +
				fmt.Sprintf("format_version: %v\n", artifact.FormatVersion) +
				"title: Carry Probe\n" +
				"status: active\n" +
				"trigger: always\n" +
				frontmatter +
				"---\n\nbody\n")

			rr := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/import-artifact", data)
			if rr.Code != http.StatusCreated {
				t.Fatalf("import: expected 201, got %d: %s", rr.Code, rr.Body.String())
			}
			var envelope struct {
				Slug     string   `json:"slug"`
				Warnings []string `json:"warnings"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode response: %v\nbody: %s", err, rr.Body.String())
			}
			stored, err := srv.store.GetItemBySlug(ws.ID, envelope.Slug)
			if err != nil || stored == nil {
				t.Fatalf("GetItemBySlug(%s): %v", envelope.Slug, err)
			}
			var blob map[string]any
			if err := json.Unmarshal([]byte(stored.Fields), &blob); err != nil {
				t.Fatalf("decode stored fields: %v", err)
			}

			got, present := blob["role"]
			switch tc.wantKind {
			case droppedKey:
				if present {
					t.Errorf("an injected default that resolves to nothing must be DISCARDED, not stored; blob holds role=%#v", got)
				}
			default:
				if !present {
					t.Errorf("the carrying door DROPPED %q. An import is often the only copy of the intent, so a value it cannot resolve is kept and reported, never discarded (BUG-3015). Stored blob: %s", role, stored.Fields)
				} else if gotStr, _ := got.(string); gotStr != want {
					t.Errorf("stored role = %q, want %q", gotStr, want)
				}
			}

			mentions := warningsMentioning(envelope.Warnings, "role")
			if tc.warning == "" {
				if len(mentions) > 0 {
					t.Errorf("a value that resolves must produce no relation warning, got %#v", mentions)
				}
				return
			}
			if len(mentions) == 0 {
				t.Fatalf("nothing in the response named %q. Silent is the failure mode BUG-3015 filed: the caller believes the value landed as written. Warnings were %#v", "role", envelope.Warnings)
			}
			if !strings.Contains(strings.Join(mentions, "\n"), tc.warning) {
				t.Errorf("warning for %q = %#v, want one containing %q", "role", mentions, tc.warning)
			}
		})
	}
}

// relationField builds the conventions collection's `role` field retyped as a
// relation pointing at the Secrets collection, with extra keys merged over it.
func relationField(extra map[string]any) map[string]any {
	f := map[string]any{"key": "role", "label": "Role", "type": "relation", "collection": "secrets"}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

// retypeConventionRole replaces the conventions collection's DECLARED `role`
// field with def. `role` is chosen because it is on conventionFieldKeys: a key
// the artifact format carries, so the value survives decode and reaches the
// relation code. Retyping an undeclared key would prove nothing — the decode
// allow-list drops it first.
func retypeConventionRole(t *testing.T, srv *Server, workspaceID string, def map[string]any) {
	t.Helper()
	colls, err := srv.store.ListCollections(workspaceID)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	var conv *models.Collection
	for i := range colls {
		if colls[i].Slug == "conventions" {
			conv = &colls[i]
		}
	}
	if conv == nil {
		t.Fatalf("no conventions collection in the seeded workspace (have %d)", len(colls))
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(conv.Schema), &schema); err != nil {
		t.Fatalf("decode conventions schema: %v", err)
	}
	fields, _ := schema["fields"].([]any)
	replaced := false
	out := make([]any, 0, len(fields))
	for _, f := range fields {
		if m, _ := f.(map[string]any); m != nil && m["key"] == "role" {
			out = append(out, def)
			replaced = true
			continue
		}
		out = append(out, f)
	}
	if !replaced {
		t.Fatalf("the seeded conventions schema no longer declares `role`, so this test's reachability argument is stale: %s", conv.Schema)
	}
	schema["fields"] = out
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("encode schema: %v", err)
	}
	str := string(encoded)
	if _, err := srv.store.UpdateCollection(conv.ID, models.CollectionUpdate{Schema: &str}); err != nil {
		t.Fatalf("UpdateCollection: %v", err)
	}
	// The retype must have taken, or every assertion below passes because
	// `role` is an ordinary text field that no relation code ever looks at.
	reread, err := srv.store.GetCollection(conv.ID)
	if err != nil {
		t.Fatalf("re-read conventions: %v", err)
	}
	wantType, _ := def["type"].(string)
	if !strings.Contains(reread.Schema, `"type":"`+wantType+`"`) {
		t.Fatalf("the schema retype did not take, so this test would prove nothing: %s", reread.Schema)
	}
}

// createDecoyInAnotherCollection makes one item OUTSIDE the Secrets collection,
// so a test can name a live item that a relation declaring `secrets` must not
// accept.
func createDecoyInAnotherCollection(t *testing.T, srv *Server, workspaceID string) *models.Item {
	t.Helper()
	colls, err := srv.store.ListCollections(workspaceID)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	for i := range colls {
		if colls[i].Slug != "tasks" {
			continue
		}
		item, err := srv.store.CreateItem(workspaceID, colls[i].ID, models.ItemCreate{Title: "Decoy Task"})
		if err != nil {
			t.Fatalf("CreateItem(decoy): %v", err)
		}
		return item
	}
	t.Fatalf("no tasks collection in the seeded workspace")
	return nil
}

// warningsMentioning returns the warnings naming key. The import door renders
// every warnings member into one flat []string, so a test asserting on the
// whole array cannot tell which member spoke; filtering by key is as narrow as
// this surface allows.
func warningsMentioning(warnings []string, key string) []string {
	var out []string
	for _, w := range warnings {
		if strings.Contains(w, `"`+key+`"`) {
			out = append(out, w)
		}
	}
	return out
}
