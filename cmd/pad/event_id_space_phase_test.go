package main

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
)

// TASK-4895: the Redis startup log names the event ID-space phase and where it
// came from, and warns once when phase 1 comes from config.toml. Every Save
// before the phase-2 default wrote `events_publish_epoch = false`, so that
// line in a file is usually not a choice; the warning says how to take the
// default. Phase 1 from the env var is a deliberate opt-out and stays quiet.
func TestEventIDSpacePhase(t *testing.T) {
	for _, tc := range []struct {
		name      string
		on        bool
		source    string
		wantPhase int
		wantWarn  bool
	}{
		{"phase 2 by default", true, config.EventsPublishEpochSourceDefault, 2, false},
		{"phase 2 from the file", true, config.EventsPublishEpochSourceConfigFile, 2, false},
		{"phase 2 from env", true, config.EventsPublishEpochSourceEnv, 2, false},
		{"phase 1 from the file warns", false, config.EventsPublishEpochSourceConfigFile, 1, true},
		{"phase 1 from env is an opt-out", false, config.EventsPublishEpochSourceEnv, 1, false},
		// A Config not built by Load (no source recorded) does not warn.
		{"phase 1 with no source", false, "", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{EventsPublishEpoch: tc.on, EventsPublishEpochSource: tc.source}
			phase, source, warn := eventIDSpacePhase(cfg)
			if phase != tc.wantPhase || source != tc.source || warn != tc.wantWarn {
				t.Errorf("eventIDSpacePhase = (%d, %q, %v), want (%d, %q, %v)",
					phase, source, warn, tc.wantPhase, tc.source, tc.wantWarn)
			}
		})
	}
}
