package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	URL      string `toml:"url"`      // Optional: full base URL (e.g., https://api.getpad.dev). Overrides host/port for CLI.
	Editor   string `toml:"editor"`
	LogLevel string `toml:"log_level"`
	Password string `toml:"password"` // Optional: password for web UI access. Set via config or PAD_PASSWORD env var.
	DBPath   string `toml:"-"`        // computed, not from config file
	DataDir  string `toml:"-"`        // computed
}

// AuthEnabled returns true if a password is configured.
func (c *Config) AuthEnabled() bool {
	return c.Password != ""
}

func DefaultConfig() *Config {
	homeDir, _ := os.UserHomeDir()
	dataDir := filepath.Join(homeDir, ".pad")
	return &Config{
		Host:     "127.0.0.1",
		Port:     7777,
		Editor:   "",
		LogLevel: "info",
		DBPath:   filepath.Join(dataDir, "pad.db"),
		DataDir:  dataDir,
	}
}

func Load() (*Config, error) {
	cfg := DefaultConfig()

	// Ensure data directory exists
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		return nil, err
	}

	// Ensure logs directory exists
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "logs"), 0755); err != nil {
		return nil, err
	}

	// Load config file if it exists
	configPath := filepath.Join(cfg.DataDir, "config.toml")
	if _, err := os.Stat(configPath); err == nil {
		if _, err := toml.DecodeFile(configPath, cfg); err != nil {
			return nil, err
		}
	}

	// Environment variable overrides
	if v := os.Getenv("PAD_HOST"); v != "" {
		cfg.Host = v
	}
	if v := os.Getenv("PAD_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.Port = port
		}
	}
	if v := os.Getenv("PAD_URL"); v != "" {
		cfg.URL = v
	}
	if v := os.Getenv("PAD_DATA_DIR"); v != "" {
		cfg.DataDir = v
		cfg.DBPath = filepath.Join(v, "pad.db")
	}
	if v := os.Getenv("PAD_DB_PATH"); v != "" {
		cfg.DBPath = v
		cfg.DataDir = filepath.Dir(cfg.DBPath)
	}
	if v := os.Getenv("PAD_PASSWORD"); v != "" {
		cfg.Password = v
	}

	return cfg, nil
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
