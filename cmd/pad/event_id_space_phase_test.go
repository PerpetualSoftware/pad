package main

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
)

// TASK-4895: the Redis startup log names the event ID-space phase and where it
// came from, and warns once when config.toml holds an events_publish_epoch
// line, which Load ignores: the setting lives in PAD_EVENTS_PUBLISH_EPOCH only.
// The warning depends on that flag alone, in either phase.
func TestEventIDSpacePhase(t *testing.T) {
	for _, tc := range []struct {
		name      string
		on        bool
		source    string
		ignored   bool
		wantPhase int
		wantWarn  bool
	}{
		{"phase 2 by default", true, config.EventsPublishEpochSourceDefault, false, 2, false},
		{"phase 2 from env", true, config.EventsPublishEpochSourceEnv, false, 2, false},
		{"phase 1 from env", false, config.EventsPublishEpochSourceEnv, false, 1, false},
		{"phase 2 by default, file line ignored", true, config.EventsPublishEpochSourceDefault, true, 2, true},
		{"phase 2 from env, file line ignored", true, config.EventsPublishEpochSourceEnv, true, 2, true},
		{"phase 1 from env, file line ignored", false, config.EventsPublishEpochSourceEnv, true, 1, true},
		// A Config not built by Load (no source recorded) does not warn.
		{"phase 1 with no source", false, "", false, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				EventsPublishEpoch:            tc.on,
				EventsPublishEpochSource:      tc.source,
				EventsPublishEpochFileIgnored: tc.ignored,
			}
			phase, source, warn := eventIDSpacePhase(cfg)
			if phase != tc.wantPhase || source != tc.source || warn != tc.wantWarn {
				t.Errorf("eventIDSpacePhase = (%d, %q, %v), want (%d, %q, %v)",
					phase, source, warn, tc.wantPhase, tc.source, tc.wantWarn)
			}
		})
	}
}
