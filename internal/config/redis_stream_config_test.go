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
	t.Setenv("PAD_EVENTS_PUBLISH_EPOCH", "true")

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
	// BUG-2736's phase-2 flip. Its consumer (the Redis bus wire form) has its
	// own tests and they all pass with Load() never populating this — the
	// deployment would simply stay on phase 1 forever, which looks exactly
	// like a correct phase-1 deployment. That is the wiring gap this closes.
	if !cfg.EventsPublishEpoch {
		t.Error("EventsPublishEpoch = false, want true from PAD_EVENTS_PUBLISH_EPOCH")
	}
}

// A value that is not a boolean must leave the field alone rather than be
// read as truthy. Getting this backwards flips a deployment into phase 2 on a
// typo, which is the one direction of this migration that loses events on
// instances that have not been upgraded.
func TestEventsPublishEpochIgnoresANonBooleanValue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_EVENTS_PUBLISH_EPOCH", "yes-please")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EventsPublishEpoch {
		t.Error("an unparseable value must leave the flip off, not turn it on")
	}
}

// TestStreamAndRedisDefaults pins the shipped defaults. The per-user
// default is the one that changes behaviour for an existing deployment
// that sets nothing, so it is worth a test rather than a code comment.
func TestStreamAndRedisDefaults(t *testing.T) {
	for _, key := range []string{
		"PAD_REDIS_NAMESPACE", "PAD_SSE_MAX_PER_USER",
		"PAD_SSE_MAX_CONNECTIONS", "PAD_SSE_MAX_PER_WORKSPACE",
		"PAD_EVENTS_PUBLISH_EPOCH",
	} {
		if _, set := os.LookupEnv(key); set {
			t.Setenv(key, "")
		}
	}

	cfg := DefaultConfig()

	if cfg.RedisNamespace != "" {
		t.Errorf("default RedisNamespace = %q, want empty — a default namespace would move every existing deployment's keys", cfg.RedisNamespace)
	}
	// The default MUST be off. Phase 2 emits a wire form older instances
	// cannot parse, so defaulting it on would break a rolling upgrade for
	// every deployment that upgrades without reading the release notes —
	// which is the failure the two-phase rollout exists to prevent.
	if cfg.EventsPublishEpoch {
		t.Error("default EventsPublishEpoch = true, want false — phase 2 must be opted into after every instance accepts the new form")
	}
	if cfg.SSEMaxPerUser != 50 {
		t.Errorf("default SSEMaxPerUser = %d, want 50", cfg.SSEMaxPerUser)
	}
}
