package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4643: the collection create and update doors refuse awaits_human_options
// that name a value outside the field's options or one of its terminal
// options, with 400 validation_error, as they refuse invalid abandoned_options.
// A valid declaration is stored and read back.
func TestAwaitsHumanOptionsValidation(t *testing.T) {
	t.Parallel()
	schemaWith := func(awaits string) string {
		return `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","needs-review","done"],"terminal_options":["done"],"awaits_human_options":` + awaits + `}]}`
	}
	const clean = `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","needs-review","done"],"terminal_options":["done"]}]}`
	for _, door := range []string{"create", "update"} {
		for _, tc := range []struct {
			name, awaits, wantInMessage string
		}{
			{"valid subset", `["Needs-Review"]`, ""},
			{"not an option", `["waiting"]`, `awaits_human_options value "waiting" is not one of its options`},
			{"a terminal option", `["needs-review","done"]`, `awaits_human_options value "done" is one of its terminal_options`},
			{"on a non-done field", "", `awaits_human_options value "later" is not one of its options`},
		} {
			t.Run(door+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				srv := testServer(t)
				ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Awaits " + door})
				if err != nil {
					t.Fatal(err)
				}
				schema := schemaWith(tc.awaits)
				if tc.awaits == "" {
					// No placement rule: a second select field is validated the same way.
					schema = `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"terminal_options":["done"]},{"key":"review","label":"Review","type":"select","options":["asked","given"],"awaits_human_options":["later"]}]}`
				}
				base := "/api/v1/workspaces/" + ws.Slug + "/collections"
				method, path, body, okCode := "POST", base, `{"name":"Things","schema":`+strconv.Quote(schema)+`}`, http.StatusCreated
				if door == "update" {
					if rr := rawJSONRequest(srv, "POST", base, `{"name":"Things","schema":`+strconv.Quote(clean)+`}`); rr.Code != http.StatusCreated {
						t.Fatalf("premise: clean create answered %d %s", rr.Code, rr.Body.String())
					}
					method, path, body, okCode = "PATCH", base+"/things", `{"schema":`+strconv.Quote(schema)+`}`, http.StatusOK
				}
				rr := rawJSONRequest(srv, method, path, body)
				if tc.wantInMessage == "" {
					if rr.Code != okCode {
						t.Fatalf("%s: %d %s, want %d", door, rr.Code, rr.Body.String(), okCode)
					}
					coll, err := srv.store.GetCollectionBySlug(ws.ID, "things")
					if err != nil || coll == nil {
						t.Fatalf("read back: %v", err)
					}
					var stored models.CollectionSchema
					if err := json.Unmarshal([]byte(coll.Schema), &stored); err != nil {
						t.Fatal(err)
					}
					if got := stored.Fields[0].AwaitsHumanOptions; len(got) != 1 || got[0] != "Needs-Review" {
						t.Fatalf("stored awaits_human_options = %v, want [Needs-Review]", got)
					}
					return
				}
				if rr.Code != http.StatusBadRequest {
					t.Fatalf("%s: %d %s, want 400", door, rr.Code, rr.Body.String())
				}
				if !strings.Contains(rr.Body.String(), "validation_error") || !strings.Contains(rr.Body.String(), strings.ReplaceAll(tc.wantInMessage, `"`, `\"`)) {
					t.Errorf("%s: body %s, want validation_error naming %s", door, rr.Body.String(), tc.wantInMessage)
				}
			})
		}
	}
}
