package main

import (
	"bytes"
	"encoding/json"
	"errors"
	pad "github.com/PerpetualSoftware/pad"
	"github.com/PerpetualSoftware/pad/internal/cli"
	"net/http"
	"os"
	"strings"
	"testing"
)

// BUG-3999: any failed write that sent an op_key names it on stderr, so a
// retry can reuse it. A success prints nothing.
func TestBUG3999_FailedWritesPrintTheirKey(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, status int) (error, string)
	}{
		{"patch", func(t *testing.T, st int) (error, string) {
			bodies := patchServer(t, st, st)
			err := runPatch(t, "--old", "a", "--new", "b", "--op-key", "kk")
			_ = bodies
			return err, "kk"
		}},
		{"create", func(t *testing.T, st int) (error, string) {
			createServer(t, true, st, st)
			return runCreate(t, "--op-key", "kk"), "kk"
		}},
		{"comment", func(t *testing.T, st int) (error, string) {
			commentServer(t, true, st, st)
			return runComment(t, "--op-key", "kk"), "kk"
		}},
		{"decide", func(t *testing.T, st int) (error, string) {
			appendServer(t, true, st, st)
			return runDecide(t, "--op-key", "kk"), "kk"
		}},
		{"push", func(t *testing.T, st int) (error, string) {
			setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(st)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "x", "message": "m"}})
			}))
			cmd := pushCmd()
			cmd.SetArgs([]string{"TASK-5", "-m", "hi", "--op-key", "kk"})
			return cmd.Execute(), "kk"
		}},
		{"note", func(t *testing.T, st int) (error, string) {
			appendServer(t, true, st, st)
			return runNote(t, "--op-key", "kk"), "kk"
		}},
	}
	for _, tc := range cases {
		for _, st := range []int{http.StatusServiceUnavailable, http.StatusConflict} {
			t.Run(tc.name+"/"+http.StatusText(st), func(t *testing.T) {
				err, key := tc.run(t, st)
				if err == nil {
					t.Fatal("the write must fail")
				}
				var buf bytes.Buffer
				writeRootStructuredError(&buf, err)
				want := "--op-key " + key
				if !strings.Contains(buf.String(), "idempotency key was "+key) || !strings.Contains(buf.String(), want) {
					t.Fatalf("stderr must name the key; got %q", buf.String())
				}
			})
		}
	}
}

func TestBUG3999_SuccessAndKeylessPrintNothing(t *testing.T) {
	patchServer(t)
	if err := runPatch(t, "--old", "a", "--new", "b"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	writeRootStructuredError(&buf, nil)
	if buf.Len() != 0 {
		t.Fatalf("nil error printed %q", buf.String())
	}
}

// Both copies of the skill: the one embedded in the binary (pad agent guide,
// pad agent install) and the separately maintained one the Claude Code plugin
// ships, which is not synced from it (BUG-3999 r1 H1).
func TestBUG3999_SkillNoLongerPromisesAutomaticRetrySafety(t *testing.T) {
	plugin, err := os.ReadFile("../../plugin/skills/pad/SKILL.md")
	if err != nil {
		t.Fatalf("read the plugin skill: %v", err)
	}
	for _, f := range []struct{ name, body string }{
		{"skills/pad/SKILL.md (embedded)", string(pad.PadSkill)},
		{"plugin/skills/pad/SKILL.md", string(plugin)},
	} {
		if strings.Contains(f.body, "never applies it twice") {
			t.Fatalf("%s still claims a re-run never applies a patch twice", f.name)
		}
		for _, want := range []string{"choose the key before the first attempt", "--op-key"} {
			if !strings.Contains(f.body, want) {
				t.Fatalf("%s must say %q", f.name, want)
			}
		}
	}
}

// A failed write that sent NO op_key has no key to reuse, so it prints no
// hint (BUG-3999 r1 H2). The body is what the client marshalled; WrapOpKey is
// the transport's own decision.
func TestBUG3999_KeylessFailedWritePrintsNoHint(t *testing.T) {
	failed := errors.New("server error (503)")
	for _, body := range []string{`{"fields":"{}"}`, `{"op_key":""}`, ``} {
		err := cli.WrapOpKey([]byte(body), failed)
		var buf bytes.Buffer
		writeRootStructuredError(&buf, err)
		if strings.Contains(buf.String(), "idempotency key") || strings.Contains(buf.String(), "--op-key") {
			t.Fatalf("a keyless failed write (body %q) printed a hint: %q", body, buf.String())
		}
	}
}

func runDecide(t *testing.T, args ...string) error {
	t.Helper()
	cmd := decideCmd()
	cmd.SetArgs(append([]string{"TASK-5", "d"}, args...))
	var err error
	_ = captureStdout(t, func() { err = cmd.Execute() })
	return err
}

// BUG-3999 F1: a keyed create comes back wrapped in *OpKeyError. The plan-limit
// branch asserted *cli.APIError directly and stopped matching, which lost the
// structured marker the MCP stdio classifier reads.
func TestBUG3999_KeyedCreatePlanLimitKeepsItsStructuredMarker(t *testing.T) {
	setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			_ = json.NewEncoder(w).Encode(map[string]any{"collection_resolution": true})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "plan_limit_exceeded", "message": "item limit reached",
			"details": map[string]any{"feature": "items", "limit": 10, "current": 10, "plan": "free", "upgrade_url": "https://example.test/up"},
		}})
	}))
	formatFlag = "table"
	var err error
	stderr := captureStderr(t, func() {
		cmd := createCmd()
		cmd.SetArgs([]string{"tasks", "Keyed", "--op-key", "kk"})
		_ = captureStdout(t, func() { err = cmd.Execute() })
	})
	if err == nil {
		t.Fatal("the create must fail")
	}
	if !strings.Contains(stderr, "pad-structured-error/v1:") || !strings.Contains(stderr, "plan_limit_exceeded") {
		t.Fatalf("the plan-limit marker is missing from stderr: %q", stderr)
	}
	var buf bytes.Buffer
	writeRootStructuredError(&buf, err)
	if !strings.Contains(buf.String(), "--op-key kk") {
		t.Fatalf("the key hint is missing: %q", buf.String())
	}
}

// BUG-3999 F2: under --format json the key line goes to stderr only, so stdout
// stays empty or valid JSON.
func TestBUG3999_FormatJSONFailedWriteKeepsStdoutParseable(t *testing.T) {
	createServer(t, true, http.StatusConflict)
	var err error
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			formatFlag = "json"
			cmd := createCmd()
			cmd.SetArgs([]string{"tasks", "Keyed", "--op-key", "kk"})
			err = cmd.Execute()
		})
		if err != nil {
			writeRootStructuredError(os.Stderr, err)
		}
	})
	if err == nil {
		t.Fatal("the write must fail")
	}
	if strings.TrimSpace(stdout) != "" && !json.Valid([]byte(stdout)) {
		t.Fatalf("stdout must be empty or valid JSON: %q", stdout)
	}
	if strings.Contains(stdout, "idempotency key") {
		t.Fatalf("the key line leaked to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "--op-key kk") {
		t.Fatalf("stderr must carry the key line: %q", stderr)
	}
}
