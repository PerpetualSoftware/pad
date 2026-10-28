package schedule

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestBetweenGapPopulationKeepsEarlierRDate is the New York repro of a gap
// holding more than the old constant slack (TASK-4100 r1): 1,800 gap readings
// sort ahead of an RDATE that resolves earlier than all of them. Between must
// start at the RDATE and equal the chained NextAfter.
func TestBetweenGapPopulationKeepsEarlierRDate(t *testing.T) {
	mins := make([]string, 30)
	secs := make([]string, 60)
	for i := range mins {
		mins[i] = strconv.Itoa(30 + i)
	}
	for i := range secs {
		secs[i] = strconv.Itoa(i)
	}
	s := pinned(t, nyZone, "2025-03-09T02:30", "FREQ=DAILY;BYHOUR=2;BYMINUTE="+strings.Join(mins, ",")+";BYSECOND="+strings.Join(secs, ","))
	s.RDates = []Wall{mustWall(t, "2025-03-09T03:10")}
	c := compile(t, s)
	from, to := inst(t, "2025-03-09T06:00:00Z"), inst(t, "2025-03-09T09:00:00Z")
	first, ok, err := c.NextAfter(from, nil, Limits{})
	if err != nil || !ok || first.At.Format(time.RFC3339) != "2025-03-09T07:10:00Z" {
		t.Fatalf("NextAfter = %v %v %v; want the 07:10Z RDATE", first.At, ok, err)
	}
	got, err := c.Between(from, to, nil, Limits{})
	asCap(t, err, CapInstances)
	if len(got) != 500 || got[0] != first {
		t.Fatalf("Between: %d occurrences, first %v; want 500 starting at %v", len(got), got[0].At, first.At)
	}
	chain := chainBetween(t, c, from, from.Add(time.Hour), nil, Limits{})
	for i := range chain {
		if got[i] != chain[i] {
			t.Fatalf("#%d: Between %v vs chain %v", i, got[i].At, chain[i].At)
		}
	}
}

// TestUntilZIsAnInstantBound covers a Z UNTIL on the wall-clock path
// (TASK-4100 r1): it bounds the resolved instant, inclusively, not the wall.
func TestUntilZIsAnInstantBound(t *testing.T) {
	for _, tc := range []struct {
		name, start, rule string
		want              []string
	}{
		// 02:50 on the gap day resolves to 07:50Z, after UNTIL 07:45Z.
		{"gap-emits-past", "2025-03-08T02:50", "FREQ=DAILY;UNTIL=20250309T074500Z", []string{"2025-03-08T07:50:00Z"}},
		// 01:30 on the overlap day is 05:30Z (EDT pass), before UNTIL 06:15Z (01:15 EST).
		{"overlap-drops-before", "2025-11-01T01:30", "FREQ=DAILY;UNTIL=20251102T061500Z", []string{"2025-11-01T05:30:00Z", "2025-11-02T05:30:00Z"}},
		// Inclusive: UNTIL equal to an instant keeps it.
		{"inclusive", "2025-03-08T02:50", "FREQ=DAILY;UNTIL=20250309T075000Z", []string{"2025-03-08T07:50:00Z", "2025-03-09T07:50:00Z"}},
		// All-day: At is local midnight (05:00Z in EST).
		{"all-day-in", "2025-03-07", "FREQ=DAILY;UNTIL=20250309T050000Z", []string{"2025-03-07T05:00:00Z", "2025-03-08T05:00:00Z", "2025-03-09T05:00:00Z"}},
		{"all-day-out", "2025-03-07", "FREQ=DAILY;UNTIL=20250309T045959Z", []string{"2025-03-07T05:00:00Z", "2025-03-08T05:00:00Z"}},
	} {
		c := compile(t, pinned(t, nyZone, tc.start, tc.rule))
		from := inst(t, tc.start[:10]+"T00:00:00Z").Add(-24 * time.Hour)
		to := from.Add(60 * 24 * time.Hour)
		got, err := c.Between(from, to, nil, Limits{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		g := ats(got)
		if strings.Join(g, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s: got %v; want %v", tc.name, g, tc.want)
		}
		chain := chainBetween(t, c, from, to, nil, Limits{})
		if strings.Join(ats(chain), " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s: chained NextAfter %v; want %v", tc.name, ats(chain), tc.want)
		}
	}
}

// An RDATE past a Z UNTIL still fires: UNTIL bounds the rule, not the RDATEs.
func TestUntilZDoesNotBoundRDates(t *testing.T) {
	s := pinned(t, nyZone, "2025-03-08T02:50", "FREQ=DAILY;UNTIL=20250309T074500Z")
	s.RDates = []Wall{mustWall(t, "2025-03-09T02:50")}
	got, err := compile(t, s).Between(inst(t, "2025-03-01T00:00:00Z"), inst(t, "2025-04-01T00:00:00Z"), nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	wantAts(t, got, "2025-03-08T07:50:00Z", "2025-03-09T07:50:00Z")
}

// TestStartSyncedTokyoZUntil: UNTIL equal to Start, ahead of UTC (TASK-4100 r1).
func TestStartSyncedTokyoZUntil(t *testing.T) {
	c := compile(t, pinned(t, "Asia/Tokyo", "2025-01-06T09:00", "FREQ=DAILY;UNTIL=20250106T000000Z"))
	if !c.StartSynced() {
		t.Error("StartSynced = false; the rule produces Start")
	}
	got, err := c.Between(inst(t, "2025-01-01T00:00:00Z"), inst(t, "2025-02-01T00:00:00Z"), nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	wantAts(t, got, "2025-01-06T00:00:00Z")
}

// TestCapScanIntervalProductSaturates: period*INTERVAL wrapped int64 and
// produced a false CapScan (213504) or no check at all (a negative wrap).
func TestCapScanIntervalProductSaturates(t *testing.T) {
	for _, iv := range []string{"213504", "150000"} {
		c := compile(t, pinned(t, "UTC", "2025-01-01T09:00", "FREQ=DAILY;INTERVAL="+iv))
		q := inst(t, "2125-01-01T00:00:00Z")
		if _, _, err := c.NextAfter(q, nil, Limits{}); err != nil {
			t.Errorf("INTERVAL=%s NextAfter: %v", iv, err)
		}
		if _, err := c.Between(q, q.Add(24*time.Hour), nil, Limits{}); err != nil {
			t.Errorf("INTERVAL=%s Between: %v", iv, err)
		}
	}
}

// TestExhaustedRuleIsNotACapScan: a rule spent before the query start reads as
// exhausted (found=false, an empty slice), not as a scan cap.
func TestExhaustedRuleIsNotACapScan(t *testing.T) {
	q := inst(t, "2035-01-01T00:00:00Z")
	for _, rule := range []string{"FREQ=MINUTELY;BYHOUR=9;UNTIL=19971231T000000Z", "FREQ=MINUTELY;BYHOUR=9;UNTIL=19971231T000000"} {
		c := compile(t, pinned(t, "UTC", "1997-01-01T09:00", rule))
		if _, ok, err := c.NextAfter(q, nil, Limits{}); ok || err != nil {
			t.Errorf("%s NextAfter: ok=%v err=%v; want exhausted", rule, ok, err)
		}
		if occ, err := c.Between(q, q.Add(time.Hour), nil, Limits{}); len(occ) != 0 || err != nil {
			t.Errorf("%s Between: %v %v; want empty", rule, occ, err)
		}
	}
	c := compile(t, pinned(t, "UTC", "1997-01-01T09:00", "FREQ=MINUTELY;BYHOUR=9;COUNT=10"))
	if _, ok, err := c.NextAfter(q, nil, Limits{}); ok || err != nil {
		t.Errorf("COUNT NextAfter: ok=%v err=%v; want exhausted", ok, err)
	}
}
