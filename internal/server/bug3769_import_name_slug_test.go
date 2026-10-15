package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3769, through the HTTP import door: ?name= with a space answered 201
// with that raw name as the slug, and the next request to the workspace's
// own export URL could not even be built.
func TestBUG3769_ImportNameOverrideGetsAUsableSlug(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Source 3769"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var src models.Workspace
	if err := json.Unmarshal(rr.Body.Bytes(), &src); err != nil {
		t.Fatal(err)
	}
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+src.Slug+"/export", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rr.Code, rr.Body.String())
	}

	rr = rawJSONRequest(srv, "POST", "/api/v1/workspaces/import?name=Interchange%20Golden", rr.Body.String())
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	if err := json.Unmarshal(rr.Body.Bytes(), &ws); err != nil {
		t.Fatal(err)
	}
	if ws.Slug != "interchange-golden" || ws.Name != "Interchange Golden" {
		t.Fatalf("import = slug %q name %q, want interchange-golden / Interchange Golden", ws.Slug, ws.Name)
	}
	if rr = doRequest(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/export", nil); rr.Code != http.StatusOK {
		t.Errorf("export of the imported workspace by its slug: %d %s", rr.Code, rr.Body.String())
	}
}
