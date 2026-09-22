package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

// BUG-3142, the classifier half. cobra and pflag refuse a bad argv before the
// command runs, and none of those refusals matched a stderr pattern, so the
// stdio transport reported every one as a retryable server_error. Each message
// here is PRODUCED by the real library call rather than typed, so a wording
// change in a dependency bump fails this test instead of an agent's retry
// loop. The population is the parse-time family enumerated from cobra v1.10.2
// (args.go, command.go) and pflag v1.0.10 (errors.go) on BUG-3142's trail.
func TestCobraUsageRefusalsClassifyAsValidationFailed(t *testing.T) {
	newCmd := func() *cobra.Command {
		c := &cobra.Command{Use: "search <query>", RunE: func(*cobra.Command, []string) error { return nil }}
		c.Flags().String("status", "", "")
		c.Flags().Int("limit", 0, "")
		c.Flags().StringP("sort", "s", "", "")
		c.Flags().String("must", "", "")
		_ = c.MarkFlagRequired("must")
		return c
	}
	parse := func(args ...string) error { return newCmd().ParseFlags(args) }
	parent := &cobra.Command{Use: "item", Args: cobra.NoArgs}
	parent.AddCommand(newCmd())

	produced := map[string]error{
		"unknown shorthand flag":     parse("-xyz"),
		"unknown flag":               parse("--nope"),
		"flag needs an argument":     parse("--status"),
		"flag needs an argument (-)": parse("-s"),
		"invalid argument (flag)":    parse("--limit", "abc"),
		"requires at least":          cobra.MinimumNArgs(1)(newCmd(), nil),
		"accepts at most":            cobra.MaximumNArgs(1)(newCmd(), []string{"a", "b"}),
		"accepts exactly":            cobra.ExactArgs(1)(newCmd(), []string{"a", "b"}),
		"accepts between":            cobra.RangeArgs(1, 2)(newCmd(), []string{"a", "b", "c"}),
		"unknown command":            cobra.NoArgs(parent, []string{"serach"}),
		"required flag not set": func() error {
			c := newCmd()
			_ = c.ParseFlags(nil)
			return c.ValidateRequiredFlags()
		}(),
	}

	for name, err := range produced {
		t.Run(name, func(t *testing.T) {
			if err == nil {
				t.Fatalf("PRECONDITION: the library call produced no error, so this row tests nothing")
			}
			// What the CLI prints: cobra's usage block, then main's "Error: " line.
			stderr := "Usage:\n  pad item search <query> [flags]\n\nFlags:\n  -h, --help   help for search\n\nError: " + err.Error()
			res := classifyExecError(context.Background(), []string{"item", "search"}, errors.New("exit status 1"), stderr, nil)
			env, ok := res.StructuredContent.(ErrorEnvelope)
			if !ok {
				t.Fatalf("expected ErrorEnvelope, got %T", res.StructuredContent)
			}
			if env.Error.Code != ErrValidationFailed {
				t.Fatalf("%q classified as %q, want %q — a deterministic argv refusal must not read as retryable",
					err.Error(), env.Error.Code, ErrValidationFailed)
			}
		})
	}
}

// CONTROL: the pattern is anchored, so a server message that merely contains
// one of these phrases mid-sentence, and a genuinely transient failure, are
// not pulled into validation_failed.
func TestCobraUsagePatternDoesNotCaptureServerMessages(t *testing.T) {
	for _, stderr := range []string{
		"Error: internal server error",
		"Error: the upstream accepts 3 arg(s) only when idle", // not at a line start after the prefix
		"Error: Too many requests. Please try again later.",
	} {
		res := classifyExecError(context.Background(), []string{"item", "search"}, errors.New("exit status 1"), stderr, nil)
		env, ok := res.StructuredContent.(ErrorEnvelope)
		if !ok {
			t.Fatalf("expected ErrorEnvelope, got %T", res.StructuredContent)
		}
		if env.Error.Code == ErrValidationFailed {
			t.Errorf("%q classified as validation_failed; the usage pattern is too loose", stderr)
		}
	}
}
