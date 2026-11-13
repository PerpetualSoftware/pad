package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// Remote mappers for the schedule actions of pad_item (TASK-4105). They build
// the same request bodies the CLI builds from its flags (internal/schedule/commitment_flags.go);
// the parameters carry the flags' names. Rules only the server can judge (the
// firing floor on HOURLY and MINUTELY rules, anchor completion with a BYxxx
// part, an occurrence that never comes) are left to it, and its message is
// returned unchanged. The shared helper canonicalizes rule syntax; the server
// validates the complete timing.

const (
	itemSchedulePathTemplate = "/api/v1/workspaces/{workspace}/items/{ref}/commitment"
	itemScheduleResolvePath  = itemSchedulePathTemplate + "/resolve"

	itemScheduleRenegotiatePath = itemSchedulePathTemplate + "/renegotiate"
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
	for _, k := range scheduleParamKeys {
		dst, isStr := strs[k]
		if !isStr {
			continue
		}
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
	if remindAtGiven(input) {
		return "", "", nil, errRemindAtWithSchedule
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
	body, err := scheduleUpdateBody(p, "")
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

// mapScheduleResolve is `item remind done|skip|snooze|move|ack`: the state is the
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
		body := schedule.CommitmentResolve{State: state, OccurrenceAt: occ}
		if c, _ := input["comment"].(string); c != "" {
			body.Comment = c
		}
		if u, _ := input["until"].(string); u != "" {
			body.Until = u
		}
		if a, _ := input["actual_done_at"].(string); a != "" && state == "done" {
			body.ActualDoneAt = a
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return "", "", nil, err
		}
		return http.MethodPost, path, raw, nil
	}
}

// mapScheduleRenegotiate is `item remind renegotiate`: a preview unless apply
// is set. The input is what actionItemScheduleRenegotiate left; the scope
// defaults as the CLI's does, and which members belong with which scope is left
// to the server.
func mapScheduleRenegotiate(input map[string]any) (string, string, []byte, error) {
	str := func(k string) string { s, _ := input[k].(string); return strings.TrimSpace(s) }
	reason := str("reason")
	if reason == "" {
		return "", "", nil, fmt.Errorf("reason is required: a deadline is renegotiated with a reason on record")
	}
	scope := str("scope")
	if scope == "" {
		scope = "occurrence"
	}
	apply := false
	if p, ok := input["preview"].(bool); ok {
		apply = !p
	}
	body := map[string]any{"reason": reason, "scope": scope, "preview": !apply}
	if d := str("deadline"); d != "" {
		body["deadline"] = d
	}
	off := map[string]any{}
	for in, member := range map[string]string{"deadline_days": "deadline_days", "deadline_secs": "deadline_secs"} {
		if n, ok, err := intParam(input, in); err != nil {
			return "", "", nil, err
		} else if ok {
			off[member] = n
		}
	}
	if len(off) > 0 {
		body["deadline_offset"] = off
	}
	if b := str("deadline_behavior"); b != "" {
		body["deadline_behavior"] = b
	}
	if rm, _ := input["remove_deadline"].(bool); rm {
		body["remove_deadline"] = true
	}
	if r := str("revision"); r != "" {
		body["revision"] = r
	}
	return marshalScheduleRequest(http.MethodPost, itemScheduleRenegotiatePath, input, body)
}

func commitmentFlags(p scheduleParams) schedule.CommitmentFlags {
	return schedule.CommitmentFlags{Every: p.every, Start: p.start, Deadline: p.deadline, Window: p.window, Anchor: p.anchor, Audience: p.audience, Tz: p.tz, Floating: p.floating}
}

func scheduleCreateBody(p scheduleParams) (map[string]any, error) {
	return schedule.CommitmentBody(commitmentFlags(p), nil, "")
}

func scheduleUpdateBody(p scheduleParams, currentStart string) (map[string]any, error) {
	return schedule.CommitmentBody(commitmentFlags(p), func(name string) bool { return p.given[name] }, currentStart)
}

// dispatchItemCommitmentEdit prefetches only when a window offset needs the
// current start. Other edits stay a single PATCH, including deadline refusals.
func (d *HTTPHandlerDispatcher) dispatchItemCommitmentEdit(ctx context.Context, input map[string]any, user *models.User) (*CallToolResult, error) {
	p, err := readScheduleParams(input)
	if err != nil {
		return validationFailedResult("item remind edit", err.Error(), "Check the schedule flags."), nil
	}
	start := ""
	if p.given["window"] && !p.given["start"] && !p.given["deadline"] {
		path, err := expandPath(itemSchedulePathTemplate, input)
		if err != nil {
			return validationFailedResult("item remind edit", err.Error(), "Pass workspace and ref."), nil
		}
		result, err := d.executeRequest(ctx, "item remind show", user, http.MethodGet, path, nil)
		if err != nil || result.IsError {
			return result, err
		}
		var current struct {
			Dtstart string `json:"dtstart"`
		}
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &current); err != nil {
			return nil, err
		}
		start = current.Dtstart
	}
	body, err := scheduleUpdateBody(p, start)
	if err != nil {
		return validationFailedResult("item remind edit", err.Error(), "Check the schedule flags."), nil
	}
	method, path, raw, err := marshalScheduleRequest(http.MethodPatch, itemSchedulePathTemplate, input, body)
	if err != nil {
		return validationFailedResult("item remind edit", err.Error(), "Pass workspace and ref."), nil
	}

	return d.executeRequest(ctx, "item remind edit", user, method, path, raw)
}

func graphWriteBody(input map[string]any, keys ...string) map[string]any {
	body := map[string]any{}
	for _, k := range keys {
		if v, ok := input[k]; ok && v != nil {
			body[k] = v
		}
	}
	return body
}

func mapScheduleLink(edit bool) RouteMapper {
	return func(input map[string]any) (string, string, []byte, error) {
		method, template := http.MethodPost, itemSchedulePathTemplate+"/links"
		if edit {
			method, template = http.MethodPatch, "/api/v1/workspaces/{workspace}/commitment-links/{link_id}"
		}
		body := graphWriteBody(input, "cancel_policy", "reason", "revision", "acknowledge_violations")
		if off, ok := input["link_offset"]; ok && off != nil {
			body["offset"] = off
		}
		if !edit {
			for _, k := range []string{"child", "parent_role", "child_role"} {
				body[k] = input[k]
			}
		}
		if apply, _ := input["apply"].(bool); !apply {
			body["preview"] = true
		}
		return marshalScheduleRequest(method, template, input, body)
	}
}

func mapScheduleMove(input map[string]any) (string, string, []byte, error) {
	if role, _ := input["role"].(string); role == "" {
		return mapScheduleResolve("move")(input)
	}
	body := graphWriteBody(input, "role", "to", "reason")
	path := itemSchedulePathTemplate + "/move-preview"
	if apply, _ := input["apply"].(bool); apply {
		path = itemSchedulePathTemplate + "/move"
		for _, k := range []string{"revision", "acknowledge_violations"} {
			if v, ok := input[k]; ok {
				body[k] = v
			}
		}
	}
	return marshalScheduleRequest(http.MethodPost, path, input, body)
}
