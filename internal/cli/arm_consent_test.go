package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// TestResolveAutoArm is the full consent truth table (PLAN-2613 S2, D4).
// Every row is a distinct assertion so a mutation that flips one cell —
// dropping the veto, making a per-user true an enabler, defaulting on —
// fails on exactly that row and names it.
func TestResolveAutoArm(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		repoAutoArm bool
		userAutoArm *bool
		want        bool
	}{
		{"default: neither set", false, nil, false},
		{"per-user true is NOT an enabler (no machine-global always-on)", false, boolPtr(true), false},
		{"per-user false with no repo opt-in stays off", false, boolPtr(false), false},
		{"repo opt-in, user no opinion", true, nil, true},
		{"repo opt-in, user also true", true, boolPtr(true), true},
		{"repo opt-in VETOED by per-user false (deny-wins)", true, boolPtr(false), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ResolveAutoArm(tc.repoAutoArm, tc.userAutoArm); got != tc.want {
				t.Fatalf("ResolveAutoArm(%v, %v) = %v, want %v", tc.repoAutoArm, tc.userAutoArm, got, tc.want)
			}
		})
	}
}

// TestResolveAutoArmFromDisk_ReadsBothSources wires the resolver to the
// two real config files: a repo .pad.toml and the per-user config.toml.
// It exercises the enable path and the veto path end to end, so a
// regression in either loader (wrong toml key, wrong nil-handling) shows
// up here and not just in the pure resolver.
func TestResolveAutoArmFromDisk_ReadsBothSources(t *testing.T) {
	// Not parallel: mutates HOME and the working directory.
	tests := []struct {
		name        string
		padTomlBody string
		configBody  string
		wantArmed   bool
		wantRepo    bool
		wantVeto    bool
	}{
		{
			name:        "repo opt-in, no user config → armed",
			padTomlBody: "workspace = \"demo\"\n[push]\nauto_arm = true\n",
			configBody:  "",
			wantArmed:   true,
			wantRepo:    true,
			wantVeto:    false,
		},
		{
			name:        "repo opt-in vetoed by per-user false → not armed",
			padTomlBody: "workspace = \"demo\"\n[push]\nauto_arm = true\n",
			configBody:  "[push]\nauto_arm = false\n",
			wantArmed:   false,
			wantRepo:    true,
			wantVeto:    true,
		},
		{
			name:        "no [push] table anywhere → default off",
			padTomlBody: "workspace = \"demo\"\n",
			configBody:  "",
			wantArmed:   false,
			wantRepo:    false,
			wantVeto:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			// config.Load keys the user config off PAD_DATA_DIR when set;
			// point it at HOME/.pad so the per-user config.toml is found.
			dataDir := filepath.Join(home, ".pad")
			if err := os.MkdirAll(dataDir, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PAD_DATA_DIR", dataDir)
			if tc.configBody != "" {
				if err := os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(tc.configBody), 0600); err != nil {
					t.Fatal(err)
				}
			}

			repoDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(repoDir, ".pad.toml"), []byte(tc.padTomlBody), 0600); err != nil {
				t.Fatal(err)
			}
			chdir(t, repoDir)

			got := ResolveAutoArmFromDisk()
			if got.Armed != tc.wantArmed {
				t.Errorf("Armed = %v, want %v", got.Armed, tc.wantArmed)
			}
			if got.RepoAutoArm != tc.wantRepo {
				t.Errorf("RepoAutoArm = %v, want %v", got.RepoAutoArm, tc.wantRepo)
			}
			if got.UserVeto != tc.wantVeto {
				t.Errorf("UserVeto = %v, want %v", got.UserVeto, tc.wantVeto)
			}
		})
	}
}
