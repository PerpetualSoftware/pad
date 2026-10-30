package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4137: oauth-login can bind its 2FA challenge to a PKCE S256 challenge
// instead of the caller's address.

const (
	t4137Secret   = "JBSWY3DPEHPK3PXP"
	t4137Verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk" // RFC 7636 appendix B
	t4137Chall    = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	t4137IPA      = "192.0.2.10:1111"
	t4137IPB      = "198.51.100.20:2222"
)

type t4137Fixture struct {
	srv  *Server
	user *models.User
}

// newT4137Fixture signs a Google user in once (creating it), then turns 2FA on.
func newT4137Fixture(t *testing.T) t4137Fixture {
	t.Helper()
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	rr := t4137OAuth(t, srv, t4137IPA, map[string]interface{}{})
	if rr.Code != http.StatusOK {
		t.Fatalf("create user: %d %s", rr.Code, rr.Body.String())
	}
	u, err := srv.store.GetUserByEmail("pkce@example.com")
	if err != nil || u == nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if err := srv.store.SetTOTPSecret(u.ID, t4137Secret); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.EnableTOTP(u.ID, t4137Secret, strings.Join(hashRecoveryCodes([]string{"AAAAAAAAAA"}), "\n")); err != nil {
		t.Fatal(err)
	}
	u, _ = srv.store.GetUser(u.ID)
	return t4137Fixture{srv, u}
}

func t4137OAuth(t *testing.T, srv *Server, remote string, extra map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]interface{}{
		"provider": "google", "email": "pkce@example.com", "email_verified": true,
		"subject": "g-pkce", "cloud_secret": oauthProviderTestSecret,
	}
	for k, v := range extra {
		body[k] = v
	}
	req := cloudAdminReq(t, "POST", "/api/v1/auth/oauth-login", body,
		map[string]string{"X-Cloud-Secret": oauthProviderTestSecret})
	req.RemoteAddr = remote
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// challengeFor runs oauth-login from remote and returns the 2FA challenge.
func (f t4137Fixture) challengeFor(t *testing.T, remote string, extra map[string]interface{}) string {
	t.Helper()
	rr := t4137OAuth(t, f.srv, remote, extra)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("oauth-login: %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Challenge string `json:"challenge_token"`
			} `json:"details"`
		} `json:"error"`
	}
	parseJSON(t, rr, &body)
	if body.Error.Code != "two_factor_required" || body.Error.Details.Challenge == "" {
		t.Fatalf("no challenge: %s", rr.Body.String())
	}
	return body.Error.Details.Challenge
}

func (f t4137Fixture) verify(remote string, body map[string]interface{}) *httptest.ResponseRecorder {
	return doRequestFromRemoteAddr(f.srv, "POST", "/api/v1/auth/2fa/login-verify", body, remote)
}

func t4137NoSession(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(rr.Body.String(), `"token"`) || len(rr.Result().Cookies()) > 0 {
		t.Fatalf("a session was minted: %s", rr.Body.String())
	}
}

func TestTASK4137_PKCEBound_VerifiesFromOtherIP(t *testing.T) {
	f := newT4137Fixture(t)
	ch := f.challengeFor(t, t4137IPA, map[string]interface{}{"two_factor_pkce_challenge": t4137Chall})
	rr := f.verify(t4137IPB, map[string]interface{}{
		"challenge_token": ch, "code": validTOTPCode(t, t4137Secret), "code_verifier": t4137Verifier,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("verify from another IP: %d %s", rr.Code, rr.Body.String())
	}
}

func TestTASK4137_PKCEBound_WrongOrMissingVerifier_401(t *testing.T) {
	f := newT4137Fixture(t)
	ch := f.challengeFor(t, t4137IPA, map[string]interface{}{"two_factor_pkce_challenge": t4137Chall})
	code := validTOTPCode(t, t4137Secret)
	for name, v := range map[string]interface{}{
		"missing":   nil,
		"wrong":     strings.Repeat("a", 43),
		"truncated": t4137Verifier[:42] + "x",
	} {
		body := map[string]interface{}{"challenge_token": ch, "code": code}
		if v != nil {
			body["code_verifier"] = v
		}
		// from the minting IP too: the address does not substitute for the verifier
		rr := f.verify(t4137IPA, body)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s verifier: %d %s", name, rr.Code, rr.Body.String())
		}
		t4137NoSession(t, rr)
	}
}

func TestTASK4137_IPBound_WithVerifier_401(t *testing.T) {
	f := newT4137Fixture(t)
	ch := f.challengeFor(t, t4137IPA, nil)
	code := validTOTPCode(t, t4137Secret)
	// a verifier on an IP-bound token is refused, even from the minting IP
	for _, remote := range []string{t4137IPA, t4137IPB} {
		rr := f.verify(remote, map[string]interface{}{"challenge_token": ch, "code": code, "code_verifier": t4137Verifier})
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("IP-bound + verifier from %s: %d %s", remote, rr.Code, rr.Body.String())
		}
		t4137NoSession(t, rr)
	}
}

// A pkce failure and an IP mismatch are indistinguishable on the wire.
func TestTASK4137_PKCEFailureAndIPMismatch_IdenticalResponses(t *testing.T) {
	f := newT4137Fixture(t)
	pk := f.challengeFor(t, t4137IPA, map[string]interface{}{"two_factor_pkce_challenge": t4137Chall})
	ip := f.challengeFor(t, t4137IPA, nil)
	code := validTOTPCode(t, t4137Secret)
	cases := map[string]*httptest.ResponseRecorder{
		"pkce wrong verifier":   f.verify(t4137IPA, map[string]interface{}{"challenge_token": pk, "code": code, "code_verifier": strings.Repeat("b", 43)}),
		"pkce missing verifier": f.verify(t4137IPA, map[string]interface{}{"challenge_token": pk, "code": code}),
		"ip mismatch":           f.verify(t4137IPB, map[string]interface{}{"challenge_token": ip, "code": code}),
		"ip + verifier":         f.verify(t4137IPA, map[string]interface{}{"challenge_token": ip, "code": code, "code_verifier": t4137Verifier}),
	}
	dump := func(rr *httptest.ResponseRecorder) string {
		h := rr.Result().Header.Clone()
		h.Del("Date")
		return fmt.Sprintf("%d\n%v\n%s", rr.Code, h, rr.Body.String())
	}
	want := dump(cases["ip mismatch"])
	if !strings.HasPrefix(want, "401\n") {
		t.Fatalf("baseline is not a 401: %s", want)
	}
	for name, rr := range cases {
		if got := dump(rr); got != want {
			t.Errorf("%s differs from the IP mismatch:\n%s\n--- vs ---\n%s", name, got, want)
		}
	}
}

func TestTASK4137_BadPKCEChallenge_400(t *testing.T) {
	f := newT4137Fixture(t)
	for name, c := range map[string]string{
		"42 chars": t4137Chall[:42], "44 chars": t4137Chall + "A", "bad char": t4137Chall[:42] + "+",
		"padded": t4137Chall[:42] + "=", "empty": "",
	} {
		rr := t4137OAuth(t, f.srv, t4137IPA, map[string]interface{}{"two_factor_pkce_challenge": c})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
	}
}

// A malformed challenge is refused before any account lookup: an unknown and
// a known address (and one with no 2FA) get byte-identical answers.
func TestTASK4137_BadPKCEChallenge_NoAccountOracle(t *testing.T) {
	f := newT4137Fixture(t)
	var first string
	for _, email := range []string{"pkce@example.com", "nobody@example.com"} {
		rr := t4137OAuth(t, f.srv, t4137IPA, map[string]interface{}{"email": email, "two_factor_pkce_challenge": "short"})
		got := fmt.Sprintf("%d %s", rr.Code, rr.Body.String())
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: %s", email, got)
		}
		if first == "" {
			first = got
		} else if got != first {
			t.Fatalf("%s differs:\n%s\n%s", email, got, first)
		}
	}
}

func TestTASK4137_BadCodeVerifier_400(t *testing.T) {
	f := newT4137Fixture(t)
	ch := f.challengeFor(t, t4137IPA, map[string]interface{}{"two_factor_pkce_challenge": t4137Chall})
	for name, v := range map[string]string{
		"short": strings.Repeat("a", 42), "long": strings.Repeat("a", 129), "reserved": strings.Repeat("a", 42) + "+", "empty": "",
	} {
		rr := f.verify(t4137IPA, map[string]interface{}{"challenge_token": ch, "code": validTOTPCode(t, t4137Secret), "code_verifier": v})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
	}
}

func TestTASK4137_PKCEBound_SeventhCode_429(t *testing.T) {
	f := newT4137Fixture(t)
	ch := f.challengeFor(t, t4137IPA, map[string]interface{}{"two_factor_pkce_challenge": t4137Chall})
	for i := 1; i <= 7; i++ {
		// a fresh address per try: the per-IP login bucket (5/min) is not what is under test
		rr := f.verify(fmt.Sprintf("203.0.113.%d:1", i), map[string]interface{}{"challenge_token": ch, "code": "000000", "code_verifier": t4137Verifier})
		want := http.StatusUnauthorized
		if i == 7 {
			want = http.StatusTooManyRequests
		}
		if rr.Code != want {
			t.Fatalf("try %d: %d %s", i, rr.Code, rr.Body.String())
		}
	}
	if rr := f.verify("203.0.113.99:1", map[string]interface{}{"challenge_token": ch, "code": validTOTPCode(t, t4137Secret), "code_verifier": t4137Verifier}); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("correct code after lockout: %d", rr.Code)
	}
}

func TestTASK4137_PKCEBound_CodeReplay_401(t *testing.T) {
	f := newT4137Fixture(t)
	code := validTOTPCode(t, t4137Secret)
	for i, want := range []int{http.StatusOK, http.StatusUnauthorized} {
		ch := f.challengeFor(t, t4137IPA, map[string]interface{}{"two_factor_pkce_challenge": t4137Chall})
		rr := f.verify(t4137IPB, map[string]interface{}{"challenge_token": ch, "code": code, "code_verifier": t4137Verifier})
		if rr.Code != want {
			t.Fatalf("use %d: %d %s", i+1, rr.Code, rr.Body.String())
		}
	}
}

func TestTASK4137_PKCEBound_EpochFence(t *testing.T) {
	f := newT4137Fixture(t)
	ch := f.challengeFor(t, t4137IPA, map[string]interface{}{"two_factor_pkce_challenge": t4137Chall})
	pw := "a-new-password-123"
	if _, err := f.srv.store.UpdateUser(f.user.ID, models.UserUpdate{Password: &pw}); err != nil {
		t.Fatal(err)
	}
	rr := f.verify(t4137IPB, map[string]interface{}{"challenge_token": ch, "code": validTOTPCode(t, t4137Secret), "code_verifier": t4137Verifier})
	if rr.Code == http.StatusOK {
		t.Fatalf("a stale-epoch challenge minted a session: %s", rr.Body.String())
	}
	t4137NoSession(t, rr)
}

// t4137Legacy mints the pre-TASK-4137 format by hand (a BARE address).
func t4137Legacy(userID, binding string, epoch int64, secret []byte) string {
	payload := fmt.Sprintf("%s|%s|%d|%d", userID, binding, time.Now().Add(time.Minute).Unix(), epoch)
	enc := base64.RawURLEncoding.EncodeToString([]byte(payload))
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(enc))
	return enc + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestTASK4137_LegacyBareIPBinding(t *testing.T) {
	f := newT4137Fixture(t)
	sec := f.srv.twoFAChallengeSecret
	code := validTOTPCode(t, t4137Secret)
	// not an address, and not a recognised kind
	for i, b := range []string{"garbage", "xyz:192.0.2.10", "192.0.2.10 ", ""} {
		rr := f.verify(fmt.Sprintf("203.0.113.%d:1", i+1), map[string]interface{}{"challenge_token": t4137Legacy(f.user.ID, b, f.user.CredentialEpoch, sec), "code": code})
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("binding %q: %d %s", b, rr.Code, rr.Body.String())
		}
	}
	// a legacy bare address from another IP, or with a verifier: refused
	legacy := t4137Legacy(f.user.ID, "192.0.2.10", f.user.CredentialEpoch, sec)
	if rr := f.verify(t4137IPB, map[string]interface{}{"challenge_token": legacy, "code": code}); rr.Code != http.StatusUnauthorized {
		t.Errorf("legacy from another IP: %d", rr.Code)
	}
	if rr := f.verify(t4137IPA, map[string]interface{}{"challenge_token": legacy, "code": code, "code_verifier": t4137Verifier}); rr.Code != http.StatusUnauthorized {
		t.Errorf("legacy with a verifier: %d", rr.Code)
	}
	// from the minting IP it survives the deploy
	if rr := f.verify(t4137IPA, map[string]interface{}{"challenge_token": legacy, "code": code}); rr.Code != http.StatusOK {
		t.Errorf("legacy from its own IP: %d %s", rr.Code, rr.Body.String())
	}
}
