package schedule

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

var worstAdmittedRule = "FREQ=YEARLY;BYDAY=SU,MO,TU,WE,TH,FR,SA;BYHOUR=0,1,2,3,4,5,6,7,8;BYMINUTE=0,20,40"

// densityPeriod labels UTC wall candidates with their FREQ period. Weekly
// buckets use the rule's WKST, including the week spanning a year boundary.
func densityPeriod(tm time.Time, r *Rule) time.Time {
	y, m, d := tm.Date()
	h, min := 0, 0
	switch r.Freq {
	case freqYearly:
		m, d = time.January, 1
	case freqMonthly:
		d = 1
	case freqWeekly:
		wkst := time.Monday
		if r.Wkst != "" {
			wkst = weekdays[r.Wkst]
		}
		d -= (int(tm.Weekday()) - int(wkst) + 7) % 7
	case freqHourly:
		h = tm.Hour()
	case freqMinutely:
		h, min = tm.Hour(), tm.Minute()
	}
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

func TestPeriodDensityBoundsGenerator(t *testing.T) {
	parts := []struct{ name, value string }{
		{"BYMONTH", "1,2"}, {"BYWEEKNO", "1,2"}, {"BYYEARDAY", "1,2"},
		{"BYMONTHDAY", "1,2"}, {"BYDAY", "MO,TU"}, {"BYHOUR", "0,1"},
		{"BYMINUTE", "0,1"}, {"BYSECOND", "0,0,30"}, {"BYSETPOS", "1,-1"},
	}
	for _, freq := range []string{freqYearly, freqMonthly, freqWeekly, freqDaily, freqHourly, freqMinutely} {
		allowed := func(name string) bool {
			switch name {
			case "BYWEEKNO":
				return freq == freqYearly
			case "BYYEARDAY":
				return freq != freqDaily && freq != freqWeekly && freq != freqMonthly
			case "BYMONTHDAY":
				return freq != freqWeekly
			}
			return true
		}
		var shapes []string
		for _, p := range parts {
			if allowed(p.name) {
				shape := ";" + p.name + "=" + p.value
				if p.name == "BYSETPOS" {
					shape += ";BYSECOND=0,30" // BYSETPOS requires another BY part.
				}
				shapes = append(shapes, shape)
			}
		}
		for _, pair := range [][2]int{{4, 5}, {5, 6}, {6, 7}, {3, 5}} {
			a, b := parts[pair[0]], parts[pair[1]]
			if allowed(a.name) && allowed(b.name) {
				shapes = append(shapes, ";"+a.name+"="+a.value+";"+b.name+"="+b.value)
			}
		}
		tight := ";BYDAY=SU,MO,TU,WE,TH,FR,SA"
		switch freq {
		case freqDaily:
			tight = ";BYHOUR=0,1"
		case freqHourly:
			tight = ";BYMINUTE=0,1"
		case freqMinutely:
			tight = ";BYSECOND=0,30"
		}
		shapes = append(shapes, tight)
		for i, shape := range shapes {
			t.Run(freq+shape, func(t *testing.T) {
				c := compile(t, pinned(t, "UTC", "2024-01-01T00:00", "FREQ="+freq+";WKST=SU"+shape))
				next := oracleRule(t, c, time.UTC, time.Time{}, 0).Iterator()
				buckets := map[time.Time]uint64{}
				// Finish four populated periods; sparse limiting parts may skip periods.
				for {
					tm, ok := next()
					if !ok {
						t.Fatal("generator ended before four populated periods")
					}
					key := densityPeriod(tm, c.rule)
					if _, seen := buckets[key]; !seen && len(buckets) == 4 {
						break
					}
					buckets[key]++
				}
				var peak uint64
				for _, n := range buckets {
					peak = max(peak, n)
				}
				bound := periodDensity(c.rule)
				if peak > bound {
					t.Fatalf("generator peak %d exceeds density %d", peak, bound)
				}
				if i == len(shapes)-1 {
					t.Logf("%s: peak/bound = %d/%d = %.3f", freq, peak, bound, float64(peak)/float64(bound))
					if peak != bound {
						t.Errorf("tight witness: peak %d; want bound %d", peak, bound)
					}
				}
			})
		}
	}
}

func TestDensityCapRefusesCheaply(t *testing.T) {
	hours, minutes := list("BYHOUR", seq(24)), list("BYMINUTE", seq(60))
	for _, row := range []struct{ rule, count string }{
		{"FREQ=YEARLY;BYDAY=SU,MO,TU,WE,TH,FR,SA" + hours + minutes + list("BYSECOND", seq(60)), "31622400"},
		{"FREQ=MONTHLY" + hours + minutes, "44640"},
	} {
		t.Run(row.count, func(t *testing.T) {
			assertRefusal := func(err error) {
				t.Helper()
				var me *MalformedError
				if !errors.As(err, &me) || me.Code != CodeRange || me.Part != "FREQ" ||
					!strings.Contains(me.Detail, row.count) || !strings.Contains(me.Detail, "limit is 10000") ||
					!strings.Contains(me.Detail, strings.Split(row.rule, ";")[0][5:]+" period") {
					t.Fatalf("want density range refusal naming count, period and limit; got %v", err)
				}
			}
			if row.count == "31622400" && len(row.rule) != 465 {
				t.Fatalf("reproduced rule is %d bytes; want 465", len(row.rule))
			}
			_, err := ParseRule(row.rule)
			assertRefusal(err)
			s := pinned(t, "UTC", "2024-01-01T00:00", row.rule)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			begin := time.Now()
			_, err = Compile(s)
			elapsed := time.Since(begin)
			runtime.ReadMemStats(&after)
			assertRefusal(err)
			if elapsed >= 50*time.Millisecond || after.TotalAlloc-before.TotalAlloc >= 1<<20 {
				t.Errorf("Compile refusal took %s and allocated %d bytes", elapsed, after.TotalAlloc-before.TotalAlloc)
			}
		})
	}
}

func TestDensityCensus(t *testing.T) {
	checkNoDensityRefusal := func(s Spec) {
		t.Helper()
		_, err := Compile(s)
		var me *MalformedError
		if errors.As(err, &me) && me.Code == CodeRange && me.Part == "FREQ" &&
			strings.HasPrefix(me.Detail, "the rule can produce up to ") && strings.HasSuffix(me.Detail, "the limit is 10000") {
			t.Errorf("density refused %s: %v", s.Rule, err)
		}
	}
	for _, rows := range [][]Row{RFCRows, RFCErratumRows} {
		for _, row := range rows {
			t.Run(row.Name, func(t *testing.T) { checkNoDensityRefusal(specFromICal(t, row.ICal)) })
		}
	}
	reachabilityGrid(func(freq string, iv int, hs, ms []int, st [2]int) {
		s := Spec{Start: Wall{Year: 2025, Month: time.March, Day: 3, Hour: st[0], Minute: st[1]}, Zone: "UTC",
			Rule: fmt.Sprintf("FREQ=%s;INTERVAL=%d%s%s", freq, iv, list("BYHOUR", hs), list("BYMINUTE", ms))}
		r := Rule{Freq: freq, Interval: iv, ByHour: hs, ByMinute: ms}
		if !r.neverFires(s.Start) {
			checkNoDensityRefusal(s)
		}
	})
	allDays := ";BYDAY=SU,MO,TU,WE,TH,FR,SA"
	for _, row := range []struct {
		name, rule string
		density    uint64
	}{
		{"yearly-hours", "FREQ=YEARLY" + allDays + list("BYHOUR", seq(24)), 8784},
		{"daily-minutes", "FREQ=DAILY" + list("BYHOUR", seq(24)) + list("BYMINUTE", seq(60)), 1440},
		{"weekly-hours", "FREQ=WEEKLY" + allDays + list("BYHOUR", seq(24)), 168},
		{"monthly-twice-daily", "FREQ=MONTHLY" + list("BYMONTHDAY", seq(31)[1:]) + ",31;BYHOUR=9,17", 62},
		{"yearly-hours-minute-zero", "FREQ=YEARLY" + allDays + list("BYHOUR", seq(24)) + ";BYMINUTE=0", 8784},
		{"worst-admitted", worstAdmittedRule, 9882},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := compile(t, pinned(t, "UTC", "2024-01-01T00:00", row.rule))
			if got := periodDensity(c.rule); got != row.density {
				t.Errorf("density = %d; want %d", got, row.density)
			}
		})
	}
}
