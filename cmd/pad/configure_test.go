package main

import (
	"testing"

	"github.com/xarmian/pad/internal/config"
)

func TestApplyConfigureValuesLocal(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.URL = "http://example.com"

	err := applyConfigureValues(cfg, &configureValues{
		Mode: config.ModeLocal,
		Host: "127.0.0.2",
		Port: 8787,
	})
	if err != nil {
		t.Fatalf("apply local configure values: %v", err)
	}
	if cfg.Mode != config.ModeLocal {
		t.Fatalf("expected local mode, got %q", cfg.Mode)
	}
	if cfg.URL != "" {
		t.Fatalf("expected local mode to clear URL, got %q", cfg.URL)
	}
	if cfg.Host != "127.0.0.2" || cfg.Port != 8787 {
		t.Fatalf("unexpected local bind %s:%d", cfg.Host, cfg.Port)
	}
}

func TestApplyConfigureValuesRemote(t *testing.T) {
	cfg := config.DefaultConfig()

	err := applyConfigureValues(cfg, &configureValues{
		Mode: config.ModeRemote,
		URL:  "https://pad.example.com/",
	})
	if err != nil {
		t.Fatalf("apply remote configure values: %v", err)
	}
	if cfg.Mode != config.ModeRemote {
		t.Fatalf("expected remote mode, got %q", cfg.Mode)
	}
	if cfg.URL != "https://pad.example.com" {
		t.Fatalf("expected normalized URL, got %q", cfg.URL)
	}
}

func TestApplyConfigureValuesCloud(t *testing.T) {
	cfg := config.DefaultConfig()

	// Cloud mode should ignore any incoming --url and pin to the canonical
	// public endpoint. Picking Cloud is, by definition, opting into our
	// managed deployment.
	err := applyConfigureValues(cfg, &configureValues{
		Mode: config.ModeCloud,
		URL:  "https://someone-tried-to-override.example.com",
	})
	if err != nil {
		t.Fatalf("apply cloud configure values: %v", err)
	}
	if cfg.Mode != config.ModeCloud {
		t.Fatalf("expected cloud mode, got %q", cfg.Mode)
	}
	if cfg.URL != config.CloudBaseURL {
		t.Fatalf("expected cloud URL to be %q, got %q", config.CloudBaseURL, cfg.URL)
	}
}

func TestApplyConfigureValuesRejectsDocker(t *testing.T) {
	cfg := config.DefaultConfig()

	// Docker mode was removed. Pre-launch — no back-compat. Any value
	// other than "", local, remote, cloud must be rejected by ValidMode.
	err := applyConfigureValues(cfg, &configureValues{
		Mode: "docker",
		URL:  "http://127.0.0.1:7777",
	})
	if err == nil {
		t.Fatal("expected docker mode to be rejected after removal")
	}
}

func TestPromptForModeAtoiSafe(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"1", 1},
		{"3", 3},
		{"42", 42},
		{"-1", 0},        // leading minus is not a digit, rejected
		{"1.5", 0},       // dot is not a digit, rejected
		{"abc", 0},       // non-numeric input, rejected
		{"100000000", 0}, // overflow guard rejects very large numbers
	}
	for _, tc := range cases {
		if got := atoiSafe(tc.in); got != tc.want {
			t.Fatalf("atoiSafe(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestMatchModeAlias(t *testing.T) {
	options := []modeOption{
		{key: config.ModeCloud, aliases: []string{"cloud"}},
		{key: config.ModeLocal, aliases: []string{"local"}},
		{key: config.ModeRemote, aliases: []string{"remote"}},
	}
	cases := []struct {
		in   string
		want string
	}{
		{"cloud", config.ModeCloud},
		{"local", config.ModeLocal},
		{"remote", config.ModeRemote},
		{"", ""},
		{"docker", ""}, // intentionally unmapped — picker rejects "docker"
		{"bogus", ""},
	}
	for _, tc := range cases {
		if got := matchModeAlias(options, tc.in); got != tc.want {
			t.Fatalf("matchModeAlias(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "http", raw: "http://127.0.0.1:7777/", want: "http://127.0.0.1:7777"},
		{name: "https", raw: "https://pad.example.com", want: "https://pad.example.com"},
		{name: "missing scheme", raw: "pad.example.com", wantErr: true},
		{name: "query params", raw: "https://pad.example.com?x=1", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeBaseURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeBaseURL(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}
