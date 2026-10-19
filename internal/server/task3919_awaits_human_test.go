package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3919 (IDEA-3906): awaits_human through the HTTP door: the typed
// member on create and update, the settings key kept by a write that omits
// it, and a non-boolean refused on create and update.
func TestTASK3919_AwaitsHumanOverHTTP(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSForTest(t, srv)
	base := "/api/v1/workspaces/" + slug + "/collections"

	rr := doRequest(srv, "POST", base, map[string]any{"name": "Asks", "awaits_human": true})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)
	if !models.CollectionAwaitsHumanJSON(coll.Settings) {
		t.Fatalf("the create member was not applied: %s", coll.Settings)
	}

	rr = doRequest(srv, "PATCH", base+"/"+coll.Slug, map[string]any{"settings": map[string]any{"layout": "balanced"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch settings: %d %s", rr.Code, rr.Body.String())
	}
	parseJSON(t, rr, &coll)
	if !models.CollectionAwaitsHumanJSON(coll.Settings) {
		t.Fatalf("a settings PATCH without the key reset it: %s", coll.Settings)
	}

	rr = doRequest(srv, "PATCH", base+"/"+coll.Slug, map[string]any{"awaits_human": false})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch member: %d %s", rr.Code, rr.Body.String())
	}
	parseJSON(t, rr, &coll)
	if models.CollectionAwaitsHumanJSON(coll.Settings) {
		t.Fatalf("the update member was not applied: %s", coll.Settings)
	}

	for _, c := range []struct{ method, path string }{{"POST", base}, {"PATCH", base + "/" + coll.Slug}} {
		body := map[string]any{"name": "Bad", "settings": map[string]any{"awaits_human": "yes"}}
		if rr := doRequest(srv, c.method, c.path, body); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s with a string awaits_human: %d %s", c.method, rr.Code, rr.Body.String())
		}
	}
}
