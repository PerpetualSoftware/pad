package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestWarnStaleEditSeedGating pins the seventh door BUG-3033's sweep found, and
// the one whose consequence is not merely a confusing read.
//
// `pad item edit` is a read-modify-write over the WHOLE body: it seeds $EDITOR
// from the row and PATCHes the entire edited text back with no concurrency
// token. When the row is behind the live document, saving replaces edits that
// exist and are durable with a version derived from a state before them. The
// warning does not prevent that — BUG-3035 owns the refuse/prompt/diff decision —
// so what this test guards is that the signal exists, fires only for the defined
// value, and says the thing that distinguishes it from an ordinary stale read.
func TestWarnStaleEditSeedGating(t *testing.T) {
	cases := []struct {
		name string
		item *models.Item
		want bool
	}{
		{"nil item", nil, false},
		{"a current row — the common case", &models.Item{Content: "body"}, false},
		{"some future value", &models.Item{ContentState: "some_future_value"}, false},
		{"the value the server sends", &models.Item{
			ContentState: models.ContentOutcomeAppliedPendingFlush,
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStderr(t, func() { warnStaleEditSeed(tc.item) })
			if got := out != ""; got != tc.want {
				t.Fatalf("warned=%v want=%v (stderr: %q)", got, tc.want, out)
			}
			if !tc.want {
				return
			}
			// The consequence an EDITOR needs is that their save destroys
			// something, not that their text is old. A line that only said
			// "previous content" would read as a display quirk.
			lower := strings.ToLower(out)
			if !strings.Contains(lower, "saving") || !strings.Contains(lower, "replac") {
				t.Errorf("the warning does not say a save will replace the unseen edits: %q", out)
			}
		})
	}
}

// TestEditWarnsBeforeLaunchingTheEditor is the BINDING half of the test above
// (CONVE-19), and it guards the one claim the gating table cannot: the warning
// is printed BEFORE $EDITOR opens.
//
// Ordering is the whole value here. Printed afterwards the line would be read,
// at best, beside a "Updated TASK-5" confirmation — after the user has already
// edited a stale body and saved it over edits they could not see. So the
// assertion is on ORDER within one stream, not on presence: the fake editor
// writes a sentinel to its own stderr, which OpenInEditor wires to os.Stderr
// (read at call time, so captureStderr's swap catches both), and the warning
// must appear before it.
func TestEditWarnsBeforeLaunchingTheEditor(t *testing.T) {
	const sentinel = "EDITOR-RAN-HERE"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws/items/TASK-5", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "i1", "ref": "TASK-5", "title": "T", "slug": "t", "content": "new",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "i1", "ref": "TASK-5", "title": "T", "slug": "t",
			"content":       "the previous content",
			"content_state": models.ContentOutcomeAppliedPendingFlush,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	setTempHomeMain(t)
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_testtoken")
	// An "editor" that announces itself on stderr and edits the file, so the
	// command really does reach the write path rather than bailing on "No
	// changes."
	//
	// A script FILE rather than an inline `sh -c ...`: OpenInEditor splits the
	// EDITOR value on strings.Fields, which is whitespace-naive and does not
	// honour quoting, so an inline command is torn into words and the editor
	// exits 2. (That splitting is deliberate — it is what makes `code --wait`
	// work — and this test is not the place to change it.)
	editorPath := filepath.Join(t.TempDir(), "fake-editor")
	script := "#!/bin/sh\necho " + sentinel + " >&2\necho edited >> \"$1\"\n"
	if err := os.WriteFile(editorPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake editor: %v", err)
	}
	t.Setenv("EDITOR", editorPath)

	origWS, origFormat := workspaceFlag, formatFlag
	t.Cleanup(func() { workspaceFlag, formatFlag = origWS, origFormat })
	workspaceFlag, formatFlag = "ws", ""

	cmd := editCmd()
	cmd.SetArgs([]string{"TASK-5"})
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() {
			if err := cmd.Execute(); err != nil {
				t.Fatalf("item edit: %v", err)
			}
		})
	})

	editorAt := strings.Index(stderr, sentinel)
	if editorAt < 0 {
		t.Fatalf("the fake editor never ran, so this test measured nothing:\n%s", stderr)
	}
	warnAt := strings.Index(stderr, "behind its live collaborative")
	if warnAt < 0 {
		t.Fatalf("no stale-seed warning was printed:\n%s", stderr)
	}
	if warnAt > editorAt {
		t.Errorf("the warning was printed AFTER the editor opened, where it cannot change anything:\n%s", stderr)
	}
}
