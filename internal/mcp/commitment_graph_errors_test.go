package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestTASK4423_R04Errors(t *testing.T) {
	rows := []struct {
		code, message string
		status        int
	}{
		{"schedule_linked", "this schedule is linked; unlink it before changing its timing or a linked role", 409},
		{"schedule_linked", "This date is linked to something you can't view. Ask a workspace owner to change or remove the link.", 409},
		{"schedule_linked", "This date is linked to something you can't view. You can remove the link.", 409},
		{"deadline_approval_required", "this change moves a hard deadline that needs its owner's approval", 409},
		{"link_violation_ack", "a linked date lands outside one of its own bounds; acknowledge to apply", 409},
		{"commitment_changed", "The commitment changed; re-preview before applying", 409},
		{"link_budget", "the linked change took too long and was not applied", 503},
		{"link_edit_required", "linking commitments needs edit on both items", 403},
		{"schedule_link_not_found", "Commitment link or gate not found", 404},
		{"commitment_preview_required", "This linked change needs a commitment move preview before applying", 409},
	}
	for _, c := range []string{"link_cycle", "link_self", "link_duplicate_incoming", "link_limit", "gate_duplicate", "link_invalid"} {
		status := 409
		if c == "link_invalid" {
			status = 400
		}
		rows = append(rows, struct {
			code, message string
			status        int
		}{c, c, status})
	}
	for _, r := range rows {
		t.Run(r.code+"/"+r.message, func(t *testing.T) {
			body := fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, r.code, r.message)
			results := []*CallToolResult{classifyHTTPStatus(context.Background(), "item.update", r.status, []byte(body), nil), classifyExecError(context.Background(), []string{"item", "update"}, errors.New("exit 1"), structuredErrorMarker+body, nil), classifyExecError(context.Background(), []string{"item", "update"}, errors.New("exit 1"), "Error: "+r.message, nil)}
			for i, res := range results {
				env := decodeEnvelope(t, res)
				if string(env.Error.Code) != r.code {
					t.Errorf("transport %d code=%s want %s", i, env.Error.Code, r.code)
				}
				b, _ := json.Marshal(res)
				if strings.Contains(string(b), "hidden-sentinel-id") {
					t.Fatal("hidden id exposed")
				}
			}
		})
	}
	for _, field := range []string{"reason", "revision", "role", "to", "cancel_policy"} {
		t.Run(field, func(t *testing.T) {
			body := fmt.Sprintf(`{"error":{"code":"validation_error","message":"schedule %s: required","details":{"field":%q,"code":"required"}}}`, field, field)
			for _, res := range []*CallToolResult{classifyHTTPStatus(context.Background(), "item.update", 400, []byte(body), nil), classifyExecError(context.Background(), nil, errors.New("exit 1"), structuredErrorMarker+body, nil)} {
				e := decodeEnvelope(t, res)
				if e.Error.Code != "validation_error" || !strings.Contains(string(e.Error.Details), field) {
					t.Errorf("field contract: %+v", e)
				}
			}
		})
	}
	for _, row := range []struct{ message, field, code string }{
		{"schedule reason: moving a hard deadline needs a reason", "reason", "required"},
		{"schedule revision: this change needs the preview revision", "revision", "required"},
		{"schedule role: the commitment has no deadline date to move", "role", "combination"},
		{"schedule to: to must be an RFC3339 instant", "to", "format"},
		{"schedule cancel_policy: cancel_policy must be waiting_on or cancel_child_occurrence", "cancel_policy", "enum"},
	} {
		t.Run("plain field "+row.field, func(t *testing.T) {
			e := decodeEnvelope(t, classifyExecError(context.Background(), nil, errors.New("exit 1"), "Error: "+row.message, nil))
			var d map[string]string
			if err := json.Unmarshal(e.Error.Details, &d); err != nil {
				t.Fatal(err)
			}
			if e.Error.Code != "validation_error" || d["field"] != row.field || d["code"] != row.code {
				t.Fatalf("plain field contract: %+v", e)
			}
		})
	}

}
