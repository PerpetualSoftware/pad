package store

import (
	"testing"
	"time"
)

// exactElapsedStart takes done+span from the second half of a repeated hour,
// and reports none rather than a wrong start for a fractional instant.
func TestExactElapsedStart(t *testing.T) {
	t.Parallel()
	ny, _ := time.LoadLocation("America/New_York")
	done := rt("2026-11-01T06:10:00Z") // 01:10 EST, the second half
	if s, ok := exactElapsedStart(ny, time.Hour, done); !ok || !s.Equal(done.Add(time.Hour)) {
		t.Errorf("got %v %v, want done+span", s, ok)
	}
	if s, ok := exactElapsedStart(ny, 7*time.Minute, done); !ok || !s.Equal(done.Add(-63*time.Minute)) {
		t.Errorf("got %v %v, want done-9 spans", s, ok)
	}
	if _, ok := exactElapsedStart(ny, time.Hour, done.Add(time.Nanosecond)); ok {
		t.Error("a fractional instant cannot read back")
	}
}
