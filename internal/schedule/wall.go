package schedule

import (
	"fmt"
	"time"
)

// Year bounds shared by every wall reading and every rule date (TASK-4100).
const (
	minYear = 1970
	maxYear = 2200
)

// Wall is a civil reading with no zone: what a clock on the wall says.
// An occurrence is identified by its At (or Wall plus offset): bare HOURLY
// and MINUTELY rules can repeat a Wall across a fall-back.
type Wall struct {
	Year                 int
	Month                time.Month
	Day                  int
	Hour, Minute, Second int
	AllDay               bool // a bare date; Hour, Minute and Second must be 0
}

// ParseWall reads "2006-01-02" (all-day), "2006-01-02T15:04" or
// "2006-01-02T15:04:05". A zone, a Z or an offset is refused: a Wall has none.
// Years outside 1970..2200 are refused.
func ParseWall(s string) (Wall, error) {
	layout := ""
	switch len(s) {
	case len("2006-01-02"):
		layout = "2006-01-02"
	case len("2006-01-02T15:04"):
		layout = "2006-01-02T15:04"
	case len("2006-01-02T15:04:05"):
		layout = "2006-01-02T15:04:05"
	default:
		return Wall{}, malformed(CodeWall, "", s, "want 2006-01-02, 2006-01-02T15:04 or 2006-01-02T15:04:05")
	}
	t, err := time.Parse(layout, s)
	if err != nil {
		return Wall{}, malformed(CodeWall, "", s, "not a real calendar date and time")
	}
	if t.Year() < minYear || t.Year() > maxYear {
		return Wall{}, malformed(CodeWall, "", s, fmt.Sprintf("year out of range %d..%d", minYear, maxYear))
	}
	return Wall{
		Year: t.Year(), Month: t.Month(), Day: t.Day(),
		Hour: t.Hour(), Minute: t.Minute(), Second: t.Second(),
		AllDay: len(s) == len("2006-01-02"),
	}, nil
}

// String is "2006-01-02" for an all-day Wall and "2006-01-02T15:04:05" else.
func (w Wall) String() string {
	d := fmt.Sprintf("%04d-%02d-%02d", w.Year, int(w.Month), w.Day)
	if w.AllDay {
		return d
	}
	return fmt.Sprintf("%sT%02d:%02d:%02d", d, w.Hour, w.Minute, w.Second)
}

// naive labels the reading as UTC, giving a time that orders and subtracts
// like the wall clock and that no DST rule can bend.
func (w Wall) naive() time.Time {
	if w.AllDay {
		return time.Date(w.Year, w.Month, w.Day, 0, 0, 0, 0, time.UTC)
	}
	return time.Date(w.Year, w.Month, w.Day, w.Hour, w.Minute, w.Second, 0, time.UTC)
}

// Resolve turns a wall reading into a UTC instant in loc (RFC 5545 3.3.5).
// A reading in a spring-forward gap does not exist: it takes the offset in
// force before the gap, which lands it after the gap (02:30 becomes 03:30). A
// reading in a fall-back overlap exists twice: it takes the earlier instant.
// An all-day Wall is 00:00 of its date under the same rules. A nil loc is UTC.
//
// It does not use time.Date's own choice. That choice is not stable: it put a
// New York gap before the gap and a London overlap after the overlap, which is
// the defect TASK-4100 measured. Resolve tries the offsets on either side of
// the reading and keeps the earliest instant that really has that offset.
func Resolve(loc *time.Location, w Wall) time.Time {
	naive := w.naive()
	if loc == nil || loc == time.UTC {
		return naive
	}
	offsetAt := func(t time.Time) int { _, o := t.In(loc).Zone(); return o }
	before := offsetAt(naive.Add(-24 * time.Hour))
	after := offsetAt(naive.Add(24 * time.Hour))

	var best time.Time
	found := false
	for _, o := range [2]int{before, after} {
		inst := naive.Add(-time.Duration(o) * time.Second)
		if offsetAt(inst) != o {
			continue // this offset is not in force at that instant: the reading is not valid under it
		}
		if !found || inst.Before(best) {
			best, found = inst, true
		}
	}
	if !found {
		return naive.Add(-time.Duration(before) * time.Second) // a gap
	}
	return best
}
