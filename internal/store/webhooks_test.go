package store

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// newWebhookTestWorkspace creates a user + workspace so webhook FK constraints
// are satisfied, and returns the workspace ID.
func newWebhookTestWorkspace(t *testing.T, s *Store) string {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{
		Email:    "wh@test.com",
		Name:     "Webhook Tester",
		Password: "password123",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Hooks", Slug: "hooks", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	return ws.ID
}

// TestWebhookSecret_EncryptedAtRest is the BUG-2057 regression: the HMAC secret
// must be encrypted in the DB, round-trip to plaintext on read (so the
// dispatcher can sign), and that plaintext must produce a valid HMAC.
func TestWebhookSecret_EncryptedAtRest(t *testing.T) {
	s := testStore(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	s.SetEncryptionKey(key)

	wsID := newWebhookTestWorkspace(t, s)

	const secret = "super-secret-signing-key"
	hook, err := s.CreateWebhook(wsID, models.WebhookCreate{
		URL:    "https://example.com/hook",
		Secret: secret,
	})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}

	// Read back through the store — secret must be decrypted plaintext.
	if hook.Secret != secret {
		t.Errorf("expected decrypted secret %q, got %q", secret, hook.Secret)
	}
	if !hook.HasSecret {
		t.Error("HasSecret should be true when a secret is configured")
	}

	// Raw DB value must be encrypted, not plaintext.
	var rawSecret string
	if err := s.db.QueryRow(s.q("SELECT secret FROM webhooks WHERE id = ?"), hook.ID).Scan(&rawSecret); err != nil {
		t.Fatalf("read raw secret: %v", err)
	}
	if rawSecret == secret {
		t.Fatal("raw DB value should be encrypted, not plaintext")
	}
	if !strings.HasPrefix(rawSecret, "enc:") {
		t.Errorf("raw DB value should start with 'enc:', got %q", rawSecret)
	}

	// The decrypted secret must sign identically to the known plaintext (proves
	// the dispatcher gets a usable secret).
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("payload"))
	want := hex.EncodeToString(mac.Sum(nil))

	got := hmac.New(sha256.New, []byte(hook.Secret))
	got.Write([]byte("payload"))
	if hex.EncodeToString(got.Sum(nil)) != want {
		t.Error("decrypted secret produced a different HMAC than the original")
	}
}

// TestWebhookSecret_ListRoundTrips confirms ListWebhooks also decrypts the
// secret for internal/dispatch use.
func TestWebhookSecret_ListRoundTrips(t *testing.T) {
	s := testStore(t)
	key := make([]byte, 32)
	rand.Read(key)
	s.SetEncryptionKey(key)

	wsID := newWebhookTestWorkspace(t, s)
	const secret = "list-secret"
	if _, err := s.CreateWebhook(wsID, models.WebhookCreate{URL: "https://example.com/h", Secret: secret}); err != nil {
		t.Fatalf("create webhook: %v", err)
	}

	hooks, err := s.ListWebhooks(wsID)
	if err != nil {
		t.Fatalf("list webhooks: %v", err)
	}
	if len(hooks) != 1 {
		t.Fatalf("expected 1 webhook, got %d", len(hooks))
	}
	if hooks[0].Secret != secret {
		t.Errorf("expected decrypted secret %q, got %q", secret, hooks[0].Secret)
	}
	if !hooks[0].HasSecret {
		t.Error("HasSecret should be true")
	}
}

// TestWebhookSecret_BackfillEncryptsPlaintext covers the back-compat path: a
// pre-encryption plaintext row (inserted before a key was configured) still
// signs correctly, and BackfillEncryptWebhookSecrets encrypts it in place.
func TestWebhookSecret_BackfillEncryptsPlaintext(t *testing.T) {
	s := testStore(t)
	wsID := newWebhookTestWorkspace(t, s)

	// Insert with NO encryption key configured — stored plaintext (legacy row).
	const secret = "legacy-plaintext-secret"
	hook, err := s.CreateWebhook(wsID, models.WebhookCreate{URL: "https://example.com/legacy", Secret: secret})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	var rawSecret string
	s.db.QueryRow(s.q("SELECT secret FROM webhooks WHERE id = ?"), hook.ID).Scan(&rawSecret)
	if rawSecret != secret {
		t.Fatalf("expected plaintext storage without key, got %q", rawSecret)
	}

	// Now configure a key and run the backfill.
	key := make([]byte, 32)
	rand.Read(key)
	s.SetEncryptionKey(key)

	n, err := s.EncryptWebhookSecretsAtRest()
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row encrypted, got %d", n)
	}

	// Raw value now encrypted...
	s.db.QueryRow(s.q("SELECT secret FROM webhooks WHERE id = ?"), hook.ID).Scan(&rawSecret)
	if !strings.HasPrefix(rawSecret, "enc:") {
		t.Errorf("expected encrypted value after backfill, got %q", rawSecret)
	}
	// ...but still decrypts to the original plaintext.
	fetched, err := s.GetWebhook(hook.ID)
	if err != nil {
		t.Fatalf("get webhook: %v", err)
	}
	if fetched.Secret != secret {
		t.Errorf("expected %q after backfill decrypt, got %q", secret, fetched.Secret)
	}

	// Backfill is idempotent — a second run touches nothing.
	if n, err := s.EncryptWebhookSecretsAtRest(); err != nil || n != 0 {
		t.Errorf("expected idempotent backfill (0 rows), got n=%d err=%v", n, err)
	}
}

// insertRawWebhook writes a webhook row directly, bypassing CreateWebhook's
// encryption, to simulate a legacy pre-encryption row.
func insertRawWebhook(t *testing.T, s *Store, wsID, secret string) string {
	t.Helper()
	id := newID()
	ts := now()
	if _, err := s.db.Exec(s.q(`
		INSERT INTO webhooks (id, workspace_id, url, secret, events, active, created_at, updated_at, failure_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
	`), id, wsID, "https://example.com/x", secret, `["*"]`, s.dialect.BoolToInt(true), ts, ts); err != nil {
		t.Fatalf("insert legacy webhook: %v", err)
	}
	return id
}

// TestWebhookSecret_MigratesEncPrefixedLegacyPlaintext is the review follow-up:
// a legacy plaintext secret that literally starts with the reserved "enc:"
// marker must be encrypted by the one-time migration (first run encrypts every
// pre-encryption value) and round-trip on read.
func TestWebhookSecret_MigratesEncPrefixedLegacyPlaintext(t *testing.T) {
	s := testStore(t)
	wsID := newWebhookTestWorkspace(t, s)

	const secret = "enc:not-actually-encrypted"
	id := insertRawWebhook(t, s, wsID, secret)

	key := make([]byte, 32)
	rand.Read(key)
	s.SetEncryptionKey(key)

	// First run: flag unset → every existing secret is treated as plaintext,
	// including the "enc:"-prefixed one.
	n, err := s.EncryptWebhookSecretsAtRest()
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row encrypted, got %d", n)
	}

	var raw string
	s.db.QueryRow(s.q("SELECT secret FROM webhooks WHERE id = ?"), id).Scan(&raw)
	if raw == secret {
		t.Fatal("legacy enc:-prefixed plaintext should have been encrypted")
	}
	fetched, err := s.GetWebhook(id)
	if err != nil {
		t.Fatalf("get webhook after migrate: %v", err)
	}
	if fetched.Secret != secret {
		t.Errorf("expected %q after migrate, got %q", secret, fetched.Secret)
	}

	// Second run is steady-state (flag set): the now-genuine ciphertext is left
	// alone.
	if n, err := s.EncryptWebhookSecretsAtRest(); err != nil || n != 0 {
		t.Errorf("expected idempotent run (0 rows), got n=%d err=%v", n, err)
	}
}

// TestWebhookSecret_DoesNotRewrapCiphertextOnKeyChange guards the other horn:
// once migrated, genuine ciphertext must NOT be re-encrypted under a rotated /
// wrong key (which would corrupt the secret). Steady-state skips "enc:" rows and
// a wrong key surfaces as a loud decrypt error instead.
func TestWebhookSecret_DoesNotRewrapCiphertextOnKeyChange(t *testing.T) {
	s := testStore(t)
	wsID := newWebhookTestWorkspace(t, s)

	key1 := make([]byte, 32)
	rand.Read(key1)
	s.SetEncryptionKey(key1)

	const secret = "genuine-secret"
	hook, err := s.CreateWebhook(wsID, models.WebhookCreate{URL: "https://example.com/g", Secret: secret})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	// Run the migration so the flag is set (steady state hereafter).
	if _, err := s.EncryptWebhookSecretsAtRest(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var ciphertext string
	s.db.QueryRow(s.q("SELECT secret FROM webhooks WHERE id = ?"), hook.ID).Scan(&ciphertext)
	if !strings.HasPrefix(ciphertext, "enc:") {
		t.Fatalf("expected ciphertext, got %q", ciphertext)
	}

	// Rotate to a different key. A steady-state run must NOT touch the enc: row.
	key2 := make([]byte, 32)
	rand.Read(key2)
	s.SetEncryptionKey(key2)

	n, err := s.EncryptWebhookSecretsAtRest()
	if err != nil {
		t.Fatalf("run after key change: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 rows touched after key change, got %d", n)
	}
	var after string
	s.db.QueryRow(s.q("SELECT secret FROM webhooks WHERE id = ?"), hook.ID).Scan(&after)
	if after != ciphertext {
		t.Fatal("ciphertext must not be re-wrapped under a rotated key")
	}
	// And the wrong key fails loud rather than returning corrupt data.
	if _, err := s.GetWebhook(hook.ID); err == nil {
		t.Error("expected a decrypt error under the wrong key, got nil")
	}
}
