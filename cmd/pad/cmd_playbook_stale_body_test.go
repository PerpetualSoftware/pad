package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

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

	// Both directions at the COMMAND boundary, because the helper's own gating
	// table cannot see a wiring error: with the call site forced to warn
	// unconditionally, a stale-only test still passes and the marker becomes
	// noise on every edit. Codex round 1 P3.
	for _, tc := range []struct {
		name         string
		contentState string
		wantWarning  bool
	}{
		{"a current row — the common case", "", false},
		{"the value the server sends", models.ContentOutcomeAppliedPendingFlush, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var patchedContent *string
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/workspaces/ws/items/TASK-5", func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					var body struct {
						Content *string `json:"content"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					patchedContent = body.Content
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": "i1", "ref": "TASK-5", "title": "T", "slug": "t", "content": "new",
					})
					return
				}
				body := map[string]any{
					"id": "i1", "ref": "TASK-5", "title": "T", "slug": "t",
					"content": "the previous content",
				}
				if tc.contentState != "" {
					body["content_state"] = tc.contentState
				}
				_ = json.NewEncoder(w).Encode(body)
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			setTempHomeMain(t)
			t.Setenv("PAD_URL", srv.URL)
			t.Setenv("PAD_TOKEN", "pad_testtoken")

			// An "editor" that announces itself on stderr and edits the file, so
			// the command really reaches the write path rather than bailing on
			// "No changes."
			//
			// A script FILE rather than an inline `sh -c ...`: OpenInEditor splits
			// the EDITOR value on strings.Fields, which is whitespace-naive and
			// does not honour quoting, so an inline command is torn into words and
			// the editor exits 2. (That splitting is deliberate — it is what makes
			// `code --wait` work — and this test is not the place to change it.)
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

			// PREMISES, asserted before any conclusion is drawn from them. An
			// absence assertion is vacuous if the command never got this far: the
			// editor must have run, and the edited body must have been sent.
			editorAt := strings.Index(stderr, sentinel)
			if editorAt < 0 {
				t.Fatalf("the fake editor never ran, so this case measured nothing:\n%s", stderr)
			}
			// The exact CONTENT, not merely that a PATCH occurred: an update
			// payload stripped of its content still produces a PATCH, and this
			// case claims to exercise saving the edited body (codex round 2 P3).
			wantContent := "the previous content" + "edited\n"
			if patchedContent == nil {
				t.Fatalf("the PATCH carried no content, so this case did not exercise saving:\n%s", stderr)
			}
			if *patchedContent != wantContent {
				t.Fatalf("the PATCH sent %q, want %q — the edited body is not what reached the server",
					*patchedContent, wantContent)
			}

			warnAt := strings.Index(stderr, "behind its live collaborative")
			if got := warnAt >= 0; got != tc.wantWarning {
				t.Fatalf("warned=%v want=%v (stderr: %q)", got, tc.wantWarning, stderr)
			}
			if !tc.wantWarning {
				return
			}
			// Ordering is the whole value: printed after the editor opened, the
			// line would be read beside an "Updated TASK-5" confirmation — after
			// the user has already edited a stale body and saved it over edits
			// they could not see. The fake editor's stderr is wired to os.Stderr
			// by OpenInEditor (read at call time, so captureStderr's swap catches
			// both), which makes this an assertion about ORDER in one stream.
			if warnAt > editorAt {
				t.Errorf("the warning was printed AFTER the editor opened, where it cannot change anything:\n%s", stderr)
			}
		})
	}
}

// TestPlaybookListWarnsOnlyAboutStaleSummaries covers the CLI half of the
// summary door (BUG-3033, codex round 1 P2) through BOTH renderers.
//
// A `summary` is the body's first paragraph, truncated, so a stale body makes a
// stale summary — and an agent routes on that description. Each renderer decodes
// its own narrow struct, so neither inherits anything.
//
// Three stale entries INTERLEAVED with current ones, because a single stale row
// cannot distinguish three different renderers (codex round 2 P3): one that
// names every ref, one that names only the first, and the correct one all agree
// when exactly one row is stale. The assertions are therefore the COMPLETE ref
// set, the absence of the current ones, and EXACTLY ONE warning line.
func TestPlaybookListWarnsOnlyAboutStaleSummaries(t *testing.T) {
	// PLAYB-2 and PLAYB-4 stale, PLAYB-1/3/5 current — stale entries neither
	// first nor last nor contiguous, so an off-by-one or a first-only renderer
	// cannot pass by accident.
	all := []string{"PLAYB-1", "PLAYB-2", "PLAYB-3", "PLAYB-4", "PLAYB-5"}
	stale := map[string]bool{"PLAYB-2": true, "PLAYB-4": true}

	serve := func(t *testing.T, markStale bool) {
		t.Helper()
		payload := func() []map[string]any {
			out := make([]map[string]any, 0, len(all))
			for i, ref := range all {
				e := map[string]any{
					"ref": ref, "title": fmt.Sprintf("P%d", i+1), "slug": fmt.Sprintf("p%d", i+1),
					"invocation_slug": fmt.Sprintf("p%d", i+1), "status": "active",
					"summary": fmt.Sprintf("Does the P%d thing.", i+1),
				}
				if markStale && stale[ref] {
					e["content_state"] = models.ContentOutcomeAppliedPendingFlush
				}
				out = append(out, e)
			}
			return out
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v1/workspaces/ws/playbooks", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(payload())
		})
		mux.HandleFunc("/api/v1/workspaces/ws/agent/bootstrap", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workspace": map[string]any{"slug": "ws", "name": "WS"},
				"user":      map[string]any{"name": "T", "email": "t@t"},
				"playbooks": payload(),
			})
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		setTempHomeMain(t)
		t.Setenv("PAD_URL", srv.URL)
		t.Setenv("PAD_TOKEN", "pad_testtoken")
	}

	origWS, origFormat := workspaceFlag, formatFlag
	t.Cleanup(func() { workspaceFlag, formatFlag = origWS, origFormat })
	workspaceFlag, formatFlag = "ws", "markdown"

	// Both renderers, because the bootstrap one had no coverage at all and the
	// two are separate decode structs in separate files.
	renderers := map[string]func() *cobra.Command{
		"playbook list": playbookListCmd,
		"bootstrap":     bootstrapCmd,
	}

	run := func(t *testing.T, mk func() *cobra.Command) (stdout, stderr string) {
		t.Helper()
		cmd := mk()
		cmd.SetArgs(nil)
		var out string
		errOut := captureStderr(t, func() {
			out = captureStdout(t, func() {
				if e := cmd.Execute(); e != nil {
					t.Fatalf("execute: %v", e)
				}
			})
		})
		return out, errOut
	}

	for name, mk := range renderers {
		t.Run(name, func(t *testing.T) {
			// ABSENCE FIRST, with the premise asserted so the silence below is
			// not the silence of a command that rendered nothing.
			serve(t, false)
			stdout, stderr := run(t, mk)
			for _, ref := range all {
				if !strings.Contains(stdout, ref) {
					t.Fatalf("the listing did not render %s, so the absence assertion is vacuous:\n%s", ref, stdout)
				}
			}
			if strings.Contains(stdout, "Does the P2 thing.") == false {
				t.Fatalf("the listing rendered no summaries, so this measures nothing:\n%s", stdout)
			}
			if strings.Contains(stderr, "behind its live collaborative") {
				t.Fatalf("a listing with no stale summaries warned:\n%s", stderr)
			}

			serve(t, true)
			stdout, stderr = run(t, mk)

			// EXACTLY ONE warning line. A renderer emitting one per stale row
			// satisfies every "contains" assertion and is the thing that makes a
			// long listing unreadable.
			lines := 0
			for _, ln := range strings.Split(stderr, "\n") {
				if strings.Contains(ln, "behind its live collaborative") {
					lines++
				}
			}
			if lines != 1 {
				t.Errorf("got %d warning lines, want exactly 1:\n%s", lines, stderr)
			}
			// The COMPLETE stale set — a renderer naming only the first would
			// pass a single-ref assertion.
			for ref := range stale {
				if !strings.Contains(stderr, ref) {
					t.Errorf("the warning omits stale %s:\n%s", ref, stderr)
				}
			}
			// And only those.
			for _, ref := range all {
				if stale[ref] {
					continue
				}
				if strings.Contains(stderr, ref) {
					t.Errorf("the warning names %s, whose summary is current:\n%s", ref, stderr)
				}
			}
			// STDOUT stays clean: the listing is piped.
			if strings.Contains(stdout, "behind its live collaborative") {
				t.Errorf("the warning landed on STDOUT, corrupting the listing:\n%s", stdout)
			}
		})
	}
}

// TestSnippetRenderersWarnAboutStaleSources covers the two CLI renderers that
// print body-derived SNIPPETS (BUG-3033, codex round 2 P2).
//
// `pad item search` decodes the embedded models.Item and prints only the
// snippet; `pad item backlinks` decodes models.Backlink and prints only the
// snippet. Both carry the marker on the wire and both dropped it here, so the
// signal stopped at the renderer — the same shape as the playbook commands.
//
// Interleaved stale and current rows for the reason the summary test uses them:
// a single stale row cannot tell a correct renderer from one that names every
// ref or only the first.
func TestSnippetRenderersWarnAboutStaleSources(t *testing.T) {
	// THREE stale rows separated by current ones. An earlier draft had one
	// stale row while its comment claimed interleaving — the same gap this unit
	// had already fixed for summaries, reintroduced one test later, and a
	// renderer reporting only the first stale source passed it (codex round 3).
	staleRefs := []string{"TASK-2", "TASK-4"}
	currentRefs := []string{"TASK-1", "TASK-3", "TASK-5"}
	isStale := map[string]bool{}
	for _, r := range staleRefs {
		isStale[r] = true
	}
	allRefs := []string{"TASK-1", "TASK-2", "TASK-3", "TASK-4", "TASK-5"}

	serve := func(t *testing.T, markStale bool) {
		t.Helper()
		item := func(ref, title string) map[string]any {
			m := map[string]any{
				"id": ref, "ref": ref, "title": title, "slug": strings.ToLower(ref),
				"collection_name": "Tasks", "collection_icon": "✓", "content": "body",
			}
			if markStale && isStale[ref] {
				m["content_state"] = models.ContentOutcomeAppliedPendingFlush
			}
			return m
		}
		backlink := func(ref, title string) map[string]any {
			m := map[string]any{
				"source_item_id": ref, "source_ref": ref, "source_title": title,
				"source_collection_slug": "tasks", "snippet": "…a snippet from " + ref + "…",
			}
			if markStale && isStale[ref] {
				m["content_state"] = models.ContentOutcomeAppliedPendingFlush
			}
			return m
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v1/search", func(w http.ResponseWriter, r *http.Request) {
			results := make([]map[string]any, 0, len(allRefs))
			for _, ref := range allRefs {
				results = append(results, map[string]any{
					"item":    item(ref, ref),
					"snippet": "…a snippet from " + ref + "…",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"results": results, "total": len(results), "limit": 20, "offset": 0,
			})
		})
		mux.HandleFunc("/api/v1/workspaces/ws/items/TASK-9", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(item("TASK-9", "Target"))
		})
		// The CLI resolves the ref to an item first and builds the backlinks URL
		// from its SLUG, so the fixture has to serve the slug path.
		mux.HandleFunc("/api/v1/workspaces/ws/items/task-9/backlinks", func(w http.ResponseWriter, r *http.Request) {
			bls := make([]map[string]any, 0, len(allRefs))
			for _, ref := range allRefs {
				bls = append(bls, backlink(ref, ref))
			}
			_ = json.NewEncoder(w).Encode(bls)
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		setTempHomeMain(t)
		t.Setenv("PAD_URL", srv.URL)
		t.Setenv("PAD_TOKEN", "pad_testtoken")
	}

	origWS, origFormat := workspaceFlag, formatFlag
	t.Cleanup(func() { workspaceFlag, formatFlag = origWS, origFormat })
	workspaceFlag, formatFlag = "ws", ""

	// Each door names the tokens whose presence proves the listing really
	// rendered, so an absence assertion cannot pass vacuously.
	//
	// They differ, and the reason is worth recording: `item backlinks` prints
	// its snippet through cli.Dim, and fatih/color's Printf writes to the
	// package-level color.Output — bound to the process's ORIGINAL stdout at
	// init — so swapping os.Stdout does not intercept it. The snippet text is
	// therefore not assertable here; the backlink ROWS, printed with fmt.Printf,
	// are. This changes nothing about the warning under test, which goes to
	// os.Stderr directly.
	doors := map[string]struct {
		mk      func() (*cobra.Command, []string)
		present []string
	}{
		"item search": {
			mk:      func() (*cobra.Command, []string) { return searchCmd(), []string{"snippet"} },
			present: []string{"a snippet from TASK-1", "a snippet from TASK-4"},
		},
		"item backlinks": {
			mk:      func() (*cobra.Command, []string) { return backlinksCmd(), []string{"TASK-9"} },
			present: []string{"TASK-1 TASK-1", "TASK-4 TASK-4"},
		},
	}

	for name, door := range doors {
		t.Run(name, func(t *testing.T) {
			run := func(t *testing.T) (stdout, stderr string) {
				t.Helper()
				cmd, args := door.mk()
				cmd.SetArgs(args)
				var out string
				errOut := captureStderr(t, func() {
					out = captureStdout(t, func() {
						if e := cmd.Execute(); e != nil {
							t.Fatalf("%s: %v", name, e)
						}
					})
				})
				return out, errOut
			}

			// ABSENCE FIRST, premise asserted: both snippets really rendered.
			serve(t, false)
			stdout, stderr := run(t)
			for _, token := range door.present {
				if !strings.Contains(stdout, token) {
					t.Fatalf("the listing did not render %q, so the absence assertion is vacuous:\n%s", token, stdout)
				}
			}
			if strings.Contains(stderr, "behind its live collaborative") {
				t.Fatalf("a listing with no stale sources warned:\n%s", stderr)
			}

			serve(t, true)
			stdout, stderr = run(t)
			lines := 0
			for _, ln := range strings.Split(stderr, "\n") {
				if strings.Contains(ln, "behind its live collaborative") {
					lines++
				}
			}
			if lines != 1 {
				t.Errorf("got %d warning lines, want exactly 1:\n%s", lines, stderr)
			}
			// The COMPLETE stale set — a renderer naming only the first passes
			// any single-ref assertion.
			for _, ref := range staleRefs {
				if !strings.Contains(stderr, ref) {
					t.Errorf("the warning omits stale source %s:\n%s", ref, stderr)
				}
			}
			for _, ref := range currentRefs {
				if strings.Contains(stderr, ref) {
					t.Errorf("the warning names %s, whose body is current:\n%s", ref, stderr)
				}
			}
			if strings.Contains(stdout, "behind its live collaborative") {
				t.Errorf("the warning landed on STDOUT, corrupting a piped listing:\n%s", stdout)
			}
		})
	}
}
