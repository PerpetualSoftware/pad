package server

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3539 PR B: a collection create or update whose schema repeats a field
// key, has a field with no key, or repeats or empties a select option is
// refused with 400 validation_error naming what to fix. Every client renders
// these as keyed lists, and a repeated key blanks the view (BUG-3538). It is a
// write check: a schema already stored with a repeat still reads, and an
// update that does not send a schema still applies.

var task3539BadSchemas = []struct {
	name, schema, wantInMessage string
}{
	{"repeated field key", `{"fields":[{"key":"status","label":"Status","type":"text"},{"key":"status","label":"Again","type":"text"}]}`, `field key "status" is defined more than once`},
	{"field with no key", `{"fields":[{"key":"","label":"Nameless","type":"text"}]}`, `field 1 (label "Nameless") has no key`},
	{"repeated option", `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done","open"]}]}`, `field "status" lists the option "open" more than once`},
	{"empty option", `{"fields":[{"key":"status","label":"Status","type":"select","options":["open",""]}]}`, `field "status" has an empty option`},
}

const task3539CleanSchema = `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"]}]}`

func task3539Workspace(t *testing.T, srv *Server, name string) *models.Workspace {
	t.Helper()
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestTASK3539_SchemaWritesRefuseRepeatsAndBlanks(t *testing.T) {
	for _, door := range []string{"create", "update"} {
		for _, tc := range task3539BadSchemas {
			t.Run(door+"/"+tc.name, func(t *testing.T) {
				srv := testServer(t)
				ws := task3539Workspace(t, srv, "Schemas "+door)
				base := "/api/v1/workspaces/" + ws.Slug + "/collections"
				method, path, body := "POST", base, `{"name":"Things","schema":`+strconv.Quote(tc.schema)+`}`
				if door == "update" {
					if rr := rawJSONRequest(srv, "POST", base, `{"name":"Things","schema":`+strconv.Quote(task3539CleanSchema)+`}`); rr.Code != http.StatusCreated {
						t.Fatalf("premise: clean create answered %d %s", rr.Code, rr.Body.String())
					}
					method, path, body = "PATCH", base+"/things", `{"schema":`+strconv.Quote(tc.schema)+`}`
				}
				rr := rawJSONRequest(srv, method, path, body)
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

// Control: the same doors accept a clean schema, so the refusals above are
// about the repeats and not about the request shape.
func TestTASK3539_CleanSchemaIsAccepted(t *testing.T) {
	srv := testServer(t)
	ws := task3539Workspace(t, srv, "Clean")
	base := "/api/v1/workspaces/" + ws.Slug + "/collections"
	if rr := rawJSONRequest(srv, "POST", base, `{"name":"Things","schema":`+strconv.Quote(task3539CleanSchema)+`}`); rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	if rr := rawJSONRequest(srv, "PATCH", base+"/things", `{"schema":`+strconv.Quote(task3539CleanSchema)+`}`); rr.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rr.Code, rr.Body.String())
	}
}

// A schema stored with a repeat before this check keeps working: it reads,
// and an update that sends no schema applies. Sending it back is refused,
// naming the repeat, which is how its owner learns what to fix.
func TestTASK3539_StoredRepeatStillReadsAndIsNamedOnRewrite(t *testing.T) {
	srv := testServer(t)
	ws := task3539Workspace(t, srv, "Legacy")
	broken := task3539BadSchemas[0].schema
	if _, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Old", Slug: "old", Prefix: "OLD", Schema: broken}); err != nil {
		t.Fatalf("store create: %v", err)
	}
	base := "/api/v1/workspaces/" + ws.Slug + "/collections"
	if rr := doRequest(srv, "GET", base+"/old", nil); rr.Code != http.StatusOK {
		t.Fatalf("read: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := srv.store.CreateItem(ws.ID, mustCollectionID(t, srv, ws.ID, "old"), models.ItemCreate{Title: "Legacy item", Fields: `{}`}); err != nil {
		t.Fatalf("store create item: %v", err)
	}
	if rr := doRequest(srv, "GET", base+"/old/items", nil); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Legacy item") {
		t.Fatalf("item list read: %d %s", rr.Code, rr.Body.String())
	}
	if rr := rawJSONRequest(srv, "POST", base+"/old/items", `{"title":"Written today"}`); rr.Code != http.StatusCreated {
		t.Fatalf("item create in the legacy collection: %d %s", rr.Code, rr.Body.String())
	}
	if rr := rawJSONRequest(srv, "PATCH", base+"/old", `{"description":"still works"}`); rr.Code != http.StatusOK {
		t.Fatalf("update without a schema: %d %s", rr.Code, rr.Body.String())
	}
	rr := rawJSONRequest(srv, "PATCH", base+"/old", `{"schema":`+strconv.Quote(broken)+`}`)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), `field key \"status\" is defined more than once`) {
		t.Fatalf("resending the stored repeat: %d %s, want 400 naming it", rr.Code, rr.Body.String())
	}
}

// TASK-3539: Dynamic Client Registration is unauthenticated and stored the
// redirect_uris array as sent, so a repeated URI blanked the connected-apps
// page (keyed by URI) for every user of that client. It is registered once.
func TestTASK3539_DCRRegistersARepeatedRedirectURIOnce(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	rr := doRequest(srv, "POST", "/oauth/register", map[string]any{
		"client_name":   "Repeater",
		"redirect_uris": []string{"https://app.test/cb", "https://app.test/cb", "claude://oauth/callback"},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ClientID     string   `json:"client_id"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	parseJSON(t, rr, &resp)
	want := []string{"https://app.test/cb", "claude://oauth/callback"}
	if strings.Join(resp.RedirectURIs, " ") != strings.Join(want, " ") {
		t.Errorf("response redirect_uris = %q, want %q", resp.RedirectURIs, want)
	}
	client, err := srv.store.GetOAuthClient(resp.ClientID)
	if err != nil || client == nil {
		t.Fatalf("stored client: %v", err)
	}
	if strings.Join(client.RedirectURIs, " ") != strings.Join(want, " ") {
		t.Errorf("stored redirect_uris = %q, want %q", client.RedirectURIs, want)
	}
}
