package mcp

import (
	"context"
	"strings"
	"testing"
)

func TestAckReminderByRefDispatch(t *testing.T) {
	for _, tc := range []struct{ key, value, path string }{
		{"ref", "TASK-9", "/items/TASK-9/reminders/ack"},
		{"reminder_id", "67ec8261-505f-4ddc-8d40-fbb1393bb649", "/reminders/67ec8261-505f-4ddc-8d40-fbb1393bb649/ack"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			in := map[string]any{tc.key: tc.value}
			res, seen := remoteCall(t, "ack-reminder", in)
			if res.IsError || len(seen) != 1 {
				t.Fatalf("result = %+v, requests = %+v", res, seen)
			}
			if seen[0].Method != "POST" || seen[0].Path != "/api/v1/workspaces/docapp"+tc.path {
				t.Fatalf("request = %+v", seen[0])
			}
			local, disp := stdioCall(t, "ack-reminder", in)
			if local.IsError {
				t.Fatalf("local = %+v", local)
			}
			if pos := cliPositionals(disp.gotArgs); len(pos) != 1 || pos[0] != tc.value {
				t.Fatalf("CLI args = %v", disp.gotArgs)
			}
		})
	}
}

func TestAckReminderRejectsBothParams(t *testing.T) {
	in := map[string]any{"ref": "TASK-9", "reminder_id": "67ec8261-505f-4ddc-8d40-fbb1393bb649"}
	res, seen := remoteCall(t, "ack-reminder", in)
	if !res.IsError || len(seen) != 0 {
		t.Fatalf("result = %+v, requests = %+v", res, seen)
	}
	if code := envelopeFrom(res).Error.Code; code != ErrValidationFailed {
		t.Fatalf("code = %s", code)
	}
	res, disp := stdioCall(t, "ack-reminder", in)
	if !res.IsError || len(disp.gotArgs) != 0 {
		t.Fatalf("local result = %+v, args = %v", res, disp.gotArgs)
	}
}

func TestAmbiguousReminderErrorParity(t *testing.T) {
	body := `{"error":{"code":"ambiguous_reminder","message":"2 fired reminders; ack by id","details":{"reminder_ids":["r1","r2"]}}}`
	httpResult := classifyHTTPStatus(context.Background(), "item ack", 409, []byte(body), nil)
	stdioResult := extractStructuredCLIError(structuredErrorMarker + body)
	if stdioResult == nil {
		t.Fatal("structured CLI error was not extracted")
	}
	for _, res := range []*CallToolResult{httpResult, stdioResult} {
		p := envelopeFrom(res).Error
		if string(p.Code) != "ambiguous_reminder" || !strings.Contains(string(p.Details), "r1") {
			t.Fatalf("error = %+v", p)
		}
	}
}
