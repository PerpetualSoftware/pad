package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/PerpetualSoftware/pad/internal/models"
	"net/http"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// Remote mappers for the schedule actions of pad_item (TASK-4105). They build
// the same request bodies the CLI builds from its flags (cmd/pad/commitment_flags.go);
// the parameters carry the flags' names. Rules only the server can judge (the
// firing floor on HOURLY and MINUTELY rules, anchor completion with a BYxxx
// part, an occurrence that never comes) are left to it, and its message is
// returned unchanged. The rule text is sent as given: the server normalizes
// the shorthands and RRULE text itself.

const (
	itemSchedulePathTemplate = "/api/v1/workspaces/{workspace}/items/{ref}/commitment"
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
	body, err := scheduleUpdateBody(p, currentStart(input))
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
		if actual, _ := input["actual_done_at"].(string); actual != "" {
			body["actual_done_at"] = actual
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

func commitmentFlags(p scheduleParams) schedule.CommitmentFlags {
	return schedule.CommitmentFlags{Every: p.every, Start: p.start, Deadline: p.deadline, Window: p.window, Anchor: p.anchor, Audience: p.audience, Tz: p.tz, Floating: p.floating}
}

func scheduleCreateBody(p scheduleParams) (map[string]any, error) {
	return schedule.CommitmentBody(commitmentFlags(p), nil, "")
}

func scheduleUpdateBody(p scheduleParams, currentStart string) (map[string]any, error) {
	return schedule.CommitmentBody(commitmentFlags(p), func(name string) bool { return p.given[name] }, currentStart)
}

func currentStart(input map[string]any) string {
	value, _ := input["commitment_start"].(string)
	return value
}

// dispatchItemCommitmentEdit prefetches only when a window offset needs the
// current start. Other edits stay a single PATCH, including deadline refusals.
func (d *HTTPHandlerDispatcher) dispatchItemCommitmentEdit(ctx context.Context, input map[string]any, user *models.User) (*CallToolResult, error) {
	p, err := readScheduleParams(input)
	if err != nil {
		return validationFailedResult("item remind edit", err.Error(), "Check the schedule flags."), nil
	}
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
		copied := make(map[string]any, len(input)+1)
		for k, v := range input {
			copied[k] = v
		}
		copied["commitment_start"] = current.Dtstart
		input = copied
	}
	method, path, body, err := mapItemScheduleEdit(input)
	if err != nil {
		return validationFailedResult("item remind edit", err.Error(), "Check the schedule flags."), nil
	}
	return d.executeRequest(ctx, "item remind edit", user, method, path, body)
}
