package schedule

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDSTGapNewYork(t *testing.T) { runRows(t, rowsNamed(t, "dst-daily-ny-gap")) }
func TestDSTOverlapNewYork(t *testing.T) {
	runRows(t, rowsNamed(t, "dst-daily-ny-overlap"))
}
func TestDSTGapLondon(t *testing.T) { runRows(t, rowsNamed(t, "dst-daily-london-gap")) }
func TestDSTOverlapLondon(t *testing.T) {
	runRows(t, rowsNamed(t, "dst-daily-london-overlap"))
}

// TestDSTPopulationTable runs every DST row of the doc.go table: weekly,
// monthly, yearly, hourly, minutely, BYHOUR collisions, RDATE, EXDATE, UNTIL.
func TestDSTPopulationTable(t *testing.T) {
	var rows []Row
	for _, r := range ExtraRows {
		if strings.HasPrefix(r.Name, "dst-") {
			rows = append(rows, r)
		}
	}
	if len(rows) != 19 {
		t.Fatalf("%d dst rows; want 19", len(rows))
	}
	runRows(t, rows)
}

func TestDSTAllDayWeeklyNoDrift(t *testing.T) { TestAllDayWeeklyNoDriftAcrossDST(t) }

func TestDSTFloatingViewZone(t *testing.T) {
	c := compile(t, Spec{Start: mustWall(t, "2025-03-08T02:30"), Floating: true, Rule: "FREQ=DAILY;COUNT=3"})
	from, to := inst(t, "2025-03-01T00:00:00Z"), inst(t, "2025-03-20T00:00:00Z")
	ny, err := c.Between(from, to, mustLoc(t, nyZone), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	wantAts(t, ny, "2025-03-08T07:30:00Z", "2025-03-09T07:30:00Z", "2025-03-10T06:30:00Z")
	phx, _ := c.Between(from, to, mustLoc(t, "America/Phoenix"), Limits{})
	wantAts(t, phx, "2025-03-08T09:30:00Z", "2025-03-09T09:30:00Z", "2025-03-10T09:30:00Z")
}

// dstRules is one rule per FREQ and path of the doc.go table, each hitting
// the gap and the overlap hours of both zones below (01:00-02:59 local).
func dstRules() map[string]string {
	days := make([]string, 31)
	for i := range days {
		days[i] = strconv.Itoa(i + 1)
	}
	every := strings.Join(days, ",")
	return map[string]string{
		"daily":         "FREQ=DAILY;BYHOUR=1,2",
		"weekly":        "FREQ=WEEKLY;BYDAY=SU,MO,TU,WE,TH,FR,SA;BYHOUR=1,2",
		"monthly":       "FREQ=MONTHLY;BYMONTHDAY=" + every + ";BYHOUR=1,2",
		"yearly":        "FREQ=YEARLY;BYMONTH=3,10,11;BYMONTHDAY=" + every + ";BYHOUR=1,2",
		"hourly-bare":   "FREQ=HOURLY",
		"minutely-bare": "FREQ=MINUTELY;INTERVAL=30",
		"hourly-by":     "FREQ=HOURLY;BYHOUR=1,2,3",
		"minutely-by":   "FREQ=MINUTELY;INTERVAL=15;BYHOUR=1,2,3",
	}
}

// TestOutputStrictlyIncreasingAcrossDST is the ORDER INVARIANT: Resolve is
// not monotonic inside a gap (02:59 -> 07:59Z but 03:00 -> 07:00Z), yet the
// output of Between and NextAfter is strictly increasing in At with no
// duplicate, across a gap and an overlap, for every FREQ.
func TestOutputStrictlyIncreasingAcrossDST(t *testing.T) {
	lim := Limits{MaxWindow: 400 * 24 * time.Hour, MaxInstances: 100000, MaxDuration: 30 * time.Second}
	from, to := inst(t, "2025-03-01T00:00:00Z"), inst(t, "2025-11-30T00:00:00Z")
	// Each zone's two 2025 transitions, and a gap-day pair that inverts under
	// Resolve with no collision: the RDATE sits in the gap (resolves to a LATER
	// instant) and precedes the rule's own post-gap occurrence in wall order.
	zones := map[string]struct {
		trs               [2]string
		ruleAt, gapRDates string
	}{
		nyZone:          {[2]string{"2025-03-09T07:00:00Z", "2025-11-02T06:00:00Z"}, "03:15", "2025-03-09T02:45"},
		"Europe/London": {[2]string{"2025-03-30T01:00:00Z", "2025-10-26T01:00:00Z"}, "02:15", "2025-03-30T01:45"},
	}
	for zone, z := range zones {
		specs := map[string]Spec{}
		for name, rule := range dstRules() {
			specs[name] = pinned(t, zone, "2025-03-01T00:00", rule)
		}
		inv := pinned(t, zone, "2025-03-01T"+z.ruleAt, "FREQ=DAILY")
		inv.RDates = []Wall{mustWall(t, z.gapRDates)}
		specs["daily-rdate-inversion"] = inv
		trs := z.trs
		for name, s := range specs {
			c := compile(t, s)
			occ, err := c.Between(from, to, nil, lim)
			if err != nil {
				t.Fatalf("%s %s: %v", zone, name, err)
			}
			if len(occ) == 0 {
				t.Fatalf("%s %s: no occurrences", zone, name)
			}
			for i := 1; i < len(occ); i++ {
				if !occ[i].At.After(occ[i-1].At) {
					t.Fatalf("%s %s: not strictly increasing at %d: %v then %v", zone, name, i, occ[i-1], occ[i])
				}
			}
			for _, tr := range trs {
				lo, hi := inst(t, tr).Add(-30*time.Hour), inst(t, tr).Add(30*time.Hour)
				win, err := c.Between(lo, hi, nil, lim)
				if err != nil {
					t.Fatal(err)
				}
				chain := chainBetween(t, c, lo, hi, nil, lim)
				if len(win) != len(chain) {
					t.Fatalf("%s %s %s: Between %d vs chain %d", zone, name, tr, len(win), len(chain))
				}
				for i := range win {
					if win[i] != chain[i] {
						t.Fatalf("%s %s %s #%d: Between %v vs chain %v", zone, name, tr, i, win[i], chain[i])
					}
				}
			}
		}
	}
}
