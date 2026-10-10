package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-3535: tracks_work through the HTTP door: the typed member, the
// settings key, and a non-boolean refused on create and update.
func TestPLAN3535_TracksWorkOverHTTP(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSForTest(t, srv)
	base := "/api/v1/workspaces/" + slug + "/collections"

	rr := doRequest(srv, "POST", base, map[string]any{"name": "Refs", "tracks_work": false})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)
	if models.CollectionTracksWorkJSON(coll.Settings) {
		t.Fatalf("the create member was not applied: %s", coll.Settings)
	}

	// A settings write without the key keeps reference.
	rr = doRequest(srv, "PATCH", base+"/"+coll.Slug, map[string]any{"settings": map[string]any{"layout": "balanced"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch settings: %d %s", rr.Code, rr.Body.String())
	}
	parseJSON(t, rr, &coll)
	if models.CollectionTracksWorkJSON(coll.Settings) {
		t.Fatalf("a settings PATCH without the key reset it: %s", coll.Settings)
	}

	// The member alone flips it.
	rr = doRequest(srv, "PATCH", base+"/"+coll.Slug, map[string]any{"tracks_work": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch member: %d %s", rr.Code, rr.Body.String())
	}
	parseJSON(t, rr, &coll)
	if !models.CollectionTracksWorkJSON(coll.Settings) {
		t.Fatalf("the update member was not applied: %s", coll.Settings)
	}

	// A non-boolean is refused, on create and update, and nothing changes.
	for _, c := range []struct{ method, path string }{{"POST", base}, {"PATCH", base + "/" + coll.Slug}} {
		body := map[string]any{"name": "Bad", "settings": map[string]any{"tracks_work": "no"}}
		if rr := doRequest(srv, c.method, c.path, body); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s with a string tracks_work: %d %s", c.method, rr.Code, rr.Body.String())
		}
	}
}
