package config

import (
	"strings"
	"testing"
)

// TASK-3321 G4: PAD_OPENAI_APPS_CHALLENGE reaches the resolved endpoints
// only as a token that can be served as exactly itself.

func TestLoadReadsOpenAIAppsChallenge(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_OPENAI_APPS_CHALLENGE", "tok-123")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ResolveMCPEndpoints().AppsChallenge; got != "tok-123" {
		t.Fatalf("AppsChallenge = %q, want tok-123", got)
	}
}

func TestResolveAppsChallenge(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		problem         bool
	}{
		{"unset", "", "", false},
		{"blank", "  \n", "", false},
		{"plain", "abc_DEF-123.xyz", "abc_DEF-123.xyz", false},
		{"outer whitespace trimmed", "  tok\n", "tok", false},
		{"inner space refused", "tok en", "", true},
		{"inner newline refused", "tok\nen", "", true},
		{"control refused", "tok\x7fen", "", true},
		{"over the limit refused", strings.Repeat("a", maxAppsChallengeLen+1), "", true},
		{"at the limit", strings.Repeat("a", maxAppsChallengeLen), strings.Repeat("a", maxAppsChallengeLen), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := (&Config{OpenAIAppsChallenge: tc.raw}).ResolveMCPEndpoints()
			if e.AppsChallenge != tc.want {
				t.Errorf("AppsChallenge = %q, want %q", e.AppsChallenge, tc.want)
			}
			named := false
			for _, p := range e.Problems() {
				if strings.Contains(p, "PAD_OPENAI_APPS_CHALLENGE") {
					named = true
				}
			}
			if named != tc.problem {
				t.Errorf("Problems() names PAD_OPENAI_APPS_CHALLENGE = %v, want %v (%v)", named, tc.problem, e.Problems())
			}
		})
	}
}
