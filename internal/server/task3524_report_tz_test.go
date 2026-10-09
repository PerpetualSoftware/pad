package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TASK-3524: the report's `tz` parameter.

func TestReportTZ_EchoedWhenNamedAbsentOtherwise(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/report?window=week&tz=America/Los_Angeles", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("with tz: %d %s", rr.Code, rr.Body.String())
	}
	var withTZ map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &withTZ)
	if withTZ["tz"] != "America/Los_Angeles" {
		t.Fatalf("tz echo = %v", withTZ["tz"])
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/report?window=week", nil)
	var without map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &without)
	if _, has := without["tz"]; has {
		t.Fatalf("a request without tz must not carry one: %v", without["tz"])
	}
}

func TestReportTZ_RefusesAnUnknownZoneAndLocal(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	for _, tz := range []string{"Mars/Olympus", "Local", "%2E%2E%2Fetc"} {
		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/report?tz="+tz, nil)
		if rr.Code != http.StatusBadRequest || errorCode(t, rr) != "invalid_tz" {
			t.Fatalf("tz=%s: %d %s, want 400 invalid_tz", tz, rr.Code, rr.Body.String())
		}
	}
}
