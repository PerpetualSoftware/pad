package config

import (
	"strings"
	"testing"
	"time"
)

func TestDBRuntimeLimits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		stmt, lk, idl string
		want          DBRuntimeLimits
		errEnv        string
	}{
		{name: "defaults", want: DBRuntimeLimits{30 * time.Second, 10 * time.Second, 60 * time.Second}},
		{name: "raised", stmt: "2m", lk: "30s", idl: "5m", want: DBRuntimeLimits{2 * time.Minute, 30 * time.Second, 5 * time.Minute}},
		{name: "zero is no limit", stmt: "0", lk: "off", idl: "OFF", want: DBRuntimeLimits{}},
		{name: "one off", stmt: " 0 ", want: DBRuntimeLimits{0, 10 * time.Second, 60 * time.Second}},
		{name: "malformed statement", stmt: "soon", errEnv: "PAD_DB_STATEMENT_TIMEOUT"},
		{name: "negative lock", lk: "-5s", errEnv: "PAD_DB_LOCK_TIMEOUT"},
		{name: "bare number", idl: "30", errEnv: "PAD_DB_IDLE_IN_TX_TIMEOUT"},
		{name: "whitespace only is set and malformed", stmt: "  ", errEnv: "PAD_DB_STATEMENT_TIMEOUT"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{DBStatementTimeoutRaw: c.stmt, DBLockTimeoutRaw: c.lk, DBIdleInTxTimeoutRaw: c.idl}
			got, err := cfg.DBRuntimeLimits()
			if c.errEnv != "" {
				if err == nil || !strings.Contains(err.Error(), c.errEnv) {
					t.Fatalf("err = %v, want it to name %s", err, c.errEnv)
				}
				for _, raw := range []string{c.stmt, c.lk, c.idl} {
					if raw != "" && strings.TrimSpace(raw) != "" && strings.Contains(err.Error(), strings.TrimSpace(raw)) && len(strings.TrimSpace(raw)) > 2 {
						t.Errorf("error echoes the value %q: %v", raw, err)
					}
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, c.want)
			}
		})
	}
}

func TestLoadReadsDBRuntimeEnv(t *testing.T) {
	t.Setenv("PAD_DB_STATEMENT_TIMEOUT", "45s")
	t.Setenv("PAD_DB_LOCK_TIMEOUT", "7s")
	t.Setenv("PAD_DB_IDLE_IN_TX_TIMEOUT", "off")
	t.Setenv("PAD_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.DBRuntimeLimits()
	if err != nil || got != (DBRuntimeLimits{45 * time.Second, 7 * time.Second, 0}) {
		t.Fatalf("got %+v, %v", got, err)
	}
}
