package config

import "testing"

// PAD_COLLAB_MAX_PER_USER reaches the config (BUG-1308), and its default is
// the measured one. The gate has its own tests in internal/server; this is the
// wiring, for the same reason TestStreamAndRedisEnvMapping exists.
func TestCollabMaxPerUserEnvAndDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CollabMaxPerUser != 50 {
		t.Errorf("default CollabMaxPerUser = %d, want 50 (BUG-1308 checkpoint 2)", cfg.CollabMaxPerUser)
	}

	t.Setenv("PAD_COLLAB_MAX_PER_USER", "7")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CollabMaxPerUser != 7 {
		t.Errorf("CollabMaxPerUser = %d, want 7 from PAD_COLLAB_MAX_PER_USER", cfg.CollabMaxPerUser)
	}
}
