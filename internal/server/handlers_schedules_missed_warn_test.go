package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), sub)
}

// A rule that compiles but hits a cap in the missed count fails the same way on
// every read. The warning is logged once per schedule edit, not once per GET,
// and the limiter holds one entry per schedule however often it is edited
// (TASK-4103 C2b-4). RED with the unguarded log.
func TestScheduleViewMissedWarningIsRateLimitedPerEdit(t *testing.T) {
	f := newSchedHTTP(t) // not parallel: it swaps the default logger
	sc := f.create(okSchedule)
	id := sc["id"].(string)
	// From the 2000 dtstart with no re-anchor this is CapScan in the missed walk.
	if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(
		`UPDATE item_schedules SET dtstart = ?, rrule = ?, next_wall = ?, next_at = ?, updated_at = ? WHERE id = ?`),
		"2000-01-01T09:00:00", "FREQ=MINUTELY;BYHOUR=9", "2026-10-28T09:00:00", "2026-10-28T09:00:00Z", "2026-01-01T00:00:00Z", id); err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	warns := func() int { return logs.count("missed count failed") }
	size := func() int {
		n := 0
		f.srv.missedWarned.Range(func(_, _ any) bool { n++; return true })
		return n
	}

	for i := 0; i < 2; i++ {
		if rr := f.do("GET", nil); rr.Code != http.StatusOK {
			t.Fatalf("GET: %d %s", rr.Code, rr.Body.String())
		}
	}
	if n := warns(); n != 1 {
		t.Fatalf("two GETs of a capped schedule logged %d warnings, want 1", n)
	}

	for i, lead := range []int{5, 10} {
		time.Sleep(1100 * time.Millisecond) // updated_at has second resolution
		if rr := f.do("PATCH", map[string]any{"lead_minutes": lead}); rr.Code != http.StatusOK {
			t.Fatalf("PATCH %d: %d %s", i, rr.Code, rr.Body.String())
		}
		// The PATCH response renders the view too: its own warning is the one
		// for this edit, so the GET after it adds none.
		before := warns()
		if rr := f.do("GET", nil); rr.Code != http.StatusOK {
			t.Fatalf("GET: %d", rr.Code)
		}
		if after := warns(); after != before {
			t.Errorf("GET after PATCH %d logged again", i)
		}
	}
	if n := warns(); n != 3 {
		t.Errorf("warnings after two edits = %d, want 3 (one per edit, plus the first)", n)
	}
	if n := size(); n != 1 {
		t.Errorf("limiter holds %d entries after two edits of one schedule, want 1", n)
	}
}

// TestScheduleViewMissedWarningNeverRegressesOrDuplicates pins the limiter's
// atomicity (TASK-4103 C2b-i r1-1): a view built from a snapshot older than the
// one last warned at neither logs nor moves the stored value back, and
// concurrent first reads log once.
func TestScheduleViewMissedWarningNeverRegressesOrDuplicates(t *testing.T) {
	f := newSchedHTTP(t) // not parallel: it swaps the default logger
	sc := f.create(okSchedule)
	id := sc["id"].(string)
	breakMissedWalk(t, f, id)
	logs := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	warns := func() int { return logs.count("missed count failed") }

	// (a) A newer edit was already warned at; this read carries the older
	// updated_at the stored row has.
	newer := "2026-06-01T00:00:00Z"
	f.srv.missedWarned.Store(id, newer)
	if rr := f.do("GET", nil); rr.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rr.Code, rr.Body.String())
	}
	if n := warns(); n != 0 {
		t.Errorf("a read older than the last warned edit logged %d warnings, want 0", n)
	}
	if v, _ := f.srv.missedWarned.Load(id); v != newer {
		t.Errorf("limiter value regressed to %v, want %s", v, newer)
	}

	// (b) Concurrent first reads log once.
	f.srv.missedWarned.Delete(id)
	base := warns()
	const k = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < k; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			f.do("GET", nil)
		}()
	}
	close(start)
	wg.Wait()
	if n := warns() - base; n != 1 {
		t.Errorf("%d concurrent first reads logged %d warnings, want 1", k, n)
	}
}

// TestScheduleViewMissedWarningIsEvicted pins the limiter's bound (TASK-4103
// C2b-i r1-2): an entry leaves when the schedule's count succeeds again and
// when the schedule is deleted.
func TestScheduleViewMissedWarningIsEvicted(t *testing.T) {
	f := newSchedHTTP(t)
	sc := f.create(okSchedule)
	id := sc["id"].(string)
	size := func() int {
		n := 0
		f.srv.missedWarned.Range(func(_, _ any) bool { n++; return true })
		return n
	}

	breakMissedWalk(t, f, id)
	f.do("GET", nil)
	if n := size(); n != 1 {
		t.Fatalf("capped read left %d entries, want 1", n)
	}
	if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(
		`UPDATE item_schedules SET rrule = ? WHERE id = ?`), "FREQ=DAILY", id); err != nil {
		t.Fatal(err)
	}
	if rr := f.do("GET", nil); rr.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rr.Code, rr.Body.String())
	}
	if n := size(); n != 0 {
		t.Errorf("a recovered schedule left %d limiter entries, want 0", n)
	}

	breakMissedWalk(t, f, id)
	f.do("GET", nil)
	if n := size(); n != 1 {
		t.Fatalf("capped read left %d entries, want 1", n)
	}
	if rr := f.do("DELETE", nil); rr.Code/100 != 2 {
		t.Fatalf("DELETE: %d %s", rr.Code, rr.Body.String())
	}
	if n := size(); n != 0 {
		t.Errorf("a deleted schedule left %d limiter entries, want 0", n)
	}
}

// breakMissedWalk makes the schedule's missed walk hit CapScan: from the 2000
// dtstart with no re-anchor, a minutely rule inside one hour a day.
func breakMissedWalk(t *testing.T, f *schedHTTP, id string) {
	t.Helper()
	if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(
		`UPDATE item_schedules SET dtstart = ?, rrule = ?, next_wall = ?, next_at = ?, updated_at = ? WHERE id = ?`),
		"2000-01-01T09:00:00", "FREQ=MINUTELY;BYHOUR=9", "2026-10-28T09:00:00", "2026-10-28T09:00:00Z", "2026-01-01T00:00:00Z", id); err != nil {
		t.Fatal(err)
	}
}
