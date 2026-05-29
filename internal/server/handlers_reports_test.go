package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// Exercises the full report HTTP path as the workspace owner (admin/all-access
// → visibleCollectionIDs returns nil, so the report is unscoped). Store-level
// tests cover the visibility-scoping mechanics; this locks the route, the
// nil-visibility branch, and the JSON shape.
func TestReportEndpoint_OwnerGetsFullReport(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "A"})
	createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "B"})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/report?window=week", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("report: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp store.ReportData
	parseJSON(t, rr, &resp)

	if resp.Window != "week" || resp.Granularity != "day" {
		t.Fatalf("expected week/day, got %s/%s", resp.Window, resp.Granularity)
	}
	if resp.Totals.Created != 2 {
		t.Fatalf("expected 2 created, got %d", resp.Totals.Created)
	}

	// Arrays must serialize as [] not null.
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &rawMap); err != nil {
		t.Fatalf("parse raw: %v", err)
	}
	for _, key := range []string{"buckets", "collections", "completed_by_collection", "status_distribution"} {
		if string(rawMap[key]) == "null" {
			t.Errorf("expected %s to be [], got null", key)
		}
	}
}

func TestReportEndpoint_DefaultsToWeek(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/report", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("report: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp store.ReportData
	parseJSON(t, rr, &resp)
	if resp.Window != "week" {
		t.Fatalf("expected default window=week, got %q", resp.Window)
	}
}
