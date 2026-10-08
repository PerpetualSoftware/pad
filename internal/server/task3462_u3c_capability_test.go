package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TASK-3462 U3c: the CLI's built-in commands need to tell an older server
// (no route, a bare 404) from an item made from no built-in (404
// not_builtin), so this build says it serves the built-in state and update.
func TestTASK3462U3c_CapabilityAdvertisesBuiltinUpdate(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, http.MethodGet, "/api/v1/server/capabilities", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("capabilities: status %d", rr.Code)
	}
	var caps map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &caps); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if caps["builtin_update"] != true {
		t.Fatalf("builtin_update = %v, want true", caps["builtin_update"])
	}
}
