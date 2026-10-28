package schedule

import (
	"errors"
	"runtime"
	"testing"
	"time"
)

// noLeak runs call n times and fails if the goroutine count moved: every
// expansion runs in the caller (TASK-4100 H1). It asserts on counts and kinds,
// never on wall time, so it does not flake under load or -race (r2).
func noLeak(t *testing.T, n int, call func()) {
	t.Helper()
	call() // warm any lazy runtime goroutine before counting
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()
	for i := 0; i < n; i++ {
		call()
	}
	time.Sleep(50 * time.Millisecond) // settle: an abandoned goroutine would still be spinning
	if after := runtime.NumGoroutine(); after != before {
		t.Errorf("goroutines %d -> %d across %d calls; the work must run in the caller", before, after, n)
	}
}

// stallCalls holds a call to the stall contract: a CapStall whose Limit is
// MaxSteps, from the real code path, with no goroutine left behind.
func stallCalls(t *testing.T, lim Limits, call func() error) {
	t.Helper()
	noLeak(t, 20, func() {
		ce := asCap(t, call(), CapStall)
		if want := lim.fill().MaxSteps; ce.Limit != want {
			t.Errorf("CapStall.Limit = %d; want MaxSteps %d", ce.Limit, want)
		}
	})
}

func TestBudgetFeb29Minutely(t *testing.T) {
	// 2096-03-01 start: the next match is 2104-02-29, across the non-leap 2100.
	// The expander skips a filtered day in one step, so this sparse rule costs
	// a few thousand steps: it must stay correct and in the caller.
	c := compile(t, pinned(t, "UTC", "2096-03-01T00:00", "FREQ=MINUTELY;BYMONTH=2;BYMONTHDAY=29;UNTIL=21500101T000000Z"))
	q := inst(t, "2096-03-02T00:00:00Z")
	want := inst(t, "2104-02-29T00:00:00Z")
	noLeak(t, 20, func() {
		o, ok, err := c.NextAfter(q, nil, Limits{})
		if err != nil || !ok || !o.At.Equal(want) {
			t.Fatalf("NextAfter = %v %v %v; want %v", o.At, ok, err, want)
		}
	})
	// The same rule with a budget below the gap is a CapStall, never a partial answer.
	lim := Limits{MaxSteps: 1000}
	stallCalls(t, lim, func() error { _, _, err := c.NextAfter(q, nil, lim); return err })
	// A window the rule is known to be empty through is a final, empty answer; one
	// reaching past what the budget examined is a CapStall (TASK-4100 r3).
	if occ, err := c.Between(q, q.Add(24*time.Hour), nil, lim); err != nil || len(occ) != 0 {
		t.Errorf("Between inside the examined stretch = %v %v; want an empty answer", occ, err)
	}
	short := Limits{MaxSteps: 20}
	stallCalls(t, short, func() error { _, err := c.Between(q, q.Add(30*24*time.Hour), nil, short); return err })
}

func TestBudgetUnsatisfiable(t *testing.T) {
	spec := pinned(t, "UTC", "2025-01-01T00:00", "FREQ=MINUTELY;BYMONTH=1;BYYEARDAY=200")
	noLeak(t, 20, func() { // Compile's own probe is bounded too, and is not an error
		c, err := Compile(spec)
		if err != nil || c.StartSynced() {
			t.Fatalf("Compile: synced=%v err=%v; want an unsynced rule and no error", err == nil && c.StartSynced(), err)
		}
	})
	c := compile(t, spec)
	q := inst(t, "2025-01-01T00:00:00Z")
	stallCalls(t, Limits{}, func() error { _, _, err := c.NextAfter(q, nil, Limits{}); return err })
	// Between a day: the rule is examined well past it, so the empty answer is final.
	if occ, err := c.Between(q, q.Add(24*time.Hour), nil, Limits{}); err != nil || len(occ) != 0 {
		t.Errorf("Between = %v %v; want an empty answer", occ, err)
	}
	short := Limits{MaxSteps: 20}
	stallCalls(t, short, func() error { _, err := c.Between(q, q.Add(30*24*time.Hour), nil, short); return err })
}

// TestNoGoroutineOutlivesCall is the instrument for "all work runs in the
// caller" (TASK-4100 H1). MaxDuration is tiny, so a restored abandon path
// returns CapTime at 1 ms while its goroutine keeps spending the 200k-step
// budget (about 13 ms, about 100 ms under -race); the count read here, with no
// settle, is then about 20 higher. MaxSteps is kept short for -race (r2).
func TestNoGoroutineOutlivesCall(t *testing.T) {
	c := compile(t, pinned(t, "UTC", "2025-01-01T00:00", "FREQ=MINUTELY;BYMONTH=1;BYYEARDAY=200"))
	q := inst(t, "2025-01-01T00:00:00Z")
	lim := Limits{MaxDuration: time.Millisecond, MaxSteps: 200_000}
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		var err error
		if i%2 == 0 {
			_, _, err = c.NextAfter(q, nil, lim)
		} else {
			_, err = c.Between(q, q.Add(24*time.Hour), nil, lim)
		}
		if err != nil && !errors.Is(err, ErrCapHit) { // any cap, or a final answer: this test is about the count
			t.Fatalf("call %d: err = %v; want a *CapError or none", i, err)
		}
	}
	if after := runtime.NumGoroutine(); after != before {
		t.Errorf("goroutines %d -> %d right after 20 calls; none may outlive a call", before, after)
	}
}
