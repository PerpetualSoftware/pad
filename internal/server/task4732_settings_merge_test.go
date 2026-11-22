package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TASK-4732: a PATCH's settings are merged onto the stored settings, so a
// writer that sends a subset of keys keeps a key it does not know about, and
// a null tracks_work is still refused.
func TestTASK4732_PatchCollectionSettingsMerge(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	path := "/api/v1/workspaces/" + slug + "/collections/tasks"

	settings := func() map[string]any {
		t.Helper()
		rr := doRequest(srv, "GET", path, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("get: %d %s", rr.Code, rr.Body.String())
		}
		var c struct {
			Settings json.RawMessage `json:"settings"`
		}
		parseJSON(t, rr, &c)
		raw := c.Settings
		var str string
		if json.Unmarshal(raw, &str) == nil {
			raw = json.RawMessage(str)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("settings do not parse: %s (%v)", c.Settings, err)
		}
		return m
	}

	rr := doRequest(srv, "PATCH", path, map[string]any{
		"settings": map[string]any{"layout": "balanced", "clear_checked_after": "7d"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("seed patch: %d %s", rr.Code, rr.Body.String())
	}
	before := settings()

	rr = doRequest(srv, "PATCH", path, map[string]any{
		"settings": map[string]any{"layout": "fields-primary", "default_view": "board", "board_group_by": "status"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("partial patch: %d %s", rr.Code, rr.Body.String())
	}
	after := settings()
	if after["clear_checked_after"] != "7d" {
		t.Errorf("a partial settings PATCH dropped clear_checked_after: %v", after)
	}
	if after["layout"] != "fields-primary" || after["board_group_by"] != "status" {
		t.Errorf("the supplied keys did not land: %v", after)
	}
	if after["tracks_work"] != before["tracks_work"] {
		t.Errorf("tracks_work moved: %v -> %v", before["tracks_work"], after["tracks_work"])
	}

	// null clears a key.
	rr = doRequest(srv, "PATCH", path, map[string]any{
		"settings": map[string]any{"board_group_by": nil},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("null patch: %d %s", rr.Code, rr.Body.String())
	}
	if got := settings(); got["clear_checked_after"] != "7d" {
		t.Errorf("null patch dropped another key: %v", got)
	} else if _, ok := got["board_group_by"]; ok {
		t.Errorf("null patch: %v", got)
	}

	// tracks_work refuses null.
	for _, key := range []string{"tracks_work", "awaits_human"} {
		rr = doRequest(srv, "PATCH", path, map[string]any{
			"settings": map[string]any{key: nil},
		})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: null answered %d, want 400: %s", key, rr.Code, rr.Body.String())
		}
	}
	if got := settings(); got["tracks_work"] != before["tracks_work"] {
		t.Errorf("a refused null moved tracks_work: %v", got)
	}
}
