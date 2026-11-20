package mcp

import (
	"net/url"
	"strings"
	"testing"
)

// TASK-4489: pad_item.agenda takes recipient. Absent, null or blank is me;
// all is the workspace scope (stdio passes --recipient all, the route sends no
// recipient); a user ID passes through; stale refuses a recipient.
func TestTASK4489_AgendaRecipientOnBothTransports(t *testing.T) {
	window := map[string]any{"from": "2026-11-09T00:00:00Z", "to": "2026-11-16T00:00:00Z"}
	for name, tc := range map[string]struct {
		recipient  any
		set        bool
		wantStdio  string // the --recipient value on stdio
		wantRemote string // "" means the query carries no recipient
	}{
		"absent": {nil, false, "me", "me"},
		"null":   {nil, true, "me", "me"},
		"blank":  {"  ", true, "me", "me"},
		"me":     {"me", true, "me", "me"},
		"all":    {"all", true, "all", ""},
		"id":     {"user-123", true, "user-123", "user-123"},
	} {
		t.Run(name, func(t *testing.T) {
			in := copyInput(window)
			if tc.set {
				in["recipient"] = tc.recipient
			}
			sres, disp := stdioCall(t, "agenda", copyInput(in))
			rres, seen := remoteCall(t, "agenda", withWorkspace(in))
			if sres.IsError || rres.IsError {
				t.Fatalf("refused: stdio=%s remote=%s", textOf(sres), textOf(rres))
			}
			if !hasFlagPair(disp.gotArgs, "--recipient", tc.wantStdio) {
				t.Errorf("stdio args = %q, want --recipient %s", disp.gotArgs, tc.wantStdio)
			}
			if len(seen) != 1 {
				t.Fatalf("remote requests = %+v", seen)
			}
			q, _ := url.ParseQuery(seen[0].Query)
			if q.Get("recipient") != tc.wantRemote || q.Has("recipient") != (tc.wantRemote != "") {
				t.Errorf("remote query = %v, want recipient %q", q, tc.wantRemote)
			}
		})
	}
}

func TestTASK4489_AgendaRecipientRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		in   map[string]any
		want string
	}{
		"non-string":     {map[string]any{"from": "2026-11-09T00:00:00Z", "to": "2026-11-16T00:00:00Z", "recipient": 5}, "recipient must be a string"},
		"stale + id":     {map[string]any{"stale": true, "recipient": "user-123"}, "--stale lists your own occurrences; drop --recipient"},
		"stale + me":     {map[string]any{"stale": true, "recipient": "me"}, "--stale lists your own occurrences; drop --recipient"},
		"stale + nonstr": {map[string]any{"stale": true, "recipient": false}, "--stale lists your own occurrences; drop --recipient"},
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
	// A blank or null recipient with stale is not given: stripped, not refused.
	for _, blank := range []any{nil, "", "  "} {
		in := map[string]any{"stale": true, "recipient": blank}
		sres, disp := stdioCall(t, "agenda", copyInput(in))
		rres, seen := remoteCall(t, "agenda", withWorkspace(in))
		if sres.IsError || rres.IsError || contains(disp.gotArgs, "--recipient") || len(seen) != 1 {
			t.Fatalf("stale with blank recipient %q: stdio=%s %q remote=%s", blank, textOf(sres), disp.gotArgs, textOf(rres))
		}
		if q, _ := url.ParseQuery(seen[0].Query); q.Has("recipient") {
			t.Fatalf("stale remote query = %v", q)
		}
	}
}

// The route mapper itself: recipient is a query parameter, all is dropped.
func TestTASK4489_AgendaRouteMapsRecipient(t *testing.T) {
	for in, want := range map[string]string{"me": "me", "user-1": "user-1", "all": ""} {
		_, path, _, err := mapAgenda(map[string]any{"workspace": "docapp", "from": "a", "to": "b", "recipient": in})
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(path)
		if q := u.Query(); q.Get("recipient") != want || q.Has("recipient") != (want != "") {
			t.Fatalf("recipient %q: path %s", in, path)
		}
	}
}
