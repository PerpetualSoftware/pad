package mcp

import (
	"context"
	"encoding/json"
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
// schedule (`pad item remind --start ...` on stdio, POST .../commitment on
// /mcp). Inert values are dropped first, so the one-shot form is identical on
// both transports whatever a client pads it with.
func actionItemRemind(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	out, given, err := withoutInertScheduleParams(input)
	if err != nil {
		return errStructured("pad_item.remind", err), nil
	}
	if given {
		if remindAtGiven(input) {
			return errStructured("pad_item.remind", errRemindAtWithSchedule), nil
		}
		// A blank remind_at is not given: leave it out, so the CLI does not
		// read an empty --remind-at as the one-shot form beside the schedule.
		delete(out, "remind_at")
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
	for _, k := range []string{"until", "comment", "actual_done_at"} {
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
	// actual_done_at is for done, and a blank one is not given.
	actual := ""
	if a, _ := input["actual_done_at"].(string); strings.TrimSpace(a) != "" {
		actual = strings.TrimSpace(a)
		if state != "done" {
			return errStructured(name, fmt.Errorf("actual_done_at applies to done only, not %s", state)), nil
		}
		if t, err := time.Parse(time.RFC3339, actual); err != nil {
			return errStructured(name, fmt.Errorf("actual_done_at %q is not an RFC3339 instant (2026-11-08T18:30:00Z)", actual)), nil
		} else if err := wholeSecond("actual_done_at", actual, t); err != nil {
			return errStructured(name, err), nil
		}
	}
	// Only resolve's own members may reach dispatch. Tool-wide keys such as
	// role and to must never select the linked move on either transport.
	out := make(map[string]any)
	for _, k := range []string{"workspace", "ref", "comment"} {
		if v, ok := input[k]; ok {
			out[k] = v
		}
	}
	out["occurrence"] = occ
	// A blank actual_done_at is not given: leave it out for the route, and give
	// the CLI its --at only when there is a value.
	if actual != "" {
		out["actual_done_at"], out["at"] = actual, actual
	}
	if needsUntil {
		out["until"] = until
	}
	return env.Dispatch(ctx, []string{"item", "remind", state}, out)
}

// actionItemAgenda handles pad_item.action=agenda. from and to are required
// RFC3339 instants on both transports (the CLI also takes a date and a day
// count; the route takes neither). With stale=true the window is replaced by the
// caller's occurrences unresolved for more than 14 days (TASK-4237): to is
// refused, from (optional) resumes a long list, and the stale flag itself is
// validated here, once, so stdio and /mcp read it the same way.
func actionItemAgenda(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	stale := false
	if v, present := input["stale"]; present && v != nil {
		b, ok := v.(bool)
		if !ok {
			return errStructured("pad_item.agenda", fmt.Errorf("stale must be a boolean, got %T", v)), nil
		}
		stale = b
	}
	if stale {
		if v, present := input["to"]; present && v != nil {
			return errStructured("pad_item.agenda", fmt.Errorf("to is not allowed with stale: it lists everything unresolved for more than 14 days; pass from to resume a long list")), nil
		}
		in := copyInputMap(input)
		delete(in, "to")
		if v, present := in["from"]; present {
			from, isString := v.(string)
			switch {
			case v == nil || (isString && strings.TrimSpace(from) == ""):
				delete(in, "from") // not given, on both transports
			case !isString:
				return errStructured("pad_item.agenda", fmt.Errorf("from must be a string, got %T", v)), nil
			default:
				if t, err := time.Parse(time.RFC3339, from); err != nil {
					return errStructured("pad_item.agenda", fmt.Errorf("from %q is not an RFC3339 instant (2026-11-09T00:00:00Z)", from)), nil
				} else if err := wholeSecond("from", from, t); err != nil {
					return errStructured("pad_item.agenda", err), nil
				}
			}
		}
		if err := agendaTzIsText(input); err != nil {
			return errStructured("pad_item.agenda", err), nil
		}
		return env.Dispatch(ctx, []string{"item", "remind", "agenda"}, in)
	}
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
	if err := agendaTzIsText(input); err != nil {
		return errStructured("pad_item.agenda", err), nil
	}
	// A stale of false is not sent: stdio would pass --stale=false and the route
	// a "false" the server rejects.
	in := copyInputMap(input)
	delete(in, "stale")
	return env.Dispatch(ctx, []string{"item", "remind", "agenda"}, in)
}

// agendaTzIsText refuses a non-string tz: tz is the only other optional member,
// and a non-string would be dropped by the route's query mapper and sent as text
// by the CLI.
func agendaTzIsText(input map[string]any) error {
	if v, present := input["tz"]; present && v != nil {
		if _, ok := v.(string); !ok {
			return fmt.Errorf("tz must be a string, got %T", v)
		}
	}
	return nil
}

func copyInputMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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

// renegotiateStringKeys are the string members of schedule-renegotiate. A blank
// one is not given on either transport.
var renegotiateStringKeys = []string{"reason", "scope", "deadline", "deadline_behavior", "revision"}

// actionItemScheduleRenegotiate handles pad_item.action=schedule-renegotiate:
// a preview unless preview=false, which applies the revision a preview returned.
// What both transports must agree on is decided here: types, the reason, the
// revision an apply needs, and the instant. Which members go with which scope
// is the server's to judge, and its message is returned unchanged. The CLI
// names a few members differently (--deadline-offset-days, --behavior, --apply),
// so the input also carries those flag names for stdio; the remote mapper reads
// the declared names.
func actionItemScheduleRenegotiate(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	const name = "pad_item.schedule-renegotiate"
	out := make(map[string]any, len(input)+2)
	for k, v := range input {
		out[k] = v
	}
	for _, k := range renegotiateStringKeys {
		v, present := out[k]
		delete(out, k)
		if !present || v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return errStructured(name, fmt.Errorf("%s must be a string, got %T", k, v)), nil
		}
		if s = strings.TrimSpace(s); s != "" {
			out[k] = s
		}
	}
	if _, ok := out["reason"]; !ok {
		return errStructured(name, fmt.Errorf("reason is required: a deadline is renegotiated with a reason on record")), nil
	}
	apply := false
	for _, k := range []string{"preview", "remove_deadline"} {
		v, present := out[k]
		delete(out, k)
		if !present || v == nil {
			continue
		}
		b, ok := v.(bool)
		if !ok {
			return errStructured(name, fmt.Errorf("%s must be a boolean, got %T", k, v)), nil
		}
		switch {
		case k == "preview" && !b:
			apply = true
		case k == "remove_deadline" && b:
			out["remove_deadline"] = true
		}
	}
	if apply {
		if _, ok := out["revision"]; !ok {
			return errStructured(name, fmt.Errorf("revision is required to apply: preview first (preview omitted or true) and pass the revision it returns")), nil
		}
		out["preview"], out["apply"] = false, true
	}
	if d, ok := out["deadline"].(string); ok {
		if t, err := time.Parse(time.RFC3339, d); err != nil {
			return errStructured(name, fmt.Errorf("deadline %q is not an RFC3339 instant (2099-01-01T09:00:00Z)", d)), nil
		} else if err := wholeSecond("deadline", d, t); err != nil {
			return errStructured(name, err), nil
		}
	}
	for _, k := range []string{"deadline_days", "deadline_secs"} {
		v, present := out[k]
		delete(out, k)
		if !present || v == nil {
			continue
		}
		if _, ok, err := intParam(map[string]any{k: v}, k); err != nil {
			return errStructured(name, err), nil
		} else if ok {
			out[k] = v
			out[strings.Replace(k, "deadline_", "deadline_offset_", 1)] = v
		}
	}
	if b, ok := out["deadline_behavior"]; ok {
		out["behavior"] = b
	}
	return env.Dispatch(ctx, []string{"item", "remind", "renegotiate"}, out)
}

// actionItemScheduleGraph keeps previews and typed graph inputs identical on
// both transports. offset-json carries the API union without dropping zeros,
// false, or unknown members that the strict API must judge itself.
func actionItemScheduleGraph(action string) ActionFn {
	return func(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
		name := "pad_item." + action
		// pad_item shares parameters across actions. Forward only this action's
		// members, before validation or CLI flag aliases can interpret them.
		keys := []string{"workspace"}
		switch action {
		case "schedule-link":
			keys = append(keys, "ref", "child", "parent_role", "child_role", "link_id", "link_offset", "cancel_policy", "reason", "revision", "apply", "preview", "acknowledge_violations")
		case "schedule-unlink":
			keys = append(keys, "link_id")
		case "schedule-move":
			keys = append(keys, "ref", "role", "to", "reason", "revision", "apply", "preview", "acknowledge_violations")
		case "schedule-gate":
			keys = append(keys, "ref", "blocker", "gate_id")
		}
		out := make(map[string]any, len(keys))
		for _, k := range keys {
			if v, ok := input[k]; ok {
				out[k] = v
			}
		}
		fail := func(err error) (*CallToolResult, error) { return errStructured(name, err), nil }
		for _, k := range []string{"ref", "child", "parent_role", "child_role", "link_id", "gate_id", "blocker", "cancel_policy", "reason", "revision"} {
			v, ok := out[k]
			delete(out, k)
			if !ok || v == nil {
				continue
			}
			s, ok := v.(string)
			if !ok {
				return fail(fmt.Errorf("%s must be a string, got %T", k, v))
			}
			if s = strings.TrimSpace(s); s != "" {
				out[k] = s
			}
		}
		for _, k := range []string{"apply", "preview", "acknowledge_violations"} {
			v, ok := out[k]
			delete(out, k)
			if !ok || v == nil {
				continue
			}
			b, ok := v.(bool)
			if !ok {
				return fail(fmt.Errorf("%s must be a boolean, got %T", k, v))
			}
			out[k] = b
		}
		apply, _ := out["apply"].(bool)
		if p, ok := out["preview"].(bool); ok && !p && !apply {
			return fail(fmt.Errorf("preview=false requires apply=true; preview first and repeat its inputs"))
		}
		// False flags are absent on the CLI and the HTTP body alike.
		delete(out, "preview")
		delete(out, "apply")
		if apply {
			out["apply"] = true
		}
		if ack, _ := out["acknowledge_violations"].(bool); ack {
			out["ack_violations"] = true
		} else {
			delete(out, "acknowledge_violations")
		}
		require := func(keys ...string) error {
			for _, k := range keys {
				if _, ok := out[k]; !ok {
					return fmt.Errorf("%s is required for %s", k, action)
				}
			}
			return nil
		}
		path := []string{"item", "remind"}
		switch action {
		case "schedule-link":
			if off, ok := out["link_offset"]; ok && off != nil {
				if _, ok := off.(map[string]any); !ok {
					return fail(fmt.Errorf("link_offset must be an offset object"))
				}
				raw, err := json.Marshal(off)
				if err != nil {
					return fail(err)
				}
				out["offset_json"] = string(raw)
			}
			if _, edit := out["link_id"]; edit {
				if out["offset_json"] == nil && out["cancel_policy"] == nil {
					return fail(fmt.Errorf("link_offset or cancel_policy is required to edit a link"))
				}
				path = append(path, "link", "edit")
			} else {
				if err := require("ref", "child", "parent_role", "child_role", "offset_json"); err != nil {
					return fail(err)
				}
				out["parent_ref"], out["child_ref"] = out["ref"], out["child"]
				path = append(path, "link", "add")
			}
		case "schedule-unlink":
			if err := require("link_id"); err != nil {
				return fail(err)
			}
			path = append(path, "link", "rm")
		case "schedule-move":
			for _, k := range []string{"role", "to"} {
				v, ok := out[k]
				if !ok || v == nil {
					delete(out, k)
					continue
				}
				str, ok := v.(string)
				if !ok {
					return fail(fmt.Errorf("%s must be a string, got %T", k, v))
				}
				if strings.TrimSpace(str) == "" {
					delete(out, k)
				} else {
					out[k] = strings.TrimSpace(str)
				}
			}
			if err := require("ref", "role", "to"); err != nil {
				return fail(err)
			}
			to := out["to"].(string)
			t, err := time.Parse(time.RFC3339, to)
			if err != nil {
				return fail(fmt.Errorf("to must be an RFC3339 instant"))
			}
			if err = wholeSecond("to", to, t); err != nil {
				return fail(err)
			}
			out["to"] = t.UTC().Format(time.RFC3339)
			path = append(path, "move")
		case "schedule-gate":
			if _, remove := out["gate_id"]; remove {
				if out["blocker"] != nil {
					return fail(fmt.Errorf("gate_id cannot be combined with blocker"))
				}
				path = append(path, "gate", "rm")
			} else {
				if err := require("ref", "blocker"); err != nil {
					return fail(err)
				}
				out["blocker_ref"] = out["blocker"]
				path = append(path, "gate", "add")
			}
		}
		return env.Dispatch(ctx, path, out)
	}
}

// actionItemScheduleForecast validates the same combinations on both transports.
func actionItemScheduleForecast(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	refuse := func(message string) (*CallToolResult, error) {
		return errStructured("pad_item.schedule-forecast", fmt.Errorf("%s", message)), nil
	}
	ref, ok := input["ref"].(string)
	if !ok || strings.TrimSpace(ref) == "" {
		return refuse("ref is required")
	}
	value, hasValue := input["forecast"]
	if hasValue {
		if _, ok := value.(string); !ok {
			return refuse("forecast must be a string")
		}
	}
	clear, hasClear := input["clear_forecast"]
	if hasClear {
		if _, ok := clear.(bool); !ok {
			return refuse("clear_forecast must be a boolean")
		}
	}
	source, hasSource := input["forecast_source"]
	if hasSource {
		if _, ok := source.(string); !ok {
			return refuse("forecast_source must be a string")
		}
	}
	if hasValue == (clear == true) || (hasClear && clear != true) {
		return refuse("exactly one of forecast or clear_forecast: true is required")
	}
	if clear == true && hasSource {
		return refuse("forecast_source requires forecast")
	}
	out := map[string]any{"ref": ref}
	if ws, ok := input["workspace"]; ok {
		out["workspace"] = ws
	}
	verb := "clear"
	if hasValue {
		verb = "set"
		out["value"] = value
		if hasSource {
			out["source"] = source
		}
	}
	return env.Dispatch(ctx, []string{"item", "remind", "forecast", verb}, out)
}
