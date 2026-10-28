package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Rule-text limits (TASK-4100, ruling Q7).
const (
	maxRuleBytes  = 512
	maxInterval   = 1_000_000
	maxCount      = 2_147_483_647
	freqDaily     = "DAILY"
	freqWeekly    = "WEEKLY"
	freqMonthly   = "MONTHLY"
	freqYearly    = "YEARLY"
	freqHourly    = "HOURLY"
	freqMinutely  = "MINUTELY"
	untilKindDate = "date"
	untilKindWall = "wall"
	untilKindInst = "instant"
)

var weekdays = map[string]time.Weekday{
	"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday,
}

// ByDay is one BYDAY item: a weekday with an optional ordinal (N == 0 means
// every such weekday).
type ByDay struct {
	N   int
	Day time.Weekday
}

// Until is a parsed UNTIL. A date is inclusive through 23:59:59 of that day,
// a wall is read in the schedule's effective zone, an instant (the Z form) is
// absolute.
type Until struct {
	Kind    string    // "date", "wall" or "instant"
	Wall    Wall      // date and wall kinds
	Instant time.Time // instant kind, UTC
}

// Rule is a strictly parsed RRULE value. Zero means absent for every field.
type Rule struct {
	Freq       string
	Interval   int
	Count      int
	Until      *Until
	Wkst       string
	ByMonth    []int
	ByWeekNo   []int
	ByYearDay  []int
	ByMonthDay []int // BYMONTHDAY
	ByDay      []ByDay
	ByHour     []int
	ByMinute   []int
	BySecond   []int
	BySetPos   []int
}

// ParseRule reads an RRULE value (no "RRULE:" prefix) strictly: RFC 5545
// 3.3.10 grammar, every combination the RFC forbids, and rules that can never
// produce an occurrence. Names and values are case-insensitive. Every refusal
// is a *MalformedError. Checks that need the schedule's Start are in Validate.
func ParseRule(s string) (Rule, error) {
	var r Rule
	if len(s) > maxRuleBytes {
		return r, malformed(CodeRange, "", s, "rule longer than 512 bytes")
	}
	if s == "" {
		return r, malformed(CodeSyntax, "", "", "empty rule")
	}
	seen := map[string]bool{}
	for _, p := range strings.Split(s, ";") {
		eq := strings.IndexByte(p, '=')
		if eq <= 0 || eq == len(p)-1 {
			return r, malformed(CodeSyntax, "", p, "want NAME=VALUE parts separated by single semicolons")
		}
		name, val := strings.ToUpper(p[:eq]), p[eq+1:]
		if strings.Trim(name, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" { // "RRULE:FREQ", a space, a quote
			return r, malformed(CodeSyntax, "", p, "a part name is letters only; give the value without an RRULE: prefix")
		}
		if seen[name] {
			return r, malformed(CodeDuplicatePart, name, val, "part given twice")
		}
		seen[name] = true
		if err := r.setPart(name, val); err != nil {
			return r, err
		}
	}
	if err := r.checkCombinations(seen); err != nil {
		return r, err
	}
	if n := periodDensity(&r); n > maxPeriodCandidates {
		return r, malformed(CodeRange, "FREQ", r.Freq, fmt.Sprintf("the rule can produce up to %d occurrences per %s period; the limit is %d", n, r.Freq, maxPeriodCandidates))
	}
	return r, nil
}

func (r *Rule) setPart(name, val string) error {
	var err error
	switch name {
	case "FREQ":
		f := strings.ToUpper(val)
		switch f {
		case freqDaily, freqWeekly, freqMonthly, freqYearly, freqHourly, freqMinutely:
			r.Freq = f
		case "SECONDLY": // the tick is 30 s and no cadence needs it (ruling Q3)
			return malformed(CodeUnsupported, "FREQ", val, "SECONDLY is not supported")
		default:
			return malformed(CodeRange, "FREQ", val, "want DAILY, WEEKLY, MONTHLY, YEARLY, HOURLY or MINUTELY")
		}
	case "INTERVAL":
		r.Interval, err = parseBounded("INTERVAL", val, false, 0, 1, maxInterval)
	case "COUNT":
		r.Count, err = parseBounded("COUNT", val, false, 0, 1, maxCount)
	case "UNTIL":
		r.Until, err = parseUntil(val)
	case "WKST":
		if _, ok := weekdays[strings.ToUpper(val)]; !ok {
			return malformed(CodeSyntax, "WKST", val, "want a weekday: MO TU WE TH FR SA SU")
		}
		r.Wkst = strings.ToUpper(val)
	case "BYMONTH":
		r.ByMonth, err = parseList(name, val, false, 2, 1, 12)
	case "BYWEEKNO":
		r.ByWeekNo, err = parseList(name, val, true, 2, 1, 53)
	case "BYYEARDAY":
		r.ByYearDay, err = parseList(name, val, true, 3, 1, 366)
	case "BYMONTHDAY":
		r.ByMonthDay, err = parseList(name, val, true, 2, 1, 31)
	case "BYDAY":
		r.ByDay, err = parseByDay(val, 2)
	case "BYHOUR":
		r.ByHour, err = parseList(name, val, false, 2, 0, 23)
	case "BYMINUTE":
		r.ByMinute, err = parseList(name, val, false, 2, 0, 59)
	case "BYSECOND":
		r.BySecond, err = parseList(name, val, false, 2, 0, 59)
	case "BYSETPOS":
		r.BySetPos, err = parseList(name, val, true, 3, 1, 366)
	case "RSCALE", "SKIP":
		return malformed(CodeUnsupported, name, val, "calendar scales other than Gregorian are not supported")
	default:
		return malformed(CodeUnknownPart, name, val, "not a supported RRULE part")
	}
	return err
}

// rfcInt reads RFC 5545 3.3.10 integers. Check the whole grammar before
// conversion so a malformed suffix remains syntax even after huge digits.
// maxDigits == 0 allows unlimited digits (COUNT and INTERVAL).
func rfcInt(s string, signed bool, maxDigits int) (n int, ok, overflow bool) {
	digits := s
	if signed && len(digits) > 0 && (digits[0] == '+' || digits[0] == '-') {
		digits = digits[1:]
	}
	if len(digits) == 0 || (maxDigits > 0 && len(digits) > maxDigits) {
		return 0, false, false
	}
	for i := range digits {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false, false
		}
	}
	n, err := strconv.Atoi(s)
	if err == nil {
		return n, true, false
	}
	return 0, false, true // valid decimal grammar: only int overflow remains
}

func parseBounded(part, val string, signed bool, maxDigits, lo, hi int) (int, error) {
	n, ok, over := rfcInt(val, signed, maxDigits)
	switch {
	case !ok && !over:
		return 0, malformed(CodeSyntax, part, val, "want an integer")
	case over || n < lo || n > hi:
		return 0, malformed(CodeRange, part, val, "out of range "+strconv.Itoa(lo)+".."+strconv.Itoa(hi))
	}
	return n, nil
}

// parseList reads a comma list. signed lists accept -hi..-lo as well and
// refuse 0; unsigned lists accept lo..hi.
func parseList(part, val string, signed bool, maxDigits, lo, hi int) ([]int, error) {
	items := strings.Split(val, ",")
	out := make([]int, 0, len(items))
	for _, it := range items {
		n, ok, over := rfcInt(it, signed, maxDigits)
		if !ok && !over {
			return nil, malformed(CodeSyntax, part, it, "want a comma-separated list of integers")
		}
		a := n
		if signed && a < 0 {
			a = -a
		}
		if over || a < lo || a > hi || (!signed && n < 0) {
			detail := "out of range " + strconv.Itoa(lo) + ".." + strconv.Itoa(hi)
			if signed {
				detail += " or -" + strconv.Itoa(hi) + "..-" + strconv.Itoa(lo)
			}
			return nil, malformed(CodeRange, part, it, detail)
		}
		out = append(out, n)
	}
	return out, nil
}

func parseByDay(val string, maxDigits int) ([]ByDay, error) {
	var out []ByDay
	for _, it := range strings.Split(val, ",") {
		u := strings.ToUpper(it)
		if len(u) < 2 {
			return nil, malformed(CodeSyntax, "BYDAY", it, "want [ordinal]weekday, e.g. MO or -1FR")
		}
		day, ok := weekdays[u[len(u)-2:]]
		if !ok {
			return nil, malformed(CodeSyntax, "BYDAY", it, "want [ordinal]weekday, e.g. MO or -1FR")
		}
		bd := ByDay{Day: day}
		if pre := u[:len(u)-2]; pre != "" {
			n, ok, over := rfcInt(pre, true, maxDigits)
			if !ok && !over {
				return nil, malformed(CodeSyntax, "BYDAY", it, "want [ordinal]weekday, e.g. MO or -1FR")
			}
			if over || n == 0 || n > 53 || n < -53 {
				return nil, malformed(CodeRange, "BYDAY", it, "ordinal out of range 1..53 or -53..-1")
			}
			bd.N = n
		}
		out = append(out, bd)
	}
	return out, nil
}

// parseUntil reads YYYYMMDD, YYYYMMDDTHHMMSS or YYYYMMDDTHHMMSSZ.
func parseUntil(val string) (*Until, error) {
	u := strings.ToUpper(val)
	layout, kind := "", ""
	switch len(u) {
	case 8:
		layout, kind = "20060102", untilKindDate
	case 15:
		layout, kind = "20060102T150405", untilKindWall
	case 16:
		layout, kind = "20060102T150405Z", untilKindInst
	default:
		return nil, malformed(CodeSyntax, "UNTIL", val, "want YYYYMMDD, YYYYMMDDTHHMMSS or YYYYMMDDTHHMMSSZ")
	}
	t, err := time.Parse(layout, u)
	if err != nil {
		return nil, malformed(CodeSyntax, "UNTIL", val, "want YYYYMMDD, YYYYMMDDTHHMMSS or YYYYMMDDTHHMMSSZ")
	}
	if t.Year() < minYear || t.Year() > maxYear {
		return nil, malformed(CodeRange, "UNTIL", val, "year out of range 1970..2200")
	}
	return &Until{
		Kind:    kind,
		Instant: t.UTC(),
		Wall: Wall{
			Year: t.Year(), Month: t.Month(), Day: t.Day(),
			Hour: t.Hour(), Minute: t.Minute(), Second: t.Second(),
			AllDay: kind == untilKindDate,
		},
	}, nil
}

func (r *Rule) checkCombinations(seen map[string]bool) error {
	if r.Freq == "" {
		return malformed(CodeMissingPart, "FREQ", "", "FREQ is required")
	}
	combo := func(part, detail string) error { return malformed(CodeCombination, part, "", detail) }
	if r.Count != 0 && r.Until != nil {
		return combo("UNTIL", "COUNT and UNTIL must not both be given (RFC 5545 3.3.10)")
	}
	if len(r.ByWeekNo) > 0 && r.Freq != freqYearly {
		return combo("BYWEEKNO", "BYWEEKNO is only valid with FREQ=YEARLY")
	}
	if len(r.ByYearDay) > 0 && (r.Freq == freqDaily || r.Freq == freqWeekly || r.Freq == freqMonthly) {
		return combo("BYYEARDAY", "BYYEARDAY is not valid with DAILY, WEEKLY or MONTHLY")
	}
	if len(r.ByMonthDay) > 0 && r.Freq == freqWeekly {
		return combo("BYMONTHDAY", "BYMONTHDAY is not valid with FREQ=WEEKLY")
	}
	for _, d := range r.ByDay {
		if d.N == 0 {
			continue
		}
		switch {
		case r.Freq != freqMonthly && r.Freq != freqYearly:
			return combo("BYDAY", "an ordinal BYDAY is only valid with MONTHLY or YEARLY")
		case r.Freq == freqMonthly && (d.N > 5 || d.N < -5):
			return malformed(CodeRange, "BYDAY", strconv.Itoa(d.N), "a monthly ordinal is within 1..5 or -5..-1")
		case r.Freq == freqYearly && len(r.ByWeekNo) > 0:
			return combo("BYDAY", "an ordinal BYDAY is not valid together with BYWEEKNO")
		}
	}
	if len(r.BySetPos) > 0 && !(seen["BYMONTH"] || seen["BYWEEKNO"] || seen["BYYEARDAY"] || seen["BYMONTHDAY"] ||
		seen["BYDAY"] || seen["BYHOUR"] || seen["BYMINUTE"] || seen["BYSECOND"]) {
		return combo("BYSETPOS", "BYSETPOS needs another BY part to select from")
	}
	return r.checkSatisfiable()
}

// checkSatisfiable refuses a BYMONTHDAY that no month in scope can ever hold
// (Feb 30, the 31st of a 30-day month). Left alone such a rule makes the
// expander walk to year 9999 (about 1 s, TASK-4100). Feb 29 counts as
// satisfiable: a leap year has it.
func (r *Rule) checkSatisfiable() error {
	if len(r.ByMonthDay) == 0 {
		return nil
	}
	months := r.ByMonth
	if len(months) == 0 {
		months = []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	}
	longest := [13]int{0, 31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	for _, m := range months {
		for _, d := range r.ByMonthDay {
			if d < 0 {
				d = -d
			}
			if d <= longest[m] {
				return nil
			}
		}
	}
	return malformed(CodeUnsatisfiable, "BYMONTHDAY", "", "no month in scope has such a day, so the rule never produces an occurrence")
}

// neverFires says an HOURLY or MINUTELY rule, stepped from start, provably
// never lands on its BYHOUR and BYMINUTE values (TASK-4100 r1). The library
// steps naive walls, so the offsets reachable from the start time-of-day are
// exact: with INTERVAL i and g = gcd(i, 24) hours or gcd(i, 1440) minutes,
// a time-of-day is reachable iff it is congruent to the start's modulo g.
// It only answers for a rule made of FREQ, INTERVAL, BYHOUR and BYMINUTE; any
// other part (a BY filter, BYSECOND, COUNT, UNTIL) can change what fires, and
// the caller leaves RDATEs out, so those rules are never refused here.
func (r *Rule) neverFires(start Wall) bool {
	if (r.Freq != freqHourly && r.Freq != freqMinutely) || len(r.ByHour)+len(r.ByMinute) == 0 ||
		r.Count != 0 || r.Until != nil || len(r.ByMonth)+len(r.ByWeekNo)+len(r.ByYearDay)+len(r.ByMonthDay)+
		len(r.ByDay)+len(r.BySecond)+len(r.BySetPos) > 0 {
		return false
	}
	hours, minutes := r.ByHour, r.ByMinute
	if len(hours) == 0 {
		hours = seq(24)
	}
	if r.Freq == freqHourly { // the minute is free; only the hour is stepped
		g := gcd(max(r.Interval, 1), 24)
		for _, h := range hours {
			if (h-start.Hour)%g == 0 {
				return false
			}
		}
		return true
	}
	if len(minutes) == 0 {
		minutes = seq(60)
	}
	g := gcd(max(r.Interval, 1), 1440)
	for _, h := range hours {
		for _, m := range minutes {
			if (60*(h-start.Hour)+m-start.Minute)%g == 0 {
				return false
			}
		}
	}
	return true
}

// seq is 0..n-1.
func seq(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// Validate runs the checks that need the schedule's Start. loc is the pinned
// zone, or nil for a floating schedule. It refuses an all-day schedule with a
// sub-daily FREQ or a BYHOUR, BYMINUTE or BYSECOND, a Z UNTIL on a floating
// schedule (it has no instant), and an UNTIL before Start.
func (r Rule) Validate(start Wall, loc *time.Location) error {
	if start.AllDay {
		switch {
		case r.Freq == freqHourly || r.Freq == freqMinutely:
			return malformed(CodeCombination, "FREQ", r.Freq, "an all-day schedule cannot repeat by the hour or minute")
		case len(r.ByHour) > 0:
			return malformed(CodeCombination, "BYHOUR", "", "an all-day schedule has no time of day")
		case len(r.ByMinute) > 0:
			return malformed(CodeCombination, "BYMINUTE", "", "an all-day schedule has no time of day")
		case len(r.BySecond) > 0:
			return malformed(CodeCombination, "BYSECOND", "", "an all-day schedule has no time of day")
		}
	}
	u := r.Until
	if u == nil {
		return nil
	}
	if u.Kind == untilKindInst {
		if loc == nil {
			return malformed(CodeCombination, "UNTIL", u.Instant.Format("20060102T150405Z"), "a floating schedule has no instant: give UNTIL without Z")
		}
		if u.Instant.Before(Resolve(loc, start)) {
			return malformed(CodeRange, "UNTIL", u.Instant.Format("20060102T150405Z"), "UNTIL is before Start")
		}
		return nil
	}
	if u.Wall.naive().Before(Wall{Year: start.Year, Month: start.Month, Day: start.Day,
		Hour: start.Hour, Minute: start.Minute, Second: start.Second, AllDay: u.Kind == untilKindDate}.naive()) {
		return malformed(CodeRange, "UNTIL", u.Wall.String(), "UNTIL is before Start")
	}
	return nil
}

// String is the canonical spelling: upper-case names, parts in the fixed
// order FREQ, INTERVAL, COUNT, UNTIL, WKST, BYMONTH, BYWEEKNO, BYYEARDAY, BYMONTHDAY, BYDAY, BYHOUR, BYMINUTE, BYSECOND, BYSETPOS, list items as given, INTERVAL and WKST only when
// given.
func (r Rule) String() string {
	ints := func(name string, v []int) string {
		if len(v) == 0 {
			return ""
		}
		s := make([]string, len(v))
		for i, n := range v {
			s[i] = strconv.Itoa(n)
		}
		return name + "=" + strings.Join(s, ",")
	}
	parts := []string{"FREQ=" + r.Freq}
	add := func(p string) {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if r.Interval != 0 {
		add("INTERVAL=" + strconv.Itoa(r.Interval))
	}
	if r.Count != 0 {
		add("COUNT=" + strconv.Itoa(r.Count))
	}
	if u := r.Until; u != nil {
		v := u.Wall.naive().Format("20060102")
		if u.Kind != untilKindDate {
			v = u.Wall.naive().Format("20060102T150405")
		}
		if u.Kind == untilKindInst {
			v += "Z"
		}
		add("UNTIL=" + v)
	}
	if r.Wkst != "" {
		add("WKST=" + r.Wkst)
	}
	add(ints("BYMONTH", r.ByMonth))
	add(ints("BYWEEKNO", r.ByWeekNo))
	add(ints("BYYEARDAY", r.ByYearDay))
	add(ints("BYMONTHDAY", r.ByMonthDay))
	if len(r.ByDay) > 0 {
		names := map[time.Weekday]string{}
		for k, v := range weekdays {
			names[v] = k
		}
		s := make([]string, len(r.ByDay))
		for i, d := range r.ByDay {
			if d.N != 0 {
				s[i] = strconv.Itoa(d.N)
			}
			s[i] += names[d.Day]
		}
		add("BYDAY=" + strings.Join(s, ","))
	}
	add(ints("BYHOUR", r.ByHour))
	add(ints("BYMINUTE", r.ByMinute))
	add(ints("BYSECOND", r.BySecond))
	add(ints("BYSETPOS", r.BySetPos))
	return strings.Join(parts, ";")
}
