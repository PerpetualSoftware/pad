package config

import (
	"os"
	"testing"
)

// TestStreamAndRedisEnvMapping covers the config plumbing for
// PAD_REDIS_NAMESPACE and PAD_SSE_MAX_PER_USER (codex round 5).
//
// The namespace PARSER and the admission GATE both have their own tests,
// and both would keep passing if Load() never populated these fields —
// the deployment would simply run with no namespace and no per-user
// bound, silently, which is exactly the state each feature exists to
// prevent. A knob tested only where it is consumed is a knob whose wiring
// is untested; this is the wiring.
func TestStreamAndRedisEnvMapping(t *testing.T) {
	// HOME first: Load() resolves ~/.pad and creates it. Without this the
	// test passes anywhere a developer's HOME is writable and fails in a
	// sandbox that sets HOME to an unwritable path — which is exactly
	// what the Nix check does, and the only gate that caught it. Every
	// other test in this package does the same.
	t.Setenv("HOME", t.TempDir())

	t.Setenv("PAD_REDIS_NAMESPACE", "staging-eu")
	t.Setenv("PAD_SSE_MAX_PER_USER", "7")
	t.Setenv("PAD_SSE_MAX_CONNECTIONS", "11")
	t.Setenv("PAD_SSE_MAX_PER_WORKSPACE", "13")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.RedisNamespace != "staging-eu" {
		t.Errorf("RedisNamespace = %q, want %q", cfg.RedisNamespace, "staging-eu")
	}
	if cfg.SSEMaxPerUser != 7 {
		t.Errorf("SSEMaxPerUser = %d, want 7", cfg.SSEMaxPerUser)
	}
	// The neighbours, so a copy-paste that pointed two env vars at one
	// field cannot pass.
	if cfg.SSEMaxConnections != 11 {
		t.Errorf("SSEMaxConnections = %d, want 11", cfg.SSEMaxConnections)
	}
	if cfg.SSEMaxPerWorkspace != 13 {
		t.Errorf("SSEMaxPerWorkspace = %d, want 13", cfg.SSEMaxPerWorkspace)
	}
}

// TestStreamAndRedisDefaults pins the shipped defaults. The per-user
// default is the one that changes behaviour for an existing deployment
// that sets nothing, so it is worth a test rather than a code comment.
func TestStreamAndRedisDefaults(t *testing.T) {
	for _, key := range []string{
		"PAD_REDIS_NAMESPACE", "PAD_SSE_MAX_PER_USER",
		"PAD_SSE_MAX_CONNECTIONS", "PAD_SSE_MAX_PER_WORKSPACE",
	} {
		if _, set := os.LookupEnv(key); set {
			t.Setenv(key, "")
		}
	}

	cfg := DefaultConfig()

	if cfg.RedisNamespace != "" {
		t.Errorf("default RedisNamespace = %q, want empty — a default namespace would move every existing deployment's keys", cfg.RedisNamespace)
	}
	if cfg.SSEMaxPerUser != 50 {
		t.Errorf("default SSEMaxPerUser = %d, want 50", cfg.SSEMaxPerUser)
	}
}
