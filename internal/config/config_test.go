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

	cfg.Mode = ModeRemote
	cfg.URL = "https://pad.example.com"
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
	if reloaded.Mode != ModeRemote {
		t.Fatalf("expected mode %q, got %q", ModeRemote, reloaded.Mode)
	}
	if reloaded.URL != "https://pad.example.com" {
		t.Fatalf("expected remote URL to round-trip, got %q", reloaded.URL)
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

	cfg.Mode = ModeRemote
	if cfg.ManagesLocalServer() {
		t.Fatal("expected remote mode to avoid local server management")
	}

	cfg.Mode = ModeCloud
	if cfg.ManagesLocalServer() {
		t.Fatal("expected cloud mode to avoid local server management")
	}
}

func TestValidModeAcceptsKnownModes(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{"", true},
		{ModeLocal, true},
		{ModeRemote, true},
		{ModeCloud, true},
		{"docker", false}, // removed in favor of Remote — pre-launch, no back-compat
		{"bogus", false},
	}
	for _, tc := range cases {
		if got := ValidMode(tc.mode); got != tc.want {
			t.Fatalf("ValidMode(%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestIsCloudReportsCloudMode(t *testing.T) {
	cfg := &Config{Mode: ModeCloud}
	if !cfg.IsCloud() {
		t.Fatal("expected IsCloud() to be true when Mode == ModeCloud")
	}
	cfg.Mode = ModeRemote
	if cfg.IsCloud() {
		t.Fatal("expected IsCloud() to be false when Mode == ModeRemote")
	}
}

func TestCloudBaseURLIsCanonicalAppURL(t *testing.T) {
	if CloudBaseURL != "https://app.getpad.dev" {
		t.Fatalf("CloudBaseURL changed unexpectedly: %q — coordinate with pad-cloud and `pad configure` Cloud-mode handler before changing", CloudBaseURL)
	}
}

func TestBrowserURLNormalizesUnspecifiedHost(t *testing.T) {
	cases := []struct {
		name string
		host string
		want string
	}{
		{"loopback unchanged", "127.0.0.1", "http://127.0.0.1:7777"},
		{"named host unchanged", "pad.local", "http://pad.local:7777"},
		{"empty host normalized", "", "http://127.0.0.1:7777"},
		{"ipv4 unspecified normalized", "0.0.0.0", "http://127.0.0.1:7777"},
		{"ipv6 unspecified normalized", "::", "http://127.0.0.1:7777"},
		{"ipv6 bracketed unspecified normalized", "[::]", "http://127.0.0.1:7777"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Host: tc.host, Port: 7777}
			if got := cfg.BrowserURL(); got != tc.want {
				t.Fatalf("BrowserURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBrowserURLPrefersExplicitURL(t *testing.T) {
	cfg := &Config{
		Host: "0.0.0.0", // would normally be normalized
		Port: 7777,
		URL:  "https://app.getpad.dev/",
	}
	want := "https://app.getpad.dev"
	if got := cfg.BrowserURL(); got != want {
		t.Fatalf("BrowserURL() = %q, want %q (explicit URL must win and trailing slash trimmed)", got, want)
	}
}
