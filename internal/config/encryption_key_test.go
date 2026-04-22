package config

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func newTestConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	return &Config{DataDir: dir}
}

func TestEnsureEncryptionKey_GeneratesWhenMissing(t *testing.T) {
	c := newTestConfig(t)
	if err := c.EnsureEncryptionKey(); err != nil {
		t.Fatalf("EnsureEncryptionKey: %v", err)
	}

	// Source should be "generated".
	if c.EncryptionKeySource != "generated" {
		t.Errorf("source = %q, want %q", c.EncryptionKeySource, "generated")
	}

	// Key must be a 64-char hex string decoding to 32 bytes.
	raw, err := hex.DecodeString(c.EncryptionKey)
	if err != nil {
		t.Fatalf("key is not hex: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("key decodes to %d bytes, want 32", len(raw))
	}

	// File should exist with 0600 permissions.
	info, err := os.Stat(c.EncryptionKeyFile())
	if err != nil {
		t.Fatalf("key file not written: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("key file perms = %o, want 0600", info.Mode().Perm())
	}
}

func TestEnsureEncryptionKey_LoadsFromFile(t *testing.T) {
	c := newTestConfig(t)
	// Pre-seed the file with a known key.
	const known = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(c.EncryptionKeyFile(), []byte(known+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := c.EnsureEncryptionKey(); err != nil {
		t.Fatalf("EnsureEncryptionKey: %v", err)
	}
	if c.EncryptionKey != known {
		t.Errorf("key = %q, want %q", c.EncryptionKey, known)
	}
	if c.EncryptionKeySource != "file" {
		t.Errorf("source = %q, want %q", c.EncryptionKeySource, "file")
	}
}

func TestEnsureEncryptionKey_RespectsConfigured(t *testing.T) {
	c := newTestConfig(t)
	const configured = "cafebabe" // not valid hex length but that's checked by caller
	c.EncryptionKey = configured
	c.EncryptionKeySource = "env"

	if err := c.EnsureEncryptionKey(); err != nil {
		t.Fatalf("EnsureEncryptionKey: %v", err)
	}

	if c.EncryptionKey != configured {
		t.Errorf("configured key was overwritten: got %q, want %q", c.EncryptionKey, configured)
	}
	if c.EncryptionKeySource != "env" {
		t.Errorf("source = %q, want %q (should not be clobbered)", c.EncryptionKeySource, "env")
	}

	// No file should have been written when the key was already configured.
	if _, err := os.Stat(c.EncryptionKeyFile()); !os.IsNotExist(err) {
		t.Errorf("encryption.key file written despite configured key")
	}
}

func TestEnsureEncryptionKey_IsIdempotentAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	c1 := &Config{DataDir: dir}
	if err := c1.EnsureEncryptionKey(); err != nil {
		t.Fatalf("first EnsureEncryptionKey: %v", err)
	}
	first := c1.EncryptionKey

	// Second "run" — new Config, same DataDir. Must load the same key.
	c2 := &Config{DataDir: dir}
	if err := c2.EnsureEncryptionKey(); err != nil {
		t.Fatalf("second EnsureEncryptionKey: %v", err)
	}
	if c2.EncryptionKey != first {
		t.Fatalf("key rotated across restarts: %q != %q", c2.EncryptionKey, first)
	}
	if c2.EncryptionKeySource != "file" {
		t.Errorf("second source = %q, want %q", c2.EncryptionKeySource, "file")
	}

	// Sanity: file path exists.
	if _, err := os.Stat(filepath.Join(dir, "encryption.key")); err != nil {
		t.Fatalf("encryption.key missing on second boot: %v", err)
	}
}
