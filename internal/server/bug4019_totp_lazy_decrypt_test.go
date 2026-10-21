package server

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
	"github.com/pquerna/otp/totp"
)

// BUG-4019: a TOTP secret that will not decrypt (an operator key change) fails
// only the 2FA paths (the second-factor check, and enabling over it), never an
// ordinary read of the user; TOTPSecretForVerify is the only reader that
// returns the plaintext. These tests pin each door that reads a secret, on
// SQLite and, under make test-pg, on Postgres.

const bug4019Secret = "JBSWY3DPEHPK3PXP"

// bug4019Server is testServer on whichever backend the run selected.
func bug4019Server(t *testing.T) *Server {
	t.Helper()
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		return testServer(t)
	}
	srv := New(storetest.NewPostgres(t))
	t.Cleanup(srv.Stop)
	return srv
}

func bug4019Key(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func bug4019Placeholder() string {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		return "$1"
	}
	return "?"
}

// bug4019Placeholder2 is the second bind parameter, for two-parameter statements.
func bug4019Placeholder2() string {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		return "$2"
	}
	return "?"
}

func bug4019Sessions(t *testing.T, srv *Server, userID string) int {
	t.Helper()
	var n int
	if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = `+bug4019Placeholder(), userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func bug4019Enabled(t *testing.T, srv *Server, userID string) bool {
	t.Helper()
	u, err := srv.store.GetUser(userID)
	if err != nil || u == nil {
		t.Fatalf("GetUser: %v", err)
	}
	return u.TOTPEnabled
}

// bug4019Corrupt creates a password user with 2FA on, under key A, then
// reconfigures the store with key B so the stored secret no longer decrypts.
// It returns the user, the ciphertext as stored, and the plaintext recovery
// codes.
func bug4019Corrupt(t *testing.T, srv *Server, email string) (*models.User, string, []string) {
	t.Helper()
	srv.store.SetEncryptionKey(bug4019Key(t))
	u, err := srv.store.CreateUser(models.UserCreate{Email: email, Name: "Four", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	codes := []string{"AAAAAAAAAA", "BBBBBBBBBB"}
	if err := srv.store.SetTOTPSecret(u.ID, bug4019Secret); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.EnableTOTP(u.ID, bug4019Secret, strings.Join(hashRecoveryCodes(codes), "\n")); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := srv.store.DB().QueryRow(`SELECT totp_secret FROM users WHERE id = `+bug4019Placeholder(), u.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "" || stored == bug4019Secret {
		t.Fatalf("precondition: the stored secret must be ciphertext, got %q", stored)
	}
	srv.store.SetEncryptionKey(bug4019Key(t))
	return u, stored, codes
}

func bug4019Login(srv *Server, userID string, body map[string]any) (rrCode int, resp string, token string, cookie bool) {
	body["challenge_token"] = generateTwoFAChallengeAt(userID, "192.0.2.1", -1, srv.twoFAChallengeSecret)
	rr := doRequest(srv, "POST", "/api/v1/auth/2fa/login-verify", body)
	var parsed struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &parsed)
	return rr.Code, rr.Body.String(), parsed.Token, len(rr.Result().Cookies()) > 0
}

type bug4019LogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *bug4019LogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *bug4019LogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// T2: a TOTP code on an undecryptable secret mints no session, answers 500,
// logs exactly one record naming the user, and the log carries no secret.
func TestBUG4019_LoginCodeOnAnUndecryptableSecretFailsClosedAndLogsOnce(t *testing.T) {
	srv := bug4019Server(t)
	u, ciphertext, _ := bug4019Corrupt(t, srv, "t2-4019@example.com")

	logs := &bug4019LogBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	code, body, token, cookie := bug4019Login(srv, u.ID, map[string]any{"code": validTOTPCode(t, bug4019Secret)})
	if code != http.StatusInternalServerError || token != "" || cookie {
		t.Fatalf("login verify: %d token=%q cookie=%v body=%s, want 500 and no session", code, token, cookie, body)
	}
	if n := bug4019Sessions(t, srv, u.ID); n != 0 {
		t.Fatalf("%d sessions minted for the user", n)
	}
	// Every ERROR record counts, not only ours: the failure is logged once,
	// not once here and again by a generic 500 writer (BUG-4019 r1). The one
	// exclusion is the request logger's per-request line, which every 500 gets
	// and which carries no error (the secret-material check below still reads
	// the whole log).
	var records []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=ERROR") && !strings.Contains(line, `msg="http request"`) {
			records = append(records, line)
		}
	}
	if len(records) != 1 || !strings.Contains(records[0], "load totp secret for second factor") {
		t.Fatalf("%d slog error records, want exactly 1:\n%s", len(records), logs.String())
	}
	if !strings.Contains(records[0], u.ID) {
		t.Fatalf("the record does not carry the user id: %s", records[0])
	}
	for _, secret := range []string{ciphertext, strings.TrimPrefix(ciphertext, "enc:"), bug4019Secret} {
		if strings.Contains(logs.String(), secret) || strings.Contains(body, secret) {
			t.Fatalf("secret material %q leaked into the log or response", secret)
		}
	}
}

// T3: enabling 2FA over an undecryptable stored secret is a 409 and 2FA stays off.
func TestBUG4019_EnableOnAnUndecryptableStoredSecretRefuses(t *testing.T) {
	srv := bug4019Server(t)
	srv.store.SetEncryptionKey(bug4019Key(t))
	u, err := srv.store.CreateUser(models.UserCreate{Email: "t3-4019@example.com", Name: "Three", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetTOTPSecret(u.ID, bug4019Secret); err != nil {
		t.Fatal(err)
	}
	srv.store.SetEncryptionKey(bug4019Key(t))
	tok, err := srv.store.CreateSession(u.ID, "test", "192.0.2.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/2fa/verify", map[string]any{"code": validTOTPCode(t, bug4019Secret), "secret": bug4019Secret}, tok)
	if rr.Code != http.StatusConflict {
		t.Fatalf("enable: %d %s, want 409", rr.Code, rr.Body.String())
	}
	if bug4019Enabled(t, srv, u.ID) {
		t.Fatal("2FA was enabled over a secret that will not decrypt")
	}
}

// T4: the no-password disable door and the account-delete door refuse a TOTP
// code on an undecryptable secret; 2FA stays on and the account stays.
func TestBUG4019_DisableAndDeleteDoorsRefuseOnAnUndecryptableSecret(t *testing.T) {
	t.Run("disable", func(t *testing.T) {
		srv := bug4019Server(t)
		srv.store.SetEncryptionKey(bug4019Key(t))
		userID, secret, tok := passwordlessTOTPUser(t, srv, "t4a-4019@example.com")
		srv.store.SetEncryptionKey(bug4019Key(t))
		rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/2fa/disable", map[string]any{"code": validTOTPCode(t, secret)}, tok)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("disable: %d %s, want 500", rr.Code, rr.Body.String())
		}
		if !bug4019Enabled(t, srv, userID) {
			t.Fatal("2FA was disabled on a code that could not be checked")
		}
	})
	t.Run("delete", func(t *testing.T) {
		srv := bug4019Server(t)
		srv.SetCloudSidecar(&fakeSidecar{})
		srv.store.SetEncryptionKey(bug4019Key(t))
		userID, tok := bootstrapAccountDeleteUser(t, srv, "")
		secret := enableTOTPForDeleteUser(t, srv, userID)
		srv.store.SetEncryptionKey(bug4019Key(t))
		rr := deleteAccountReq(srv, map[string]interface{}{
			"password":  "correct-horse-battery-staple",
			"totp_code": validTOTPCode(t, secret),
		}, tok)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("delete: %d %s, want 500", rr.Code, rr.Body.String())
		}
		u, err := srv.store.GetUser(userID)
		if err != nil || u == nil {
			t.Fatalf("the account is gone (%v, %v)", u, err)
		}
		if !u.TOTPEnabled {
			t.Fatal("2FA was turned off")
		}
	})
}

// T5: an enabled row whose stored secret is "" refuses a code computed from "",
// and logs one warning naming the user (BUG-4019 r2).
func TestBUG4019_EmptyStoredSecretRefusesACodeComputedFromEmpty(t *testing.T) {
	srv := bug4019Server(t)
	u, err := srv.store.CreateUser(models.UserCreate{Email: "t5-4019@example.com", Name: "Five", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetTOTPSecret(u.ID, bug4019Secret); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.EnableTOTP(u.ID, bug4019Secret, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.DB().Exec(`UPDATE users SET totp_secret = '' WHERE id = `+bug4019Placeholder(), u.ID); err != nil {
		t.Fatal(err)
	}
	emptyCode, err := totp.GenerateCode("", time.Now())
	if err != nil {
		t.Fatalf("precondition: a code is computable from an empty secret: %v", err)
	}
	logs := &bug4019LogBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	code, body, token, _ := bug4019Login(srv, u.ID, map[string]any{"code": emptyCode})
	if code != http.StatusUnauthorized || token != "" {
		t.Fatalf("login with a code computed from \"\": %d %s, want 401 and no token", code, body)
	}
	if n := bug4019Sessions(t, srv, u.ID); n != 0 {
		t.Fatalf("%d sessions minted", n)
	}
	var warns []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "no totp secret to verify a code against") {
			warns = append(warns, line)
		}
	}
	if len(warns) != 1 || !strings.Contains(warns[0], u.ID) {
		t.Fatalf("%d warnings naming the user, want exactly 1:\n%s", len(warns), logs.String())
	}
}

// T5b: an enabled row whose stored value is non-empty but DECRYPTS to "" (a
// hand-sealed empty plaintext; encrypt never emits one) also refuses a code
// computed from "". Without the decrypted-empty guard, "" would be verified
// against and ConsumeTOTPStep would match it (BUG-4019, pre-GO mutant).
func TestBUG4019_SealedEmptySecretRefusesACodeComputedFromEmpty(t *testing.T) {
	srv := bug4019Server(t)
	key := bug4019Key(t)
	srv.store.SetEncryptionKey(key)
	u, err := srv.store.CreateUser(models.UserCreate{Email: "t5b-4019@example.com", Name: "FiveB", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetTOTPSecret(u.ID, bug4019Secret); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.EnableTOTP(u.ID, bug4019Secret, "x"); err != nil {
		t.Fatal(err)
	}
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
	sealed := "enc:" + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, nil, nil))
	if _, err := srv.store.DB().Exec(`UPDATE users SET totp_secret = `+bug4019Placeholder()+` WHERE id = `+bug4019Placeholder2(), sealed, u.ID); err != nil {
		t.Fatal(err)
	}
	emptyCode, err := totp.GenerateCode("", time.Now())
	if err != nil {
		t.Fatalf("precondition: a code is computable from an empty secret: %v", err)
	}
	code, body, token, _ := bug4019Login(srv, u.ID, map[string]any{"code": emptyCode})
	if code != http.StatusUnauthorized || token != "" {
		t.Fatalf("login with a code computed from \"\" over a sealed empty secret: %d %s, want 401 and no token", code, body)
	}
	if n := bug4019Sessions(t, srv, u.ID); n != 0 {
		t.Fatalf("%d sessions minted", n)
	}
}

// T6: a recovery code still signs in over an undecryptable secret and is
// consumed; the user can then replace the secret and sign in with its codes.
func TestBUG4019_RecoveryCodeRecoversAnUndecryptableSecret(t *testing.T) {
	srv := bug4019Server(t)
	u, _, codes := bug4019Corrupt(t, srv, "t6-4019@example.com")

	status, body, token, _ := bug4019Login(srv, u.ID, map[string]any{"recovery_code": codes[0]})
	if status != http.StatusOK || token == "" {
		t.Fatalf("recovery login: %d %s, want 200 with a token", status, body)
	}
	status, body, tok2, _ := bug4019Login(srv, u.ID, map[string]any{"recovery_code": codes[0]})
	if status != http.StatusUnauthorized || tok2 != "" {
		t.Fatalf("reused recovery code: %d %s, want 401", status, body)
	}

	// Setup refuses while 2FA is on, so the way out is the password disable door,
	// which never reads the secret.
	rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/2fa/disable", map[string]any{"password": "pw-test-12345"}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("disable with password: %d %s", rr.Code, rr.Body.String())
	}
	// The disable rotates the session; the response carries the live token.
	var dis struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &dis)
	if dis.Token != "" {
		token = dis.Token
	}
	rr = doRequestWithCookie(srv, "POST", "/api/v1/auth/2fa/setup", map[string]any{}, token)
	var setup struct {
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &setup)
	if rr.Code != http.StatusOK || setup.Secret == "" {
		t.Fatalf("setup: %d %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(srv, "POST", "/api/v1/auth/2fa/verify", map[string]any{"code": validTOTPCode(t, setup.Secret), "secret": setup.Secret}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("enable with the new secret: %d %s", rr.Code, rr.Body.String())
	}
	status, body, token, _ = bug4019Login(srv, u.ID, map[string]any{"code": validTOTPCode(t, setup.Secret)})
	if status != http.StatusOK || token == "" {
		t.Fatalf("login with the new secret's code: %d %s", status, body)
	}
}
