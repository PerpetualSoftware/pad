package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
	"github.com/PerpetualSoftware/pad/internal/usertz"
)

// Schedules ride `pad item remind`: the flags below switch it from the
// one-shot reminder to a schedule, and the subcommands resolve and inspect one.
// The rule grammar is internal/schedule's; this file only turns flags into the
// request and renders the answer. Rules the server alone can judge (the firing
// floor, anchor completion with a BYxxx part, an occurrence that never comes)
// are not copied here: the server's message is passed through unchanged.

// scheduleFlags are the schedule-mode flags of `pad item remind` and
// `pad item remind edit`. RemindAt and Rearm belong to the one-shot path; they
// are carried only so a mixed call is refused with a pointer to --start.
type scheduleFlags struct {
	Every, Start, Deadline, Window, Anchor, Audience, Tz string
	Floating                                             bool
	RemindAt, Rearm                                      string
}

const scheduleFlagHelp = `
  --start     wall time the first occurrence is read at: 2026-11-09T09:00, or 2026-11-09 for an all-day date (no Z, no offset)
  --every     cadence: daily, weekdays, weekly, biweekly, monthly, monthly-last, quarterly, yearly, or an RRULE such as FREQ=WEEKLY;BYDAY=MO
  --deadline  hard | soft: a due time rather than a check
  --window    a span from --start: a duration (90m, 2h, 3d) or an end wall (2026-11-09T17:00)
  --anchor    calendar (default) | completion (next one an interval after you mark it done)
  --audience  human | agent | both (default both)
  --tz        IANA zone the wall is read in (default: your account zone)
  --floating  read the wall in the zone of whoever it fires for; exclusive with --tz`

func bindScheduleFlags(cmd *cobra.Command, f *scheduleFlags) {
	cmd.Flags().StringVar(&f.Start, "start", "", "schedule: wall time, 2026-11-09T09:00 or 2026-11-09 (no Z or offset)")
	cmd.Flags().StringVar(&f.Every, "every", "", "schedule: daily|weekdays|weekly|biweekly|monthly|monthly-last|quarterly|yearly, or an RRULE")
	cmd.Flags().StringVar(&f.Deadline, "deadline", "", "schedule: hard|soft makes it a deadline")
	cmd.Flags().StringVar(&f.Window, "window", "", "schedule: a span from --start, a duration (90m, 2h, 3d) or an end wall")
	cmd.Flags().StringVar(&f.Anchor, "anchor", "", "schedule: calendar|completion")
	cmd.Flags().StringVar(&f.Audience, "audience", "", "schedule: human|agent|both")
	cmd.Flags().StringVar(&f.Tz, "tz", "", "schedule: IANA zone the wall is read in (default: your account zone)")
	cmd.Flags().BoolVar(&f.Floating, "floating", false, "schedule: read the wall in the recipient's zone; exclusive with --tz")
}

var scheduleModeFlagNames = []string{"every", "start", "deadline", "window", "anchor", "audience", "tz", "floating"}

func scheduleModeRequested(cmd *cobra.Command) bool {
	for _, n := range scheduleModeFlagNames {
		if cmd.Flags().Changed(n) {
			return true
		}
	}
	return false
}

func parseStartWall(s string) (schedule.Wall, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return schedule.Wall{}, fmt.Errorf("--start is required (a wall time such as 2026-11-09T09:00, or 2026-11-09 for an all-day date)")
	}
	w, err := schedule.ParseWall(s)
	if err != nil {
		return schedule.Wall{}, fmt.Errorf("--start %q is not a wall time: want 2026-11-09T09:00 or 2026-11-09, with no Z or offset (the zone comes from --tz)", s)
	}
	return w, nil
}

func normalizeEvery(s string) (string, error) {
	rule, err := schedule.NormalizeRule(s)
	if err != nil {
		return "", fmt.Errorf("--every %q: %v\nuse one of %s, or an RRULE such as FREQ=WEEKLY;BYDAY=MO",
			strings.TrimSpace(s), err, strings.Join(schedule.ShorthandNames(), ", "))
	}
	return rule, nil
}

func wallFromTime(t time.Time, allDay bool) schedule.Wall {
	w := schedule.Wall{Year: t.Year(), Month: t.Month(), Day: t.Day(), AllDay: allDay}
	if !allDay {
		w.Hour, w.Minute, w.Second = t.Hour(), t.Minute(), t.Second()
	}
	return w
}

func wallTime(w schedule.Wall) time.Time {
	return time.Date(w.Year, w.Month, w.Day, w.Hour, w.Minute, w.Second, 0, time.UTC)
}

var windowDurationRE = regexp.MustCompile(`^(\d+)([mhd])$`)

// windowEnd reads --window against the start wall: a duration is added on the
// wall clock, an end wall is taken as given.
func windowEnd(start schedule.Wall, s string) (schedule.Wall, error) {
	s = strings.TrimSpace(s)
	bad := fmt.Errorf("--window %q: want a duration (90m, 2h, 3d) or an end wall (2026-11-09T17:00)", s)
	if m := windowDurationRE.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 || n > 100000 {
			return schedule.Wall{}, bad
		}
		base := wallTime(start)
		switch m[2] {
		case "d":
			return wallFromTime(base.AddDate(0, 0, n), start.AllDay), nil
		case "h":
			if start.AllDay {
				return schedule.Wall{}, fmt.Errorf("--window %q: an all-day start takes a window in days (3d) or an end date", s)
			}
			return wallFromTime(base.Add(time.Duration(n)*time.Hour), false), nil
		default:
			if start.AllDay {
				return schedule.Wall{}, fmt.Errorf("--window %q: an all-day start takes a window in days (3d) or an end date", s)
			}
			return wallFromTime(base.Add(time.Duration(n)*time.Minute), false), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 || start.AllDay {
			return schedule.Wall{}, bad
		}
		return wallFromTime(wallTime(start).Add(d), false), nil
	}
	if w, err := schedule.ParseWall(s); err == nil {
		return w, nil
	}
	return schedule.Wall{}, bad
}

// updateWindowEnd is windowEnd for an edit: a duration needs the start given in
// the same call, an end wall does not.
func updateWindowEnd(start schedule.Wall, haveStart bool, s string) (schedule.Wall, error) {
	s = strings.TrimSpace(s)
	_, durErr := time.ParseDuration(s)
	if windowDurationRE.MatchString(s) || durErr == nil {
		if !haveStart {
			return schedule.Wall{}, fmt.Errorf("--window %q is a duration counted from the start; give --start too, or an end wall instead", s)
		}
	}
	return windowEnd(start, s)
}

func checkEnum(flag, v string, allowed ...string) error {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return fmt.Errorf("--%s %q: want %s", flag, v, strings.Join(allowed, " | "))
}

func checkTz(s string) error {
	if _, err := usertz.Validate(s); err != nil {
		return fmt.Errorf("--tz %q: %v", s, err)
	}
	return nil
}

func deadlineKind(v string) (string, error) {
	switch v {
	case "hard":
		return "deadline_hard", nil
	case "soft":
		return "deadline_soft", nil
	}
	return "", fmt.Errorf("--deadline %q: want hard | soft", v)
}

// buildScheduleCreate turns the flags into the POST body. It validates only
// what it can know from the flags; the server owns every other refusal.
func buildScheduleCreate(f scheduleFlags) (models.ScheduleCreate, error) {
	var out models.ScheduleCreate
	if f.RemindAt != "" || f.Rearm != "" {
		return out, fmt.Errorf("--remind-at and --rearm arm a one-shot reminder and cannot be combined with schedule flags; give the time with --start (e.g. --start 2026-11-09T09:00)")
	}
	if f.Tz != "" && f.Floating {
		return out, fmt.Errorf("--tz and --floating are mutually exclusive")
	}
	start, err := parseStartWall(f.Start)
	if err != nil {
		return out, err
	}
	out.Dtstart = start.String()
	out.Kind = "check"
	if f.Deadline != "" && f.Window != "" {
		return out, fmt.Errorf("--deadline and --window cannot be combined: a window is its own kind")
	}
	if f.Deadline != "" {
		if out.Kind, err = deadlineKind(f.Deadline); err != nil {
			return out, err
		}
	}
	if f.Window != "" {
		end, err := windowEnd(start, f.Window)
		if err != nil {
			return out, err
		}
		out.Kind, out.Dtend = "window", end.String()
	}
	if strings.TrimSpace(f.Every) != "" {
		if out.Rrule, err = normalizeEvery(f.Every); err != nil {
			return out, err
		}
	}
	if f.Tz != "" {
		if err := checkTz(f.Tz); err != nil {
			return out, err
		}
		out.Tz = f.Tz
	}
	out.Floating = f.Floating
	if f.Audience != "" {
		if err := checkEnum("audience", f.Audience, "human", "agent", "both"); err != nil {
			return out, err
		}
		out.Audience = f.Audience
	}
	if f.Anchor != "" {
		if err := checkEnum("anchor", f.Anchor, "calendar", "completion"); err != nil {
			return out, err
		}
		out.Anchor = f.Anchor
	}
	return out, nil
}

// buildScheduleUpdate builds the PATCH body from the flags the user set. A
// flag left alone is not sent.
func buildScheduleUpdate(f scheduleFlags, changed func(string) bool) (models.ScheduleUpdate, error) {
	var out models.ScheduleUpdate
	if changed("tz") && changed("floating") && f.Floating {
		return out, fmt.Errorf("--tz and --floating are mutually exclusive")
	}
	if changed("deadline") && changed("window") {
		return out, fmt.Errorf("--deadline and --window cannot be combined: a window is its own kind")
	}
	var start schedule.Wall
	if changed("start") {
		w, err := parseStartWall(f.Start)
		if err != nil {
			return out, err
		}
		start = w
		s := w.String()
		out.Dtstart = &s
	}
	if changed("deadline") {
		k, err := deadlineKind(f.Deadline)
		if err != nil {
			return out, err
		}
		out.Kind = &k
	}
	if changed("window") {
		k := "window"
		end, err := updateWindowEnd(start, changed("start"), f.Window)
		if err != nil {
			return out, err
		}
		e := end.String()
		out.Kind, out.Dtend = &k, &e
	}
	if changed("every") {
		rule := ""
		if strings.TrimSpace(f.Every) != "" {
			var err error
			if rule, err = normalizeEvery(f.Every); err != nil {
				return out, err
			}
		}
		out.Rrule = &rule
	}
	if changed("tz") {
		if err := checkTz(f.Tz); err != nil {
			return out, err
		}
		tz, fl := f.Tz, false
		out.Tz, out.Floating = &tz, &fl
	}
	if changed("floating") && f.Floating {
		t := true
		out.Floating = &t
	}
	if changed("audience") {
		if err := checkEnum("audience", f.Audience, "human", "agent", "both"); err != nil {
			return out, err
		}
		out.Audience = &f.Audience
	}
	if changed("anchor") {
		if err := checkEnum("anchor", f.Anchor, "calendar", "completion"); err != nil {
			return out, err
		}
		out.Anchor = &f.Anchor
	}
	if out == (models.ScheduleUpdate{}) {
		return out, fmt.Errorf("nothing to change: give at least one of --start, --every, --deadline, --window, --anchor, --audience, --tz, --floating")
	}
	return out, nil
}
