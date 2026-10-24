package main

// TASK-4035: `pad project catch-up` is a read-only wrapper over
// GET /workspaces/{ws}/catch-up. Mark caught up (PUT /me/seen) is web-only
// (ruling 380aa793), so every test here also pins that nothing but that one
// GET is ever sent.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

const catchUpFixture = `{"window":{"since":"2026-10-09T00:00:00Z","until":"2026-10-23T00:00:00Z","has_cursor":%s,"capped_at_ceiling":%s},` +
	`"needs_you":{"total":7,"lines":[{"ref":"TASK-9","title":"Decide the thing","collection":"tasks","what":"needs a decision"}]},` +
	`"finished":{"total":1,"lines":[{"ref":"TASK-3","title":"Shipped it","collection":"tasks","what":"finished","actor":"Bob"}]},` +
	`"new_and_reopened":{"total":0,"lines":[]},` +
	`"comments":{"total":0,"lines":[]},` +
	`"agents":{"total":2,"groups":[{"agent":"claude","changes":2,"items":1,"lines":[{"ref":"TASK-3","title":"Shipped it","collection":"tasks","what":"updated","agent":"claude"}]}]},` +
	`"future_field":{"kept":true}%s}`

func catchUpBody(hasCursor, capped bool, degraded string) string {
	b := func(v bool) string {
		if v {
			return "true"
		}
		return "false"
	}
	extra := ""
	if degraded != "" {
		extra = `,"degraded":["` + degraded + `"]`
	}
	return fmt.Sprintf(catchUpFixture, b(hasCursor), b(capped), extra)
}

type catchUpRecorder struct {
	mu   sync.Mutex
	reqs []string
	body string
	code int
}

func (r *catchUpRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.reqs = append(r.reqs, req.Method+" "+req.URL.RequestURI())
		r.mu.Unlock()
		if r.code != 0 && r.code != http.StatusOK {
			w.WriteHeader(r.code)
			_, _ = w.Write([]byte(`{"error":{"code":"x","message":"nope"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(r.body))
	})
}

func runCatchUp(t *testing.T, rec *catchUpRecorder, format string) (string, error) {
	t.Helper()
	setupFormatRoutingTest(t, rec.handler())
	formatFlag = format
	cmd := catchUpCmd()
	cmd.SetArgs(nil)
	var err error
	out := captureStdout(t, func() { err = cmd.Execute() })
	return out, err
}

func TestCatchUpCommand_JSON(t *testing.T) {
	body := catchUpBody(true, false, "")
	rec := &catchUpRecorder{body: body}
	out, err := runCatchUp(t, rec, "json")
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	_ = json.Unmarshal([]byte(body), &want)
	gj, _ := json.Marshal(got)
	wj, _ := json.Marshal(want)
	if string(gj) != string(wj) {
		t.Errorf("stdout dropped a section or field of the response (it is the server's JSON, re-indented):\n got %s\nwant %s", gj, wj)
	}
}

func TestCatchUpCommand_TextAndMarkdown(t *testing.T) {
	for _, tc := range []struct {
		name, format string
		hasCursor    bool
		degraded     string
		want         []string
		notWant      []string
	}{
		{"markdown with cursor", "markdown", true, "", []string{
			"# Catch up", "since 2026-10-09T00:00:00Z", "## Needs you (7)", "## Finished (1)",
			"## New and reopened (0)", "## Comments (0)", "## Agents (2)",
			"| Ref | Title | Collection | What | Who | At |", "| TASK-9 | Decide the thing |",
			"Bob", "claude: 2 changes on 1 items", "_Nothing._", "and 6 more", "Mark caught up is done in the web UI",
		}, []string{"no cursor", "Could not read"}},
		{"markdown no cursor degraded", "markdown", false, "needs_you", []string{
			"no cursor: last 14 days", "Could not read: needs_you",
		}, []string{"since 2026-10-09"}},
		{"table with cursor", "table", true, "", []string{
			"Catch up (since 2026-10-09T00:00:00Z", "Needs you (7)", "TASK-9  Decide the thing  [needs a decision]",
			"Finished (1)", "[finished by Bob]", "Nothing.", "and 6 more", "Mark caught up is done in the web UI",
		}, []string{"| Ref |", "# Catch up"}},
		{"table no cursor degraded", "table", false, "needs_you", []string{
			"no cursor: last 14 days", "Could not read: needs_you (shown empty)",
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &catchUpRecorder{body: catchUpBody(tc.hasCursor, !tc.hasCursor, tc.degraded)}
			out, err := runCatchUp(t, rec, tc.format)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q:\n%s", w, out)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(out, w) {
					t.Errorf("output should not contain %q:\n%s", w, out)
				}
			}
		})
	}
}

func TestCatchUpCommand_ReadNeverWrites(t *testing.T) {
	for _, format := range []string{"table", "json", "markdown"} {
		t.Run(format, func(t *testing.T) {
			rec := &catchUpRecorder{body: catchUpBody(false, true, "")}
			if _, err := runCatchUp(t, rec, format); err != nil {
				t.Fatal(err)
			}
			if len(rec.reqs) != 1 || rec.reqs[0] != "GET /api/v1/workspaces/demo/catch-up" {
				t.Errorf("requests = %v, want exactly [GET /api/v1/workspaces/demo/catch-up]", rec.reqs)
			}
			for _, r := range rec.reqs {
				if !strings.HasPrefix(r, "GET ") || strings.Contains(r, "/me/seen") {
					t.Errorf("a request that could move the cursor: %s", r)
				}
			}
		})
	}
}

func TestCatchUpCommand_ErrorSurfaces(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		rec := &catchUpRecorder{code: code}
		out, err := runCatchUp(t, rec, "table")
		if err == nil {
			t.Errorf("status %d: no error; stdout:\n%s", code, out)
		}
		if strings.Contains(out, "Catch up") {
			t.Errorf("status %d: printed a digest on failure:\n%s", code, out)
		}
		if len(rec.reqs) != 1 || !strings.HasPrefix(rec.reqs[0], "GET ") {
			t.Errorf("status %d: requests = %v", code, rec.reqs)
		}
	}
}

// A body whose field type the CLI struct does not hold: json prints the
// server's JSON anyway, table and markdown refuse (TASK-4035, rulings
// eedbf63d and cb588bde).
func TestCatchUpCommand_JSONSkipsTypedDecode(t *testing.T) {
	body := strings.Replace(catchUpBody(true, false, ""), `"comments":{"total":0,"lines":[]}`, `"comments":{"total":"seven","lines":[]}`, 1)
	if !strings.Contains(body, `"seven"`) {
		t.Fatal("fixture did not take the bad field")
	}
	out, err := runCatchUp(t, &catchUpRecorder{body: body}, "json")
	if err != nil {
		t.Fatalf("json errored on a valid body of an unknown shape: %v", err)
	}
	if !strings.Contains(out, `"seven"`) {
		t.Errorf("json output lost the retyped field:\n%s", out)
	}
	for _, format := range []string{"table", "markdown"} {
		out, err := runCatchUp(t, &catchUpRecorder{body: body}, format)
		if err == nil {
			t.Errorf("%s: no error on a body the typed decode cannot hold; stdout:\n%s", format, out)
		}
	}
}

// TASK-4051: an existing cursor older than the ceiling is labelled as capped
// in both human-readable formats. Kills: M5b.
func TestCatchUpCommand_OldCursorLabelsFourteenDayCap(t *testing.T) {
	for _, format := range []string{"table", "markdown"} {
		t.Run(format, func(t *testing.T) {
			out, err := runCatchUp(t, &catchUpRecorder{body: catchUpBody(true, true, "")}, format)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "since 2026-10-09T00:00:00Z (capped at 14 days)") {
				t.Fatalf("old cursor lacks the cap label:\n%s", out)
			}
		})
	}
}
