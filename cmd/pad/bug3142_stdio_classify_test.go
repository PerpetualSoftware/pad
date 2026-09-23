package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/mcp"
)

// padHelperEnv makes this test binary behave as the `pad` CLI, so the stdio
// transport can shell out to the REAL command tree and read its REAL stderr.
const padHelperEnv = "PAD_TEST_BINARY_AS_CLI"

func TestMain(m *testing.M) {
	if os.Getenv(padHelperEnv) == "1" {
		// Same shape as main(): the root command's own error printing and
		// exit status, with the argv the dispatcher passed.
		root := newRootCmd()
		root.SetArgs(os.Args[1:])
		if err := root.Execute(); err != nil {
			writeRootStructuredError(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// BUG-3142, the classifier half, end to end: ExecDispatcher runs the real
// command tree, which refuses the argv before any command body runs (so no
// server is contacted), and the envelope must say validation_failed — not the
// retryable server_error it said before.
//
// The flag-parse row is the one that carried a second defect: the root's flag
// error hook prints cobra's usage block BEFORE the "Error:" line, and the usage
// strip truncated at "Usage:", deleting the message. Only real stderr can show
// that ordering, which is why this runs the binary instead of a string.
func TestStdioArgvRefusalsClassifyAsValidationFailed(t *testing.T) {
	t.Setenv(padHelperEnv, "1")
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	d := &mcp.ExecDispatcher{Binary: bin}

	rows := []struct {
		name    string
		cmdPath []string
		argv    []string
	}{
		// The pre-fix vector for `item search "-ship it"`: a flag-parse refusal.
		{"flag parse", []string{"item", "search"}, []string{"-ship it", "--format", "json"}},
		// An arg-count refusal, which prints no usage block at all.
		{"arg count", []string{"item", "show"}, []string{"--format", "json", "--", "TASK-1", "TASK-2"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			res, err := d.Dispatch(context.Background(), r.cmdPath, r.argv)
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			env, ok := res.StructuredContent.(mcp.ErrorEnvelope)
			if !ok {
				t.Fatalf("PRECONDITION: the argv should have been refused; got %T", res.StructuredContent)
			}
			if env.Error.Code != mcp.ErrValidationFailed {
				t.Fatalf("code = %q (message %q, hint %q), want %q",
					env.Error.Code, env.Error.Message, env.Error.Hint, mcp.ErrValidationFailed)
			}
			if env.Error.Hint == "" {
				t.Fatalf("the envelope carries no hint, so the refusal's own text was lost")
			}
		})
	}
}

// BUG-3147, end to end: a 429 on any command reaches a stdio caller as
// rate_limited, with the server's Retry-After, instead of server_error. The
// fake server answers with the bytes internal/server's writeRateLimitResponse
// writes (middleware_ratelimit.go: the Retry-After header, then writeError's
// {"error":{"code","message"}} body), since that function is unexported.
func TestStdioRateLimitClassifiesAsRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"Too many requests. Please try again later."}}`))
	}))
	defer srv.Close()

	t.Setenv(padHelperEnv, "1")
	t.Setenv("HOME", t.TempDir()) // no real credentials or config reach the fake
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	d := &mcp.ExecDispatcher{Binary: bin}
	res, err := d.Dispatch(context.Background(), []string{"item", "show"},
		[]string{"--url", srv.URL, "--workspace", "ws", "--format", "json", "--", "TASK-1"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	env, ok := res.StructuredContent.(mcp.ErrorEnvelope)
	if !ok {
		t.Fatalf("PRECONDITION: the call should have failed; got %T", res.StructuredContent)
	}
	if env.Error.Code != mcp.ErrRateLimited {
		t.Fatalf("code = %q (message %q, hint %q), want %q",
			env.Error.Code, env.Error.Message, env.Error.Hint, mcp.ErrRateLimited)
	}
	var details struct {
		RetryAfterSeconds int `json:"retry_after_seconds"`
	}
	if err := json.Unmarshal(env.Error.Details, &details); err != nil || details.RetryAfterSeconds != 7 {
		t.Fatalf("details = %s, want retry_after_seconds 7 (err %v)", env.Error.Details, err)
	}
}
