package server

import (
	"errors"
	"testing"
)

// BUG-2778. A rename serializes per workspace with a 5s lock timeout, so
// contention surfaces as SQLSTATE 55P03 — and a deadlock Postgres chooses to
// break surfaces as 40P01. Both mean "try again"; a generic 500 tells the
// caller the opposite.
//
// This drives the classifier directly rather than manufacturing contention
// through the HTTP stack: producing a real 55P03 needs two concurrent
// transactions and a Postgres backend, and the property under test is which
// errors are classified retryable — not whether Postgres emits them.
func TestIsRetryableLockError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"lock timeout", errors.New(`ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)`), true},
		{"deadlock detected", errors.New(`ERROR: deadlock detected (SQLSTATE 40P01)`), true},
		// Controls: a classifier that answered true for everything would pass
		// the two legs above on its own.
		{"unique violation", errors.New(`ERROR: duplicate key value violates unique constraint (SQLSTATE 23505)`), false},
		{"invalid byte sequence", errors.New(`ERROR: invalid byte sequence for encoding "UTF8" (SQLSTATE 22021)`), false},
		{"plain error", errors.New("update document: connection refused"), false},
		{"nil", nil, false},
	} {
		if got := isRetryableLockError(tc.err); got != tc.want {
			t.Errorf("%s: isRetryableLockError = %v, want %v", tc.name, got, tc.want)
		}
	}
}
