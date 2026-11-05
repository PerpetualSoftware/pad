package mcp

import (
	"net/url"
	"strings"
	"testing"
)

// TASK-4237: pad_item.agenda with stale=true lists the caller's occurrences
// unresolved for more than 14 days. Every input member is read once, in
// actionItemAgenda, so stdio and /mcp cannot differ.
//
// Population of input[...] reads for this action: from, to, tz, stale (here);
// ref and workspace are not read by it.

func TestTASK4237_StaleAgendaOnBothTransports(t *testing.T) {
	for name, tc := range map[string]struct {
		in       map[string]any
		wantFrom string // "" = the member is not sent
	}{
		"no from":      {map[string]any{"stale": true}, ""},
		"from":         {map[string]any{"stale": true, "from": "2026-10-03T09:00:00Z"}, "2026-10-03T09:00:00Z"},
		"blank from":   {map[string]any{"stale": true, "from": "  "}, ""},
		"null from":    {map[string]any{"stale": true, "from": nil}, ""},
		"with tz":      {map[string]any{"stale": true, "tz": "Europe/Paris"}, ""},
		"null to":      {map[string]any{"stale": true, "to": nil}, ""},
		"false + wind": {map[string]any{"stale": false, "from": "2026-11-09T00:00:00Z", "to": "2026-11-16T00:00:00Z"}, "2026-11-09T00:00:00Z"},
	} {
		t.Run(name, func(t *testing.T) {
			sres, disp := stdioCall(t, "agenda", copyInput(tc.in))
			rres, seen := remoteCall(t, "agenda", withWorkspace(tc.in))
			if sres.IsError || rres.IsError {
				t.Fatalf("refused: stdio=%s remote=%s", textOf(sres), textOf(rres))
			}
			wantStale := tc.in["stale"] == true
			if got := contains(disp.gotArgs, "--stale"); got != wantStale {
				t.Errorf("stdio --stale = %v, want %v (args %q)", got, wantStale, disp.gotArgs)
			}
			if contains(disp.gotArgs, "--to") != (tc.in["to"] != nil && !wantStale) {
				t.Errorf("stdio --to wrong: args %q", disp.gotArgs)
			}
			if tc.wantFrom == "" && contains(disp.gotArgs, "--from") {
				t.Errorf("stdio sent --from: %q", disp.gotArgs)
			}
			if tc.wantFrom != "" && !hasFlagPair(disp.gotArgs, "--from", tc.wantFrom) {
				t.Errorf("stdio args = %q, want --from %s", disp.gotArgs, tc.wantFrom)
			}
			if len(seen) != 1 {
				t.Fatalf("remote requests = %+v", seen)
			}
			q, _ := url.ParseQuery(seen[0].Query)
			if q.Has("stale") != wantStale || (wantStale && q.Get("stale") != "true") {
				t.Errorf("remote query = %v, want stale=true only when asked", q)
			}
			if q.Has("to") != (tc.in["to"] != nil && !wantStale) {
				t.Errorf("remote to wrong: %v", q)
			}
			if got := q.Get("from"); got != tc.wantFrom {
				t.Errorf("remote from = %q, want %q", got, tc.wantFrom)
			}
		})
	}
}

func TestTASK4237_StaleAgendaRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		in   map[string]any
		want string
	}{
		"to with stale":     {map[string]any{"stale": true, "to": "2026-11-16T00:00:00Z"}, "to is not allowed with stale"},
		"blank to":          {map[string]any{"stale": true, "to": ""}, "to is not allowed with stale"},
		"non-bool stale":    {map[string]any{"stale": "true"}, "stale must be a boolean"},
		"number stale":      {map[string]any{"stale": 1, "from": "2026-11-09T00:00:00Z", "to": "2026-11-16T00:00:00Z"}, "stale must be a boolean"},
		"bad from":          {map[string]any{"stale": true, "from": "yesterday"}, "is not an RFC3339 instant"},
		"fractional from":   {map[string]any{"stale": true, "from": "2026-10-03T09:00:00.5Z"}, "fraction of a second"},
		"non-string from":   {map[string]any{"stale": true, "from": 5}, "from must be a string"},
		"non-string tz":     {map[string]any{"stale": true, "tz": false}, "tz must be a string"},
		"window missing to": {map[string]any{"stale": false, "from": "2026-11-09T00:00:00Z"}, "to is required"},
	} {
		t.Run(name, func(t *testing.T) {
			sres, disp := stdioCall(t, "agenda", copyInput(tc.in))
			rres, seen := remoteCall(t, "agenda", withWorkspace(tc.in))
			if !sres.IsError || !rres.IsError || len(disp.gotPath) != 0 || len(seen) != 0 {
				t.Fatalf("both must refuse before dispatch; stdio=%v/%v remote=%v/%v", sres.IsError, disp.gotPath, rres.IsError, seen)
			}
			if !strings.Contains(textOf(sres), tc.want) || textOf(sres) != textOf(rres) {
				t.Fatalf("stdio: %s\nremote: %s\nwant %q on both", textOf(sres), textOf(rres), tc.want)
			}
		})
	}
}
