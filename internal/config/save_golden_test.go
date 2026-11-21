package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/secret"
)

// populatedConfig sets every persisted field, so the golden below covers the
// whole of config.toml, credentials included.
func populatedConfig(dir string) *Config {
	autoArm := false
	return &Config{
		Mode:                "remote",
		Host:                "10.0.0.7",
		Port:                7778,
		URL:                 "https://pad.example.test",
		Editor:              "vim",
		LogLevel:            "debug",
		MailerooAPIKey:      secret.New("fixture-SECRET-maileroo"),
		EmailFrom:           "noreply@example.test",
		EmailFromName:       "Pad Test",
		DecisionProvider:    "typesafe",
		TypesafeAPIKey:      secret.New("fixture-SECRET-typesafe"),
		DecisionModel:       "jev-1.0.0",
		CloudSecret:         secret.New("fixture-SECRET-cloud-a,fixture-SECRET-cloud-b"),
		CloudSidecarURL:     "http://sidecar.example.test:7778",
		CloudOutboundSecret: secret.New("fixture-SECRET-outbound"),
		PushGatewayURL:      "https://push.example.test",
		PushGatewaySecret:   secret.New("fixture-SECRET-push"),
		MCPPublicURL:        "https://mcp.example.test",
		AuthServerURL:       "https://auth.example.test",
		OpenAIAppsChallenge: "challenge-value",
		EncryptionKey:       secret.New("fixture-SECRET-encryption"),
		CORSOrigins:         "https://a.example.test,https://b.example.test",
		SecureCookies:       true,
		TrustedProxies:      "10.0.0.0/8",
		MetricsToken:        secret.New("fixture-SECRET-metrics"),
		IPChangeEnforce:     "strict",
		SSEMaxConnections:   11,
		SSEMaxPerWorkspace:  12,
		SSEMaxPerUser:       13,
		CollabMaxPerUser:    14,
		RedisNamespace:      "ns1",
		EventsPublishEpoch:  true,
		EventsHeartbeat:     true,
		WatchHeartbeat:      true,
		Push:                &PushConfig{AutoArm: &autoArm},
		DataDir:             dir,
		ConfigPath:          filepath.Join(dir, "config.toml"),
	}
}

// TestSaveGoldenBytes pins config.toml byte for byte: the file Save writes
// must not change shape, and a credential must be written as its value, never
// as a redaction marker. Regenerate with PAD_UPDATE_CONFIG_GOLDEN=1 only for a
// deliberate format change.
func TestSaveGoldenBytes(t *testing.T) {
	dir := t.TempDir()
	c := populatedConfig(dir)
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := os.ReadFile(c.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "save_golden.toml")
	if os.Getenv("PAD_UPDATE_CONFIG_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Save bytes differ from %s\n got:\n%s\nwant:\n%s", golden, got, want)
	}

	// The file must read back to the same credentials.
	t.Setenv("PAD_DATA_DIR", dir)
	for _, k := range []string{"PAD_MAILEROO_API_KEY", "PAD_CLOUD_SECRET", "PAD_CLOUD_OUTBOUND_SECRET", "PAD_PUSH_GATEWAY_SECRET", "PAD_ENCRYPTION_KEY", "PAD_METRICS_TOKEN", "PAD_MODE", "PAD_CLOUD", "PAD_URL", "PAD_HOST", "PAD_PORT"} {
		t.Setenv(k, "")
	}
	back, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for name, pair := range map[string][2]string{
		"maileroo_api_key":      {back.MailerooAPIKey.Reveal(), c.MailerooAPIKey.Reveal()},
		"typesafe_api_key":      {back.TypesafeAPIKey.Reveal(), c.TypesafeAPIKey.Reveal()},
		"cloud_secret":          {back.CloudSecret.Reveal(), c.CloudSecret.Reveal()},
		"cloud_outbound_secret": {back.CloudOutboundSecret.Reveal(), c.CloudOutboundSecret.Reveal()},
		"push_gateway_secret":   {back.PushGatewaySecret.Reveal(), c.PushGatewaySecret.Reveal()},
		"encryption_key":        {back.EncryptionKey.Reveal(), c.EncryptionKey.Reveal()},
		"metrics_token":         {back.MetricsToken.Reveal(), c.MetricsToken.Reveal()},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s read back as %q, want %q", name, pair[0], pair[1])
		}
	}
}
