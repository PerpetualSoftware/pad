package schedule

import (
	"runtime"
	"testing"
	"time"
)

// budgetCalls runs call 50 times and holds each to the H1 contract
// (TASK-4100): a CapScan, in well under MaxDuration, with no goroutine left
// behind.
func budgetCalls(t *testing.T, call func() error) {
	t.Helper()
	call() // warm any lazy runtime goroutine before counting
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()
	var slowest time.Duration
	for i := 0; i < 50; i++ {
		began := time.Now()
		err := call()
		if d := time.Since(began); d > slowest {
			slowest = d
		}
		asCap(t, err, CapScan)
	}
	if slowest > DefaultLimits.MaxDuration {
		t.Errorf("slowest call %v; want under MaxDuration %v", slowest, DefaultLimits.MaxDuration)
	}
	time.Sleep(50 * time.Millisecond) // settle: an abandoned goroutine would still be spinning
	if after := runtime.NumGoroutine(); after != before {
		t.Errorf("goroutines %d -> %d across 50 calls; the work must run in the caller", before, after)
	}
}

func TestBudgetFeb29Minutely(t *testing.T) {
	// 2096-03-01 start: the next match is 2104-02-29, across the non-leap 2100.
	// The expander skips a filtered day in one step, so this sparse rule costs
	// a few thousand steps: it must stay correct, fast, and in the caller.
	c := compile(t, pinned(t, "UTC", "2096-03-01T00:00", "FREQ=MINUTELY;BYMONTH=2;BYMONTHDAY=29;UNTIL=21500101T000000Z"))
	q := inst(t, "2096-03-02T00:00:00Z")
	want := inst(t, "2104-02-29T00:00:00Z")
	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		began := time.Now()
		o, ok, err := c.NextAfter(q, nil, Limits{})
		if err != nil || !ok || !o.At.Equal(want) {
			t.Fatalf("NextAfter = %v %v %v; want %v", o.At, ok, err, want)
		}
		if d := time.Since(began); d > DefaultLimits.MaxDuration/5 {
			t.Fatalf("took %v; a sparse rule must be far under MaxDuration", d)
		}
	}
	// The same rule with a budget below the gap is a CapScan, never a partial answer.
	for i := 0; i < 50; i++ {
		_, _, err := c.NextAfter(q, nil, Limits{MaxSteps: 1000})
		asCap(t, err, CapScan)
		_, err = c.Between(q, q.Add(24*time.Hour), nil, Limits{MaxSteps: 1000})
		asCap(t, err, CapScan)
	}
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after != before {
		t.Errorf("goroutines %d -> %d; the work must run in the caller", before, after)
	}
}

func TestBudgetUnsatisfiable(t *testing.T) {
	spec := pinned(t, "UTC", "2025-01-01T00:00", "FREQ=MINUTELY;BYMONTH=1;BYYEARDAY=200")
	budgetCalls(t, func() error { // Compile's own probe must be bounded too, and not an error
		c, err := Compile(spec)
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		if c.StartSynced() {
			t.Error("an unsatisfiable rule cannot produce Start")
		}
		return &CapError{Kind: CapScan}
	})
	c := compile(t, spec)
	q := inst(t, "2025-01-01T00:00:00Z")
	budgetCalls(t, func() error {
		_, _, err := c.NextAfter(q, nil, Limits{})
		return err
	})
	budgetCalls(t, func() error {
		_, err := c.Between(q, q.Add(24*time.Hour), nil, Limits{})
		return err
	})
}
