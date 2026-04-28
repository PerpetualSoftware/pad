package main

import (
	"errors"
	"fmt"
	"testing"
)

// TestIsCancellationRecognizesWrappedSentinel verifies that the helper
// works on errors that have been wrapped with fmt.Errorf("...: %w", err)
// — which is what every layer between the prompt and the init RunE does.
func TestIsCancellationRecognizesWrappedSentinel(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil err is not cancellation", err: nil, want: false},
		{name: "unrelated err is not cancellation", err: errors.New("network down"), want: false},
		{name: "raw sentinel is cancellation", err: errCancelled, want: true},
		{name: "wrapped once", err: fmt.Errorf("configure: %w", errCancelled), want: true},
		{name: "wrapped twice", err: fmt.Errorf("init: %w", fmt.Errorf("configure: %w", errCancelled)), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCancellation(tc.err); got != tc.want {
				t.Errorf("isCancellation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
