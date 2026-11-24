package mcp

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func errorCodeOf(t *testing.T, res *CallToolResult) ErrorCode {
	t.Helper()
	if env, ok := res.StructuredContent.(ErrorEnvelope); ok {
		return env.Error.Code
	}
	var env ErrorEnvelope
	if err := json.Unmarshal([]byte(textOf(res)), &env); err == nil && env.Error.Code != "" {
		return env.Error.Code
	}
	return ""
}

func TestMCPAgendaAudienceDefaultAgent(t *testing.T) {
	window := map[string]any{"from": "2026-11-09T00:00:00Z", "to": "2026-11-16T00:00:00Z"}

	sres, disp := stdioCall(t, "agenda", copyInput(window))
	rres, seen := remoteCall(t, "agenda", withWorkspace(window))
	if sres.IsError || rres.IsError {
		t.Fatalf("refused: stdio=%s remote=%s", textOf(sres), textOf(rres))
	}
	if !hasFlagPair(disp.gotArgs, "--audience", "agent") {
		t.Errorf("stdio args = %q, want --audience agent", disp.gotArgs)
	}
	if len(seen) != 1 {
		t.Fatalf("remote requests = %+v", seen)
	}
	q, _ := url.ParseQuery(seen[0].Query)
	if q.Get("audience") != "agent" {
		t.Errorf("remote query = %v, want audience=agent", q)
	}

	humanIn := copyInput(window)
	humanIn["audience"] = "human"
	sres, disp = stdioCall(t, "agenda", copyInput(humanIn))
	rres, seen = remoteCall(t, "agenda", withWorkspace(humanIn))
	if sres.IsError || rres.IsError {
		t.Fatalf("human refused: stdio=%s remote=%s", textOf(sres), textOf(rres))
	}
	if !hasFlagPair(disp.gotArgs, "--audience", "human") {
		t.Errorf("stdio args = %q, want --audience human", disp.gotArgs)
	}
	q, _ = url.ParseQuery(seen[0].Query)
	if q.Get("audience") != "human" {
		t.Errorf("remote query = %v, want audience=human", q)
	}
}

func TestMCPAgendaAllWorkspacesRoute(t *testing.T) {
	in := map[string]any{
		"from":           "2026-11-09T00:00:00Z",
		"to":             "2026-11-16T00:00:00Z",
		"tz":             "America/New_York",
		"audience":       "human",
		"all_workspaces": true,
	}

	sres, disp := stdioCall(t, "agenda", copyInput(in))
	rres, seen := remoteCall(t, "agenda", copyInput(in))
	if sres.IsError || rres.IsError {
		t.Fatalf("refused: stdio=%s remote=%s", textOf(sres), textOf(rres))
	}
	if !contains(disp.gotArgs, "--all-workspaces") {
		t.Errorf("stdio args = %q, want --all-workspaces", disp.gotArgs)
	}
	if !hasFlagPair(disp.gotArgs, "--audience", "human") {
		t.Errorf("stdio args = %q, want --audience human", disp.gotArgs)
	}
	if contains(disp.gotArgs, "--workspace") {
		t.Errorf("stdio args leaked session --workspace: %q", disp.gotArgs)
	}
	if len(seen) != 1 {
		t.Fatalf("remote requests = %+v", seen)
	}
	if seen[0].Path != "/api/v1/me/agenda" {
		t.Fatalf("remote path = %q, want /api/v1/me/agenda", seen[0].Path)
	}
	q, _ := url.ParseQuery(seen[0].Query)
	if q.Get("audience") != "human" || q.Get("counts") != "omit" || !q.Has("include") || q.Get("include") != "" || q.Get("tz") != "America/New_York" {
		t.Errorf("remote query = %v", q)
	}
	if q.Has("workspace") {
		t.Errorf("remote query leaked session workspace: %v", q)
	}
}

func TestMCPAgendaAllWorkspacesRefusals(t *testing.T) {
	cases := map[string]map[string]any{
		"explicit workspace": {"from": "2026-11-09T00:00:00Z", "to": "2026-11-16T00:00:00Z", "all_workspaces": true, "workspace": "docapp"},
		"recipient all":      {"from": "2026-11-09T00:00:00Z", "to": "2026-11-16T00:00:00Z", "all_workspaces": true, "recipient": "all"},
		"recipient id":       {"from": "2026-11-09T00:00:00Z", "to": "2026-11-16T00:00:00Z", "all_workspaces": true, "recipient": "user-123"},
		"stale":              {"stale": true, "all_workspaces": true},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			sres, disp := stdioCall(t, "agenda", copyInput(in))
			rres, seen := remoteCall(t, "agenda", copyInput(in))
			if !sres.IsError || !rres.IsError || len(disp.gotPath) != 0 || len(seen) != 0 {
				t.Fatalf("both must refuse before dispatch; stdio=%v/%v remote=%v/%v", sres.IsError, disp.gotPath, rres.IsError, seen)
			}
			sc, rc := errorCodeOf(t, sres), errorCodeOf(t, rres)
			if sc == "" || sc != rc {
				t.Fatalf("error code mismatch: stdio=%q (%s) remote=%q (%s)", sc, textOf(sres), rc, textOf(rres))
			}
			if !strings.Contains(textOf(sres), "all_workspaces") || textOf(sres) != textOf(rres) {
				t.Fatalf("unexpected refusal text: stdio=%s remote=%s", textOf(sres), textOf(rres))
			}
		})
	}
}

func TestMCPAgendaStaleRefusesAudience(t *testing.T) {
	in := map[string]any{"stale": true, "audience": "human"}
	sres, disp := stdioCall(t, "agenda", copyInput(in))
	rres, seen := remoteCall(t, "agenda", copyInput(in))
	if !sres.IsError || !rres.IsError || len(disp.gotPath) != 0 || len(seen) != 0 {
		t.Fatalf("both must refuse before dispatch; stdio=%v/%v remote=%v/%v", sres.IsError, disp.gotPath, rres.IsError, seen)
	}
	sc, rc := errorCodeOf(t, sres), errorCodeOf(t, rres)
	if sc == "" || sc != rc || textOf(sres) != textOf(rres) || !strings.Contains(textOf(sres), "drop audience") {
		t.Fatalf("unexpected refusal: stdio=%q %s remote=%q %s", sc, textOf(sres), rc, textOf(rres))
	}
}
