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

// TestIsCloudServerOnlyOptsInViaEnv guards the bug codex caught in PR
// #272: a `pad init`-written `mode = "cloud"` in config.toml must NOT
// turn the local pad server into a cloud-tenant deployment. Only an
// explicit env-var opt-in (PAD_CLOUD=true|1 or PAD_MODE=cloud) should
// flip IsCloudServer().
func TestIsCloudServerOnlyOptsInViaEnv(t *testing.T) {
	t.Run("config-file mode=cloud does NOT opt into server cloud mode", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		// Defensive: ensure no env vars leak into this case.
		t.Setenv("PAD_MODE", "")
		t.Setenv("PAD_CLOUD", "")

		cfg := DefaultConfig()
		cfg.Mode = ModeCloud
		cfg.URL = CloudBaseURL
		if err := cfg.Save(); err != nil {
			t.Fatalf("save: %v", err)
		}

		reloaded, err := Load()
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if !reloaded.IsCloud() {
			t.Fatal("expected IsCloud() to be true (Mode == ModeCloud)")
		}
		if reloaded.IsCloudServer() {
			t.Fatal("expected IsCloudServer() to be FALSE for a config-file-only mode=cloud — only env-vars must opt into server cloud-tenant mode")
		}
	})

	t.Run("PAD_CLOUD=true opts into server cloud mode", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("PAD_CLOUD", "true")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !cfg.IsCloud() {
			t.Fatal("PAD_CLOUD=true should set IsCloud() = true")
		}
		if !cfg.IsCloudServer() {
			t.Fatal("PAD_CLOUD=true must opt into server cloud-tenant mode")
		}
	})

	t.Run("PAD_CLOUD=1 opts into server cloud mode", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("PAD_CLOUD", "1")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !cfg.IsCloudServer() {
			t.Fatal("PAD_CLOUD=1 must opt into server cloud-tenant mode")
		}
	})

	t.Run("PAD_MODE=cloud opts into server cloud mode", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("PAD_MODE", "cloud")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !cfg.IsCloud() {
			t.Fatal("PAD_MODE=cloud should set IsCloud() = true")
		}
		if !cfg.IsCloudServer() {
			t.Fatal("PAD_MODE=cloud must opt into server cloud-tenant mode")
		}
	})

	t.Run("PAD_MODE=remote does NOT opt into server cloud mode", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("PAD_MODE", "remote")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.IsCloud() {
			t.Fatal("PAD_MODE=remote should not set IsCloud() = true")
		}
		if cfg.IsCloudServer() {
			t.Fatal("PAD_MODE=remote must not opt into server cloud-tenant mode")
		}
	})

	t.Run("env-var opt-in overrides a non-cloud file config", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		// Persist a non-cloud config first.
		cfg := DefaultConfig()
		cfg.Mode = ModeRemote
		cfg.URL = "https://pad.example.com"
		if err := cfg.Save(); err != nil {
			t.Fatalf("save: %v", err)
		}

		// Then start with PAD_CLOUD=true: the env opt-in must win and
		// flip both signals.
		t.Setenv("PAD_CLOUD", "true")
		reloaded, err := Load()
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if !reloaded.IsCloud() {
			t.Fatal("PAD_CLOUD=true must override file mode=remote in IsCloud()")
		}
		if !reloaded.IsCloudServer() {
			t.Fatal("PAD_CLOUD=true must opt into server cloud-tenant mode regardless of file config")
		}
	})
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

// TestBaseURLIgnoresPublicURL pins the CLI-only contract for BaseURL():
// the function backs the local `pad` CLI's choice of API endpoint and
// must NOT be influenced by PublicURL, even when PublicURL is set.
// Otherwise a developer with a host-level PUBLIC_URL set for unrelated
// reasons would have their CLI silently route requests to that URL
// instead of their actual local server (Codex review of BUG-899).
func TestBaseURLIgnoresPublicURL(t *testing.T) {
	cfg := &Config{
		Host:      "127.0.0.1",
		Port:      7777,
		PublicURL: "https://app.example.com",
	}
	const want = "http://127.0.0.1:7777"
	if got := cfg.BaseURL(); got != want {
		t.Fatalf("BaseURL() = %q, want %q (PublicURL must not leak into CLI routing)", got, want)
	}
}

// TestPublicLinkBaseURLPrecedence pins the resolution order on the
// server-side accessor used to build emailed link targets: URL >
// PublicURL > constructed http://host:port. This is what fixes BUG-899
// — Pad Cloud sets PUBLIC_URL on the pad-cloud sidecar and forwards it
// to the pad service, so emailed links use the public domain instead
// of the bind address.
func TestPublicLinkBaseURLPrecedence(t *testing.T) {
	cases := []struct {
		name      string
		url       string
		publicURL string
		host      string
		port      int
		want      string
	}{
		{"URL wins over PublicURL", "https://api.example.com", "https://app.example.com", "0.0.0.0", 7777, "https://api.example.com"},
		{"URL wins with trailing slash trimmed", "https://api.example.com/", "", "0.0.0.0", 7777, "https://api.example.com"},
		{"PublicURL used when URL empty", "", "https://app.example.com", "0.0.0.0", 7777, "https://app.example.com"},
		{"PublicURL trims trailing slash", "", "https://app.example.com/", "0.0.0.0", 7777, "https://app.example.com"},
		{"falls through to host:port when both empty", "", "", "127.0.0.1", 7777, "http://127.0.0.1:7777"},
		{"host:port fallback exposes 0.0.0.0 (BUG-899 repro shape)", "", "", "0.0.0.0", 7777, "http://0.0.0.0:7777"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{URL: tc.url, PublicURL: tc.publicURL, Host: tc.host, Port: tc.port}
			if got := cfg.PublicLinkBaseURL(); got != tc.want {
				t.Fatalf("PublicLinkBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLoadWithPublicURLDoesNotFlipMode is the safety belt against the
// footgun of treating PUBLIC_URL the same as PAD_URL: PUBLIC_URL is a
// generic env var name commonly set in unrelated deployment contexts, so
// reading it must NOT change the CLI's Mode (which would route the local
// CLI at a remote URL the operator never asked it to use).
func TestLoadWithPublicURLDoesNotFlipMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAD_DATA_DIR", home)
	t.Setenv("PUBLIC_URL", "https://app.example.com")
	// Explicitly clear PAD_URL in case the runner's environment has it.
	t.Setenv("PAD_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config with PUBLIC_URL: %v", err)
	}
	if cfg.PublicURL != "https://app.example.com" {
		t.Fatalf("expected PUBLIC_URL to populate cfg.PublicURL, got %q", cfg.PublicURL)
	}
	if cfg.URL != "" {
		t.Fatalf("PUBLIC_URL must not populate cfg.URL (would flip CLI to remote mode), got %q", cfg.URL)
	}
	if cfg.Mode == ModeRemote {
		t.Fatal("PUBLIC_URL must not flip Mode to Remote — that's PAD_URL's job")
	}
	// Server-side public-link accessor sees PUBLIC_URL...
	if cfg.PublicLinkBaseURL() != "https://app.example.com" {
		t.Fatalf("expected PublicLinkBaseURL to use PUBLIC_URL when PAD_URL is unset, got %q", cfg.PublicLinkBaseURL())
	}
	// ...but the CLI client accessor does not (would otherwise hijack
	// CLI routing on hosts with PUBLIC_URL set for unrelated reasons).
	if cfg.BaseURL() == "https://app.example.com" {
		t.Fatal("BaseURL() must not be influenced by PUBLIC_URL — CLI routing isolation")
	}
}

// TestPublicURLAloneDoesNotMarkConfigured guards against PUBLIC_URL
// affecting CLI control flow. IsConfigured() gates whether the CLI shows
// its "not configured / run setup" branch (cmd/pad/configure.go); if a
// generic PUBLIC_URL on the host marked the config as env-loaded, a
// developer who's never run `pad init` would get past that gate and the
// CLI would happily talk to a default-mode endpoint built from PUBLIC_URL.
// PUBLIC_URL is server-only — it must never participate in IsConfigured().
func TestPublicURLAloneDoesNotMarkConfigured(t *testing.T) {
	cfg := &Config{PublicURL: "https://app.example.com"}
	if cfg.IsConfigured() {
		t.Fatal("PUBLIC_URL on its own must not make IsConfigured() true — would short-circuit the CLI's not-configured branch on hosts that set the var for unrelated reasons")
	}
}

// TestLoadPADURLBeatsPUBLICURL pins the precedence at the env-load layer
// so an operator running on a host that has PUBLIC_URL set for unrelated
// reasons can still override it explicitly with PAD_URL.
func TestLoadPADURLBeatsPUBLICURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAD_DATA_DIR", home)
	t.Setenv("PAD_URL", "https://api.example.com")
	t.Setenv("PUBLIC_URL", "https://app.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.URL != "https://api.example.com" {
		t.Fatalf("expected PAD_URL to populate cfg.URL, got %q", cfg.URL)
	}
	if cfg.PublicURL != "https://app.example.com" {
		t.Fatalf("expected PUBLIC_URL to still populate cfg.PublicURL even when PAD_URL set, got %q", cfg.PublicURL)
	}
	if cfg.BaseURL() != "https://api.example.com" {
		t.Fatalf("PAD_URL must win in BaseURL(), got %q", cfg.BaseURL())
	}
	if cfg.PublicLinkBaseURL() != "https://api.example.com" {
		t.Fatalf("PAD_URL must also win in PublicLinkBaseURL(), got %q", cfg.PublicLinkBaseURL())
	}
}
