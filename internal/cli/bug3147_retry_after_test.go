package cli

import (
	"net/http"
	"testing"
	"time"
)

// BUG-3147: Retry-After arrives in either of RFC 9110's two forms, and a
// value that cannot be honoured must read as "no suggestion" (0), never as a
// negative or garbage wait.
func TestParseRetryAfterSeconds(t *testing.T) {
	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	past := time.Now().Add(-90 * time.Second).UTC().Format(http.TimeFormat)
	for _, c := range []struct {
		in       string
		min, max int
	}{
		{"", 0, 0},
		{"7", 7, 7},
		{" 12 ", 12, 12},
		{"-3", 0, 0},
		{"soon", 0, 0},
		{past, 0, 0},
		{future, 88, 91}, // an HTTP-date, measured against now
	} {
		if got := parseRetryAfterSeconds(c.in); got < c.min || got > c.max {
			t.Errorf("parseRetryAfterSeconds(%q) = %d, want %d..%d", c.in, got, c.min, c.max)
		}
	}
}
