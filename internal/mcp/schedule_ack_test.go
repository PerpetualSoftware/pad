package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestScheduleResolveAckTransportParity(t *testing.T) {
	in := map[string]any{"ref": "TASK-5", "state": "ack", "occurrence_at": "2026-11-13T09:00:00Z"}
	res, d := stdioCall(t, "schedule-resolve", in)
	if res.IsError || strings.Join(d.gotPath, " ") != "item remind ack" {
		t.Fatalf("stdio=%+v path=%v", res, d.gotPath)
	}
	res, seen := remoteCall(t, "schedule-resolve", in)
	if res.IsError || len(seen) != 1 || seen[0].Method != "POST" || seen[0].Path != "/api/v1/workspaces/docapp/items/TASK-5/commitment/resolve" {
		t.Fatalf("remote=%+v requests=%v", res, seen)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(seen[0].Body), &body)
	if body["state"] != "ack" || body["occurrence_at"] != in["occurrence_at"] || len(body) != 2 {
		t.Fatalf("body=%v", body)
	}
	for _, extra := range []map[string]any{{"occurrence_at": ""}, {"occurrence_at": "2026-11-13T09:00:00.1Z"}, {"until": ""}, {"comment": ""}, {"actual_done_at": ""}} {
		bad := copyInput(in)
		for k, v := range extra {
			bad[k] = v
		}
		a, disp := stdioCall(t, "schedule-resolve", bad)
		b, seen := remoteCall(t, "schedule-resolve", bad)
		if !a.IsError || !b.IsError || len(disp.gotPath) != 0 || len(seen) != 0 {
			t.Fatalf("invalid=%v stdio=%+v remote=%+v", extra, a, b)
		}
	}
}

func TestScheduleResolveAckErrorParity(t *testing.T) {
	for _, c := range []struct {
		code    string
		status  int
		details string
	}{{"not_fired", 409, ""}, {"stale", 409, `,"details":{"ended":true}`}, {"schedule_error", 409, `,"details":{"error_code":"syntax"}`}, {"validation_error", 400, `,"details":{"field":"comment","code":"combination"}`}} {
		body := fmt.Sprintf(`{"error":{"code":%q,"message":"Refused"%s}}`, c.code, c.details)
		for _, r := range []*CallToolResult{classifyHTTPStatus(context.Background(), "item remind ack", c.status, []byte(body), nil), classifyExecError(context.Background(), []string{"item", "remind", "ack"}, errors.New("exit status 1"), structuredErrorMarker+" "+body+"\n", nil)} {
			env, ok := r.StructuredContent.(ErrorEnvelope)
			if !ok || string(env.Error.Code) != c.code {
				t.Fatalf("%s became %+v", c.code, r)
			}
		}
	}
}
