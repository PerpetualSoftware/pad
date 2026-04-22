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
	if err := c.EnsureEncryptionKey(true); err != nil {
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

	if err := c.EnsureEncryptionKey(true); err != nil {
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

	if err := c.EnsureEncryptionKey(true); err != nil {
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

func TestEnsureEncryptionKey_RefusesToGenerateWhenClustered(t *testing.T) {
	// Codex P1 on PR #189: in a clustered Postgres deployment, each replica
	// would generate its own local key and cross-instance decryption of
	// shared DB rows would fail. The caller MUST set allowGenerate=false in
	// that case, and EnsureEncryptionKey must refuse to fabricate a key.
	c := newTestConfig(t)
	if err := c.EnsureEncryptionKey(false); err == nil {
		t.Fatal("clustered mode without configured key should error, got nil")
	}
	// Sanity: nothing was written.
	if _, err := os.Stat(c.EncryptionKeyFile()); !os.IsNotExist(err) {
		t.Errorf("encryption.key was written despite allowGenerate=false")
	}
}

func TestEnsureEncryptionKey_ClusteredWithPreSeededFileStillLoads(t *testing.T) {
	// If the operator pre-seeds the file (e.g. shared volume mounted into
	// every replica), EnsureEncryptionKey should load it even when
	// allowGenerate is false. Only the GENERATE step is forbidden.
	c := newTestConfig(t)
	const known = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	if err := os.WriteFile(c.EncryptionKeyFile(), []byte(known), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureEncryptionKey(false); err != nil {
		t.Fatalf("pre-seeded file in clustered mode should load: %v", err)
	}
	if c.EncryptionKey != known {
		t.Errorf("loaded key = %q, want %q", c.EncryptionKey, known)
	}
}

func TestEnsureEncryptionKey_ConcurrentStartIsRaceSafe(t *testing.T) {
	// Codex P2 on PR #189: two processes racing on first boot must NOT
	// generate divergent keys. O_EXCL creation + reload-on-EEXIST makes
	// every racing goroutine converge on the same persisted value.
	dir := t.TempDir()

	const N = 16
	results := make(chan *Config, N)
	for i := 0; i < N; i++ {
		go func() {
			c := &Config{DataDir: dir}
			if err := c.EnsureEncryptionKey(true); err != nil {
				t.Errorf("goroutine EnsureEncryptionKey: %v", err)
			}
			results <- c
		}()
	}

	var keys []string
	for i := 0; i < N; i++ {
		c := <-results
		keys = append(keys, c.EncryptionKey)
	}
	first := keys[0]
	if first == "" {
		t.Fatal("no key generated")
	}
	for _, k := range keys {
		if k != first {
			t.Fatalf("racing startups produced divergent keys:\n  %q\n  %q", first, k)
		}
	}
}

func TestEnsureEncryptionKey_IsIdempotentAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	c1 := &Config{DataDir: dir}
	if err := c1.EnsureEncryptionKey(true); err != nil {
		t.Fatalf("first EnsureEncryptionKey: %v", err)
	}
	first := c1.EncryptionKey

	// Second "run" — new Config, same DataDir, clustered mode. The file
	// exists now so the load path succeeds even with allowGenerate=false.
	c2 := &Config{DataDir: dir}
	if err := c2.EnsureEncryptionKey(false); err != nil {
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
