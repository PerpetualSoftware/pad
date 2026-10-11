package main

import (
	"net/http"
	"strings"
	"testing"
)

// PLAN-3555 phase 1 at the CLI: --replace reaches the server, is refused
// before any request when it contradicts a token or has no body, and an
// unguarded body write prints the warning while a guarded or deliberate one
// does not.
func TestPLAN3555_ItemUpdateReplace(t *testing.T) {
	run := func(args ...string) (stderr string, err error, reqs *[]recordedRequest) {
		t.Helper()
		reqs = recordingServer(t)
		cmd := updateCmd()
		cmd.SetArgs(append([]string{"TASK-5"}, args...))
		stderr = captureStderr(t, func() {
			_ = captureStdout(t, func() { err = cmd.Execute() })
		})
		return stderr, err, reqs
	}

	t.Run("unguarded body write warns", func(t *testing.T) {
		stderr, err, reqs := run("--content", "x")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stderr, "unguarded content write") {
			t.Fatalf("no warning on stderr: %q", stderr)
		}
		if p := writesOf(*reqs, http.MethodPatch); len(p) != 1 || p[0].body["replace"] != nil {
			t.Fatalf("an unguarded write must not send replace: %+v", *reqs)
		}
	})
	t.Run("--replace is sent and silences the warning", func(t *testing.T) {
		stderr, err, reqs := run("--content", "x", "--replace")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stderr, "unguarded content write") {
			t.Fatalf("a deliberate replace was warned about: %q", stderr)
		}
		if p := writesOf(*reqs, http.MethodPatch); len(p) != 1 || p[0].body["replace"] != true {
			t.Fatalf("--replace did not reach the PATCH: %+v", *reqs)
		}
	})
	t.Run("--expected-seq silences the warning", func(t *testing.T) {
		stderr, err, _ := run("--content", "x", "--expected-seq", "3")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stderr, "unguarded content write") {
			t.Fatalf("a guarded write was warned about: %q", stderr)
		}
	})
	t.Run("a field-only write does not warn", func(t *testing.T) {
		stderr, err, _ := run("--status", "done")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stderr, "unguarded content write") {
			t.Fatalf("a field-only write was warned about: %q", stderr)
		}
	})
	for name, args := range map[string][]string{
		"with --expected-seq":        {"--content", "x", "--replace", "--expected-seq", "3"},
		"with --expected-updated-at": {"--content", "x", "--replace", "--expected-updated-at", "2026-10-11T00:00:00Z"},
		"without a body":             {"--status", "done", "--replace"},
	} {
		t.Run("refused "+name, func(t *testing.T) {
			_, err, reqs := run(args...)
			if err == nil || !strings.Contains(err.Error(), "--replace") {
				t.Fatalf("want a --replace refusal, got %v", err)
			}
			if len(*reqs) != 0 {
				t.Fatalf("a refused flag combination reached the server: %d request(s)", len(*reqs))
			}
		})
	}
}
