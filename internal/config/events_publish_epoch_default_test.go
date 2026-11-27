package config

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TASK-4895: phase 2 of the event ID-space migration (BUG-2736) is the
// default. These tests pin the three things that make the flip safe for an
// install that already has a config.toml: where the effective value came from
// is recorded, a saved default writes no key at all, and an explicit false
// (typed by an operator, or written by an older `pad init`) is kept.

var epochKeyLine = regexp.MustCompile(`(?m)^events_publish_epoch\s*=`)

func readConfigText(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEventsPublishEpochSourceDefault(t *testing.T) {
	isolateEnv(t)
	cfg := mustLoad(t)
	if !cfg.EventsPublishEpoch {
		t.Error("EventsPublishEpoch = false with no file key and no env, want true (the default)")
	}
	if cfg.EventsPublishEpochSource != EventsPublishEpochSourceDefault {
		t.Errorf("EventsPublishEpochSource = %q, want %q", cfg.EventsPublishEpochSource, EventsPublishEpochSourceDefault)
	}
}

// A config.toml that holds an unrelated key must not count as having set the
// epoch key: the source is the key, not the file.
func TestEventsPublishEpochSourceDefaultWithAFileWithoutTheKey(t *testing.T) {
	dir := isolateEnv(t)
	writeConfigFile(t, dir, "editor = \"nano\"\n")
	cfg := mustLoad(t)
	if !cfg.EventsPublishEpoch || cfg.EventsPublishEpochSource != EventsPublishEpochSourceDefault {
		t.Errorf("got %v from %q, want true from %q", cfg.EventsPublishEpoch, cfg.EventsPublishEpochSource, EventsPublishEpochSourceDefault)
	}
}

func TestEventsPublishEpochSourceConfigFile(t *testing.T) {
	dir := isolateEnv(t)
	writeConfigFile(t, dir, "events_publish_epoch = false\n")
	cfg := mustLoad(t)
	if cfg.EventsPublishEpoch {
		t.Error("EventsPublishEpoch = true, want false from config.toml")
	}
	if cfg.EventsPublishEpochSource != EventsPublishEpochSourceConfigFile {
		t.Errorf("EventsPublishEpochSource = %q, want %q", cfg.EventsPublishEpochSource, EventsPublishEpochSourceConfigFile)
	}
}

func TestEventsPublishEpochSourceEnvOverFile(t *testing.T) {
	dir := isolateEnv(t)
	writeConfigFile(t, dir, "events_publish_epoch = false\n")
	t.Setenv("PAD_EVENTS_PUBLISH_EPOCH", "true")
	cfg := mustLoad(t)
	if !cfg.EventsPublishEpoch {
		t.Error("EventsPublishEpoch = false, want true: the env var wins over the file")
	}
	if cfg.EventsPublishEpochSource != EventsPublishEpochSourceEnv {
		t.Errorf("EventsPublishEpochSource = %q, want %q", cfg.EventsPublishEpochSource, EventsPublishEpochSourceEnv)
	}
}

func TestEventsPublishEpochSourceEnvFalse(t *testing.T) {
	isolateEnv(t)
	t.Setenv("PAD_EVENTS_PUBLISH_EPOCH", "false")
	cfg := mustLoad(t)
	if cfg.EventsPublishEpoch {
		t.Error("EventsPublishEpoch = true, want false: PAD_EVENTS_PUBLISH_EPOCH=false is the opt-out")
	}
	if cfg.EventsPublishEpochSource != EventsPublishEpochSourceEnv {
		t.Errorf("EventsPublishEpochSource = %q, want %q", cfg.EventsPublishEpochSource, EventsPublishEpochSourceEnv)
	}
}

// A non-boolean env value is ignored loudly and leaves the source where the
// file or the default put it.
func TestEventsPublishEpochSourceUnchangedByANonBooleanEnv(t *testing.T) {
	dir := isolateEnv(t)
	writeConfigFile(t, dir, "events_publish_epoch = false\n")
	t.Setenv("PAD_EVENTS_PUBLISH_EPOCH", "yes-please")
	cfg := mustLoad(t)
	if cfg.EventsPublishEpoch || cfg.EventsPublishEpochSource != EventsPublishEpochSourceConfigFile {
		t.Errorf("got %v from %q, want false from %q", cfg.EventsPublishEpoch, cfg.EventsPublishEpochSource, EventsPublishEpochSourceConfigFile)
	}
}

// Save of a default config writes no events_publish_epoch key, so a later
// change of default reaches the install. Before TASK-4895 every Save wrote
// `events_publish_epoch = false`, which pinned phase 1 for good.
func TestSaveOfDefaultWritesNoEventsPublishEpochKey(t *testing.T) {
	dir := isolateEnv(t)
	cfg := mustLoad(t)
	mustSave(t, cfg)
	if text := readConfigText(t, dir); epochKeyLine.MatchString(text) {
		t.Errorf("config.toml holds an events_publish_epoch key for a default config:\n%s", text)
	}
	back := mustLoad(t)
	if !back.EventsPublishEpoch || back.EventsPublishEpochSource != EventsPublishEpochSourceDefault {
		t.Errorf("round trip: got %v from %q, want true from %q", back.EventsPublishEpoch, back.EventsPublishEpochSource, EventsPublishEpochSourceDefault)
	}
}

// Save without Load (pad init builds a Config from DefaultConfig) takes the
// same path.
func TestSaveWithoutLoadOfDefaultWritesNoEventsPublishEpochKey(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.ConfigPath = filepath.Join(dir, "config.toml")
	mustSave(t, cfg)
	if text := readConfigText(t, dir); epochKeyLine.MatchString(text) {
		t.Errorf("config.toml holds an events_publish_epoch key for a default config:\n%s", text)
	}
}

func TestSaveOfOptOutWritesEventsPublishEpochFalse(t *testing.T) {
	dir := isolateEnv(t)
	cfg := mustLoad(t)
	cfg.EventsPublishEpoch = false
	mustSave(t, cfg)
	text := readConfigText(t, dir)
	if !regexp.MustCompile(`(?m)^events_publish_epoch = false$`).MatchString(text) {
		t.Errorf("config.toml lacks `events_publish_epoch = false` for an opted-out config:\n%s", text)
	}
	back := mustLoad(t)
	if back.EventsPublishEpoch || back.EventsPublishEpochSource != EventsPublishEpochSourceConfigFile {
		t.Errorf("round trip: got %v from %q, want false from %q", back.EventsPublishEpoch, back.EventsPublishEpochSource, EventsPublishEpochSourceConfigFile)
	}
	// And a second Save of the reloaded config keeps the opt-out.
	mustSave(t, back)
	if !regexp.MustCompile(`(?m)^events_publish_epoch = false$`).MatchString(readConfigText(t, dir)) {
		t.Error("a second Save dropped the opt-out")
	}
}

// An existing file holding `events_publish_epoch = false` (what every Save
// wrote before TASK-4895) is not reinterpreted: a Save that changes something
// else keeps it.
func TestSaveKeepsAnExistingFalseInTheFile(t *testing.T) {
	dir := isolateEnv(t)
	writeConfigFile(t, dir, "editor = \"vim\"\nevents_publish_epoch = false\n")
	cfg := mustLoad(t)
	cfg.Editor = "nano"
	mustSave(t, cfg)
	if !regexp.MustCompile(`(?m)^events_publish_epoch = false$`).MatchString(readConfigText(t, dir)) {
		t.Error("Save dropped an events_publish_epoch = false the file already held")
	}
}

// The env opt-out is not persisted (the persistView rule every env override
// follows), and the env opt-in over a file false does not erase the file false.
func TestSaveDoesNotPersistTheEnvEpochValue(t *testing.T) {
	t.Run("env false over no key", func(t *testing.T) {
		dir := isolateEnv(t)
		t.Setenv("PAD_EVENTS_PUBLISH_EPOCH", "false")
		mustSave(t, mustLoad(t))
		if text := readConfigText(t, dir); epochKeyLine.MatchString(text) {
			t.Errorf("env false was persisted:\n%s", text)
		}
	})
	t.Run("env true over file false", func(t *testing.T) {
		dir := isolateEnv(t)
		writeConfigFile(t, dir, "events_publish_epoch = false\n")
		t.Setenv("PAD_EVENTS_PUBLISH_EPOCH", "true")
		mustSave(t, mustLoad(t))
		if !regexp.MustCompile(`(?m)^events_publish_epoch = false$`).MatchString(readConfigText(t, dir)) {
			t.Error("env true erased the file's false")
		}
	})
}
