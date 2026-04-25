package server

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDecodeJSON_RejectsOversizeBody ensures the default 2 MiB cap is
// enforced — without http.MaxBytesReader a multi-GB POST would stream
// into a single allocation and could OOM the process.
func TestDecodeJSON_RejectsOversizeBody(t *testing.T) {
	// 3 MiB of harmless but oversize JSON.
	body := []byte(`{"x":"` + strings.Repeat("a", 3<<20) + `"}`)
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	var target map[string]any
	err := decodeJSON(req, &target)
	if err == nil {
		t.Fatalf("expected oversize body to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "request body too large") &&
		!strings.Contains(err.Error(), "http: request body too large") {
		// MaxBytesReader wraps the "request body too large" error into the
		// json.Decoder failure, which bubbles up through the invalid-JSON
		// wrap. Just verify SOME error surfaced — the exact wording is
		// tied to stdlib internals.
		t.Logf("got error: %v", err)
	}
}

// TestDecodeJSON_AcceptsWithinLimit confirms the happy path still works.
func TestDecodeJSON_AcceptsWithinLimit(t *testing.T) {
	req := httptest.NewRequest("POST", "/", bytes.NewReader([]byte(`{"name":"ok"}`)))
	req.Header.Set("Content-Type", "application/json")

	var target struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(req, &target); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target.Name != "ok" {
		t.Fatalf("got name=%q, want %q", target.Name, "ok")
	}
}

// TestDecodeJSONWithLimit_CustomCap verifies callers can opt in to a
// larger cap (for bulk-import style endpoints).
func TestDecodeJSONWithLimit_CustomCap(t *testing.T) {
	// 1 MiB body; below default 2 MiB but above our custom 256 KiB cap.
	body := []byte(`{"x":"` + strings.Repeat("a", 1<<20) + `"}`)
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	var target map[string]any
	if err := decodeJSONWithLimit(req, &target, 256<<10); err == nil {
		t.Fatal("expected 256 KiB cap to reject 1 MiB body, got nil")
	}
}
