package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Retro CONVE-735 finding on #2065: create checked the workspace item limit
// before claiming its idempotency key, so a create that succeeded AT the
// limit and lost its answer could not replay: the retry met the limit its
// own success had reached. The claim now comes first. The control: a NEW
// create at the cap is still refused.
func TestTASK3585_CreateAtTheLimitReplays(t *testing.T) {
	e := newRestoreLimitEnv(t)
	e.setCap(t, e.liveItems(t)+1)
	path := "/api/v1/workspaces/" + e.home.Slug + "/collections/tasks/items"
	body, _ := json.Marshal(map[string]any{"title": "The last one", "fields": `{"status":"open"}`, "op_key": "at-cap"})

	first := e.do("POST", path, "application/json", body, "")
	if first.Code != http.StatusCreated {
		t.Fatalf("the create that reaches the cap: %d %s", first.Code, first.Body.String())
	}
	again := e.do("POST", path, "application/json", body, "")
	if again.Code != http.StatusCreated || again.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("its retry must replay, not meet the cap: %d %s", again.Code, again.Body.String())
	}

	other, _ := json.Marshal(map[string]any{"title": "One too many", "fields": `{"status":"open"}`, "op_key": "over-cap"})
	if rr := e.do("POST", path, "application/json", other, ""); rr.Code == http.StatusCreated {
		t.Fatalf("a new create at the cap must still be refused: %d", rr.Code)
	}
}
