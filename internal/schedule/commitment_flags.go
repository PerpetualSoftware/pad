package schedule

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CommitmentFlags is the existing remind command's time flags, shared by the
// CLI and MCP. Changed is nil on create; on edit it preserves explicit values
// such as --every "" and --floating=false. CurrentStart is read only when a
// window edit omits start.
type CommitmentFlags struct {
	Every, Start, Deadline, Window, Anchor, Audience, Tz string
	Floating                                             bool
}

// CommitmentBody translates flags to native roles using the former create
// mapping: hard has a zero deadline offset, soft/check only a target, window
// an occupied offset. Neither kind nor dtend is a request member.
func CommitmentBody(f CommitmentFlags, changed func(string) bool, currentStart string) (map[string]any, error) {
	create := changed == nil
	given := func(name string) bool {
		if !create {
			return changed(name)
		}
		switch name {
		case "start":
			return f.Start != ""
		case "every":
			return strings.TrimSpace(f.Every) != ""
		case "deadline":
			return f.Deadline != ""
		case "window":
			return f.Window != ""
		case "anchor":
			return f.Anchor != ""
		case "audience":
			return f.Audience != ""
		case "tz":
			return f.Tz != ""
		case "floating":
			return f.Floating
		}
		return false
	}
	if given("tz") && f.Floating {
		return nil, fmt.Errorf("--tz and --floating are mutually exclusive")
	}
	if given("deadline") && given("window") {
		return nil, fmt.Errorf("--deadline and --window cannot be combined")
	}
	body := map[string]any{}
	var start Wall
	var err error
	if create || given("start") || given("window") {
		raw := strings.TrimSpace(f.Start)
		if !create && !given("start") {
			raw = currentStart
		}
		if raw == "" {
			return nil, fmt.Errorf("--start is required (a wall time such as 2026-11-09T09:00, or 2026-11-09 for an all-day date)")
		}
		start, err = ParseWall(raw)
		if err != nil {
			return nil, fmt.Errorf("--start %q is not a wall time: want 2026-11-09T09:00 or 2026-11-09, with no Z or offset (the zone comes from --tz)", raw)
		}
		if create || given("start") {
			body["dtstart"] = start.String()
		}
	}
	if create || given("deadline") || given("window") {
		roles := map[string]any{}
		if create {
			roles["target_days"], roles["target_secs"] = 0, 0
		}
		if given("deadline") {
			switch f.Deadline {
			case "hard":
				roles["deadline_days"], roles["deadline_secs"], roles["deadline_behavior"] = 0, 0, "stays_owed"
			case "soft":
				if !create {
					roles["deadline_days"], roles["deadline_secs"], roles["deadline_behavior"] = nil, nil, nil
				}
			case "none":
				roles["deadline_days"], roles["deadline_secs"], roles["deadline_behavior"] = nil, nil, nil
				if create {
					return nil, fmt.Errorf("--deadline none is an edit value; a new schedule is a check unless you give hard | soft")
				}
			default:
				return nil, fmt.Errorf("--deadline %q: want hard | soft", f.Deadline)
			}
		}
		if given("window") {
			end, err := commitmentWindowEnd(start, strings.TrimSpace(f.Window))
			if err != nil {
				return nil, err
			}
			if end.AllDay != start.AllDay || end.String() <= start.String() {
				return nil, fmt.Errorf("--window must end after --start and have the same all-day-ness")
			}
			day := func(w Wall) time.Time { return time.Date(w.Year, w.Month, w.Day, 0, 0, 0, 0, time.UTC) }
			roles["occupied_days"] = int(day(end).Sub(day(start)) / (24 * time.Hour))
			roles["occupied_secs"] = (end.Hour-start.Hour)*3600 + (end.Minute-start.Minute)*60 + end.Second - start.Second
		}
		body["roles"] = roles
	}
	if given("every") {
		rule := ""
		if strings.TrimSpace(f.Every) != "" {
			rule, err = NormalizeRule(f.Every)
			if err != nil {
				return nil, fmt.Errorf("--every %q: %v; use one of %s, or an RRULE", strings.TrimSpace(f.Every), err, strings.Join(ShorthandNames(), ", "))
			}
		}
		body["rrule"] = rule
	}
	if given("tz") {
		if f.Tz == "Local" || f.Tz == "" {
			return nil, fmt.Errorf("--tz %q: use an IANA zone", f.Tz)
		}
		if _, err := time.LoadLocation(f.Tz); err != nil {
			return nil, fmt.Errorf("--tz %q: %v", f.Tz, err)
		}
		body["tz"] = f.Tz
		if !create {
			body["floating"] = false
		}
	}
	if given("floating") {
		body["floating"] = f.Floating
	}
	for _, field := range []struct {
		name, value string
		allowed     []string
	}{
		{"audience", f.Audience, []string{"human", "agent", "both"}},
		{"anchor", f.Anchor, []string{"calendar", "completion"}},
	} {
		if given(field.name) {
			valid := false
			for _, v := range field.allowed {
				valid = valid || v == field.value
			}
			if !valid {
				return nil, fmt.Errorf("--%s %q: want %s", field.name, field.value, strings.Join(field.allowed, " | "))
			}
			body[field.name] = field.value
		}
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("nothing to change: give at least one of --start, --every, --deadline, --window, --anchor, --audience, --tz, --floating")
	}
	return body, nil
}

// CommitmentKind is a display label derived from roles, with deadline first.
func CommitmentKind(hasDeadline, hasOccupiedUntil bool) string {
	if hasDeadline {
		return "deadline_hard"
	}
	if hasOccupiedUntil {
		return "window"
	}
	return "check"
}

var commitmentWindowDurationRE = regexp.MustCompile(`^(\d+)([mhd])$`)

func commitmentWindowEnd(start Wall, value string) (Wall, error) {
	bad := fmt.Errorf("--window %q: want a duration (90m, 2h, 3d) or an end wall (2026-11-09T17:00)", value)
	base := start.naive()
	wall := func(at time.Time) Wall {
		return Wall{Year: at.Year(), Month: at.Month(), Day: at.Day(), Hour: at.Hour(), Minute: at.Minute(), Second: at.Second(), AllDay: start.AllDay}
	}
	if m := commitmentWindowDurationRE.FindStringSubmatch(value); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 || n > 100000 {
			return Wall{}, bad
		}
		if m[2] == "d" {
			return wall(base.AddDate(0, 0, n)), nil
		}
		if start.AllDay {
			return Wall{}, fmt.Errorf("--window %q: an all-day start takes a window in days (3d) or an end date", value)
		}
		unit := time.Minute
		if m[2] == "h" {
			unit = time.Hour
		}
		return wall(base.Add(time.Duration(n) * unit)), nil
	}
	if d, err := time.ParseDuration(value); err == nil {
		if d <= 0 || start.AllDay {
			return Wall{}, bad
		}
		return wall(base.Add(d)), nil
	}
	if end, err := ParseWall(value); err == nil {
		return end, nil
	}
	return Wall{}, bad
}

// CommitmentResolve is the body of POST .../commitment/resolve.
type CommitmentResolve struct {
	OccurrenceAt string `json:"occurrence_at"`
	State        string `json:"state"`
	Comment      string `json:"comment,omitempty"`
	Until        string `json:"until,omitempty"`
}
