package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestScheduleReopenMCPParity(t *testing.T) {
	in := map[string]any{"ref": "TASK-5", "resolution_id": "11111111-1111-4111-8111-111111111111"}
	res, d := stdioCall(t, "schedule-reopen", in)
	if res.IsError || strings.Join(d.gotPath, " ") != "item remind reopen" {
		t.Fatalf("stdio=%+v path=%v", res, d.gotPath)
	}
	res, seen := remoteCall(t, "schedule-reopen", in)
	if res.IsError || len(seen) != 1 || seen[0].Method != "POST" || seen[0].Path != "/api/v1/workspaces/docapp/items/TASK-5/commitment/reopen" {
		t.Fatalf("remote=%+v requests=%v", res, seen)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(seen[0].Body), &body)
	if len(body) != 1 || body["resolution_id"] != in["resolution_id"] {
		t.Fatalf("body=%v", body)
	}
	for _, bad := range []map[string]any{{"ref": "TASK-5"}, {"ref": "TASK-5", "resolution_id": 1}} {
		a, disp := stdioCall(t, "schedule-reopen", bad)
		b, seen := remoteCall(t, "schedule-reopen", bad)
		if !a.IsError || !b.IsError || len(disp.gotPath) != 0 || len(seen) != 0 {
			t.Fatalf("invalid dispatched: %v", bad)
		}
	}
	for _, code := range []string{"reopen_unavailable", "reopen_not_supported", "reopen_conflict", "reopen_dependency", "reopen_deadline_passed", "validation_error"} {
		status, details := 409, ""
		if code == "validation_error" {
			status = 400
			details = `,"details":{"field":"resolution_id","code":"required"}`
		}
		body := fmt.Sprintf(`{"error":{"code":%q,"message":"Refused"%s}}`, code, details)
		for _, r := range []*CallToolResult{classifyHTTPStatus(context.Background(), "item remind reopen", status, []byte(body), nil), classifyExecError(context.Background(), []string{"item", "remind", "reopen"}, errors.New("exit status 1"), structuredErrorMarker+" "+body+"\n", nil)} {
			env, ok := r.StructuredContent.(ErrorEnvelope)
			if !ok || string(env.Error.Code) != code {
				t.Fatalf("%s became %+v", code, r)
			}
			if code == "validation_error" {
				raw, _ := json.Marshal(env)
				if !strings.Contains(string(raw), `"field":"resolution_id"`) {
					t.Fatal(string(raw))
				}
			}
		}
	}
}
