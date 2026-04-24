package store

import (
	"testing"
	"time"
)

// TestUnmarkStripeEventProcessed_RoundTrip exercises the mark → unmark → re-mark
// cycle that the sidecar relies on for TASK-736. After an unmark, the SAME
// event ID must behave as brand-new on the next mark call so Stripe
// retries can re-run the handler. A bug that made unmark a no-op (e.g.
// wrong WHERE clause) would break this invariant.
func TestUnmarkStripeEventProcessed_RoundTrip(t *testing.T) {
	s := testStore(t)

	// Mark a fresh event.
	already, tok1, err := s.MarkStripeEventProcessed("evt_roundtrip")
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if already {
		t.Fatal("first mark must return already_processed=false")
	}
	if tok1 == "" {
		t.Fatal("first mark must return a non-empty processed_at token")
	}

	// Duplicate mark — must be detected. Token must match the original.
	already, tok2, err := s.MarkStripeEventProcessed("evt_roundtrip")
	if err != nil {
		t.Fatalf("re-mark before unmark: %v", err)
	}
	if !already {
		t.Fatal("second mark (no unmark yet) must detect duplicate")
	}
	if tok2 != tok1 {
		t.Errorf("duplicate mark must return the existing row's processed_at (%q), got %q", tok1, tok2)
	}

	// Unmark with the correct token — row deleted.
	unmarked, err := s.UnmarkStripeEventProcessed("evt_roundtrip", tok1)
	if err != nil {
		t.Fatalf("unmark: %v", err)
	}
	if !unmarked {
		t.Fatal("unmark of existing row (matching token) must return unmarked=true")
	}

	// Re-mark — row is gone, must be treated as brand-new so the handler
	// can run again. This is the whole point of the endpoint.
	already, _, err = s.MarkStripeEventProcessed("evt_roundtrip")
	if err != nil {
		t.Fatalf("re-mark after unmark: %v", err)
	}
	if already {
		t.Fatal("mark after unmark must return already_processed=false (retry path broken)")
	}
}

// TestUnmarkStripeEventProcessed_StaleTokenIsNoOp is the race-protection
// test that addresses Codex's HIGH finding. Scenario: an earlier handler
// failure queued an unmark, but before it fired, Stripe retried and a
// SUCCESSFUL retry re-marked the same event (fresh processed_at). If the
// stale unmark proceeded, it would wipe the fresh marker and reopen the
// retry window, causing the handler to double-apply.
//
// The (event_id, processed_at) composite delete key prevents this: the
// stale token doesn't match the fresh row, so the delete silently affects
// zero rows and the fresh marker stays put.
func TestUnmarkStripeEventProcessed_StaleTokenIsNoOp(t *testing.T) {
	s := testStore(t)

	// First attempt — record a token.
	_, staleTok, err := s.MarkStripeEventProcessed("evt_race")
	if err != nil {
		t.Fatalf("first mark: %v", err)
	}

	// Simulate the sidecar successfully rolling back and then a fresh
	// retry re-marking under a new timestamp. We delete by the stale
	// token first (cleans up), then re-mark to get a new token that
	// differs from the stale one.
	if _, err := s.UnmarkStripeEventProcessed("evt_race", staleTok); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// Make sure the clock advances so the re-mark writes a distinct ts.
	// now() is RFC3339 second-resolution, so a bare `time.Sleep(1s)` is
	// enough without introducing a flake.
	time.Sleep(1100 * time.Millisecond)
	_, freshTok, err := s.MarkStripeEventProcessed("evt_race")
	if err != nil {
		t.Fatalf("fresh mark: %v", err)
	}
	if freshTok == staleTok {
		t.Fatalf("fresh mark wrote the same token as the stale one — test setup assumption broken (%q)", freshTok)
	}

	// Now a stale unmark fires. It MUST NOT delete the fresh row — that
	// would reopen the retry window after a successful handler.
	unmarked, err := s.UnmarkStripeEventProcessed("evt_race", staleTok)
	if err != nil {
		t.Fatalf("stale unmark: %v", err)
	}
	if unmarked {
		t.Error("stale unmark must NOT delete the fresh marker (race: would reopen retry window)")
	}

	// Sanity check: the fresh row is still there.
	already, tok, err := s.MarkStripeEventProcessed("evt_race")
	if err != nil {
		t.Fatalf("sanity mark: %v", err)
	}
	if !already {
		t.Error("fresh marker must still be in place after stale unmark")
	}
	if tok != freshTok {
		t.Errorf("fresh token changed unexpectedly; got %q, want %q", tok, freshTok)
	}
}

// TestUnmarkStripeEventProcessed_MissingRow verifies idempotency: calling
// unmark for an event ID that was never marked returns (false, nil). The
// sidecar uses unmark as a best-effort rollback, so it may fire for an
// event that was already cleaned up; a hard error in that case would mask
// real problems in the sidecar logs.
func TestUnmarkStripeEventProcessed_MissingRow(t *testing.T) {
	s := testStore(t)

	unmarked, err := s.UnmarkStripeEventProcessed("evt_never_marked", "2025-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("unexpected error for missing row: %v", err)
	}
	if unmarked {
		t.Error("unmark of non-existent row must return unmarked=false")
	}
}

// TestUnmarkStripeEventProcessed_RejectsEmptyID guards against accidental
// "delete every row" calls through a nil/empty event_id. The handler
// already rejects at the HTTP layer, but the store method is exported
// and called in tests, so defensive empty-input rejection matters.
func TestUnmarkStripeEventProcessed_RejectsEmptyID(t *testing.T) {
	s := testStore(t)

	_, err := s.UnmarkStripeEventProcessed("", "2025-01-01T00:00:00Z")
	if err == nil {
		t.Fatal("empty event_id must return an error")
	}
}

// TestUnmarkStripeEventProcessed_RejectsEmptyProcessedAt guards against
// calls that forget to pass the processed_at token. Without the token the
// race-protection contract falls back to "delete by event_id alone", which
// is exactly the unsafe shape the composite key exists to prevent.
func TestUnmarkStripeEventProcessed_RejectsEmptyProcessedAt(t *testing.T) {
	s := testStore(t)

	_, err := s.UnmarkStripeEventProcessed("evt_missing_token", "")
	if err == nil {
		t.Fatal("empty processed_at must return an error")
	}
}
