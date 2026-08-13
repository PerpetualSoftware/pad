package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// BUG-2542. X-Pad-Agent is the only signal the server has for attributing a
// write to an agent, and it used to be populated from .pad.toml's agent_name
// and nothing else — so an agent session in a workspace that had not opted in
// was recorded as the human whose credentials it used.
//
// Each case pins one rung of the precedence AND the negative that makes it
// meaningful: a plain human shell with no markers must still resolve to "",
// because a resolver that returned a name unconditionally would satisfy every
// positive case here while attributing Dave's own commands to an agent.
func TestResolveAgentName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		padToml string
		env     map[string]string
		want    string
	}{
		{
			name: "plain human shell resolves to nothing",
			want: "",
		},
		{
			name: "detected runtime is used when nothing more explicit exists",
			env:  map[string]string{"CLAUDECODE": "1"},
			want: "claude-code",
		},
		{
			name: "PAD_AGENT overrides a detected runtime",
			env:  map[string]string{"CLAUDECODE": "1", "PAD_AGENT": "some-other-harness"},
			want: "some-other-harness",
		},
		{
			name:    "pad.toml wins over both",
			padToml: "workspace = \"w\"\nagent_name = \"wren\"\n",
			env:     map[string]string{"CLAUDECODE": "1", "PAD_AGENT": "some-other-harness"},
			want:    "wren",
		},
		{
			name: "an empty marker is not a marker",
			env:  map[string]string{"CLAUDECODE": "", "PAD_AGENT": ""},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Run from a scratch cwd so a .pad.toml higher up the real tree
			// (this repo has one) cannot leak into the no-toml cases.
			dir := t.TempDir()
			if tc.padToml != "" {
				if err := os.WriteFile(filepath.Join(dir, ".pad.toml"), []byte(tc.padToml), 0o600); err != nil {
					t.Fatalf("write .pad.toml: %v", err)
				}
			}
			chdir(t, dir)

			// Clear every variable the resolver consults, then set this case's.
			for _, k := range []string{"PAD_AGENT", "CLAUDECODE"} {
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			if got := ResolveAgentName(); got != tc.want {
				t.Errorf("ResolveAgentName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// The resolver is only half the path: its value has to reach the wire as
// X-Pad-Agent, which is the header the server keys attribution on. The
// server-side tests inject that header directly, so without this the
// resolver could be correct and the client still send nothing (which is
// exactly the pre-BUG-2542 state).
func TestClientSendsResolvedAgentHeader(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "agent session sends the header", env: map[string]string{"CLAUDECODE": "1"}, want: "claude-code"},
		{name: "human shell sends none", env: nil, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chdir(t, t.TempDir())
			for _, k := range []string{"PAD_AGENT", "CLAUDECODE"} {
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			var got string
			var seen bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, seen = r.Header.Get("X-Pad-Agent"), true
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"ok"}`))
			}))
			defer srv.Close()

			if err := NewClientFromURL(srv.URL).Health(); err != nil {
				t.Fatalf("health: %v", err)
			}
			if !seen {
				t.Fatal("server never saw the request")
			}
			if got != tc.want {
				t.Errorf("X-Pad-Agent = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPushItemSendsResolvedAgentHeader (IDEA-2544 Phase 1, dispatcher
// review) pins that PushItem inherits X-Pad-Agent the same as every
// other mutating client method, rather than assuming: PushItem doesn't
// build its own request, it goes through c.post -> c.newRequest like
// CreateWatch and everything else, so the BUG-2542 fix upgraded it for
// free — but "the code path implies it" and "verified against a live
// request" are different claims, and only the second belongs in a
// report. Also confirms the server side of the loop: handlePushToItem's
// actor/actorName come from actorFromRequest/actorNameFromRequest,
// which read this exact header.
func TestPushItemSendsResolvedAgentHeader(t *testing.T) {
	chdir(t, t.TempDir())
	for _, k := range []string{"PAD_AGENT", "CLAUDECODE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("CLAUDECODE", "1")

	var got string
	var seen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, seen = r.Header.Get("X-Pad-Agent"), true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ref":"TASK-1","pushed":true}`))
	}))
	defer srv.Close()

	if err := NewClientFromURL(srv.URL).PushItem("demo", "TASK-1", "triage this"); err != nil {
		t.Fatalf("PushItem: %v", err)
	}
	if !seen {
		t.Fatal("server never saw the request")
	}
	if got != "claude-code" {
		t.Errorf("X-Pad-Agent = %q, want %q", got, "claude-code")
	}
}

// ActorKind backs the one place the client must self-describe: structured
// entries inside an item's fields JSON, which the server never parses. An
// earlier revision of the BUG-2542 fix removed the CLI's hardcoded "user"
// there on the theory that the server would stamp it — it does not, and the
// entries would have been written authorless.
func TestActorKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "agent session", env: map[string]string{"CLAUDECODE": "1"}, want: "agent"},
		{name: "explicit PAD_AGENT", env: map[string]string{"PAD_AGENT": "some-harness"}, want: "agent"},
		{name: "human shell", env: nil, want: "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chdir(t, t.TempDir())
			for _, k := range []string{"PAD_AGENT", "CLAUDECODE"} {
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := ActorKind(); got != tc.want {
				t.Errorf("ActorKind() = %q, want %q", got, tc.want)
			}
		})
	}
}
