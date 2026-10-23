package config

import (
	"strings"
	"testing"
	"time"
)

func loadWithPushEnv(t *testing.T, env map[string]string) *Config {
	t.Helper()
	t.Setenv("PAD_DATA_DIR", t.TempDir())
	for _, k := range []string{"PAD_PUSH_DISPATCH_INTERVAL", "PAD_PUSH_DISPATCH_SETTLE", "PAD_PUSH_DISPATCH_LEASE", "PAD_PUSH_DISPATCH_LEASE_MARGIN"} {
		t.Setenv(k, env[k])
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load = %v, want nil: a bad push knob must not break Load", err)
	}
	return cfg
}

// TASK-3939 part C: each env knob is read into its own timing field. Kills a
// knob read from the wrong variable or dropped from the parse.
func TestPushDispatchTimings_EachKnobIsParsed(t *testing.T) {
	cfg := loadWithPushEnv(t, map[string]string{
		"PAD_PUSH_DISPATCH_INTERVAL":     "2s",
		"PAD_PUSH_DISPATCH_SETTLE":       "3s",
		"PAD_PUSH_DISPATCH_LEASE":        "1m30s",
		"PAD_PUSH_DISPATCH_LEASE_MARGIN": "20s",
	})
	got, err := cfg.PushDispatchTimings()
	if err != nil {
		t.Fatal(err)
	}
	want := PushDispatchTimings{Interval: 2 * time.Second, Settle: 3 * time.Second, Lease: 90 * time.Second, LeaseMargin: 20 * time.Second}
	if got != want {
		t.Fatalf("PushDispatchTimings = %+v, want %+v", got, want)
	}
}

func TestPushDispatchTimings_UnsetIsZero(t *testing.T) {
	got, err := loadWithPushEnv(t, nil).PushDispatchTimings()
	if err != nil || got != (PushDispatchTimings{}) {
		t.Fatalf("PushDispatchTimings = %+v, %v, want zero and nil", got, err)
	}
}

// A set knob that is malformed or not positive is an error naming its
// variable, never a silent default.
func TestPushDispatchTimings_BadValueNamesTheVariable(t *testing.T) {
	vars := []string{"PAD_PUSH_DISPATCH_INTERVAL", "PAD_PUSH_DISPATCH_SETTLE", "PAD_PUSH_DISPATCH_LEASE", "PAD_PUSH_DISPATCH_LEASE_MARGIN"}
	for _, v := range vars {
		for _, bad := range []string{"soon", "10", "0", "0s", "-5s", " "} {
			t.Run(v+"="+bad, func(t *testing.T) {
				cfg := loadWithPushEnv(t, map[string]string{v: bad})
				_, err := cfg.PushDispatchTimings()
				if err == nil || !strings.Contains(err.Error(), v+" ") {
					t.Fatalf("PushDispatchTimings = %v, want an error naming %s", err, v)
				}
				if strings.Contains(err.Error(), bad+")") || strings.HasSuffix(err.Error(), bad) {
					t.Errorf("error echoes the value: %v", err)
				}
			})
		}
	}
}
