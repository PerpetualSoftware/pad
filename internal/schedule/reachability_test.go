package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/schedule/internal/rrule"
)

func list(name string, v []int) string {
	if len(v) == 0 {
		return ""
	}
	s := make([]string, len(v))
	for i, n := range v {
		s[i] = strconv.Itoa(n)
	}
	return ";" + name + "=" + strings.Join(s, ",")
}

// engineFires runs the vendored expander directly, with no Compile check in
// front of it, and says whether it yields an occurrence within horizon. The
// budget (60,000 steps; a 2,000,000 run gave the same verdicts) is far past
// what a firing rule needs.
func engineFires(t *testing.T, freq rrule.Frequency, start time.Time, interval int, hours, minutes []int, horizon time.Duration) bool {
	t.Helper()
	rr, err := rrule.NewRRule(rrule.ROption{Freq: freq, Dtstart: start, Interval: interval, Byhour: hours, Byminute: minutes})
	if err != nil {
		t.Fatal(err)
	}
	rr.SetBudget(60_000)
	first, ok := rr.Iterator()()
	return ok && !first.After(start.Add(horizon))
}

// TestReachabilityCheckIsExact is the guard on the Compile check
// (TASK-4100 r1): over a grid, Compile refuses with CodeUnsatisfiable IFF the
// vendored engine, run directly, yields nothing within a horizon that covers
// the lcm of INTERVAL and the day. A disagreement either way is a failure: a
// refusal of a schedule that fires is the worse one.
func TestReachabilityCheckIsExact(t *testing.T) {
	intervals := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 15, 20, 24, 30, 45, 60, 90, 120, 360, 720, 1440, 1441}
	byHours := [][]int{nil, {0}, {1}, {0, 12}, {5, 17}}
	byMinutes := [][]int{nil, {0}, {1}, {30}, {0, 30}, {7, 59}}
	starts := [][2]int{{0, 0}, {0, 1}, {5, 30}, {23, 59}}
	freqs := map[string]rrule.Frequency{"HOURLY": rrule.HOURLY, "MINUTELY": rrule.MINUTELY}
	cells, refused := 0, 0
	for _, freq := range []string{"HOURLY", "MINUTELY"} {
		for _, iv := range intervals {
			for _, hs := range byHours {
				for _, ms := range byMinutes {
					for _, st := range starts {
						w := Wall{Year: 2025, Month: time.March, Day: 3, Hour: st[0], Minute: st[1]}
						rule := fmt.Sprintf("FREQ=%s;INTERVAL=%d%s%s", freq, iv, list("BYHOUR", hs), list("BYMINUTE", ms))
						_, err := Compile(Spec{Start: w, Zone: "UTC", Rule: rule})
						var me *MalformedError
						got := errors.As(err, &me) && me.Code == CodeUnsatisfiable
						if err != nil && !got {
							t.Fatalf("%s from %02d:%02d: %v", rule, st[0], st[1], err)
						}
						fires := engineFires(t, freqs[freq], w.naive(), iv, hs, ms, 1500*24*time.Hour)
						cells++
						if got {
							refused++
						}
						if got == fires {
							t.Errorf("%s from %02d:%02d: Compile refused=%v but the engine fires=%v", rule, st[0], st[1], got, fires)
						}
					}
				}
			}
		}
	}
	t.Logf("%d cells, %d refused, 0 disagreements wanted", cells, refused)
	if refused == 0 || refused == cells {
		t.Fatalf("%d of %d refused: the grid does not exercise both sides", refused, cells)
	}
}

func TestReachabilityNamedRules(t *testing.T) {
	start := mustWall(t, "2025-03-03T00:00")
	for _, rule := range []string{"FREQ=MINUTELY;INTERVAL=60;BYMINUTE=1", "FREQ=MINUTELY;INTERVAL=2;BYMINUTE=1", "FREQ=HOURLY;INTERVAL=24;BYHOUR=1"} {
		_, err := Compile(Spec{Start: start, Zone: "UTC", Rule: rule})
		var me *MalformedError
		if !errors.As(err, &me) || me.Code != CodeUnsatisfiable {
			t.Errorf("%s: err = %v; want unsatisfiable", rule, err)
		}
		// An RDATE still fires, so the same rule with one is accepted.
		s := Spec{Start: start, Zone: "UTC", Rule: rule, RDates: []Wall{mustWall(t, "2025-03-04T08:00")}}
		if _, err := Compile(s); err != nil {
			t.Errorf("%s with an RDATE: %v", rule, err)
		}
		// A part the check does not model is never refused, whatever it does.
		s = Spec{Start: start, Zone: "UTC", Rule: rule + ";BYSECOND=0"}
		if _, err := Compile(s); err != nil {
			t.Errorf("%s with BYSECOND: %v", rule, err)
		}
	}
	for _, rule := range []string{"FREQ=MINUTELY;INTERVAL=60;BYMINUTE=0", "FREQ=MINUTELY;INTERVAL=2;BYMINUTE=0", "FREQ=HOURLY;INTERVAL=24;BYHOUR=0"} {
		compile(t, Spec{Start: start, Zone: "UTC", Rule: rule})
	}
}
