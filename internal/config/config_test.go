package config

import (
	"path/filepath"
	"testing"
)

func TestSaveAndLoadRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load default config: %v", err)
	}
	if cfg.LoadedFromFile {
		t.Fatal("expected config to start without a config file")
	}
	if cfg.IsConfigured() {
		t.Fatal("expected config without file or overrides to be unconfigured")
	}

	cfg.Mode = ModeDocker
	cfg.URL = "http://127.0.0.1:7777"
	cfg.Host = "127.0.0.1"
	cfg.Port = 7777
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if !reloaded.LoadedFromFile {
		t.Fatal("expected config to load from file after save")
	}
	if !reloaded.IsConfigured() {
		t.Fatal("expected saved config to count as configured")
	}
	if reloaded.Mode != ModeDocker {
		t.Fatalf("expected mode %q, got %q", ModeDocker, reloaded.Mode)
	}
	if reloaded.URL != "http://127.0.0.1:7777" {
		t.Fatalf("expected docker URL to round-trip, got %q", reloaded.URL)
	}
	if reloaded.ConfigPath != filepath.Join(home, ".pad", "config.toml") {
		t.Fatalf("unexpected config path %q", reloaded.ConfigPath)
	}
}

func TestLoadWithPadURLMarksConfigConfigured(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAD_URL", "https://pad.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config with PAD_URL: %v", err)
	}
	if !cfg.LoadedFromEnv {
		t.Fatal("expected PAD_URL override to mark config as env-loaded")
	}
	if !cfg.IsConfigured() {
		t.Fatal("expected PAD_URL override to count as configured")
	}
	if cfg.Mode != ModeRemote {
		t.Fatalf("expected PAD_URL to imply remote mode, got %q", cfg.Mode)
	}
	if cfg.BaseURL() != "https://pad.example.com" {
		t.Fatalf("unexpected base URL %q", cfg.BaseURL())
	}
}

func TestManagesLocalServerRequiresConfiguredLocalMode(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ManagesLocalServer() {
		t.Fatal("expected default config without explicit mode to avoid local server management")
	}

	cfg.Mode = ModeLocal
	if cfg.ManagesLocalServer() {
		t.Fatal("expected local mode without persisted/env config to avoid local server management")
	}

	cfg.LoadedFromFile = true
	if !cfg.ManagesLocalServer() {
		t.Fatal("expected configured local mode to manage a local server")
	}

	cfg.Mode = ModeDocker
	if cfg.ManagesLocalServer() {
		t.Fatal("expected docker mode to avoid local server management")
	}
}
