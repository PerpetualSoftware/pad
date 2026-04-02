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

func TestApplyConfigureValuesCloudUnavailable(t *testing.T) {
	cfg := config.DefaultConfig()

	err := applyConfigureValues(cfg, &configureValues{
		Mode: config.ModeCloud,
		URL:  "https://cloud.getpad.dev",
	})
	if err == nil {
		t.Fatal("expected cloud mode to be unavailable")
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
