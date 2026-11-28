package config

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TASK-4895: phase 2 of the event ID-space migration (BUG-2736) is the
// default, and the setting lives in PAD_EVENTS_PUBLISH_EPOCH only. A fleet
// mixing phase-1 and phase-2 publishers keeps dropping replay buffers and
// resyncing clients, and older `pad init` / `pad auth configure` wrote
// `events_publish_epoch = false` into config.toml without anyone choosing it,
// so honouring that line would split fleets indefinitely. These tests pin
// that the file key never reaches the field, that its presence is recorded
// so the server can warn, and that Save never writes the key.

var epochKeyLine = regexp.MustCompile(`(?m)^events_publish_epoch\s*=`)

func readConfigText(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEventsPublishEpochLoad(t *testing.T) {
	for _, tc := range []struct {
		name        string
		file        string // config.toml body; "" writes no file
		env         string
		wantOn      bool
		wantSource  string
		wantIgnored bool
	}{
		{"no file key, no env", "", "", true, EventsPublishEpochSourceDefault, false},
		{"a file without the key", "editor = \"nano\"\n", "", true, EventsPublishEpochSourceDefault, false},
		{"file false, no env, is ignored", "events_publish_epoch = false\n", "", true, EventsPublishEpochSourceDefault, true},
		{"file true, no env, is ignored", "events_publish_epoch = true\n", "", true, EventsPublishEpochSourceDefault, true},
		{"file true, env false", "events_publish_epoch = true\n", "false", false, EventsPublishEpochSourceEnv, true},
		{"file false, env true", "events_publish_epoch = false\n", "true", true, EventsPublishEpochSourceEnv, true},
		{"no file key, env false", "", "false", false, EventsPublishEpochSourceEnv, false},
		{"no file key, env true", "", "true", true, EventsPublishEpochSourceEnv, false},
		// A non-boolean env value is ignored loudly and changes nothing,
		// the source included; the file key still does not count.
		{"file false, env not a boolean", "events_publish_epoch = false\n", "yes-please", true, EventsPublishEpochSourceDefault, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolateEnv(t)
			if tc.file != "" {
				writeConfigFile(t, dir, tc.file)
			}
			t.Setenv("PAD_EVENTS_PUBLISH_EPOCH", tc.env)
			cfg := mustLoad(t)
			if cfg.EventsPublishEpoch != tc.wantOn || cfg.EventsPublishEpochSource != tc.wantSource ||
				cfg.EventsPublishEpochFileIgnored != tc.wantIgnored {
				t.Errorf("Load: EventsPublishEpoch %v source %q file-ignored %v, want %v %q %v",
					cfg.EventsPublishEpoch, cfg.EventsPublishEpochSource, cfg.EventsPublishEpochFileIgnored,
					tc.wantOn, tc.wantSource, tc.wantIgnored)
			}
		})
	}
}

// Save never writes the key, whatever the in-memory value and wherever it
// came from, and a Load+Save of a file holding it removes it.
func TestSaveNeverWritesEventsPublishEpoch(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		env  string
		set  *bool // assigned after Load, as a caller would
	}{
		{"default", "", "", nil},
		{"set false after Load", "", "", epochPtr(false)},
		{"set true after Load", "", "", epochPtr(true)},
		{"env false", "", "false", nil},
		{"file false", "editor = \"vim\"\nevents_publish_epoch = false\n", "", nil},
		{"file true", "events_publish_epoch = true\n", "", nil},
		{"file false, set false after Load", "events_publish_epoch = false\n", "", epochPtr(false)},
		{"file true, env false", "events_publish_epoch = true\n", "false", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolateEnv(t)
			if tc.file != "" {
				writeConfigFile(t, dir, tc.file)
			}
			t.Setenv("PAD_EVENTS_PUBLISH_EPOCH", tc.env)
			cfg := mustLoad(t)
			if tc.set != nil {
				cfg.EventsPublishEpoch = *tc.set
			}
			mustSave(t, cfg)
			if text := readConfigText(t, dir); epochKeyLine.MatchString(text) {
				t.Errorf("config.toml holds an events_publish_epoch key after Save:\n%s", text)
			}
		})
	}
}

// Save without Load (pad init builds a Config from DefaultConfig) writes no
// key either, for either value.
func TestSaveWithoutLoadNeverWritesEventsPublishEpoch(t *testing.T) {
	for _, on := range []bool{true, false} {
		dir := t.TempDir()
		cfg := DefaultConfig()
		cfg.DataDir = dir
		cfg.ConfigPath = filepath.Join(dir, "config.toml")
		cfg.EventsPublishEpoch = on
		mustSave(t, cfg)
		if text := readConfigText(t, dir); epochKeyLine.MatchString(text) {
			t.Errorf("EventsPublishEpoch %v: config.toml holds an events_publish_epoch key:\n%s", on, text)
		}
	}
}

func epochPtr(v bool) *bool { return &v }
