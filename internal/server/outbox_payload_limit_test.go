package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// TestWriteInternalErrorMapsTheOutboxRowCapRefusal pins the HTTP half of
// BUG-2827.
//
// It drives writeInternalError directly rather than through a handler because
// reaching the real refusal takes a payload over 128 MiB, and a test that
// allocates that to observe a status code is measuring the wrong thing. What
// has to be true here is the CLASSIFICATION — a stated refusal is not a 500 —
// and that survives the substitution. The store-side tests own the question of
// whether the refusal fires at the right size.
func TestWriteInternalErrorMapsTheOutboxRowCapRefusal(t *testing.T) {
	rec := httptest.NewRecorder()
	writeInternalError(rec, fmt.Errorf("update item: %w", &store.OversizedOutboxPayloadError{
		EventType: "item.bulk_updated",
		Bytes:     200 << 20,
		Limit:     store.MaxOutboxPayloadBytes,
	}))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d — a refusal under a stated rule must not surface as a server "+
			"fault the caller cannot act on", rec.Code, http.StatusRequestEntityTooLarge)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	if body.Error.Code != "event_payload_too_large" {
		t.Errorf("code = %q, want event_payload_too_large", body.Error.Code)
	}
	// The WRAPPER must not leak. The message is composed from the typed
	// fields, so "update item:" — added by a caller on the way up — has no
	// business in a client-facing string.
	if strings.Contains(body.Error.Message, "update item") {
		t.Errorf("message leaks the call path's wrapper: %q", body.Error.Message)
	}
	if !strings.Contains(body.Error.Message, "item.bulk_updated") {
		t.Errorf("message does not name the event that was refused: %q", body.Error.Message)
	}
	if !strings.Contains(body.Error.Message, fmt.Sprint(store.MaxOutboxPayloadBytes)) {
		t.Errorf("message does not state the limit: %q", body.Error.Message)
	}
}

// TestWriteInternalErrorStillFallsThroughToFiveHundred is the negative control:
// without it the test above passes just as well against a writeInternalError
// that answers 413 to everything.
func TestWriteInternalErrorStillFallsThroughToFiveHundred(t *testing.T) {
	rec := httptest.NewRecorder()
	writeInternalError(rec, fmt.Errorf("some unrelated failure"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
