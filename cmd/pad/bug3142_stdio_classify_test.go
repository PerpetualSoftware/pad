package main

import (
	"context"
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
