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

// codex round 4. The env-var mapping test above proves PAD_EVENTS_PUBLISH_EPOCH
// reaches the field; it says nothing about the TOML tag. A wrong or missing
// `toml:"events_publish_epoch"` would keep every other test green while an
// operator who set the flag in ~/.pad/config.toml — which is the form the
// rollback procedure warns about, because a file value outlives an unset env
// var — silently stayed on phase 1.
func TestEventsPublishEpochRoundTripsThroughTheConfigFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAD_EVENTS_PUBLISH_EPOCH", "")

	cfg := DefaultConfig()
	cfg.EventsPublishEpoch = true
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.EventsPublishEpoch {
		t.Error("events_publish_epoch did not survive a save/load round trip through config.toml")
	}

	// The other half of the rollback warning: an ABSENT env var must leave the
	// file's value standing, not silently reset it. If this ever changes,
	// docs/deployment.md's rollback procedure changes with it.
	if os.Getenv("PAD_EVENTS_PUBLISH_EPOCH") != "" {
		t.Fatal("fixture: the env var must be unset for this half to mean anything")
	}
	if !reloaded.EventsPublishEpoch {
		t.Error("an unset env var must leave the config file's value alone")
	}
}

// codex round 12. The rollback procedure tells an operator to make the
// EFFECTIVE value false, and warns that unsetting the environment variable is
// not the same thing. Neither half of that had a test, so a load order that
// let the file win over an explicit env-var false would have kept a deployment
// stuck on phase 2 while its operator believed they had rolled back.
func TestEventsPublishEpochPrecedenceBetweenEnvAndFile(t *testing.T) {
	writeFileValue := func(t *testing.T) {
		t.Helper()
		cfg := DefaultConfig()
		cfg.EventsPublishEpoch = true
		if err := cfg.Save(); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	t.Run("an explicit env false overrides a true in the file", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		writeFileValue(t)
		t.Setenv("PAD_EVENTS_PUBLISH_EPOCH", "false")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if cfg.EventsPublishEpoch {
			t.Error("an explicit env-var false must win over the config file — this is the documented rollback")
		}
	})

	t.Run("an unparseable env value leaves the file's value standing", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		writeFileValue(t)
		t.Setenv("PAD_EVENTS_PUBLISH_EPOCH", "off-ish")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		// The value is IGNORED, not read as false: a typo must not flip a
		// migration in either direction. The warning is what tells the
		// operator; the behaviour is to change nothing.
		if !cfg.EventsPublishEpoch {
			t.Error("an unparseable env value must leave the configured value alone, not reset it")
		}
	})
}

// ---------------------------------------------------------------------------
// BUG-2738's phase-2 flip. Structurally these mirror the EventsPublishEpoch
// tests above, and the ASSERTIONS are the same — but the RATIONALE is
// inverted, which is why they are written out rather than folded into a table
// with the epoch's comments attached. See TestEventsHeartbeatIgnoresANonBooleanValue.
// ---------------------------------------------------------------------------

// TestEventsHeartbeatEnvMapping is the wiring. The bus's heartbeat behaviour
// has its own tests and every one of them passes with Load() never populating
// this field — the deployment would simply stay on phase 1 forever, which is
// indistinguishable from a correct phase-1 deployment in every metric.
func TestEventsHeartbeatEnvMapping(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_EVENTS_HEARTBEAT", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.EventsHeartbeat {
		t.Error("EventsHeartbeat = false, want true from PAD_EVENTS_HEARTBEAT")
	}
	// The neighbour, so a copy-paste that pointed two env vars at one field
	// cannot pass. These two flags are the same SHAPE and are set in the same
	// procedures, which is exactly when that mistake happens.
	if cfg.EventsPublishEpoch {
		t.Error("PAD_EVENTS_HEARTBEAT must not move EventsPublishEpoch")
	}
}

// TestEventsHeartbeatIgnoresANonBooleanValue.
//
// READ THIS COMMENT BEFORE "FIXING" THIS TEST TO MATCH ITS EPOCH TWIN. The
// assertion is identical to TestEventsPublishEpochIgnoresANonBooleanValue and
// the reason for it is the OPPOSITE one.
//
// For the epoch flip, OFF was the data-LOSING direction: an instance stuck on
// phase 1 published a wire form nothing could misread, and the hazard was a
// typo carrying a deployment FORWARD into a phase its peers could not parse.
//
// Here OFF is the SAFE direction. An instance that publishes no heartbeat does
// no idle detection at all — exactly the behaviour that existed before this
// feature, so the worst case of a wrong OFF is that a wedged route goes
// unnoticed, which is where every deployment already was. An instance that publishes into a MIXED
// fleet makes every un-upgraded peer fail to decode the frame, drop that
// workspace's replay buffer, and tell every one of its live subscribers to
// resync — every 30 seconds, per workspace, for the length of the roll. The
// blast radius is the instances you have NOT upgraded, which no amount of care
// in the new code can reach.
//
// So: same assertion, opposite hazard. Copying the epoch's rationale onto this
// test would leave a comment arguing for the wrong thing, and the next person
// to touch it would "fix" the behaviour to match its own comment.
func TestEventsHeartbeatIgnoresANonBooleanValue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_EVENTS_HEARTBEAT", "yes-please")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EventsHeartbeat {
		t.Error("an unparseable value must not turn the flip on; publishing into a mixed fleet resyncs every client of every un-upgraded instance")
	}
}

// The precise contract, stated because the sentence above is easy to read as
// something stronger than it is (codex round 9): an unparseable value is
// IGNORED, not read as false. From a default config that leaves the flip off,
// which is what the test above checks. From a config file that set it true it
// leaves it TRUE — deliberately, and the same as the epoch flag: a typo must
// not move a migration in either direction, and silently rolling an operator
// back to phase 1 would disable detection on a fleet that had opted in without
// anything saying so. The warning is what tells them. The file-true case is
// asserted in TestEventsHeartbeatPrecedenceBetweenEnvAndFile; this comment
// exists so nobody "fixes" the ignore into a fail-closed reset.

// The default MUST be off, for the reason above: phase 2 emits a frame older
// instances treat as a hole in coverage, so defaulting it on would break a
// rolling upgrade for every deployment that upgrades without reading the
// release notes.
func TestEventsHeartbeatDefaultsOff(t *testing.T) {
	if _, set := os.LookupEnv("PAD_EVENTS_HEARTBEAT"); set {
		t.Setenv("PAD_EVENTS_HEARTBEAT", "")
	}
	if cfg := DefaultConfig(); cfg.EventsHeartbeat {
		t.Error("default EventsHeartbeat = true, want false — phase 2 must be opted into after every instance recognises the frame")
	}
}

// The TOML tag, for the same reason its epoch twin has one: the env-var test
// above says nothing about the file, and a wrong or missing
// `toml:"events_heartbeat"` would keep every other test green while an
// operator who set the flag in ~/.pad/config.toml silently stayed on phase 1.
func TestEventsHeartbeatRoundTripsThroughTheConfigFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_EVENTS_HEARTBEAT", "")

	cfg := DefaultConfig()
	cfg.EventsHeartbeat = true
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.EventsHeartbeat {
		t.Error("events_heartbeat did not survive a save/load round trip through config.toml")
	}
}

// The rollback procedure tells an operator to make the EFFECTIVE value false
// and warns that unsetting the environment variable is not the same thing.
// Both halves are asserted here for the same reason they are for the epoch
// flag — and here rollback is the direction an operator reaches for in a
// hurry, because the failure they are rolling back FROM is a fleet-wide resync
// storm.
func TestEventsHeartbeatPrecedenceBetweenEnvAndFile(t *testing.T) {
	writeFileValue := func(t *testing.T) {
		t.Helper()
		cfg := DefaultConfig()
		cfg.EventsHeartbeat = true
		if err := cfg.Save(); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	t.Run("an explicit env false overrides a true in the file", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		writeFileValue(t)
		t.Setenv("PAD_EVENTS_HEARTBEAT", "false")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if cfg.EventsHeartbeat {
			t.Error("an explicit env-var false must win over the config file — this is the documented rollback")
		}
	})

	t.Run("an unparseable env value leaves the file's value standing", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		writeFileValue(t)
		t.Setenv("PAD_EVENTS_HEARTBEAT", "off-ish")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		// IGNORED, not read as false. A typo must not move a migration in
		// either direction; the warning is what tells the operator.
		if !cfg.EventsHeartbeat {
			t.Error("an unparseable env value must leave the configured value alone, not reset it")
		}
	})
}

// TestWatchHeartbeatEnvMapping is the wiring for BUG-2769's phase-2 flip. The
// watch bus's heartbeat behaviour has its own tests and every one passes with
// Load() never populating this — the deployment simply stays on phase 1
// forever, which is indistinguishable from a correct phase-1 deployment.
func TestWatchHeartbeatEnvMapping(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_WATCH_HEARTBEAT", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.WatchHeartbeat {
		t.Error("WatchHeartbeat = false, want true from PAD_WATCH_HEARTBEAT")
	}
	// The neighbour, because these two flags are the same SHAPE and are set in
	// the same procedures — which is exactly when a copy-paste points both env
	// vars at one field.
	if cfg.EventsHeartbeat {
		t.Error("PAD_WATCH_HEARTBEAT must not move EventsHeartbeat: the two buses roll independently")
	}
}

// TestWatchHeartbeatIgnoresANonBooleanValue. Same inverted rationale as its
// events twin: OFF is the SAFE direction here, because an instance that
// publishes no watch heartbeat simply does no idle detection — the behaviour
// that existed before the feature — while one that publishes into a mixed fleet
// makes every un-upgraded peer drop its buffer and resync all its clients every
// 30 seconds.
func TestWatchHeartbeatIgnoresANonBooleanValue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_WATCH_HEARTBEAT", "yes-please")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WatchHeartbeat {
		t.Error("an unparseable value must not turn the flip on; publishing into a mixed fleet resyncs every client " +
			"of every un-upgraded instance")
	}
}

// The default MUST be off, for the reason above.
func TestWatchHeartbeatDefaultsOff(t *testing.T) {
	if _, set := os.LookupEnv("PAD_WATCH_HEARTBEAT"); set {
		t.Setenv("PAD_WATCH_HEARTBEAT", "")
	}
	if cfg := DefaultConfig(); cfg.WatchHeartbeat {
		t.Error("default WatchHeartbeat = true, want false — phase 2 must be opted into after every instance " +
			"recognises the frame")
	}
}

// The TOML tag, for the same reason its neighbours have one: a wrong or missing
// `toml:"watch_heartbeat"` keeps every other test green while an operator who
// set the flag in ~/.pad/config.toml silently stays on phase 1.
func TestWatchHeartbeatRoundTripsThroughTheConfigFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_WATCH_HEARTBEAT", "")

	cfg := DefaultConfig()
	cfg.WatchHeartbeat = true
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, err := Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.WatchHeartbeat {
		t.Error("watch_heartbeat did not survive a save/load round trip through config.toml")
	}
}

// TestEventsHeartbeatDoesNotMoveTheWatchFlag is the OTHER direction of the
// cross-check above, and it exists because verifying one direction of a
// two-flag independence claim and then writing the symmetric conclusion is a
// mistake I have already made. "The two roll independently" is a biconditional;
// one leg does not establish it.
func TestEventsHeartbeatDoesNotMoveTheWatchFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_EVENTS_HEARTBEAT", "true")
	t.Setenv("PAD_WATCH_HEARTBEAT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.EventsHeartbeat {
		t.Fatal("fixture: PAD_EVENTS_HEARTBEAT did not take effect, so this test proves nothing")
	}
	if cfg.WatchHeartbeat {
		t.Error("PAD_EVENTS_HEARTBEAT moved WatchHeartbeat: the activity bus's phase 2 would silently flip the " +
			"watch bus's, publishing into a fleet that may not recognise the frame")
	}
}

// TestWatchHeartbeatEnvBeatsTheConfigFile pins the precedence its events twin
// has. The direction that matters is env=false over file=true: that is the
// rollback, and an operator reaching for it during an incident needs it to work
// without editing a file on every host.
func TestWatchHeartbeatEnvBeatsTheConfigFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_WATCH_HEARTBEAT", "")

	cfg := DefaultConfig()
	cfg.WatchHeartbeat = true
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	t.Setenv("PAD_WATCH_HEARTBEAT", "false")
	reloaded, err := Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.WatchHeartbeat {
		t.Error("PAD_WATCH_HEARTBEAT=false did not override watch_heartbeat=true in config.toml; the rollback " +
			"path for a bad phase-2 flip goes through this")
	}
}
