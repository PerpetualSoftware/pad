package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestPastDeadlineMCPErrorParity(t *testing.T) {
	message := "This moves the target past its hard deadline (2030-03-06T09:00:00Z). Confirm to move it anyway: send confirm_past_deadline: true."
	body := `{"error":{"code":"target_after_deadline","message":"` + message + `","details":{"deadline":"2030-03-06T09:00:00Z"}}}`
	for _, result := range []*CallToolResult{
		classifyHTTPStatus(context.Background(), "item.remind.move", 409, []byte(body), nil),
		classifyExecError(context.Background(), nil, errors.New("exit 1"), structuredErrorMarker+body, nil),
		classifyExecError(context.Background(), nil, errors.New("exit 1"), "Error: "+message, nil),
	} {
		e := decodeEnvelope(t, result)
		if e.Error.Code != "target_after_deadline" {
			t.Fatalf("error=%+v", e)
		}
	}
}

func TestPastDeadlineMCPDispatch(t *testing.T) {
	for _, action := range []string{"schedule-resolve", "schedule-move"} {
		in := map[string]any{"ref": "TASK-5", "confirm_past_deadline": true}
		if action == "schedule-resolve" {
			in["state"], in["occurrence_at"], in["until"] = "move", "2030-03-04T09:00:00Z", "2030-03-08T09:00:00Z"
		} else {
			in["role"], in["to"], in["apply"], in["revision"] = "target", "2030-03-08T09:00:00Z", true, strings.Repeat("a", 64)
		}
		res, d := stdioCall(t, action, in)
		if res.IsError || !contains(d.gotArgs, "--past-deadline") {
			t.Fatalf("%s stdio=%s args=%v", action, textOf(res), d.gotArgs)
		}
		res, seen := remoteCall(t, action, in)
		if res.IsError || len(seen) != 1 {
			t.Fatalf("%s remote=%s requests=%v", action, textOf(res), seen)
		}
		var b map[string]any
		_ = json.Unmarshal([]byte(seen[0].Body), &b)
		if b["confirm_past_deadline"] != true {
			t.Fatalf("%s body=%v", action, b)
		}
	}
}

func TestPastDeadlineMCPValidation(t *testing.T) {
	for _, state := range []string{"done", "skip", "snooze", "ack"} {
		in := map[string]any{"ref": "TASK-5", "state": state, "occurrence_at": "2030-03-04T09:00:00Z", "confirm_past_deadline": true}
		if state == "snooze" {
			in["until"] = "2030-03-05T09:00:00Z"
		}
		a, d := stdioCall(t, "schedule-resolve", in)
		b, seen := remoteCall(t, "schedule-resolve", in)
		if !a.IsError || !b.IsError || len(d.gotPath) != 0 || len(seen) != 0 || textOf(a) != textOf(b) || !strings.Contains(textOf(a), "combination") {
			t.Fatalf("%s stdio=%s http=%s", state, textOf(a), textOf(b))
		}
	}
	for _, role := range []string{"target", "deadline"} {
		in := map[string]any{"ref": "TASK-5", "role": role, "to": "2030-03-08T09:00:00Z", "confirm_past_deadline": true, "apply": role != "target"}
		a, d := stdioCall(t, "schedule-move", in)
		b, seen := remoteCall(t, "schedule-move", in)
		if !a.IsError || !b.IsError || len(d.gotPath) != 0 || len(seen) != 0 || textOf(a) != textOf(b) {
			t.Fatalf("%s stdio=%s http=%s", role, textOf(a), textOf(b))
		}
	}
}
