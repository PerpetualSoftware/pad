package config

import (
	"crypto/rand"
	"encoding/hex"
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
	EncryptionKey       string `toml:"encryption_key"` // 32-byte hex-encoded AES-256 key for encrypting sensitive fields
	EncryptionKeySource string `toml:"-"`              // "env", "file", "generated", or "" (unset); populated by EnsureEncryptionKey

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
		cfg.EncryptionKeySource = "env"
	} else if cfg.EncryptionKey != "" {
		// Set from config.toml by toml.DecodeFile above.
		cfg.EncryptionKeySource = "config"
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

// EncryptionKeyFile returns the on-disk path where an auto-generated
// encryption key is persisted. Operator-provided keys (PAD_ENCRYPTION_KEY
// env, encryption_key config) take precedence and never cause the file
// to be read or written.
func (c *Config) EncryptionKeyFile() string {
	return filepath.Join(c.DataDir, "encryption.key")
}

// EnsureEncryptionKey makes sure the server has a usable encryption key
// without requiring the operator to set one explicitly. Resolution order:
//
//  1. If c.EncryptionKey is already set (env or config file), use it
//     verbatim. EncryptionKeySource = "env" or "config" — set by Load().
//  2. Otherwise look for <DataDir>/encryption.key. If present and
//     parseable, load it. EncryptionKeySource = "file".
//  3. Otherwise — only when the caller indicates a single-instance
//     deployment via allowGenerate=true — generate a new 32-byte
//     AES-256 key, persist it to that file with 0600 permissions, and
//     use it. EncryptionKeySource = "generated" so callers can log
//     loudly.
//
// Clustered deployments (allowGenerate=false — typically indicated by
// PAD_DB_DRIVER=postgres + multiple replicas) MUST configure
// PAD_ENCRYPTION_KEY explicitly. Otherwise each replica would persist
// its own key to local disk and cross-instance decryption of shared
// database rows would fail with GCM auth errors. We return an error in
// that case rather than silently diverging.
//
// Returns an error if key-file creation fails — we never silently fall
// back to plaintext storage of sensitive fields like TOTP seeds.
func (c *Config) EnsureEncryptionKey(allowGenerate bool) error {
	if c.EncryptionKey != "" {
		// EncryptionKeySource was set by Load(); keep whatever was stored.
		if c.EncryptionKeySource == "" {
			c.EncryptionKeySource = "config"
		}
		return nil
	}

	keyPath := c.EncryptionKeyFile()
	if data, err := os.ReadFile(keyPath); err == nil {
		c.EncryptionKey = strings.TrimSpace(string(data))
		c.EncryptionKeySource = "file"
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read encryption key: %w", err)
	}

	if !allowGenerate {
		return fmt.Errorf("PAD_ENCRYPTION_KEY is required for this deployment (shared database — auto-generation would diverge across replicas). Generate one with: openssl rand -hex 32")
	}

	// Generate a fresh 32-byte key.
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("generate encryption key: %w", err)
	}
	encoded := hex.EncodeToString(buf)

	// Make sure the data dir exists with tight permissions before dropping
	// the key file into it.
	if err := os.MkdirAll(c.DataDir, 0700); err != nil {
		return fmt.Errorf("create data dir for encryption key: %w", err)
	}

	// Atomic create via temp-file + hardlink. This handles the concurrent-
	// startup race cleanly at every step:
	//
	//   1. Write the full key to a uniquely-named temp file — each racing
	//      process has its own temp path, so no collision there, and the
	//      file is fully written + closed before we touch the final path.
	//   2. os.Link(temp, keyPath) atomically creates the final file as a
	//      hardlink to the temp. If keyPath already exists, Link fails
	//      with EEXIST and leaves the existing file untouched. This is
	//      the critical property: a loser cannot partially overwrite the
	//      winner's key, and cannot read the winner's file before it is
	//      complete (hardlink points to an already-written inode).
	//   3. On loss, ReadFile(keyPath) is safe — it points to the winner's
	//      fully-written inode.
	//   4. The temp is removed in all paths so repeated startups don't
	//      accumulate junk under DataDir.
	tmpSuffix := make([]byte, 8)
	if _, err := rand.Read(tmpSuffix); err != nil {
		return fmt.Errorf("generate temp suffix: %w", err)
	}
	tmpPath := keyPath + ".tmp." + hex.EncodeToString(tmpSuffix)
	if err := os.WriteFile(tmpPath, []byte(encoded), 0600); err != nil {
		return fmt.Errorf("write encryption key temp: %w", err)
	}
	defer os.Remove(tmpPath) // Best-effort cleanup. Harmless on the winning path (already removed via rename equivalence).

	if err := os.Link(tmpPath, keyPath); err != nil {
		if os.IsExist(err) {
			// Someone else won. Their file is fully written because it
			// too went through temp-write → link, so ReadFile is safe.
			data, rerr := os.ReadFile(keyPath)
			if rerr != nil {
				return fmt.Errorf("reload encryption key after race: %w", rerr)
			}
			c.EncryptionKey = strings.TrimSpace(string(data))
			c.EncryptionKeySource = "file"
			return nil
		}
		return fmt.Errorf("link encryption key to %s: %w", keyPath, err)
	}

	c.EncryptionKey = encoded
	c.EncryptionKeySource = "generated"
	return nil
}

func (c *Config) LogFile() string {
	return filepath.Join(c.DataDir, "logs", "server.log")
}
