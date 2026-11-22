package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TASK-4699 PR2: day_start on pad_item remind / schedule-edit, on both
// transports, and the new refusals keep one code across them.
func TestTask4699_DayStartParity(t *testing.T) {
	in := map[string]any{"ref": "TASK-1", "start": "2026-11-09", "every": "daily", "day_start": true}
	_, disp := stdioCall(t, "remind", in)
	if strings.Join(disp.gotPath, " ") != "item remind" || !hasFlag(disp.gotArgs, "--day-start") || !hasFlagPair(disp.gotArgs, "--start", "2026-11-09") {
		t.Fatalf("stdio: path %q args %q", disp.gotPath, disp.gotArgs)
	}
	res, seen := remoteCall(t, "remind", withWorkspace(in))
	if res.IsError || len(seen) != 1 || seen[0].Method != "POST" {
		t.Fatalf("remote: %s %+v", textOf(res), seen)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(seen[0].Body), &got)
	want := map[string]any{"roles": map[string]any{"target_days": float64(0), "target_secs": float64(0)}, "dtstart": "2026-11-09", "rrule": "FREQ=DAILY", "day_start": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remote body = %v, want %v", got, want)
	}

	edit := map[string]any{"ref": "TASK-1", "start": "2026-11-10", "day_start": true}
	_, disp = stdioCall(t, "schedule-edit", edit)
	if !hasFlag(disp.gotArgs, "--day-start") {
		t.Fatalf("stdio edit args %q", disp.gotArgs)
	}
	res, seen = remoteCall(t, "schedule-edit", withWorkspace(edit))
	if res.IsError || len(seen) != 1 || seen[0].Method != "PATCH" || seen[0].Body != `{"day_start":true,"dtstart":"2026-11-10"}` {
		t.Fatalf("remote edit: %s %+v", textOf(res), seen)
	}

	// false is inert on both, like floating=false; a non-boolean is refused on both.
	_, disp = stdioCall(t, "remind", map[string]any{"ref": "TASK-1", "start": "2026-11-09", "day_start": false})
	if hasFlag(disp.gotArgs, "--day-start") {
		t.Fatalf("day_start=false reached the CLI: %q", disp.gotArgs)
	}
	_, seen = remoteCall(t, "remind", withWorkspace(map[string]any{"ref": "TASK-1", "start": "2026-11-09", "day_start": false}))
	if len(seen) != 1 || strings.Contains(seen[0].Body, "day_start") {
		t.Fatalf("day_start=false reached the route: %+v", seen)
	}
	bad := map[string]any{"ref": "TASK-1", "start": "2026-11-09", "day_start": "yes"}
	sres, disp := stdioCall(t, "remind", bad)
	rres, seen := remoteCall(t, "remind", withWorkspace(bad))
	if !sres.IsError || !rres.IsError || len(disp.gotPath) != 0 || len(seen) != 0 || !strings.Contains(textOf(sres), "day_start must be a boolean") || !strings.Contains(textOf(rres), "day_start must be a boolean") {
		t.Fatalf("non-boolean: stdio %s / remote %s", textOf(sres), textOf(rres))
	}
	// --window with day_start is refused before dispatch on /mcp, and by the
	// CLI on stdio.
	win := map[string]any{"ref": "TASK-1", "start": "2026-11-09", "window": "3d", "day_start": true}
	if res, seen := remoteCall(t, "remind", withWorkspace(win)); !res.IsError || len(seen) != 0 || !strings.Contains(textOf(res), "--window") {
		t.Fatalf("remote window: %s %+v", textOf(res), seen)
	}
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

func TestTask4699_RefusalsKeepOneCodeAcrossTransports(t *testing.T) {
	for _, c := range []struct {
		cmd, code, details string
	}{
		{"item remind", "validation_error", `,"details":{"field":"day_start","code":"requires_date"}`},
		{"item remind", "validation_error", `,"details":{"field":"day_start","code":"audience_agent"}`},
		{"item remind edit", "validation_error", `,"details":{"field":"day_start","code":"zone_unset"}`},
		{"item remind", "invalid_remind_at", ""},
	} {
		body := fmt.Sprintf(`{"error":{"code":%q,"message":"Refused"%s}}`, c.code, c.details)
		route := "item remind"
		if c.code == "validation_error" {
			route = c.cmd
		}
		var codes []string
		for _, r := range []*CallToolResult{
			classifyHTTPStatus(context.Background(), route, 400, []byte(body), nil),
			classifyExecError(context.Background(), strings.Fields(c.cmd), errors.New("exit status 1"), structuredErrorMarker+" "+body+"\n", nil),
		} {
			env, ok := r.StructuredContent.(ErrorEnvelope)
			if !ok {
				t.Fatalf("%s %s: %+v", c.cmd, c.code, r)
			}
			codes = append(codes, string(env.Error.Code))
		}
		if codes[0] != codes[1] {
			t.Errorf("%s %s%s: HTTP %s, stdio %s", c.cmd, c.code, c.details, codes[0], codes[1])
		}
		if c.code == "validation_error" && codes[0] != "validation_error" {
			t.Errorf("%s day_start refusal became %s, want validation_error", c.details, codes[0])
		}
	}
}
