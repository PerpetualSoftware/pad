package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	ModeLocal  = "local"
	ModeRemote = "remote"
	ModeDocker = "docker"
	ModeCloud  = "cloud"
)

type Config struct {
	Mode     string `toml:"mode"`
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	URL      string `toml:"url"` // Optional: full base URL (e.g., https://api.getpad.dev). Overrides host/port for CLI.
	Editor   string `toml:"editor"`
	LogLevel string `toml:"log_level"`
	DBPath   string `toml:"-"` // computed, not from config file
	DataDir  string `toml:"-"` // computed

	ConfigPath      string `toml:"-"`
	LoadedFromFile  bool   `toml:"-"`
	LoadedFromEnv   bool   `toml:"-"`
	LoadedFromFlags bool   `toml:"-"`

	// Email (Maileroo)
	MailerooAPIKey string `toml:"maileroo_api_key"`
	EmailFrom      string `toml:"email_from"`      // Sender address (e.g. noreply@getpad.dev)
	EmailFromName  string `toml:"email_from_name"` // Sender display name (e.g. Pad)

	// Cloud mode
	CloudSecret string `toml:"cloud_secret"` // Shared secret for pad-cloud sidecar communication

	// Encryption
	EncryptionKey string `toml:"encryption_key"` // 32-byte hex-encoded AES-256 key for encrypting sensitive fields

	// Security
	CORSOrigins    string `toml:"cors_origins"`    // Comma-separated allowed origins (e.g. "https://app.pad.dev,https://admin.pad.dev")
	SecureCookies  bool   `toml:"secure_cookies"`  // Set Secure flag on cookies (requires TLS)
	TrustedProxies string `toml:"trusted_proxies"` // Comma-separated CIDRs whose X-Forwarded-For is trusted. Empty = ignore proxy headers.
	MetricsToken   string `toml:"metrics_token"`   // Shared Bearer token required to scrape /metrics. Empty = loopback-only.

	// SSE limits
	SSEMaxConnections  int `toml:"sse_max_connections"`   // Global max SSE connections (0 = unlimited)
	SSEMaxPerWorkspace int `toml:"sse_max_per_workspace"` // Per-workspace max SSE connections (0 = unlimited)
}

func DefaultConfig() *Config {
	homeDir, _ := os.UserHomeDir()
	dataDir := filepath.Join(homeDir, ".pad")
	return &Config{
		Host:               "127.0.0.1",
		Port:               7777,
		Editor:             "",
		LogLevel:           "info",
		DBPath:             filepath.Join(dataDir, "pad.db"),
		DataDir:            dataDir,
		ConfigPath:         filepath.Join(dataDir, "config.toml"),
		SSEMaxConnections:  1000,
		SSEMaxPerWorkspace: 100,
	}
}

func Load() (*Config, error) {
	cfg := DefaultConfig()

	// Data-dir overrides affect where config.toml lives, so apply them first.
	if v := os.Getenv("PAD_DATA_DIR"); v != "" {
		cfg.DataDir = v
		cfg.DBPath = filepath.Join(v, "pad.db")
		cfg.ConfigPath = filepath.Join(v, "config.toml")
	}
	if v := os.Getenv("PAD_DB_PATH"); v != "" {
		cfg.DBPath = v
		cfg.DataDir = filepath.Dir(cfg.DBPath)
		cfg.ConfigPath = filepath.Join(cfg.DataDir, "config.toml")
	}

	// Ensure data directory exists
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		return nil, err
	}

	// Ensure logs directory exists
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "logs"), 0755); err != nil {
		return nil, err
	}

	// Load config file if it exists
	if _, err := os.Stat(cfg.ConfigPath); err == nil {
		cfg.LoadedFromFile = true
		if _, err := toml.DecodeFile(cfg.ConfigPath, cfg); err != nil {
			return nil, err
		}
	}

	// Environment variable overrides
	if v := os.Getenv("PAD_MODE"); v != "" {
		cfg.Mode = v
		cfg.LoadedFromEnv = true
	}
	if v := os.Getenv("PAD_HOST"); v != "" {
		cfg.Host = v
		cfg.LoadedFromEnv = true
	}
	if v := os.Getenv("PAD_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.Port = port
			cfg.LoadedFromEnv = true
		}
	}
	if v := os.Getenv("PAD_URL"); v != "" {
		cfg.URL = v
		cfg.LoadedFromEnv = true
		if cfg.Mode == "" {
			cfg.Mode = ModeRemote
		}
	}

	// Email (Maileroo)
	if v := os.Getenv("PAD_MAILEROO_API_KEY"); v != "" {
		cfg.MailerooAPIKey = v
	}
	if v := os.Getenv("PAD_EMAIL_FROM"); v != "" {
		cfg.EmailFrom = v
	}
	if v := os.Getenv("PAD_EMAIL_FROM_NAME"); v != "" {
		cfg.EmailFromName = v
	}
	// PAD_CLOUD=true is a convenience alias for PAD_MODE=cloud
	if v := os.Getenv("PAD_CLOUD"); v == "true" || v == "1" {
		cfg.Mode = ModeCloud
		cfg.LoadedFromEnv = true
	}
	if v := os.Getenv("PAD_CLOUD_SECRET"); v != "" {
		cfg.CloudSecret = v
	}
	if v := os.Getenv("PAD_ENCRYPTION_KEY"); v != "" {
		cfg.EncryptionKey = v
	}
	if v := os.Getenv("PAD_CORS_ORIGINS"); v != "" {
		cfg.CORSOrigins = v
	}
	if v := os.Getenv("PAD_SECURE_COOKIES"); v == "true" || v == "1" {
		cfg.SecureCookies = true
	}
	if v := os.Getenv("PAD_TRUSTED_PROXIES"); v != "" {
		cfg.TrustedProxies = v
	}
	if v := os.Getenv("PAD_METRICS_TOKEN"); v != "" {
		cfg.MetricsToken = v
	}
	if v := os.Getenv("PAD_SSE_MAX_CONNECTIONS"); v != "" {
		if max, err := strconv.Atoi(v); err == nil {
			cfg.SSEMaxConnections = max
		}
	}
	if v := os.Getenv("PAD_SSE_MAX_PER_WORKSPACE"); v != "" {
		if max, err := strconv.Atoi(v); err == nil {
			cfg.SSEMaxPerWorkspace = max
		}
	}

	return cfg, nil
}

// Save writes the persisted Pad config to disk.
func (c *Config) Save() error {
	if err := os.MkdirAll(c.DataDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(c.DataDir, "logs"), 0755); err != nil {
		return err
	}

	f, err := os.OpenFile(c.ConfigPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := toml.NewEncoder(f).Encode(c); err != nil {
		return err
	}

	c.LoadedFromFile = true
	return nil
}

// IsConfigured reports whether the client has an explicit global connection
// configuration, either from config.toml or an environment/flag override.
func (c *Config) IsConfigured() bool {
	return c.LoadedFromFile || c.LoadedFromEnv || c.LoadedFromFlags
}

// ManagesLocalServer reports whether this client configuration should
// auto-manage a local Pad server process.
func (c *Config) ManagesLocalServer() bool {
	return c.IsConfigured() && c.Mode == ModeLocal
}

func ValidMode(mode string) bool {
	switch mode {
	case "", ModeLocal, ModeRemote, ModeDocker, ModeCloud:
		return true
	default:
		return false
	}
}

// IsCloud reports whether the server is running in cloud mode.
// Cloud mode is enabled by PAD_MODE=cloud or PAD_CLOUD=true.
func (c *Config) IsCloud() bool {
	return c.Mode == ModeCloud
}

// Addr returns the host:port listen address.
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// BaseURL returns the base URL for the API.
// If URL is set (via config, --url flag, or PAD_URL), it takes precedence.
// Otherwise, constructs from host and port.
func (c *Config) BaseURL() string {
	if c.URL != "" {
		return strings.TrimRight(c.URL, "/")
	}
	return fmt.Sprintf("http://%s:%d", c.Host, c.Port)
}

func (c *Config) PIDFile() string {
	return filepath.Join(c.DataDir, "pad.pid")
}

func (c *Config) LogFile() string {
	return filepath.Join(c.DataDir, "logs", "server.log")
}
