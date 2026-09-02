package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		Measured:  "the row as the database will hand it back",
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
	// The store refuses on two different measurements against two different
	// limits, so the message has to say which one this was or a caller cannot
	// reconcile two numbers for one mutation.
	if !strings.Contains(body.Error.Message, "the row as the database will hand it back") {
		t.Errorf("message does not say what was measured: %q", body.Error.Message)
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

// TestOversizedOutboxScanIsThrottled pins the diagnostic's cadence.
//
// The query it gates is expensive exactly when it finds nothing — a
// non-sargable size predicate over every pending row, detoasting each JSONB
// payload on Postgres — so running it per 5s tick would make the drain's
// cheapest-value work its most expensive. Asserting "true then false" is the
// whole contract: the first tick after a restart reports promptly, and the
// interval keeps it off the hot path afterwards.
func TestOversizedOutboxScanIsThrottled(t *testing.T) {
	s := &Server{}
	if !s.shouldScanOversizedOutbox() {
		t.Fatal("the first call refused to scan; a restart must report an existing oversized row " +
			"without waiting out an interval first")
	}
	if s.shouldScanOversizedOutbox() {
		t.Fatal("a second call immediately after the first scanned again; the throttle is not applied")
	}

	// And it recovers once the interval has passed, or the diagnostic reports
	// once per process and never again.
	s.outboxDrain.lastOversizedScan = time.Now().Add(-2 * oversizedOutboxScanInterval)
	if !s.shouldScanOversizedOutbox() {
		t.Fatal("the scan never resumed after the interval elapsed")
	}
}

// TestOutboxDrainTickConsultsTheThrottle covers the CALL SITE, which the test
// above does not.
//
// Found by mutation: replacing `if s.shouldScanOversizedOutbox()` with `if
// true` left the throttle test green, because that test drives the helper
// directly and a helper nothing calls is still correct in isolation. The
// observable that reaches the call site is the stamp — a tick that consulted
// the throttle has set it, and one that skipped straight to the query has not.
func TestOutboxDrainTickConsultsTheThrottle(t *testing.T) {
	srv, _, _ := drainFixture(t)

	if !srv.outboxDrain.lastOversizedScan.IsZero() {
		t.Fatal("fixture started with the throttle already stamped")
	}
	srv.runOutboxDrainTick()
	first := srv.outboxDrain.lastOversizedScan
	if first.IsZero() {
		t.Fatal("a drain tick left the throttle unstamped; the diagnostic is running unthrottled")
	}

	srv.runOutboxDrainTick()
	if got := srv.outboxDrain.lastOversizedScan; !got.Equal(first) {
		t.Fatalf("a second tick restamped the throttle (%v -> %v); the interval is not being honoured",
			first, got)
	}
}
