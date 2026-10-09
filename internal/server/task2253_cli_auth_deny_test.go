package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-2253: the CLI sign-in approval page gains Deny and the context to
// decide with. These pin the server half: who asked is recorded at create
// and served on a pending poll, a deny is final and reaches the CLI as a
// non-2xx (so a CLI from before Deny stops too), and approve and deny
// cannot both win.

func createCLIAuthSessionFrom(t *testing.T, srv *Server, userAgent string) string {
	t.Helper()
	rr := doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/cli/sessions", nil,
		map[string]string{"User-Agent": userAgent}, "203.0.113.4:5555")
	if rr.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var out struct {
		SessionCode string `json:"session_code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || out.SessionCode == "" {
		t.Fatalf("create body: %s (%v)", rr.Body.String(), err)
	}
	return out.SessionCode
}

func pollCLIAuth(t *testing.T, srv *Server, code string) (int, map[string]any) {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/auth/cli/sessions/"+code, nil)
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return rr.Code, body
}

func TestTASK2253_PendingPollCarriesWhoAsked(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "owner@test.com", "Owner")

	long := "pad-cli/1.0 " + strings.Repeat("x", 300)
	code := createCLIAuthSessionFrom(t, srv, long)

	status, body := pollCLIAuth(t, srv, code)
	if status != http.StatusOK || body["status"] != "pending" {
		t.Fatalf("poll: %d %v", status, body)
	}
	if body["requester_ip"] != "203.0.113.4" {
		t.Errorf("requester_ip = %v, want 203.0.113.4", body["requester_ip"])
	}
	ua, _ := body["requester_user_agent"].(string)
	if !strings.HasPrefix(ua, "pad-cli/1.0 ") || len([]rune(ua)) != 200 {
		t.Errorf("requester_user_agent = %q (%d runes), want the agent capped at 200", ua, len([]rune(ua)))
	}
	if body["created_at"] == nil || body["created_at"] == "" {
		t.Errorf("created_at missing from a pending poll: %v", body)
	}
}

func TestTASK2253_DenyIsFinalAndReachesTheCLIAsAnError(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	code := createCLIAuthSessionFrom(t, srv, "pad-cli/1.0")

	// No account check: the code is the authority. A browser POST still
	// carries the session and CSRF cookies, as the approval page sends it.
	if rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/cli/sessions/"+code+"/deny", nil, token); rr.Code != http.StatusOK {
		t.Fatalf("deny: %d %s", rr.Code, rr.Body.String())
	}
	// A second deny (a double click, or a second tab) answers the same.
	if rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/cli/sessions/"+code+"/deny", nil, token); rr.Code != http.StatusOK {
		t.Fatalf("second deny: %d %s, want 200", rr.Code, rr.Body.String())
	}

	// The CLI's poll: a 410 with its own code, never a 200 "denied" that a
	// CLI from before Deny would keep polling for 20 minutes.
	rr := doRequest(srv, "GET", "/api/v1/auth/cli/sessions/"+code, nil)
	if rr.Code != http.StatusGone || errorCode(t, rr) != "cli_auth_denied" {
		t.Fatalf("poll after deny: %d %s, want 410 cli_auth_denied", rr.Code, rr.Body.String())
	}

	// Approving it afterwards mints nothing.
	before := countSessions(t, srv)
	rr = doRequestWithCookie(srv, "POST", "/api/v1/auth/cli/sessions/"+code+"/approve", nil, token)
	if rr.Code != http.StatusConflict || errorCode(t, rr) != "cli_auth_denied" {
		t.Fatalf("approve after deny: %d %s, want 409 cli_auth_denied", rr.Code, rr.Body.String())
	}
	if after := countSessions(t, srv); after != before {
		t.Fatalf("approve after deny minted a session: %d -> %d", before, after)
	}
}

func TestTASK2253_DenyAfterApproveIsRefused(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	code := createCLIAuthSessionFrom(t, srv, "pad-cli/1.0")

	if rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/cli/sessions/"+code+"/approve", nil, token); rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}
	rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/cli/sessions/"+code+"/deny", nil, token)
	if rr.Code != http.StatusConflict || errorCode(t, rr) != "already_approved" {
		t.Fatalf("deny after approve: %d %s, want 409 already_approved", rr.Code, rr.Body.String())
	}
	// The approval stands: the CLI still collects its token.
	status, body := pollCLIAuth(t, srv, code)
	if status != http.StatusOK || body["status"] != "approved" || body["token"] == nil {
		t.Fatalf("poll after a refused deny: %d %v, want the approved token", status, body)
	}
}

func TestTASK2253_DenyUnknownCode(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/cli/sessions/0123456789abcdef0123456789abcdef/deny", nil, token)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("deny of an unknown code: %d %s, want 404", rr.Code, rr.Body.String())
	}
}

// The race the store guards: a deny landing between approve's read and its
// write. The write is conditional on pending, so it affects no row and
// reports ErrCLIAuthNotPending, which the handler answers 409 after ending
// the session it minted.
func TestTASK2253_StoreRefusesApprovingADeniedSession(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	code := createCLIAuthSessionFrom(t, srv, "pad-cli/1.0")
	u, err := srv.store.GetUserByEmail("owner@test.com")
	if err != nil || u == nil {
		t.Fatal(err)
	}
	if err := srv.store.DenyCLIAuthSession(code); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.ApproveCLIAuthSession(code, "minted-token", u.ID); !errors.Is(err, store.ErrCLIAuthNotPending) {
		t.Fatalf("approving a denied session: %v, want ErrCLIAuthNotPending", err)
	}
}

func countSessions(t *testing.T, srv *Server) int {
	t.Helper()
	var n int
	if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
