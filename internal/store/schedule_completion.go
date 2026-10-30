package store

import (
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// The completion anchor (TASK-4103 C2b, ruling Q6): "the next one is a
// recurrence interval after this one was DONE", not after it was due.
//
// That only has a meaning for a plain-interval rule, so the anchor is valid for
// FREQ plus INTERVAL (and an UNTIL), or for no rule at all (a one-shot, which
// done ends). A rule with a BYxxx part, a COUNT, RDATEs or EXDATEs describes
// particular calendar instants and has no "interval since completion".

// validateAnchorRule refuses anchor=completion on a rule that is not a plain
// interval. spec is the compiled, canonical timing.
func validateAnchorRule(anchor string, spec schedule.Spec) error {
	if anchor != "completion" {
		return nil
	}
	if _, ok, err := plainInterval(spec.Rule, len(spec.RDates) > 0 || len(spec.ExDates) > 0); err != nil {
		return err
	} else if !ok {
		return fieldErr("anchor", "combination",
			"anchor=completion needs a plain interval rule (FREQ and INTERVAL, optionally UNTIL) or none: BYxxx, COUNT, RDATES and EXDATES are for the calendar anchor")
	}
	return nil
}

// plainInterval parses a canonical rule and reports whether it is a plain
// interval. An empty rule is a one-shot and is plain.
func plainInterval(rule string, hasDates bool) (schedule.Rule, bool, error) {
	if hasDates {
		return schedule.Rule{}, false, nil
	}
	if rule == "" {
		return schedule.Rule{}, true, nil
	}
	r, err := schedule.ParseRule(rule)
	if err != nil {
		return r, false, err
	}
	plain := r.Count == 0 && len(r.ByMonth) == 0 && len(r.ByWeekNo) == 0 && len(r.ByYearDay) == 0 &&
		len(r.ByMonthDay) == 0 && len(r.ByDay) == 0 && len(r.ByHour) == 0 && len(r.ByMinute) == 0 &&
		len(r.BySecond) == 0 && len(r.BySetPos) == 0
	return r, plain, nil
}

// addMonths adds n calendar months to t's date and clamps the day to the end of
// the target month (Jan 31 + 1 month = Feb 28 or 29; Feb 29 + 1 year = Feb 28).
func addMonths(t time.Time, n int) (year int, month time.Month, day int) {
	y, m, d := t.Date()
	total := int(m) - 1 + n
	y += total / 12
	mm := total%12 + 1
	if last := time.Date(y, time.Month(mm)+1, 0, 0, 0, 0, 0, time.UTC).Day(); d > last {
		d = last
	}
	return y, time.Month(mm), d
}

// completionSpan is the exact interval of an HOURLY or MINUTELY rule, and 0 for
// the others (which keep the schedule's time of day).
func completionSpan(rule schedule.Rule) time.Duration {
	n := time.Duration(rule.Interval)
	if n == 0 {
		n = 1
	}
	switch rule.Freq {
	case "HOURLY":
		return n * time.Hour
	case "MINUTELY":
		return n * time.Minute
	}
	return 0
}

// completionWall is the wall the next occurrence of a plain-interval schedule
// starts from when it was completed at done: the completion date in loc plus the
// interval, at the schedule's time of day (an all-day schedule stays a date).
// HOURLY and MINUTELY have no time of day to keep, so they are done plus the
// interval to the second. ok is false when the result is beyond the years a
// wall can name.
func completionWall(sc *models.ItemSchedule, rule schedule.Rule, loc *time.Location, done time.Time) (schedule.Wall, bool, error) {
	n := rule.Interval
	if n == 0 {
		n = 1
	}
	start, err := schedule.ParseWall(sc.Dtstart)
	if err != nil {
		return schedule.Wall{}, false, err
	}
	var y int
	var m time.Month
	var d int
	switch rule.Freq {
	case "HOURLY", "MINUTELY":
		at := done.Add(completionSpan(rule)).In(loc)
		w := schedule.Wall{Year: at.Year(), Month: at.Month(), Day: at.Day(), Hour: at.Hour(), Minute: at.Minute(), Second: at.Second()}
		return w, w.Year <= 2200, nil
	case "DAILY", "WEEKLY":
		days := n
		if rule.Freq == "WEEKLY" {
			days = 7 * n
		}
		if n > 100_000 {
			return schedule.Wall{}, false, nil // past 2200 whatever the base
		}
		y, m, d = done.In(loc).AddDate(0, 0, days).Date()
	case "MONTHLY":
		if n > 12*300 {
			return schedule.Wall{}, false, nil
		}
		y, m, d = addMonths(done.In(loc), n)
	default: // YEARLY
		if n > 300 {
			return schedule.Wall{}, false, nil
		}
		y, m, d = addMonths(done.In(loc), 12*n)
	}
	w := schedule.Wall{Year: y, Month: m, Day: d, AllDay: start.AllDay}
	if !start.AllDay {
		w.Hour, w.Minute, w.Second = start.Hour, start.Minute, start.Second
	}
	return w, y <= 2200, nil
}
