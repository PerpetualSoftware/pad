package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TASK-4537: end_schedule on schedule-resolve and the schedule-dismiss action.

func TestScheduleResolveEndScheduleParity(t *testing.T) {
	for _, state := range []string{"done", "skip"} {
		in := map[string]any{"ref": "TASK-5", "state": state, "occurrence_at": "2026-11-13T09:00:00Z", "end_schedule": true}
		res, d := stdioCall(t, "schedule-resolve", in)
		if res.IsError || strings.Join(d.gotPath, " ") != "item remind "+state || !contains(d.gotArgs, "--end-schedule") {
			t.Fatalf("%s stdio=%+v path=%v args=%v", state, res, d.gotPath, d.gotArgs)
		}
		res, seen := remoteCall(t, "schedule-resolve", in)
		if res.IsError || len(seen) != 1 || seen[0].Path != "/api/v1/workspaces/docapp/items/TASK-5/commitment/resolve" {
			t.Fatalf("%s remote=%+v requests=%v", state, res, seen)
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(seen[0].Body), &body)
		if body["end_schedule"] != true || body["state"] != state {
			t.Fatalf("%s body=%v", state, body)
		}
		// false is the default and is not sent.
		in["end_schedule"] = false
		_, d = stdioCall(t, "schedule-resolve", in)
		_, seen = remoteCall(t, "schedule-resolve", in)
		if contains(d.gotArgs, "--end-schedule") || len(seen) != 1 || strings.Contains(seen[0].Body, "end_schedule") {
			t.Fatalf("%s false sent: args=%v body=%v", state, d.gotArgs, seen)
		}
	}
	for name, bad := range map[string]map[string]any{
		"not a boolean": {"ref": "TASK-5", "state": "done", "occurrence_at": "2026-11-13T09:00:00Z", "end_schedule": "yes"},
		"with snooze":   {"ref": "TASK-5", "state": "snooze", "occurrence_at": "2026-11-13T09:00:00Z", "until": "2099-01-01T09:00:00Z", "end_schedule": true},
		"with move":     {"ref": "TASK-5", "state": "move", "occurrence_at": "2026-11-13T09:00:00Z", "until": "2099-01-01T09:00:00Z", "end_schedule": true},
		"with ack":      {"ref": "TASK-5", "state": "ack", "occurrence_at": "2026-11-13T09:00:00Z", "end_schedule": false},
	} {
		a, disp := stdioCall(t, "schedule-resolve", bad)
		b, seen := remoteCall(t, "schedule-resolve", bad)
		if !a.IsError || !b.IsError || len(disp.gotPath) != 0 || len(seen) != 0 || textOf(a) != textOf(b) {
			t.Fatalf("%s: stdio=%s remote=%s", name, textOf(a), textOf(b))
		}
		if name != "not a boolean" && !strings.Contains(textOf(a), "combination") {
			t.Fatalf("%s: %s", name, textOf(a))
		}
	}
}

func TestScheduleDismissParity(t *testing.T) {
	in := map[string]any{"ref": "TASK-5"}
	res, d := stdioCall(t, "schedule-dismiss", in)
	if res.IsError || strings.Join(d.gotPath, " ") != "item remind dismiss" {
		t.Fatalf("stdio=%+v path=%v", res, d.gotPath)
	}
	res, seen := remoteCall(t, "schedule-dismiss", map[string]any{"ref": "TASK-5", "workspace": "docapp"})
	if res.IsError || len(seen) != 1 || seen[0].Method != "POST" || seen[0].Path != "/api/v1/workspaces/docapp/items/TASK-5/commitment/dismiss-ended" {
		t.Fatalf("remote=%+v requests=%v", res, seen)
	}
	if isReadOnlyAction("pad_item", "schedule-dismiss") {
		t.Fatal("schedule-dismiss is a write")
	}
}

func TestScheduleEndErrorParity(t *testing.T) {
	for _, c := range []struct {
		cmd, code, details string
	}{
		{"item remind done", "item_still_open", `,"details":{"state":"done","done_field":"status","close_options":["done"],"ack_available":false}`},
		{"item remind skip", "item_still_open", `,"details":{"state":"skip","done_field":"status","close_options":["done"],"ack_available":true}`},
		{"item remind done", "validation_error", `,"details":{"field":"end_schedule","code":"combination"}`},
		{"item remind dismiss", "not_ended", ""},
	} {
		body := fmt.Sprintf(`{"error":{"code":%q,"message":"Refused"%s}}`, c.code, c.details)
		status := 409
		if c.code == "validation_error" {
			status = 400
		}
		for _, r := range []*CallToolResult{
			classifyHTTPStatus(context.Background(), c.cmd, status, []byte(body), nil),
			classifyExecError(context.Background(), strings.Fields(c.cmd), errors.New("exit status 1"), structuredErrorMarker+" "+body+"\n", nil),
		} {
			env, ok := r.StructuredContent.(ErrorEnvelope)
			if !ok || string(env.Error.Code) != c.code {
				t.Fatalf("%s %s became %+v", c.cmd, c.code, r)
			}
		}
	}
}
