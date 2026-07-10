package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// errorServer returns an httptest.Server that answers every request with the
// given status + API error envelope ({"error":{"code","message"}}), matching
// the server's writeError shape. Lets us exercise the CLI's error-wrapping
// without a live backend.
func errorServer(t *testing.T, status int, code, message string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": code, "message": message},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGetItemWrapsNotFound verifies TASK-2031b: a bare "not_found" APIError
// from the item-by-ref endpoint is rewritten to echo the failing ref and
// workspace, so the CLI prints something actionable instead of "Item not
// found".
func TestGetItemWrapsNotFound(t *testing.T) {
	srv := errorServer(t, http.StatusNotFound, "not_found", "Item not found")
	client := NewClientFromURL(srv.URL)

	want := "item TASK-999999 not found in workspace docapp"

	if _, err := client.GetItem("docapp", "TASK-999999"); err == nil || err.Error() != want {
		t.Errorf("GetItem: got %v, want %q", err, want)
	} else {
		// The enriched error must stay a *concrete* *APIError so BOTH
		// errors.As and — critically — direct `err.(*APIError)` assertions
		// keep matching. bulk-update's per-row code capture (cmd_item.go)
		// uses a non-unwrapping direct assertion; a wrapper type would
		// silently drop the not_found code there (team-lead P2 review).
		apiErr, ok := err.(*APIError)
		if !ok {
			t.Fatalf("GetItem error must be a concrete *APIError (direct assertion), got %T", err)
		}
		if apiErr.Code != "not_found" {
			t.Errorf("GetItem *APIError.Code = %q, want %q", apiErr.Code, "not_found")
		}
		// errors.As must also still reach it.
		var viaAs *APIError
		if !errors.As(err, &viaAs) || viaAs.Code != "not_found" {
			t.Errorf("errors.As should reach *APIError{Code:not_found}, got %#v", err)
		}
	}
	if _, err := client.UpdateItem("docapp", "TASK-999999", models.ItemUpdate{}); err == nil || err.Error() != want {
		t.Errorf("UpdateItem: got %v, want %q", err, want)
	}
	if err := client.DeleteItem("docapp", "TASK-999999"); err == nil || err.Error() != want {
		t.Errorf("DeleteItem: got %v, want %q", err, want)
	}
}

// TestNotFoundPreservesDetails confirms the enriched *APIError carries the
// server's original Details verbatim, so structured consumers (JSON envelopes,
// MCP agents) lose nothing when the message is rewritten.
func TestNotFoundPreservesDetails(t *testing.T) {
	rawDetails := `{"hint":"check the ref"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"Item not found","details":` + rawDetails + `}}`))
	}))
	t.Cleanup(srv.Close)
	client := NewClientFromURL(srv.URL)

	_, err := client.GetItem("docapp", "TASK-999999")
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected concrete *APIError, got %T", err)
	}
	if apiErr.Code != "not_found" {
		t.Errorf("Code = %q, want not_found", apiErr.Code)
	}
	if got := string(apiErr.Details); got != rawDetails {
		t.Errorf("Details = %q, want %q (should pass through verbatim)", got, rawDetails)
	}
}

// TestGetItemPassesThroughOtherErrors makes sure the wrapper only special-cases
// "not_found": any other API error (e.g. a 500) reaches the caller unchanged so
// we don't mislabel a server fault as a missing item.
func TestGetItemPassesThroughOtherErrors(t *testing.T) {
	srv := errorServer(t, http.StatusInternalServerError, "internal", "boom")
	client := NewClientFromURL(srv.URL)

	_, err := client.GetItem("docapp", "TASK-1")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if strings.Contains(err.Error(), "not found in workspace") {
		t.Errorf("non-not_found error should pass through unchanged, got %q", err.Error())
	}
	if err.Error() != "boom" {
		t.Errorf("expected original message %q, got %q", "boom", err.Error())
	}
}
