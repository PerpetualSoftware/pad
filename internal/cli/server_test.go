package cli

import (
	"os"
	"testing"

	"github.com/xarmian/pad/internal/config"
)

func TestEnsureServerSkipsWhenClientDoesNotManageLocalServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.DefaultConfig()
	if err := EnsureServer(cfg); err != nil {
		t.Fatalf("EnsureServer with default config: %v", err)
	}
	if _, err := os.Stat(cfg.PIDFile()); !os.IsNotExist(err) {
		t.Fatalf("expected no PID file for unconfigured client, got err=%v", err)
	}

	cfg.Mode = config.ModeRemote
	cfg.LoadedFromFile = true
	if err := EnsureServer(cfg); err != nil {
		t.Fatalf("EnsureServer with remote mode: %v", err)
	}
	if _, err := os.Stat(cfg.PIDFile()); !os.IsNotExist(err) {
		t.Fatalf("expected no PID file for remote client, got err=%v", err)
	}
}
