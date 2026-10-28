package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Row is one conformance row. ICal is the DTSTART/RRULE/RDATE/EXDATE block as
// RFC 5545 prints it. Exp is a token list (see parseExp). Unbnd marks a row
// whose rule never ends: only the first len(Exp) instances are compared.
type Row struct {
	Name  string
	ICal  string
	Exp   string
	Unbnd bool
}

const hdr = "DTSTART;TZID=America/New_York:"

var NY = time.FixedZone("unused", 0) // replaced in init: rows are parsed against America/New_York

func init() {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	NY = loc
}

// parseExp expands the expected-instant DSL. Tokens are space separated:
//
//	YYYYMMDD            09:00 local on that date
//	YYYYMMDDTHHMM       that local time
//	A..B                every day A through B inclusive at 09:00
//	A..B/n              every n-th day
//	Z:YYYYMMDDTHHMMZ    an exact UTC instant (for the DST rows)
func parseExp(exp string, loc *time.Location) []time.Time {
	var out []time.Time
	for _, tok := range strings.Fields(exp) {
		switch {
		case strings.HasPrefix(tok, "Z:"):
			t, err := time.Parse("20060102T1504Z", tok[2:])
			if err != nil {
				panic(tok)
			}
			out = append(out, t)
		case strings.Contains(tok, ".."):
			i := strings.Index(tok, "..")
			a, b, step := tok[:i], tok[i+2:], 1
			if j := strings.Index(b, "/"); j >= 0 {
				step, _ = strconv.Atoi(b[j+1:])
				b = b[:j]
			}
			for d, end := localDay(a, loc), localDay(b, loc); !d.After(end); d = time.Date(d.Year(), d.Month(), d.Day()+step, 9, 0, 0, 0, loc) {
				out = append(out, d)
			}
		case strings.Contains(tok, "T"):
			t, err := time.ParseInLocation("20060102T1504", tok, loc)
			if err != nil {
				panic(tok)
			}
			out = append(out, t)
		default:
			out = append(out, localDay(tok, loc))
		}
	}
	return out
}

func localDay(s string, loc *time.Location) time.Time {
	t, err := time.ParseInLocation("20060102", s, loc)
	if err != nil {
		panic(s)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 9, 0, 0, 0, loc)
}

// times20 builds 09:00..16:40 every 20 minutes for the given yyyymmdd days.
func times20(days ...string) string {
	var b strings.Builder
	for _, d := range days {
		for h := 9; h <= 16; h++ {
			for _, m := range []string{"00", "20", "40"} {
				fmt.Fprintf(&b, "%sT%02d%s ", d, h, m)
			}
		}
	}
	return b.String()
}

// icalWall turns 19970902T090000 into a Wall.
func icalWall(t *testing.T, s string) Wall {
	t.Helper()
	return mustWall(t, s[:4]+"-"+s[4:6]+"-"+s[6:8]+"T"+s[9:11]+":"+s[11:13]+":"+s[13:15])
}

// specFromICal reads the DTSTART, RRULE, RDATE and EXDATE lines of a row.
func specFromICal(t *testing.T, ical string) Spec {
	t.Helper()
	var s Spec
	for _, line := range strings.Split(ical, "\n") {
		colon := strings.Index(line, ":")
		vals := strings.Split(line[colon+1:], ",")
		switch {
		case strings.HasPrefix(line, "DTSTART"):
			s.Zone = line[strings.Index(line, "TZID=")+5 : colon]
			s.Start = icalWall(t, vals[0])
		case strings.HasPrefix(line, "RRULE:"):
			s.Rule = line[len("RRULE:"):]
		case strings.HasPrefix(line, "RDATE"):
			for _, v := range vals {
				s.RDates = append(s.RDates, icalWall(t, v))
			}
		case strings.HasPrefix(line, "EXDATE"):
			for _, v := range vals {
				s.ExDates = append(s.ExDates, icalWall(t, v))
			}
		}
	}
	return s
}

var rowLimits = Limits{MaxWindow: 100 * 365 * 24 * time.Hour, MaxInstances: 100000, MaxDuration: 30 * time.Second}

// expandRow runs a row through the real package. A bounded row is read with
// one Between over 40 years; an unbounded row takes len(want) NextAfter steps.
func expandRow(t *testing.T, r Row, want int) []time.Time {
	t.Helper()
	spec := specFromICal(t, r.ICal)
	c, err := Compile(spec)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	epoch := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	var got []time.Time
	if r.Unbnd {
		for at := epoch; len(got) < want; {
			o, ok, err := c.NextAfter(at, nil, rowLimits)
			if err != nil || !ok {
				t.Fatalf("NextAfter(%s): ok=%v err=%v", at.Format(time.RFC3339), ok, err)
			}
			got, at = append(got, o.At), o.At
		}
		return got
	}
	occ, err := c.Between(epoch, Resolve(c.zone, spec.Start).AddDate(40, 0, 0), nil, rowLimits)
	if err != nil {
		t.Fatalf("Between: %v", err)
	}
	for _, o := range occ {
		got = append(got, o.At)
	}
	return got
}

func fmtTimes(ts []time.Time) string {
	s := make([]string, len(ts))
	for i, x := range ts {
		s[i] = x.UTC().Format("0102T1504Z")
	}
	return strings.Join(s, " ")
}

func runRows(t *testing.T, rows []Row) {
	t.Helper()
	for _, r := range rows {
		t.Run(r.Name, func(t *testing.T) {
			want := parseExp(r.Exp, NY)
			got := expandRow(t, r, len(want))
			if len(got) != len(want) {
				t.Fatalf("count: got %d want %d\n got: %s\nwant: %s", len(got), len(want), fmtTimes(got), fmtTimes(want))
			}
			for i := range want {
				if !got[i].Equal(want[i]) {
					t.Fatalf("instance %d: got %s want %s\n got: %s\nwant: %s", i, got[i].UTC().Format(time.RFC3339), want[i].UTC().Format(time.RFC3339), fmtTimes(got), fmtTimes(want))
				}
			}
		})
	}
}

// rowsNamed picks rows from every table by name; a missing name fails the test.
func rowsNamed(t *testing.T, names ...string) []Row {
	t.Helper()
	all := map[string]Row{}
	for _, tbl := range [][]Row{RFCRows, RFCErratumRows, ExtraRows} {
		for _, r := range tbl {
			all[r.Name] = r
		}
	}
	out := make([]Row, 0, len(names))
	for _, n := range names {
		r, ok := all[n]
		if !ok {
			t.Fatalf("no conformance row %q", n)
		}
		out = append(out, r)
	}
	return out
}

func TestRFC5545Section385_3(t *testing.T) { runRows(t, RFCRows) }

// TestRFC5545ErratumHourlyUntil is the expected-different row: the example as
// RFC 5545 prints it (erratum 3883) must give 09:00 and 12:00 EDT, and the
// corrected UNTIL keeps the printed three.
func TestRFC5545ErratumHourlyUntil(t *testing.T) {
	runRows(t, RFCErratumRows)
	got := expandRow(t, RFCErratumRows[0], 0)
	if want := parseExp("19970902T0900 19970902T1200", NY); fmtTimes(got) != fmtTimes(want) {
		t.Errorf("as printed: got %s; want only 09:00 and 12:00 EDT %s", fmtTimes(got), fmtTimes(want))
	}
	fixed := rowsNamed(t, "rfc33b-hourly-until-corrected-19Z")[0]
	if got := expandRow(t, fixed, 0); len(got) != 3 {
		t.Errorf("UNTIL=190000Z: got %d instances; want 3", len(got))
	}
}

func TestRFC5545RowCount(t *testing.T) {
	rows := append(append([]Row{}, RFCRows...), RFCErratumRows...)
	if len(rows) != 43 {
		t.Fatalf("%d RFC rows; want 43 (39 examples, 5b 9b 36b, 33b, and the erratum row)", len(rows))
	}
	names, nums := map[string]bool{}, map[int]bool{}
	for _, r := range rows {
		if names[r.Name] {
			t.Errorf("duplicate row name %q", r.Name)
		}
		names[r.Name] = true
		n, err := strconv.Atoi(r.Name[3:5])
		if err != nil || !strings.HasPrefix(r.Name, "rfc") {
			t.Fatalf("row %q does not start rfcNN", r.Name)
		}
		nums[n] = true
	}
	for n := 1; n <= 39; n++ {
		if !nums[n] {
			t.Errorf("RFC example %d has no row", n)
		}
	}
	if len(ExtraRows) != 30 {
		t.Errorf("%d extra rows; want 30", len(ExtraRows))
	}
}

func TestByMonthDay31SkipsShortMonths(t *testing.T) {
	runRows(t, rowsNamed(t, "bymonthday-31-skips-short-months"))
}

func TestByMonthDayMinus1LeapAndNonLeap(t *testing.T) {
	runRows(t, rowsNamed(t, "bymonthday-minus1-leap", "bymonthday-minus1-nonleap"))
}

// TestByMonthDayMixedPositiveNegative is RFC example 18: 1 and -1 together,
// started on the 30th.
func TestByMonthDayMixedPositiveNegative(t *testing.T) {
	runRows(t, rowsNamed(t, "rfc18-monthly-first-last-day-count10"))
}

func TestRDateExDateWithCount(t *testing.T) {
	runRows(t, rowsNamed(t, "rdate-exdate-count", "exdate-of-last-count-instance",
		"rdate-duplicate-of-rule-instance-deduped", "rdate-before-dtstart-sorted", "rdate-only-no-rrule", "oneshot-no-rrule"))
}
