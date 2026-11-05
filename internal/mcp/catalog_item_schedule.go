package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Schedules and the agenda on pad_item (TASK-4105). They ride the `pad item
// remind` command and its subcommands on stdio, and the schedule and agenda
// routes on /mcp (dispatch_http_schedule.go). The action functions here do
// what both transports must agree on before anything is dispatched: which
// values count as "not given", and the checks on resolve and agenda.

// scheduleParamKeys are the pad_item parameters that make `remind` a schedule
// and are the members of schedule-edit.
var scheduleParamKeys = []string{"every", "start", "deadline", "window", "anchor", "audience", "tz", "floating"}

// withoutInertScheduleParams returns a copy of input with the schedule
// parameters that carry no value removed: null, a blank string, and
// floating=false. A client that pads unused optional parameters with "" must
// not be turned into a schedule, nor into an edit that clears a rule, so both
// transports read these as not provided; stdio would otherwise send --every ""
// to the CLI, where an empty value is a real one. Strings are trimmed, and a
// value of the wrong type is refused (floating is a boolean, the rest are
// strings), in the order of scheduleParamKeys, so both transports answer the
// same input the same way. given reports whether any schedule parameter is
// left.
func withoutInertScheduleParams(input map[string]any) (out map[string]any, given bool, err error) {
	out = make(map[string]any, len(input))
	for k, v := range input {
		out[k] = v
	}
	for _, k := range scheduleParamKeys {
		v, present := out[k]
		if !present {
			continue
		}
		if v == nil {
			delete(out, k)
			continue
		}
		if k == "floating" {
			b, ok := v.(bool)
			if !ok {
				return nil, false, fmt.Errorf("floating must be a boolean, got %T", v)
			}
			if !b {
				delete(out, k)
			}
			continue
		}
		s, ok := v.(string)
		if !ok {
			return nil, false, fmt.Errorf("%s must be a string, got %T", k, v)
		}
		if s = strings.TrimSpace(s); s == "" {
			delete(out, k)
		} else {
			out[k] = s
		}
	}
	for _, k := range scheduleParamKeys {
		if _, present := out[k]; present {
			given = true
			break
		}
	}
	return out, given, nil
}

// remindAtGiven reports whether remind_at carries a value of any type: the
// one-shot time cannot ride with schedule parameters.
func remindAtGiven(input map[string]any) bool {
	v, ok := input["remind_at"]
	if !ok || v == nil {
		return false
	}
	s, isStr := v.(string)
	return !isStr || strings.TrimSpace(s) != ""
}

var errRemindAtWithSchedule = fmt.Errorf("remind_at arms a one-shot reminder and cannot be combined with schedule parameters; give the time as start (e.g. 2026-11-09T09:00)")

// actionItemRemind handles pad_item.action=remind. With no schedule parameter
// it is the one-shot reminder it always was; any schedule parameter makes it a
// schedule (`pad item remind --start ...` on stdio, POST .../schedule on
// /mcp). Inert values are dropped first, so the one-shot form is identical on
// both transports whatever a client pads it with.
func actionItemRemind(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	out, given, err := withoutInertScheduleParams(input)
	if err != nil {
		return errStructured("pad_item.remind", err), nil
	}
	if given && remindAtGiven(input) {
		return errStructured("pad_item.remind", errRemindAtWithSchedule), nil
	}
	return env.Dispatch(ctx, []string{"item", "remind"}, out)
}

// actionItemScheduleEdit handles pad_item.action=schedule-edit: a PATCH, so
// only the parameters given change. An empty value is not a change, which
// means an edit cannot clear a rule or a window by sending "".
func actionItemScheduleEdit(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	out, given, err := withoutInertScheduleParams(input)
	if err != nil {
		return errStructured("pad_item.schedule-edit", err), nil
	}
	if !given {
		return errStructured("pad_item.schedule-edit", errNothingToChange), nil
	}
	return env.Dispatch(ctx, []string{"item", "remind", "edit"}, out)
}

var errNothingToChange = fmt.Errorf("nothing to change: give at least one of %s", strings.Join(scheduleParamKeys, ", "))

// actionItemScheduleDelete handles pad_item.action=schedule-delete. The CLI
// asks before it deletes and a subprocess cannot answer, so stdio passes
// --yes; the route has no prompt.
func actionItemScheduleDelete(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	out := make(map[string]any, len(input)+1)
	for k, v := range input {
		out[k] = v
	}
	out["yes"] = true
	return env.Dispatch(ctx, []string{"item", "remind", "rm"}, out)
}

// scheduleResolveStates are the resolutions of an occurrence.
var scheduleResolveStates = []string{"done", "skip", "snooze", "move"}

// actionItemScheduleResolve handles pad_item.action=schedule-resolve. The
// state picks the CLI subcommand (`item remind done|skip|snooze|move`) and the
// remote route fixes it in the body. It checks, once for both transports, the
// members the CLI treats as optional or reads for itself: occurrence_at is
// required, so a resolve never silently takes whichever occurrence is current
// when a read earlier may have shown another, and until is an RFC3339 instant
// (the CLI's wall times and durations read the caller's zone, which a remote
// caller does not have).
func actionItemScheduleResolve(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	const name = "pad_item.schedule-resolve"
	state, _ := input["state"].(string)
	valid := false
	for _, s := range scheduleResolveStates {
		valid = valid || s == state
	}
	if !valid {
		return errStructured(name, fmt.Errorf("state is required: one of %s", strings.Join(scheduleResolveStates, " | "))), nil
	}
	occ, _ := input["occurrence_at"].(string)
	if strings.TrimSpace(occ) == "" {
		return errStructured(name, fmt.Errorf("occurrence_at is required: the RFC3339 instant of the outstanding occurrence, as action=schedule reports it (next_occurrence.at)")), nil
	}
	if t, err := time.Parse(time.RFC3339, occ); err != nil {
		return errStructured(name, fmt.Errorf("occurrence_at %q is not an RFC3339 instant, as action=schedule reports it (next_occurrence.at)", occ)), nil
	} else if err := wholeSecond("occurrence_at", occ, t); err != nil {
		return errStructured(name, err), nil
	}
	// The optional members must be strings too: stdio would stringify a number
	// and forward it, the remote mapper would drop it.
	for _, k := range []string{"until", "comment"} {
		if v, present := input[k]; present && v != nil {
			if _, ok := v.(string); !ok {
				return errStructured(name, fmt.Errorf("%s must be a string, got %T", k, v)), nil
			}
		}
	}
	until, _ := input["until"].(string)
	needsUntil := state == "snooze" || state == "move"
	switch {
	case needsUntil && strings.TrimSpace(until) == "":
		return errStructured(name, fmt.Errorf("until is required to %s: the RFC3339 instant it re-fires or becomes due", state)), nil
	case needsUntil:
		if t, err := time.Parse(time.RFC3339, until); err != nil {
			return errStructured(name, fmt.Errorf("until %q is not an RFC3339 instant (2026-11-10T09:00:00Z)", until)), nil
		} else if err := wholeSecond("until", until, t); err != nil {
			return errStructured(name, err), nil
		}
	case strings.TrimSpace(until) != "":
		return errStructured(name, fmt.Errorf("until applies to snooze and move only, not %s", state)), nil
	}
	out := make(map[string]any, len(input)+1)
	for k, v := range input {
		if k != "state" && k != "occurrence_at" && k != "until" {
			out[k] = v
		}
	}
	out["occurrence"] = occ
	if needsUntil {
		out["until"] = until
	}
	return env.Dispatch(ctx, []string{"item", "remind", state}, out)
}

// actionItemAgenda handles pad_item.action=agenda. from and to are required
// RFC3339 instants on both transports (the CLI also takes a date and a day
// count; the route takes neither).
func actionItemAgenda(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	for _, k := range []string{"from", "to"} {
		v, _ := input[k].(string)
		if strings.TrimSpace(v) == "" {
			return errStructured("pad_item.agenda", fmt.Errorf("%s is required: an RFC3339 instant (2026-11-09T00:00:00Z); the window is at most 92 days", k)), nil
		}
		if t, err := time.Parse(time.RFC3339, v); err != nil {
			return errStructured("pad_item.agenda", fmt.Errorf("%s %q is not an RFC3339 instant (2026-11-09T00:00:00Z)", k, v)), nil
		} else if err := wholeSecond(k, v, t); err != nil {
			return errStructured("pad_item.agenda", err), nil
		}
	}
	return env.Dispatch(ctx, []string{"item", "remind", "agenda"}, input)
}

// wholeSecond refuses an instant with a fraction of a second. The CLI formats
// instants to the second and the route keeps the fraction, so the same value
// would land on different seconds on the two transports (an until that fires
// early, an agenda bound that includes an occurrence on one and not the other).
func wholeSecond(param, raw string, t time.Time) error {
	if t.Nanosecond() != 0 {
		return fmt.Errorf("%s %q has a fraction of a second: give a whole-second instant (2026-11-09T09:00:00Z)", param, raw)
	}
	return nil
}
