package store

import "testing"

// TestUnmarkStripeEventProcessed_RoundTrip exercises the mark → unmark → re-mark
// cycle that the sidecar relies on for TASK-736. After an unmark, the SAME
// event ID must behave as brand-new on the next mark call so Stripe
// retries can re-run the handler. A bug that made unmark a no-op (e.g.
// wrong WHERE clause) would break this invariant.
func TestUnmarkStripeEventProcessed_RoundTrip(t *testing.T) {
	s := testStore(t)

	// Mark a fresh event.
	already, err := s.MarkStripeEventProcessed("evt_roundtrip")
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if already {
		t.Fatal("first mark must return already_processed=false")
	}

	// Duplicate mark — must be detected.
	already, err = s.MarkStripeEventProcessed("evt_roundtrip")
	if err != nil {
		t.Fatalf("re-mark before unmark: %v", err)
	}
	if !already {
		t.Fatal("second mark (no unmark yet) must detect duplicate")
	}

	// Unmark — row existed.
	unmarked, err := s.UnmarkStripeEventProcessed("evt_roundtrip")
	if err != nil {
		t.Fatalf("unmark: %v", err)
	}
	if !unmarked {
		t.Fatal("unmark of existing row must return unmarked=true")
	}

	// Re-mark — row is gone, must be treated as brand-new so the handler
	// can run again. This is the whole point of the endpoint.
	already, err = s.MarkStripeEventProcessed("evt_roundtrip")
	if err != nil {
		t.Fatalf("re-mark after unmark: %v", err)
	}
	if already {
		t.Fatal("mark after unmark must return already_processed=false (retry path broken)")
	}
}

// TestUnmarkStripeEventProcessed_MissingRow verifies idempotency: calling
// unmark for an event ID that was never marked returns (false, nil). The
// sidecar uses unmark as a best-effort rollback, so it may fire for an
// event that Stripe retried (meaning the row already got unmarked and
// remarked between attempts); a hard error in that case would mask real
// problems in the sidecar logs.
func TestUnmarkStripeEventProcessed_MissingRow(t *testing.T) {
	s := testStore(t)

	unmarked, err := s.UnmarkStripeEventProcessed("evt_never_marked")
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

	_, err := s.UnmarkStripeEventProcessed("")
	if err == nil {
		t.Fatal("empty event_id must return an error")
	}
}
