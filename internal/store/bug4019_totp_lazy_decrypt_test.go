package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4019: a user read no longer decrypts the TOTP secret. A secret that will
// not decrypt (an operator key change) must fail the second-factor check alone,
// not every read of the user row.

// undecryptableTOTPUser stores a TOTP secret under key A, then reconfigures the
// store with key B, so the stored ciphertext no longer decrypts.
func undecryptableTOTPUser(t *testing.T, s *Store, username string) *models.User {
	t.Helper()
	keyA := make([]byte, 32)
	keyB := make([]byte, 32)
	if _, err := rand.Read(keyA); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(keyB); err != nil {
		t.Fatal(err)
	}
	s.SetEncryptionKey(keyA)
	u := mentionUser(t, s, username)
	if err := s.SetTOTPSecret(u.ID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatalf("SetTOTPSecret: %v", err)
	}
	if err := s.EnableTOTP(u.ID, "JBSWY3DPEHPK3PXP", "recovery-placeholder"); err != nil {
		t.Fatalf("EnableTOTP: %v", err)
	}
	s.SetEncryptionKey(keyB)
	if _, err := s.TOTPSecretForVerify(u.ID); err == nil || errors.Is(err, ErrNoTOTPSecret) {
		t.Fatalf("precondition: the secret must fail to decrypt, got %v", err)
	}
	return u
}

func TestBUG4019_ReadsOfAnUndecryptableUserSucceed(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Lazy TOTP")
	author := mentionUser(t, s, "author4019")
	bad := undecryptableTOTPUser(t, s, "badsecret4019")
	for _, u := range []*models.User{author, bad} {
		if err := s.AddWorkspaceMember(ws.ID, u.ID, "editor"); err != nil {
			t.Fatalf("AddWorkspaceMember: %v", err)
		}
	}
	col := createTestCollection(t, s, ws.ID, "Threads")
	item := createTestItem(t, s, ws.ID, col.ID, "Thread", "")

	// Create commits, and so does an edit that adds the mention afresh.
	c, err := s.CreateComment(ws.ID, item.ID, author.ID, models.CommentCreate{Body: "ping @badsecret4019"})
	if err != nil {
		t.Fatalf("a comment mentioning an undecryptable user must commit: %v", err)
	}
	if _, err := s.UpdateComment(c.ID, "plain"); err != nil {
		t.Fatalf("edit away the mention: %v", err)
	}
	if _, err := s.UpdateComment(c.ID, "again @badsecret4019"); err != nil {
		t.Fatalf("an edit mentioning an undecryptable user must commit: %v", err)
	}
	wantMentions(t, s, c.ID, "mention index", bad)

	members, err := s.ListWorkspaceMembers(ws.ID)
	if err != nil || len(members) < 2 {
		t.Fatalf("members list: %d, %v", len(members), err)
	}
	if _, _, err := s.ResolveBacklinksVisibility(bad.ID, ws.ID, false, true); err != nil {
		t.Fatalf("backlinks visibility for the user: %v", err)
	}
	if _, err := s.GetBacklinks(item.ID, ws.ID, 10, 0, BacklinksVisibility{Unrestricted: true}); err != nil {
		t.Fatalf("backlinks read: %v", err)
	}
	// The Stripe lookup must MATCH the undecryptable row, or it never reads it
	// (BUG-4019 r1: a lookup of an unknown customer passed at the base too).
	if err := s.SetUserStripeCustomerID(bad.ID, "cus_bug4019"); err != nil {
		t.Fatalf("SetUserStripeCustomerID: %v", err)
	}
	for name, get := range map[string]func() (*models.User, error){
		"GetUser":         func() (*models.User, error) { return s.GetUser(bad.ID) },
		"GetUserByEmail":  func() (*models.User, error) { return s.GetUserByEmail(bad.Email) },
		"GetUserByName":   func() (*models.User, error) { return s.GetUserByUsername(bad.Username) },
		"ValidatePasswd":  func() (*models.User, error) { return s.ValidatePassword(bad.Email, "password-1234") },
		"GetUserByStripe": func() (*models.User, error) { return s.GetUserByStripeCustomerID("cus_bug4019") },
	} {
		u, err := get()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if u == nil || u.ID != bad.ID || !u.TOTPEnabled {
			t.Fatalf("%s: user %v, want the enabled row", name, u)
		}
	}

	// The list reads were best-effort before, so they never failed; this pins
	// that they still return the row, with its 2FA flag, rather than drop it.
	all, err := s.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if !bug4019HasEnabled(all, bad.ID) {
		t.Fatalf("ListUsers: the undecryptable user is missing or not 2FA-enabled")
	}
	found, err := s.SearchUsers(AdminUserSearchParams{Query: bad.Email})
	if err != nil {
		t.Fatalf("SearchUsers: %v", err)
	}
	var searched []models.User
	for _, e := range found.Users {
		searched = append(searched, e.User)
	}
	if !bug4019HasEnabled(searched, bad.ID) {
		t.Fatalf("SearchUsers: the undecryptable user is missing or not 2FA-enabled")
	}
}

func bug4019HasEnabled(users []models.User, id string) bool {
	for _, u := range users {
		if u.ID == id {
			return u.TOTPEnabled
		}
	}
	return false
}

func TestBUG4019_TOTPSecretForVerify(t *testing.T) {
	s := testStore(t)
	u := mentionUser(t, s, "verify4019")
	if _, err := s.TOTPSecretForVerify(u.ID); !errors.Is(err, ErrNoTOTPSecret) {
		t.Fatalf("2FA off: %v, want ErrNoTOTPSecret", err)
	}
	if _, err := s.TOTPSecretForVerify("no-such-user"); !errors.Is(err, ErrNoTOTPSecret) {
		t.Fatalf("no row: %v, want ErrNoTOTPSecret", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	s.SetEncryptionKey(key)
	if err := s.SetTOTPSecret(u.ID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TOTPSecretForVerify(u.ID); !errors.Is(err, ErrNoTOTPSecret) {
		t.Fatalf("secret set but 2FA not enabled: %v, want ErrNoTOTPSecret", err)
	}
	if err := s.EnableTOTP(u.ID, "JBSWY3DPEHPK3PXP", "x"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.TOTPSecretForVerify(u.ID); err != nil || got != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("enabled: %q, %v", got, err)
	}
	// An enabled row holding '' is no secret, never an empty one to verify against.
	if _, err := s.db.Exec(s.q(`UPDATE users SET totp_secret = '' WHERE id = ?`), u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TOTPSecretForVerify(u.ID); !errors.Is(err, ErrNoTOTPSecret) {
		t.Fatalf("empty stored secret: %v, want ErrNoTOTPSecret", err)
	}

	// A stored value that is non-empty but DECRYPTS to "" passes the stored==""
	// check and must be stopped by the decrypted=="" one. encrypt never emits
	// such a value, so it is sealed here by hand (BUG-4019, pre-GO mutant).
	sealed := bug4019SealEmpty(t, key)
	if got, err := s.decrypt(sealed); err != nil || got != "" {
		t.Fatalf("precondition: the sealed value must decrypt to \"\": %q, %v", got, err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE users SET totp_secret = ? WHERE id = ?`), sealed, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TOTPSecretForVerify(u.ID); !errors.Is(err, ErrNoTOTPSecret) {
		t.Fatalf("stored secret that decrypts to empty: %v, want ErrNoTOTPSecret", err)
	}
}

// bug4019SealEmpty seals "" with AES-256-GCM under key in the store's
// enc:base64(nonce||ciphertext) format, independently of Store.encrypt, which
// returns "" for an empty plaintext rather than sealing it.
func bug4019SealEmpty(t *testing.T, key []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	return "enc:" + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, nil, nil))
}
