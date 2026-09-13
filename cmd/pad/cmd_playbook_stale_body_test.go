package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3033, the CLI half — and the half the server-side fix is useless without.
//
// An agent does not read the wire; it reads what the CLI prints. `pad playbook
// show` and `pad playbook run` each decode a NARROW anonymous struct of their
// own and print the body, so a marker the server sets and these commands drop
// is a fix its intended reader never sees. That is the same gap this bug
// describes for the MCP item resource.
//
// Driven through the cobra commands rather than by calling warnPlaybookBodyStale
// directly (CONVE-19): a direct-call test vouches for the helper while leaving
// the decode struct — which is where the defect actually lives — free to omit
// the key. The helper's own gating is covered by the table at the bottom.
//
// Both directions on both commands, ABSENCE FIRST: a command that warned on
// every body would satisfy every presence leg and be worthless, since an agent
// would learn to ignore it.
func TestPlaybookCommandsWarnOnAStaleBody(t *testing.T) {
	serve := func(t *testing.T, contentState string) {
		t.Helper()
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v1/workspaces/ws/playbooks/ship", func(w http.ResponseWriter, r *http.Request) {
			body := map[string]any{
				"ref": "PLAYB-1", "title": "Ship", "slug": "ship",
				"content": "Step 1: do the thing", "fields": `{"status":"active"}`,
				"status": "active",
			}
			if contentState != "" {
				body["content_state"] = contentState
			}
			_ = json.NewEncoder(w).Encode(body)
		})
		mux.HandleFunc("/api/v1/workspaces/ws/playbooks/ship/run", func(w http.ResponseWriter, r *http.Request) {
			body := map[string]any{
				"ref": "PLAYB-1", "title": "Ship", "slug": "ship", "status": "active",
				"body": "Step 1: do the thing", "arguments": []any{}, "bound_args": map[string]any{},
			}
			if contentState != "" {
				body["content_state"] = contentState
			}
			_ = json.NewEncoder(w).Encode(body)
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		setTempHomeMain(t)
		t.Setenv("PAD_URL", srv.URL)
		t.Setenv("PAD_TOKEN", "pad_testtoken")
	}

	// The commands read package-level flag vars; restore them so test order
	// cannot leak a format through to an unrelated test.
	origWS, origFormat := workspaceFlag, formatFlag
	t.Cleanup(func() { workspaceFlag, formatFlag = origWS, origFormat })
	workspaceFlag, formatFlag = "ws", "markdown"

	type door struct {
		name string
		run  func(t *testing.T) (stdout, stderr string)
	}
	doors := []door{
		{"playbook show", func(t *testing.T) (string, string) {
			t.Helper()
			cmd := playbookShowCmd()
			cmd.SetArgs([]string{"ship"})
			var out string
			err := captureStderr(t, func() {
				out = captureStdout(t, func() {
					if e := cmd.Execute(); e != nil {
						t.Fatalf("playbook show: %v", e)
					}
				})
			})
			return out, err
		}},
		{"playbook run", func(t *testing.T) (string, string) {
			t.Helper()
			cmd := playbookRunCmd()
			cmd.SetArgs([]string{"ship"})
			var out string
			err := captureStderr(t, func() {
				out = captureStdout(t, func() {
					if e := cmd.Execute(); e != nil {
						t.Fatalf("playbook run: %v", e)
					}
				})
			})
			return out, err
		}},
	}

	for _, d := range doors {
		t.Run(d.name, func(t *testing.T) {
			// ABSENCE FIRST.
			serve(t, "")
			stdout, stderr := d.run(t)
			if stdout == "" {
				t.Fatal("the command printed no body; nothing below would measure anything")
			}
			if strings.Contains(strings.ToLower(stderr), "behind its live collaborative") {
				t.Fatalf("a current body warned:\nstderr: %s", stderr)
			}

			serve(t, models.ContentOutcomeAppliedPendingFlush)
			stdout, stderr = d.run(t)
			if !strings.Contains(strings.ToLower(stderr), "behind its live collaborative") {
				t.Errorf("a stale body printed no warning:\nstderr: %q", stderr)
			}
			// STDOUT must stay clean: `--format json` is piped into scripts and
			// the markdown form exists to be redirected into a file.
			if strings.Contains(strings.ToLower(stdout), "behind its live collaborative") {
				t.Errorf("the warning landed on STDOUT, corrupting a redirected body:\n%s", stdout)
			}
			// The body is still printed. The marker qualifies it; a command that
			// withheld the body would pass a warning-only assertion.
			if !strings.Contains(stdout, "Step 1: do the thing") {
				t.Errorf("the body is missing from a marked response:\n%s", stdout)
			}
		})
	}
}

// TestWarnPlaybookBodyStaleGating pins the helper's own contract: it fires for
// the one value the vocabulary defines and is silent for everything else.
//
// The value cannot drift between the server that writes it and this renderer —
// both read models.ContentOutcomeAppliedPendingFlush — so what this guards is
// the gate, not the spelling. The "some future value" row is the load-bearing
// one: content_state shares its vocabulary with BUG-2995's content_outcome
// deliberately, so that vocabulary is expected to grow, and a helper that fired
// on any non-empty string would turn a value it has never heard of into this
// specific claim about a live editor.
func TestWarnPlaybookBodyStaleGating(t *testing.T) {
	cases := []struct {
		name  string
		state string
		want  bool
	}{
		{"empty — the common case", "", false},
		{"some future value", "some_future_value", false},
		{"the write-side sibling's other value", "content_not_applied", false},
		{"the value the server sends", models.ContentOutcomeAppliedPendingFlush, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStderr(t, func() { warnPlaybookBodyStale(tc.state) })
			if got := out != ""; got != tc.want {
				t.Fatalf("warned=%v want=%v (stderr: %q)", got, tc.want, out)
			}
			if !tc.want {
				return
			}
			// The consequence a reader of a PLAYBOOK needs is that the steps may
			// be superseded, not merely that the text may be old. That is the one
			// thing distinguishing this wording from cmd_item.go's sibling, and
			// it is the reason a second helper exists at all.
			if !strings.Contains(out, "superseded") {
				t.Errorf("the warning does not say the steps may be superseded: %q", out)
			}
		})
	}
}
