package schedule

import (
	"fmt"
	"testing"
	"time"
)

type shape string

const (
	shapeFixed shape = "fixed"
	shapeUnit  shape = "unit"
	shapeFloor shape = "floor"
)

// stallShape classifies only costs proved linear on a never-firing stretch
// from DTSTART (TASK-4100 H', ruling conditions 1 and 2).
//
// All sites below are in internal/schedule/internal/rrule/rrule.go:
//   - :615 charges spend(0) once per period-loop iteration.
//   - :774 (HOURLY), :795 (MINUTELY), :822 (SECONDLY, parser-refused)
//     charge spend(r.interval) once per inner INTERVAL step.
//   - :628-644 set filtered for BYMONTH, BYWEEKNO, BYDAY (including ordinal
//     masks), BYEASTER (:632, parser-refused), BYMONTHDAY and BYYEARDAY.
//   - :770-772 and :791-793 jump to the day's last reachable unit for free
//     when filtered; such a day costs two charges, so P is not linear in S.
//   - :732-767 advance YEARLY/MONTHLY/WEEKLY/DAILY without an inner spend.
//
// BYSETPOS is conservatively excluded from shapeUnit as required by H'.
// BYHOUR/BYMINUTE/BYSECOND cannot set filtered.
func stallShape(r *Rule) shape {
	switch r.Freq {
	case freqYearly, freqMonthly, freqWeekly, freqDaily:
		return shapeFixed
	case freqHourly, freqMinutely:
		if len(r.ByMonth)+len(r.ByWeekNo)+len(r.ByYearDay)+len(r.ByMonthDay)+len(r.ByDay)+len(r.BySetPos) == 0 {
			return shapeUnit
		}
	}
	return shapeFloor
}

// TestStallPosClosedForm independently checks the reported position using
// RFC 5545 section 3.3.10 calendar arithmetic, never generator output as an
// expectation. Equality detects both a false cap and an unsafe high position.
// Every rule has BY parts, so spec.go's elapsed path is inapplicable: p0 and P
// are UTC-labelled local walls. A DST gap or fold is still one wall period;
// ignoring a nonexistent instance does not skip a period of iteration.
//
// For shapeFloor only, P >= p0 + floor(S/2)*INTERVAL units is checked.
// Blind spot (TASK-4100 H', ruling condition 5): this floor cannot detect a P
// that is one INTERVAL too low. No equality is claimed for filtered day jumps.
func TestStallPosClosedForm(t *testing.T) {
	for _, tc := range []struct {
		name, zone, start, rule string
		shape                   shape
	}{
		{"daily", "UTC", "2025-01-01T00:00", "FREQ=DAILY;BYMONTH=2", shapeFixed},
		{"daily_interval_midday", "UTC", "2025-01-01T09:30", "FREQ=DAILY;INTERVAL=3;BYMONTH=2", shapeFixed},
		// checkSatisfiable in rule.go refuses February 30/31 even with RDATE.
		// February 29 is accepted and absent in 2025; the interval-2 monthly
		// rule never reaches February, and the interval-4 yearly rule stays
		// in non-leap years. These replace the three impossible-date cases.
		{"monthly", "UTC", "2025-01-01T00:00", "FREQ=MONTHLY;BYMONTH=2;BYMONTHDAY=29", shapeFixed},
		{"monthly_interval_midmonth", "UTC", "2025-01-15T08:00", "FREQ=MONTHLY;INTERVAL=2;BYMONTH=2;BYMONTHDAY=29", shapeFixed},
		{"yearly", "UTC", "2025-03-10T00:00", "FREQ=YEARLY;INTERVAL=4;BYMONTH=2;BYMONTHDAY=29", shapeFixed},
		// BYMONTH=2 would fire within five periods. March/April preserve
		// the requested WKST/INTERVAL shapes without firing in this stretch.
		{"weekly_wednesday", "UTC", "2025-01-08T10:00", "FREQ=WEEKLY;BYMONTH=3", shapeFixed},
		{"weekly_interval_sunday", "UTC", "2025-01-08T10:00", "FREQ=WEEKLY;INTERVAL=2;BYMONTH=4;WKST=SU", shapeFixed},
		{"hourly", "UTC", "2025-01-01T00:00", "FREQ=HOURLY;INTERVAL=2;BYHOUR=3", shapeUnit},
		{"hourly_midnight", "UTC", "2025-01-01T22:00", "FREQ=HOURLY;INTERVAL=2;BYHOUR=3", shapeUnit},
		{"minutely_hour", "UTC", "2025-01-01T00:56", "FREQ=MINUTELY;INTERVAL=2;BYMINUTE=7", shapeUnit},
		{"hourly_ny_spring", nyZone, "2025-03-09T00:00", "FREQ=HOURLY;INTERVAL=2;BYHOUR=3", shapeUnit},
		{"hourly_ny_fall", nyZone, "2025-11-02T00:00", "FREQ=HOURLY;INTERVAL=2;BYHOUR=3", shapeUnit},
		{"minutely_ny_spring", nyZone, "2025-03-09T01:56", "FREQ=MINUTELY;INTERVAL=2;BYMINUTE=7", shapeUnit},
		{"minutely_ny_fall", nyZone, "2025-11-02T01:56", "FREQ=MINUTELY;INTERVAL=2;BYMINUTE=7", shapeUnit},
		{"minutely_filtered", "UTC", "2025-01-01T00:00", "FREQ=MINUTELY;BYMONTH=1;BYYEARDAY=200", shapeFloor},
		{"hourly_filtered_byhour", "UTC", "2025-01-01T00:00", "FREQ=HOURLY;BYHOUR=5;BYMONTH=1;BYYEARDAY=200", shapeFloor},
		// Keep both M2 reproductions from neverpartial_test.go explicit.
		{"M2_minutely", "UTC", "2025-01-01T00:00", "FREQ=MINUTELY;BYMONTH=1;BYYEARDAY=200", shapeFloor},
		{"M2_hourly_tokyo", "Asia/Tokyo", "2025-01-01T00:00", "FREQ=HOURLY;BYMONTH=1;BYYEARDAY=200", shapeFloor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := pinned(t, tc.zone, tc.start, tc.rule)
			spec.RDates = []Wall{mustWall(t, "2030-01-01T00:00")}
			c := compile(t, spec)
			r := c.rule
			sh := stallShape(r)
			if sh != tc.shape {
				t.Fatalf("case=%s shape: want=%s got=%s", tc.name, tc.shape, sh)
			}
			if c.elapsed {
				t.Fatalf("case=%s: want wall-clock rule, got elapsed", tc.name)
			}
			w := spec.Start
			p0 := time.Date(w.Year, w.Month, w.Day, w.Hour, w.Minute, w.Second, 0, time.UTC)
			iv := max(r.Interval, 1)
			for _, s := range []int64{1, 2, 3, 5} {
				t.Run(fmt.Sprintf("S=%d", s), func(t *testing.T) {
					n := int(s) * iv
					var want time.Time
					switch sh {
					case shapeFixed:
						switch r.Freq {
						case freqDaily:
							want = time.Date(w.Year, w.Month, w.Day, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n)
						case freqMonthly:
							want = time.Date(w.Year, w.Month, 1, 0, 0, 0, 0, time.UTC).AddDate(0, n, 0)
						case freqYearly:
							want = time.Date(w.Year+n, time.January, 1, 0, 0, 0, 0, time.UTC)
						case freqWeekly:
							// rrule.go:757-763 aligns to WKST on the first
							// advance, then adds INTERVAL weeks each time.
							// RFC 5545 3.3.10: WKST defaults to Monday.
							wkst := time.Monday
							if r.Wkst != "" {
								wkst = weekdays[r.Wkst]
							}
							back := (int(p0.Weekday()) - int(wkst) + 7) % 7
							want = time.Date(w.Year, w.Month, w.Day, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 7*n-back)
						}
					case shapeUnit, shapeFloor:
						if sh == shapeFloor {
							n = int(s/2) * iv
						}
						if r.Freq == freqHourly {
							want = time.Date(w.Year, w.Month, w.Day, w.Hour+n, w.Minute, w.Second, 0, time.UTC)
						} else {
							want = time.Date(w.Year, w.Month, w.Day, w.Hour, w.Minute+n, w.Second, 0, time.UTC)
						}
						// spend resets fields below FREQ (rrule.go:598-604).
						// The floor retains p0's sub-unit fields; these cases
						// start on unit boundaries.
						if sh == shapeUnit {
							if r.Freq == freqHourly {
								want = time.Date(want.Year(), want.Month(), want.Day(), want.Hour(), 0, 0, 0, time.UTC)
							} else {
								want = time.Date(want.Year(), want.Month(), want.Day(), want.Hour(), want.Minute(), 0, 0, time.UTC)
							}
						}
					}
					// q = DTSTART, with no fast-forward and no RDATE stream.
					rr := oracleRule(t, c, c.zone, oracleConv(c, c.zone)(spec.Start), s)
					if rr == nil {
						t.Fatalf("case=%s S=%d shape=%s want=%v got=nil rule", tc.name, s, sh, want)
					}
					candidate, ok := rr.Iterator()()
					if ok || !rr.Exhausted() {
						t.Fatalf("case=%s S=%d shape=%s want=%v got=%v: never-firing precondition failed: ok=%v exhausted=%v", tc.name, s, sh, want, candidate, ok, rr.Exhausted())
					}
					got := stallPos(rr)
					if sh == shapeFloor {
						if got.Before(want) {
							t.Errorf("case=%s S=%d shape=%s want>=%v got=%v", tc.name, s, sh, want, got)
						}
					} else if !got.Equal(want) {
						t.Errorf("case=%s S=%d shape=%s want=%v got=%v", tc.name, s, sh, want, got)
					}
				})
			}
		})
	}
}
