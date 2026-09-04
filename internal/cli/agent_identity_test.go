package cli

import (
	"encoding/json"
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

	if _, err := NewClientFromURL(srv.URL).PushItem("demo", "TASK-1", "triage this"); err != nil {
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

// TestRegisteredAgentWinsForThisSession — BUG-2882. Two seats booted under one
// name; one re-registered under the right one with --agent; every write it
// made afterwards still carried the wrong name, because the registry row and
// $PAD_AGENT were two self-declarations and nothing reconciled them. The row
// is now the first thing the resolver reads, so `--agent` means what its help
// text says. Three properties, each its own case so a regression names
// itself: the row wins over the environment AND over .pad.toml; an anonymous
// row does not blank an environment name; a row for a different session that
// reused this pid is ignored.
//
// MUTANT: removing the registry step from ResolveAgentName fails the first
// two; dropping the proc-start comparison fails the last.
func TestRegisteredAgentWinsForThisSession(t *testing.T) {
	sessionsDir := registryEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".pad.toml"), []byte("workspace = \"w\"\nagent_name = \"toml-name\"\n"), 0o600); err != nil {
		t.Fatalf("write .pad.toml: %v", err)
	}
	chdir(t, dir)
	t.Setenv("PAD_AGENT", "wren")
	// This process is its own session owner (no PAD_SESSION_PID / CLAUDE_PID).

	if got := ResolveAgentName(); got != "toml-name" {
		t.Fatalf("before any registration, ResolveAgentName() = %q, want the .pad.toml name", got)
	}

	if _, err := RegisterSession(dir, "rook"); err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	if got := ResolveAgentName(); got != "rook" {
		t.Errorf("after registering as rook, ResolveAgentName() = %q, want %q (the row must win over .pad.toml and $PAD_AGENT)", got, "rook")
	}

	// Re-registering with the default keeps the name: the default IS the
	// resolver, which now reads the row.
	if _, err := RegisterSession(dir, ResolveAgentName()); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if got := ResolveAgentName(); got != "rook" {
		t.Errorf("after a default re-register, ResolveAgentName() = %q, want %q", got, "rook")
	}

	// An anonymous row is a statement about the row, not about the writes.
	if _, err := RegisterSession(dir, ""); err != nil {
		t.Fatalf("anonymous register: %v", err)
	}
	if got := ResolveAgentName(); got != "toml-name" {
		t.Errorf("after an anonymous registration, ResolveAgentName() = %q, want the .pad.toml name back", got)
	}

	// A record for THIS pid but a different process start is another
	// session that once had the pid; it must not name us.
	path := filepath.Join(sessionsDir, itoa(os.Getpid())+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var reg SessionRegistration
	if err := json.Unmarshal(data, &reg); err != nil {
		t.Fatalf("unmarshal record: %v", err)
	}
	if reg.ProcStart == "" {
		t.Skip("platform records no process-start token; the pid-reuse guard has nothing to compare")
	}
	reg.Agent = "ghost"
	reg.ProcStart = reg.ProcStart + "-not-us"
	out, _ := json.Marshal(reg)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("rewrite record: %v", err)
	}
	if got := ResolveAgentName(); got != "toml-name" {
		t.Errorf("a record from a different session with our pid named us: ResolveAgentName() = %q", got)
	}
}
