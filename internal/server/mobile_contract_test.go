package server

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/go-chi/chi/v5"
)

// TASK-2053. The native shells in ~/Dev/pad-mobile talk to this server over a
// handful of UNVERSIONED string contracts — route paths, cookie names, JSON
// keys, header shapes. None of them is behind the MCP tool-surface version or
// any other compatibility gate, and the min-server-version warning the app
// shows covers the opposite direction: it protects a NEW app against an OLD
// server, and says nothing when the server moves forward under a shipped app.
//
// A shipped mobile build cannot be patched on our schedule. Renaming a key here
// is a silent break for every installed copy until its owner updates, so these
// assertions exist to make that break LOUD — at CI time, in this repo, before
// it reaches a phone.
//
// HOW TO USE A FAILURE HERE. This file is not a correctness test and a red line
// does not mean the server is wrong. It means a contract a shipped client
// depends on has moved. The change may well be right; what it needs is the
// other half — a pad-mobile change, and a deliberate decision about clients
// already in the field. Update the constant here in the SAME unit that makes
// the server change, and say on the item's trail which shell was checked.
//
// One file, one table, per contract: the list is meant to be read as an
// inventory, and a new mobile-facing contract should be added here when it is
// added to the server.
//
// SCOPE, stated because a reader should not have to re-derive where the search
// stopped. Covered here: everything mobile depends on that this repo SERVES.
// Deliberately absent, each looked for rather than assumed:
//
//   - `/auth/apple/native` is a pad-cloud route (its main.go registers it);
//     that half of TASK-2053 belongs to that repo's suite.
//   - The app-scheme redirect allowlist does not exist in this repo — greps for
//     the scheme forms and for `app_scheme`/`appScheme` return nothing in Go,
//     TypeScript or Svelte. It lives with the native OAuth callback, in
//     pad-cloud.
//   - The OAuth error codes this repo emits (`invalid_client_metadata` and
//     friends, handlers_oauth.go) belong to Dynamic Client Registration for MCP
//     clients, not to the mobile sign-in flow. Pinning them here would look
//     like mobile coverage while protecting a different client entirely.

// mobileContractCookieNames pins both spellings of the session cookie.
//
// The shells read the cookie by name after a web-view login, so the value is a
// literal in their source. `__Host-pad_session` is the one that matters in
// practice — every deployment a phone talks to is TLS — and it appeared in NO
// Go test assertion before this file: only in a config-test comment. The
// insecure spelling is mentioned in nineteen test files, but as a helper
// building a request rather than as a claim about the name.
func TestMobileContract_SessionCookieNames(t *testing.T) {
	if got := sessionCookieName(false); got != "pad_session" {
		t.Errorf("sessionCookieName(false) = %q, want %q — pad-mobile reads this name literally", got, "pad_session")
	}
	if got := sessionCookieName(true); got != "__Host-pad_session" {
		t.Errorf("sessionCookieName(true) = %q, want %q — this is the spelling every TLS deployment (and so every phone) sees",
			got, "__Host-pad_session")
	}
}

// TestMobileContract_AuthRoutesExist walks the real route table rather than
// firing requests: a request-based check answers "did something handle this",
// which a catch-all or a redirect can satisfy while the route itself is gone.
// Walking asserts the METHOD + PATH pair is registered.
func TestMobileContract_AuthRoutesExist(t *testing.T) {
	srv := testServer(t)
	// The router is built lazily on first request (ensureRouter), so a server
	// that has served nothing has a nil one — walking it directly panics.
	srv.ensureRouter()

	registered := map[string]bool{}
	err := chi.Walk(srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		registered[method+" "+strings.TrimSuffix(route, "/")] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}

	// Each entry names the shell surface that breaks if the route moves.
	for _, tc := range []struct{ route, usedBy string }{
		{"GET /api/v1/auth/session", "launch: setup_required / auth_method probe before showing any UI"},
		{"POST /api/v1/auth/register", "sign-up form"},
		{"POST /api/v1/auth/login", "email+password sign-in"},
		{"POST /api/v1/auth/2fa/login-verify", "the second step of a 2FA sign-in"},
		{"POST /api/v1/auth/logout", "sign-out"},
		{"GET /api/v1/auth/me", "profile load after sign-in"},
	} {
		if !registered[tc.route] {
			t.Errorf("route %q is not registered — pad-mobile depends on it for %s", tc.route, tc.usedBy)
		}
	}
}

// TestMobileContract_SessionPayloadKeys pins the keys the shells branch on
// before showing any UI. A renamed key here reads on a phone as a blank screen
// or a login form on an instance that needs setup, not as an error.
func TestMobileContract_SessionPayloadKeys(t *testing.T) {
	srv := testServer(t)

	rr := doRequest(srv, "GET", "/api/v1/auth/session", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /auth/session = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode session payload: %v", err)
	}

	for _, key := range []string{"authenticated", "setup_required", "auth_method", "version"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("session payload has no %q key — pad-mobile branches on it at launch (keys present: %v)",
				key, sortedKeys(payload))
		}
	}
}

// TestMobileContract_TwoFactorLoginRoundTrip is the contract with the most
// moving parts and the least coverage: before this file, `requires_2fa`,
// `challenge_token` and `recovery_code` appeared in ZERO test files while being
// live response and request keys.
//
// It drives the whole two-step sign-in the shells implement — login returns a
// challenge, login-verify redeems it with a recovery code — so a rename on
// either side fails here rather than on a phone.
func TestMobileContract_TwoFactorLoginRoundTrip(t *testing.T) {
	srv := testServer(t)

	const email = "mobile-2fa@example.com"
	const password = "correct-horse-battery-staple"
	const recoveryCode = "PAD-MOBILE-RECOVERY"

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: email, Name: "Mobile Tester", Password: password, Role: "member",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Recovery codes are stored as newline-joined sha256 hex digests
	// (store.ConsumeRecoveryCode hashes the input the same way).
	sum := sha256.Sum256([]byte(recoveryCode))
	if err := srv.store.SetTOTPSecret(user.ID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatalf("SetTOTPSecret: %v", err)
	}
	if err := srv.store.EnableTOTP(user.ID, "JBSWY3DPEHPK3PXP", hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("EnableTOTP: %v", err)
	}

	rr := doRequest(srv, "POST", "/api/v1/auth/login", map[string]any{
		"email": email, "password": password,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("login = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var login map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &login); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if login["requires_2fa"] != true {
		t.Errorf("login response has no `requires_2fa: true` — the shells use it to decide whether to show "+
			"the code screen (keys: %v)", sortedKeys(login))
	}
	challenge, _ := login["challenge_token"].(string)
	if challenge == "" {
		t.Fatalf("login response carries no non-empty `challenge_token` — the second step cannot be made "+
			"(keys: %v)", sortedKeys(login))
	}

	// The shells send exactly these two request keys.
	rr = doRequest(srv, "POST", "/api/v1/auth/2fa/login-verify", map[string]any{
		"challenge_token": challenge,
		"recovery_code":   recoveryCode,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("login-verify with challenge_token + recovery_code = %d, want 200: %s", rr.Code, rr.Body.String())
	}
}

// TestMobileContract_AttachmentDispositionCarriesAFilename pins the header form
// on BOTH dispositions (BUG-2910). The shells parse the filename out of
// Content-Disposition to name a downloaded or shared file; the existing
// download tests assert the `inline;` / `attachment;` PREFIX and say nothing
// about the parameter that follows it, so the filename could be dropped from
// either branch with the suite green.
func TestMobileContract_AttachmentDispositionCarriesAFilename(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	for _, tc := range []struct {
		name        string
		filename    string
		body        []byte
		disposition string
	}{
		{"inline-served image", "shot.png", realPNG(), "inline"},
		// A ZIP, not a .txt: text/plain is on the read path's inline-safe
		// allowlist, so a text file comes back `inline` too and the case would
		// have tested the same branch twice under a name claiming otherwise.
		{"downloaded archive", "bundle.zip", realZIP(t), "attachment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := doMultipartUpload(srv, slug, tc.filename, tc.body)
			if rr.Code != http.StatusCreated {
				t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
			}
			var up struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &up); err != nil {
				t.Fatalf("decode upload: %v", err)
			}

			rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/attachments/"+up.ID, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("download: %d %s", rr.Code, rr.Body.String())
			}
			got := rr.Header().Get("Content-Disposition")
			want := tc.disposition + `; filename="` + tc.filename + `"`
			if got != want {
				t.Errorf("Content-Disposition = %q, want %q — pad-mobile parses the filename out of this header",
					got, want)
			}
		})
	}
}

// realZIP builds a valid, minimal zip archive so the upload sniffs as
// application/zip — a category the read path serves as an attachment.
func realZIP(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("readme.txt")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := w.Write([]byte("in a zip\n")); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
