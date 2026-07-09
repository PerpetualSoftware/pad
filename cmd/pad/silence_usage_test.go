package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestNewRootCmdSetsSilenceUsage locks in the core change for issue #852: the
// root command must carry SilenceUsage so a failed command doesn't dump the
// Cobra usage block on top of the real error. Because the usage-print gate in
// Cobra's ExecuteC checks the root command's SilenceUsage
// (`!cmd.SilenceUsage && !c.SilenceUsage`, where c is the root), setting it on
// the root suppresses usage for the whole tree.
func TestNewRootCmdSetsSilenceUsage(t *testing.T) {
	if !newRootCmd().SilenceUsage {
		t.Fatal("expected newRootCmd().SilenceUsage == true (issue #852)")
	}
}

// TestSilenceUsageBehavior documents Cobra's actual usage-suppression
// semantics with an isolated command, showing the on/off contrast so the
// suite would catch a regression that dropped SilenceUsage.
//
// Deviation note (see task #852): the objective phrased it as "runtime errors
// no longer print usage; flag-parse errors still do." That split is NOT what
// stock Cobra does with a bare `SilenceUsage: true`. Cobra parses flags inside
// Command.execute(), so an unknown-flag error is returned down the SAME error
// path as a RunE error and is gated by the SAME SilenceUsage check. With
// SilenceUsage on, BOTH runtime (RunE) errors AND flag-parse errors have their
// usage block suppressed. The cases below assert that real behavior.
func TestSilenceUsageBehavior(t *testing.T) {
	tests := []struct {
		name         string
		silenceUsage bool
		args         []string
		runErr       error // returned by RunE when args parse cleanly
		wantUsage    bool
	}{
		{
			name:         "RunE error with SilenceUsage suppresses usage",
			silenceUsage: true,
			args:         []string{},
			runErr:       errors.New("kaboom"),
			wantUsage:    false,
		},
		{
			name:         "RunE error without SilenceUsage prints usage",
			silenceUsage: false,
			args:         []string{},
			runErr:       errors.New("kaboom"),
			wantUsage:    true,
		},
		{
			// Documents the deviation: flag-parse errors are ALSO silenced
			// under SilenceUsage, contrary to the task's prose objective.
			name:         "unknown flag with SilenceUsage suppresses usage",
			silenceUsage: true,
			args:         []string{"--nonsense-flag"},
			runErr:       nil,
			wantUsage:    false,
		},
		{
			name:         "unknown flag without SilenceUsage prints usage",
			silenceUsage: false,
			args:         []string{"--nonsense-flag"},
			runErr:       nil,
			wantUsage:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{
				Use:          "widget",
				SilenceUsage: tt.silenceUsage,
				RunE: func(_ *cobra.Command, _ []string) error {
					return tt.runErr
				},
			}
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetArgs(tt.args)

			// Every case here is expected to surface an error (a RunE error or
			// an unknown-flag parse error).
			if err := cmd.Execute(); err == nil {
				t.Fatalf("expected an error, got nil")
			}

			combined := out.String() + errOut.String()
			gotUsage := strings.Contains(combined, "Usage:")
			if gotUsage != tt.wantUsage {
				t.Errorf("usage printed = %v, want %v; combined output:\n%s", gotUsage, tt.wantUsage, combined)
			}
		})
	}
}

// TestRealRootSuppressesUsageOnRuntimeError exercises the actual pad root
// command (via newRootCmd) rather than a synthetic one. A throwaway leaf whose
// RunE fails stands in for any real subcommand's runtime failure, so the test
// stays hermetic (no config load, no server round-trip). The root's
// SilenceUsage must keep the usage block out of the output.
func TestRealRootSuppressesUsageOnRuntimeError(t *testing.T) {
	root := newRootCmd()
	root.AddCommand(&cobra.Command{
		Use: "boom",
		RunE: func(_ *cobra.Command, _ []string) error {
			return errors.New("runtime failure")
		},
	})

	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"boom"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected RunE error to propagate, got nil")
	}

	combined := out.String() + errOut.String()
	if strings.Contains(combined, "Usage:") {
		t.Errorf("runtime error must not print the usage block; got:\n%s", combined)
	}
}
