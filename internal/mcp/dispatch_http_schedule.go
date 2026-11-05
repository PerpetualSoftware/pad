package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// Remote mappers for the schedule actions of pad_item (TASK-4105). They build
// the same request bodies the CLI builds from its flags (cmd/pad/schedule_flags.go);
// the parameters carry the flags' names. Rules only the server can judge (the
// firing floor on HOURLY and MINUTELY rules, anchor completion with a BYxxx
// part, an occurrence that never comes) are left to it, and its message is
// returned unchanged. The rule text is sent as given: the server normalizes
// the shorthands and RRULE text itself.

const (
	itemSchedulePathTemplate = "/api/v1/workspaces/{workspace}/items/{ref}/schedule"
	itemScheduleResolvePath  = itemSchedulePathTemplate + "/resolve"
)

// itemRemindOneShot is the one-shot reminder route, unchanged since IDEA-2641.
var itemRemindOneShot = routeSpec{
	method:       http.MethodPost,
	pathTemplate: "/api/v1/workspaces/{workspace}/items/{ref}/reminders",
	bodyKeys:     []string{"remind_at"},
}.toRouteMapper()

// scheduleParams are the schedule members of an input, with the inert ones
// (null, blank, floating=false) read as not given, as actionItemRemind and
// actionItemScheduleEdit do before dispatch.
type scheduleParams struct {
	every, start, deadline, window, anchor, audience, tz string
	floating                                             bool
	given                                                map[string]bool
}

func readScheduleParams(input map[string]any) (scheduleParams, error) {
	p := scheduleParams{given: map[string]bool{}}
	strs := map[string]*string{
		"every": &p.every, "start": &p.start, "deadline": &p.deadline, "window": &p.window,
		"anchor": &p.anchor, "audience": &p.audience, "tz": &p.tz,
	}
	for k, dst := range strs {
		v, ok := input[k]
		if !ok || v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return p, fmt.Errorf("%s must be a string, got %T", k, v)
		}
		if s = strings.TrimSpace(s); s != "" {
			*dst, p.given[k] = s, true
		}
	}
	if v, ok := input["floating"]; ok && v != nil {
		b, ok := v.(bool)
		if !ok {
			return p, fmt.Errorf("floating must be a boolean, got %T", v)
		}
		if b {
			p.floating, p.given["floating"] = true, true
		}
	}
	return p, nil
}

func (p scheduleParams) any() bool { return len(p.given) > 0 }

// mapItemRemind is `item remind`: the one-shot route when no schedule
// parameter is given, a schedule create when one is.
func mapItemRemind(input map[string]any) (string, string, []byte, error) {
	p, err := readScheduleParams(input)
	if err != nil {
		return "", "", nil, err
	}
	if !p.any() {
		return itemRemindOneShot(input)
	}
	if s, _ := input["remind_at"].(string); strings.TrimSpace(s) != "" {
		return "", "", nil, fmt.Errorf("remind_at arms a one-shot reminder and cannot be combined with schedule parameters; give the time as start (e.g. 2026-11-09T09:00)")
	}
	body, err := scheduleCreateBody(p)
	if err != nil {
		return "", "", nil, err
	}
	return marshalScheduleRequest(http.MethodPost, itemSchedulePathTemplate, input, body)
}

// mapItemScheduleEdit is `item remind edit`: a PATCH of the members given.
func mapItemScheduleEdit(input map[string]any) (string, string, []byte, error) {
	p, err := readScheduleParams(input)
	if err != nil {
		return "", "", nil, err
	}
	body, err := scheduleUpdateBody(p)
	if err != nil {
		return "", "", nil, err
	}
	return marshalScheduleRequest(http.MethodPatch, itemSchedulePathTemplate, input, body)
}

func marshalScheduleRequest(method, template string, input, body map[string]any) (string, string, []byte, error) {
	path, err := expandPath(template, input)
	if err != nil {
		return "", "", nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", "", nil, err
	}
	return method, path, raw, nil
}

// mapScheduleResolve is `item remind done|skip|snooze|move`: the state is the
// command's, the rest is the input actionItemScheduleResolve left. The
// occurrence is required here too, so a caller that reaches the route without
// the action still never resolves "whichever is current".
func mapScheduleResolve(state string) RouteMapper {
	return func(input map[string]any) (string, string, []byte, error) {
		path, err := expandPath(itemScheduleResolvePath, input)
		if err != nil {
			return "", "", nil, err
		}
		occ, _ := input["occurrence"].(string)
		if strings.TrimSpace(occ) == "" {
			return "", "", nil, fmt.Errorf("occurrence_at is required: the RFC3339 instant of the outstanding occurrence")
		}
		body := map[string]any{"state": state, "occurrence_at": occ}
		if c, _ := input["comment"].(string); c != "" {
			body["comment"] = c
		}
		if u, _ := input["until"].(string); u != "" {
			body["until"] = u
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return "", "", nil, err
		}
		return http.MethodPost, path, raw, nil
	}
}

// scheduleCreateBody is cmd/pad's buildScheduleCreate.
func scheduleCreateBody(p scheduleParams) (map[string]any, error) {
	if p.tz != "" && p.floating {
		return nil, fmt.Errorf("tz and floating are mutually exclusive")
	}
	start, err := parseScheduleStart(p.start)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"dtstart": start.String(), "kind": "check"}
	if p.deadline != "" && p.window != "" {
		return nil, fmt.Errorf("deadline and window cannot be combined: a window is its own kind")
	}
	if p.deadline == "none" {
		return nil, fmt.Errorf("deadline none is an edit value (action=schedule-edit turns a deadline back into a plain check); a new schedule is a check unless you give hard | soft")
	}
	if p.deadline != "" {
		if body["kind"], err = scheduleDeadlineKind(p.deadline); err != nil {
			return nil, err
		}
	}
	if p.window != "" {
		end, err := scheduleWindowEnd(start, p.window)
		if err != nil {
			return nil, err
		}
		body["kind"], body["dtend"] = "window", end.String()
	}
	if p.every != "" {
		body["rrule"] = p.every
	}
	if p.tz != "" {
		body["tz"] = p.tz
	}
	if p.floating {
		body["floating"] = true
	}
	if p.audience != "" {
		if err := scheduleEnum("audience", p.audience, "human", "agent", "both"); err != nil {
			return nil, err
		}
		body["audience"] = p.audience
	}
	if p.anchor != "" {
		if err := scheduleEnum("anchor", p.anchor, "calendar", "completion"); err != nil {
			return nil, err
		}
		body["anchor"] = p.anchor
	}
	return body, nil
}

// scheduleUpdateBody is cmd/pad's buildScheduleUpdate. A member not given is
// not sent; the server leaves what is not sent as it was.
func scheduleUpdateBody(p scheduleParams) (map[string]any, error) {
	if p.given["tz"] && p.given["floating"] {
		return nil, fmt.Errorf("tz and floating are mutually exclusive")
	}
	if p.given["deadline"] && p.given["window"] {
		return nil, fmt.Errorf("deadline and window cannot be combined: a window is its own kind")
	}
	body := map[string]any{}
	var start schedule.Wall
	if p.given["start"] {
		w, err := parseScheduleStart(p.start)
		if err != nil {
			return nil, err
		}
		start = w
		body["dtstart"] = w.String()
	}
	if p.given["deadline"] {
		kind := "check"
		if p.deadline != "none" {
			var err error
			if kind, err = scheduleDeadlineKind(p.deadline); err != nil {
				return nil, err
			}
		}
		// Only a window carries dtend, so leaving the window kind clears it.
		body["kind"], body["dtend"] = kind, ""
	}
	if p.given["window"] {
		if !p.given["start"] && windowIsDuration(p.window) {
			return nil, fmt.Errorf("window %q is a duration counted from the start; give start too, or an end wall instead", p.window)
		}
		end, err := scheduleWindowEnd(start, p.window)
		if err != nil {
			return nil, err
		}
		body["kind"], body["dtend"] = "window", end.String()
	}
	if p.given["every"] {
		body["rrule"] = p.every
	}
	if p.given["tz"] {
		body["tz"], body["floating"] = p.tz, false
	}
	if p.given["floating"] {
		body["floating"] = true
	}
	if p.given["audience"] {
		if err := scheduleEnum("audience", p.audience, "human", "agent", "both"); err != nil {
			return nil, err
		}
		body["audience"] = p.audience
	}
	if p.given["anchor"] {
		if err := scheduleEnum("anchor", p.anchor, "calendar", "completion"); err != nil {
			return nil, err
		}
		body["anchor"] = p.anchor
	}
	if len(body) == 0 {
		return nil, errNothingToChange
	}
	return body, nil
}

func parseScheduleStart(s string) (schedule.Wall, error) {
	if s == "" {
		return schedule.Wall{}, fmt.Errorf("start is required (a wall time such as 2026-11-09T09:00, or 2026-11-09 for an all-day date)")
	}
	w, err := schedule.ParseWall(s)
	if err != nil {
		return schedule.Wall{}, fmt.Errorf("start %q is not a wall time: want 2026-11-09T09:00 or 2026-11-09, with no Z or offset (the zone comes from tz)", s)
	}
	return w, nil
}

func scheduleDeadlineKind(v string) (string, error) {
	switch v {
	case "hard":
		return "deadline_hard", nil
	case "soft":
		return "deadline_soft", nil
	}
	return "", fmt.Errorf("deadline %q: want hard | soft", v)
}

func scheduleEnum(param, v string, allowed ...string) error {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return fmt.Errorf("%s %q: want %s", param, v, strings.Join(allowed, " | "))
}

var scheduleWindowDurationRE = regexp.MustCompile(`^(\d+)([mhd])$`)

func windowIsDuration(s string) bool {
	if scheduleWindowDurationRE.MatchString(s) {
		return true
	}
	_, err := time.ParseDuration(s)
	return err == nil
}

// scheduleWindowEnd is cmd/pad's windowEnd: a duration is added on the wall
// clock to start, an end wall is taken as given.
func scheduleWindowEnd(start schedule.Wall, s string) (schedule.Wall, error) {
	bad := fmt.Errorf("window %q: want a duration (90m, 2h, 3d) or an end wall (2026-11-09T17:00)", s)
	allDay := fmt.Errorf("window %q: an all-day start takes a window in days (3d) or an end date", s)
	wall := func(t time.Time) schedule.Wall {
		w := schedule.Wall{Year: t.Year(), Month: t.Month(), Day: t.Day(), AllDay: start.AllDay}
		if !start.AllDay {
			w.Hour, w.Minute, w.Second = t.Hour(), t.Minute(), t.Second()
		}
		return w
	}
	base := time.Date(start.Year, start.Month, start.Day, start.Hour, start.Minute, start.Second, 0, time.UTC)
	if m := scheduleWindowDurationRE.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 || n > 100000 {
			return schedule.Wall{}, bad
		}
		switch m[2] {
		case "d":
			return wall(base.AddDate(0, 0, n)), nil
		case "h":
			if start.AllDay {
				return schedule.Wall{}, allDay
			}
			return wall(base.Add(time.Duration(n) * time.Hour)), nil
		default:
			if start.AllDay {
				return schedule.Wall{}, allDay
			}
			return wall(base.Add(time.Duration(n) * time.Minute)), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 || start.AllDay {
			return schedule.Wall{}, bad
		}
		return wall(base.Add(d)), nil
	}
	if w, err := schedule.ParseWall(s); err == nil {
		return w, nil
	}
	return schedule.Wall{}, bad
}
