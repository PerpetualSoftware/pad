package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

// Tests covering BUG-987's MCP-layer fixes (Bugs 11, 12; Bugs 6, 8,
// 13, 14 land in their respective package tests).

// TestStripCobraUsageBlock verifies the helper that scrubs cobra's
// auto-emitted "Usage: ..." block from CLI stderr before it reaches
// MCP error envelopes (BUG-987 bug 11). The Usage block leaks old
// CLI verb names (e.g. `pad item block`) into responses agents see
// — confusing for agents using the v0.2 catalog and fragile against
// future CLI flag changes.
func TestStripCobraUsageBlock(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strips standard cobra usage block",
			in: `Error: cannot link an item to itself
Usage:
  pad item block <source-ref> <target-ref> [flags]

Flags:
  -h, --help   help for block

Global Flags:
      --format string   output format`,
			want: "Error: cannot link an item to itself",
		},
		{
			name: "passes through when no Usage block present",
			in:   "Error: item TASK-99 not found",
			want: "Error: item TASK-99 not found",
		},
		{
			name: "Usage marker as substring of word — not stripped",
			in:   "Error: this misUsage: case is rare but possible",
			want: "Error: this misUsage: case is rare but possible",
		},
		{
			name: "trims trailing whitespace before Usage",
			in:   "Error: bad input\n\n  \nUsage:\n  pad item show <ref>",
			want: "Error: bad input",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripCobraUsageBlock(tc.in); got != tc.want {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestClassifyExecError_StripsUsageBlock confirms classifyExecError
// removes Usage text BEFORE pattern matching and BEFORE placing the
// remaining stderr into the envelope. Without this, agents see
// `pad item block ...` references in error responses despite the
// MCP catalog using `pad_item action=link link_type=blocks`.
func TestClassifyExecError_StripsUsageBlock(t *testing.T) {
	stderr := `Error: cannot link an item to itself
Usage:
  pad item block <source-ref> <target-ref> [flags]

Flags:
  -h, --help   help for block`

	res := classifyExecError(context.Background(),
		[]string{"item", "block"},
		errors.New("exit 1"),
		stderr,
		nil,
	)
	body := textOf(res)
	if strings.Contains(body, "Usage:") {
		t.Errorf("envelope leaked Usage block: %s", body)
	}
	if strings.Contains(body, "pad item block <source-ref>") {
		t.Errorf("envelope leaked old CLI verb help: %s", body)
	}
	if !strings.Contains(body, "cannot link an item to itself") {
		t.Errorf("envelope dropped the actual error message: %s", body)
	}
}

// TestValidationFailedFromBuildErr verifies that BuildCLIArgs error
// strings are wrapped as structured validation_failed envelopes
// instead of bare-text results (BUG-987 bug 12).
func TestValidationFailedFromBuildErr(t *testing.T) {
	t.Run("missing required argument extracts field name", func(t *testing.T) {
		err := errors.New(`missing required argument "title"`)
		res := validationFailedFromBuildErr("item create", err)
		if !res.IsError {
			t.Errorf("IsError = false, want true")
		}
		env := decodeEnvelope(t, res)
		if env.Error.Code != ErrValidationFailed {
			t.Errorf("Code = %q, want %q", env.Error.Code, ErrValidationFailed)
		}
		if env.Error.Field != "title" {
			t.Errorf("Field = %q, want title", env.Error.Field)
		}
		if !strings.Contains(env.Error.Message, "item create") {
			t.Errorf("Message should reference cmdPath; got %q", env.Error.Message)
		}
	})

	t.Run("flag type mismatch extracts field name", func(t *testing.T) {
		err := errors.New(`flag "limit": expected number, got string`)
		res := validationFailedFromBuildErr("item list", err)
		env := decodeEnvelope(t, res)
		if env.Error.Code != ErrValidationFailed {
			t.Errorf("Code = %q, want %q", env.Error.Code, ErrValidationFailed)
		}
		if env.Error.Field != "limit" {
			t.Errorf("Field = %q, want limit", env.Error.Field)
		}
	})

	t.Run("unrecognized error message still produces validation envelope", func(t *testing.T) {
		err := errors.New("totally novel error format")
		res := validationFailedFromBuildErr("item show", err)
		env := decodeEnvelope(t, res)
		if env.Error.Code != ErrValidationFailed {
			t.Errorf("Code = %q, want %q", env.Error.Code, ErrValidationFailed)
		}
		// Field stays empty when the regex misses; the underlying
		// message text still carries the detail.
		if env.Error.Field != "" {
			t.Errorf("Field = %q, want empty", env.Error.Field)
		}
		if !strings.Contains(env.Error.Message, "totally novel error") {
			t.Errorf("Message should preserve underlying text; got %q", env.Error.Message)
		}
	})
}

// TestEnvDispatch_ValidationErrorIsStructured drives env.Dispatch
// with a missing-arg input and confirms the resulting error result is
// a structured validation_failed envelope, not a bare-text result.
// This is the integration counterpart to TestValidationFailedFromBuildErr.
func TestEnvDispatch_ValidationErrorIsStructured(t *testing.T) {
	doc := &cmdhelp.Document{
		Binary: "pad",
		Commands: map[string]cmdhelp.Command{
			"item show": {
				Args: []cmdhelp.Arg{{Name: "ref", Required: true}},
			},
		},
	}
	env := ActionEnv{
		Doc:        doc,
		Workspace:  NewWorkspaceState(""),
		Dispatcher: &fakeDispatcher{},
	}
	// Missing required `ref` — BuildCLIArgs returns an error,
	// env.Dispatch must wrap it as validation_failed.
	res, err := env.Dispatch(context.Background(), []string{"item", "show"}, map[string]any{})
	if err != nil {
		t.Fatalf("Dispatch returned protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError, got success: %s", textOf(res))
	}
	env2 := decodeEnvelope(t, res)
	if env2.Error.Code != ErrValidationFailed {
		t.Errorf("Code = %q, want %q (full envelope: %+v)",
			env2.Error.Code, ErrValidationFailed, env2.Error)
	}
	if env2.Error.Field != "ref" {
		t.Errorf("Field = %q, want ref", env2.Error.Field)
	}
}
