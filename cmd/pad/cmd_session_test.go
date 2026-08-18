package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMonitorSessionIdentity_ArmedFromAutoArm is the S2 end-to-end wiring
// check (PLAN-2613 D4): a repository that opted in via .pad.toml [push]
// auto_arm makes the monitor announce armed=true, and a repository that
// did not stays unarmed. This is what carries the whole auto path from
// config to the ?armed=true the client sends — without it, S1's server
// gate would have nothing declaring consent.
func TestMonitorSessionIdentity_ArmedFromAutoArm(t *testing.T) {
	tests := []struct {
		name        string
		padTomlBody string
		wantArmed   bool
	}{
		{"auto_arm repo announces armed", "workspace = \"demo\"\n[push]\nauto_arm = true\n", true},
		{"no auto_arm stays unarmed (safe default)", "workspace = \"demo\"\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			// No socket and a fresh HOME → no local arm-state file, so
			// Armed is decided purely by the auto_arm config resolution.
			t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
			dataDir := filepath.Join(home, ".pad")
			if err := os.MkdirAll(dataDir, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PAD_DATA_DIR", dataDir)

			repo := t.TempDir()
			if err := os.WriteFile(filepath.Join(repo, ".pad.toml"), []byte(tc.padTomlBody), 0600); err != nil {
				t.Fatal(err)
			}
			orig, _ := os.Getwd()
			if err := os.Chdir(repo); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(orig) })

			if got := monitorSessionIdentity().Armed; got != tc.wantArmed {
				t.Fatalf("monitorSessionIdentity().Armed = %v, want %v", got, tc.wantArmed)
			}
		})
	}
}
