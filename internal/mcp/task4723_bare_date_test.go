package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TASK-4723: a bare until (snooze, move) and a bare schedule-move to reach
// dispatch unchanged on both transports; the server places them.
func TestTask4723_BareDateReachesDispatchUnchanged(t *testing.T) {
	for _, state := range []string{"snooze", "move"} {
		in := map[string]any{"ref": "TASK-1", "state": state, "occurrence_at": "2026-11-09T14:00:00Z", "until": "2026-11-10"}
		res, disp := stdioCall(t, "schedule-resolve", in)
		if res.IsError || !hasFlagPair(disp.gotArgs, "--until", "2026-11-10") {
			t.Fatalf("stdio %s: %s args %q", state, textOf(res), disp.gotArgs)
		}
		rres, seen := remoteCall(t, "schedule-resolve", withWorkspace(in))
		if rres.IsError || len(seen) != 1 {
			t.Fatalf("remote %s: %s %+v", state, textOf(rres), seen)
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(seen[0].Body), &body)
		if body["until"] != "2026-11-10" {
			t.Fatalf("remote %s body %s, want until unchanged", state, seen[0].Body)
		}
	}
	mv := map[string]any{"ref": "TASK-1", "role": "target", "to": "2026-11-10"}
	res, disp := stdioCall(t, "schedule-move", mv)
	if res.IsError || !hasFlagPair(disp.gotArgs, "--to", "2026-11-10") {
		t.Fatalf("stdio move: %s args %q", textOf(res), disp.gotArgs)
	}
	rres, seen := remoteCall(t, "schedule-move", withWorkspace(mv))
	if rres.IsError || len(seen) != 1 || !strings.Contains(seen[0].Body, `"to":"2026-11-10"`) {
		t.Fatalf("remote move: %s %+v", textOf(rres), seen)
	}
	// A value that is neither names both forms and dispatches nothing.
	for _, c := range []struct {
		action string
		in     map[string]any
	}{
		{"schedule-resolve", map[string]any{"ref": "TASK-1", "state": "snooze", "occurrence_at": "2026-11-09T14:00:00Z", "until": "2026-13-40"}},
		{"schedule-move", map[string]any{"ref": "TASK-1", "role": "target", "to": "next tuesday"}},
	} {
		sres, disp := stdioCall(t, c.action, c.in)
		rres, seen := remoteCall(t, c.action, withWorkspace(c.in))
		if !sres.IsError || !rres.IsError || len(disp.gotPath) != 0 || len(seen) != 0 || !strings.Contains(textOf(sres), "bare date (YYYY-MM-DD)") || !strings.Contains(textOf(rres), "bare date (YYYY-MM-DD)") {
			t.Errorf("%s malformed: stdio %s / remote %s", c.action, textOf(sres), textOf(rres))
		}
	}
}

// zone_unset on until and to maps to validation_error on both transports.
func TestTask4723_ZoneUnsetKeepsOneCodeAcrossTransports(t *testing.T) {
	for _, c := range []struct{ cmd, field string }{
		{"item remind snooze", "until"},
		{"item remind move", "until"},
		{"item remind move", "to"},
	} {
		body := fmt.Sprintf(`{"error":{"code":"validation_error","message":"Refused","details":{"field":%q,"code":"zone_unset"}}}`, c.field)
		var codes []string
		for _, r := range []*CallToolResult{
			classifyHTTPStatus(context.Background(), c.cmd, 400, []byte(body), nil),
			classifyExecError(context.Background(), strings.Fields(c.cmd), errors.New("exit status 1"), structuredErrorMarker+" "+body+"\n", nil),
		} {
			env, ok := r.StructuredContent.(ErrorEnvelope)
			if !ok {
				t.Fatalf("%s %s: %+v", c.cmd, c.field, r)
			}
			codes = append(codes, string(env.Error.Code))
		}
		if codes[0] != codes[1] || codes[0] != "validation_error" {
			t.Errorf("%s %s zone_unset: HTTP %s, stdio %s, want validation_error on both", c.cmd, c.field, codes[0], codes[1])
		}
	}
}
