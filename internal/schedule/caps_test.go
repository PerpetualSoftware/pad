package schedule

import (
	"errors"
	"testing"
	"time"
)

func asCap(t *testing.T, err error, kind CapKind) *CapError {
	t.Helper()
	var ce *CapError
	if !errors.As(err, &ce) || !errors.Is(err, ErrCapHit) || ce.Kind != kind {
		t.Fatalf("err = %v; want a %s CapError", err, kind)
	}
	return ce
}

func TestCapMinutelyNoCountNoUntil(t *testing.T) {
	c := compile(t, pinned(t, nyZone, "2025-01-01T00:00", "FREQ=MINUTELY"))
	began := time.Now()
	occ, err := c.Between(inst(t, "2025-02-01T00:00:00Z"), inst(t, "2025-02-02T00:00:00Z"), nil, Limits{})
	asCap(t, err, CapInstances)
	if len(occ) != 500 {
		t.Fatalf("%d occurrences; want the first 500", len(occ))
	}
	for i := 1; i < len(occ); i++ {
		if !occ[i].At.After(occ[i-1].At) {
			t.Fatal("not ascending")
		}
	}
	if d := time.Since(began); d > time.Second {
		t.Errorf("took %v; the cap must stop the walk early", d)
	}
}

func TestCapWindowTooWide(t *testing.T) {
	c := compile(t, pinned(t, "UTC", "2025-01-01T00:00", "FREQ=DAILY"))
	from := inst(t, "2025-01-01T00:00:00Z")
	occ, err := c.Between(from, from.Add(92*24*time.Hour+1), nil, Limits{})
	asCap(t, err, CapWindow)
	if occ != nil {
		t.Error("a window cap returns no slice")
	}
	if occ, err := c.Between(from, from.Add(92*24*time.Hour), nil, Limits{}); err != nil || len(occ) != 92 {
		t.Errorf("exactly MaxWindow: %d, %v; want 92, nil", len(occ), err)
	}
}

func TestCapInstancesBoundary(t *testing.T) {
	c := compile(t, pinned(t, "UTC", "2025-01-01T00:00", "FREQ=DAILY"))
	from := inst(t, "2025-01-01T00:00:00Z")
	if occ, err := c.Between(from, from.Add(5*24*time.Hour), nil, Limits{MaxInstances: 5}); err != nil || len(occ) != 5 {
		t.Errorf("exactly MaxInstances: %d, %v; want 5, nil", len(occ), err)
	}
	occ, err := c.Between(from, from.Add(6*24*time.Hour), nil, Limits{MaxInstances: 5})
	asCap(t, err, CapInstances)
	if len(occ) != 5 {
		t.Errorf("%d returned; want the first 5", len(occ))
	}
}

func TestCapScanNaiveFarFromStart(t *testing.T) {
	c := compile(t, pinned(t, "UTC", "2025-01-01T09:00", "FREQ=MINUTELY;BYHOUR=9"))
	_, _, err := c.NextAfter(inst(t, "2035-01-01T00:00:00Z"), nil, Limits{})
	asCap(t, err, CapScan)
	_, err = c.Between(inst(t, "2035-01-01T00:00:00Z"), inst(t, "2035-01-02T00:00:00Z"), nil, Limits{})
	asCap(t, err, CapScan)
}

func TestElapsedFastForward(t *testing.T) {
	start := inst(t, "2020-01-01T00:00:00Z")
	c := compile(t, pinned(t, "UTC", "2020-01-01T00:00", "FREQ=MINUTELY;INTERVAL=7"))
	q := inst(t, "2035-06-01T00:00:00Z")
	began := time.Now()
	o, ok, err := c.NextAfter(q, nil, Limits{})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if d := time.Since(began); d > 100*time.Millisecond {
		t.Errorf("took %v; the fast-forward must make distance free", d)
	}
	step := 7 * time.Minute
	want := start.Add((q.Sub(start)/step + 1) * step)
	if !o.At.Equal(want) {
		t.Errorf("got %v; want %v", o.At, want)
	}
	occ, _ := c.Between(q, q.Add(time.Hour), nil, Limits{})
	if len(occ) == 0 || !occ[0].At.Equal(want) {
		t.Errorf("Between's first = %v; want %v", occ, want)
	}
}

func TestElapsedFastForwardCount(t *testing.T) {
	start := inst(t, "2020-01-01T00:00:00Z")
	c := compile(t, pinned(t, "UTC", "2020-01-01T00:00", "FREQ=MINUTELY;INTERVAL=7;COUNT=10"))
	if _, ok, err := c.NextAfter(inst(t, "2035-01-01T00:00:00Z"), nil, Limits{}); ok || err != nil {
		t.Errorf("past COUNT: ok=%v err=%v; want exhausted", ok, err)
	}
	// COUNT is spent from Start, not from the fast-forwarded origin: the
	// 6th occurrence (index 5) is the 6th even when reached by a skip.
	for i := 0; i < 10; i++ {
		after := start.Add(time.Duration(i)*7*time.Minute - time.Second)
		o, ok, err := c.NextAfter(after, nil, Limits{})
		if err != nil || !ok || !o.At.Equal(start.Add(time.Duration(i)*7*time.Minute)) {
			t.Fatalf("occurrence %d: %v %v %v", i, o.At, ok, err)
		}
	}
	if _, ok, _ := c.NextAfter(start.Add(9*7*time.Minute), nil, Limits{}); ok {
		t.Error("the 11th occurrence exists; COUNT=10")
	}
}

func TestCapTimeGuard(t *testing.T) {
	// BYYEARDAY=200 never falls in January: the library walks to year 9999.
	c := compile(t, pinned(t, "UTC", "2025-01-01T00:00", "FREQ=MINUTELY;BYMONTH=1;BYYEARDAY=200"))
	began := time.Now()
	_, _, err := c.NextAfter(inst(t, "2025-01-01T00:00:00Z"), nil, Limits{MaxDuration: 50 * time.Millisecond})
	ce := asCap(t, err, CapTime)
	if ce.Limit != int64(50*time.Millisecond) {
		t.Errorf("Limit = %d; want 50ms in ns", ce.Limit)
	}
	if d := time.Since(began); d > 500*time.Millisecond {
		t.Errorf("returned after %v; the guard must not wait for the library", d)
	}
}

func TestCapZeroFieldsUseDefaults(t *testing.T) {
	if got := (Limits{}).fill(); got != DefaultLimits {
		t.Errorf("fill() = %+v; want %+v", got, DefaultLimits)
	}
	if got := (Limits{MaxInstances: 3}).fill(); got.MaxInstances != 3 || got.MaxWindow != DefaultLimits.MaxWindow {
		t.Errorf("a set field must win and the rest default: %+v", got)
	}
	c := compile(t, pinned(t, "UTC", "2025-01-01T00:00", "FREQ=DAILY"))
	from := inst(t, "2025-01-01T00:00:00Z")
	_, err := c.Between(from, from.Add(93*24*time.Hour), nil, Limits{})
	asCap(t, err, CapWindow)
}

func TestCapErrorShape(t *testing.T) {
	c := compile(t, pinned(t, "UTC", "2025-01-01T00:00", "FREQ=MINUTELY"))
	from := inst(t, "2025-01-01T00:00:00Z")
	_, err := c.Between(from, from.Add(time.Hour), nil, Limits{MaxInstances: 10})
	if ce := asCap(t, err, CapInstances); ce.Limit != 10 || ce.Error() == "" {
		t.Errorf("%+v: want Limit 10 and a message", ce)
	}
	_, err = c.Between(from, from.Add(200*time.Hour), nil, Limits{MaxWindow: time.Hour})
	if ce := asCap(t, err, CapWindow); ce.Limit != int64(time.Hour) {
		t.Errorf("Limit = %d; want 1h in ns", ce.Limit)
	}
}
