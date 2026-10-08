package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3361: platform administration requires an interactive session. An
// admin's API token is refused on every /api/v1/admin route and on
// admin-create signup, unless a self-hosted operator opted in with
// PAD_ADMIN_API_TOKENS=allow (never honoured on Pad Cloud).

type adminPATFixture struct {
	srv     *Server
	pat     string
	session string
}

func newAdminPATFixture(t *testing.T) adminPATFixture {
	t.Helper()
	srv := testServer(t)
	admin, err := srv.store.CreateUser(models.UserCreate{Email: "admin-3361@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Admin WS", OwnerID: admin.ID})
	if err != nil {
		t.Fatal(err)
	}
	pat, err := srv.store.CreateAPIToken(admin.ID, models.APITokenCreate{Name: "admin-pat", WorkspaceID: ws.ID}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return adminPATFixture{srv: srv, pat: pat.Token, session: loginUser(t, srv, "admin-3361@example.com", "correct-horse-battery-staple")}
}

// Every route the router registers under /api/v1/admin refuses an admin's
// PAT, so an admin route added later is refused by construction and by test.
func TestBUG3361_EveryAdminRouteRefusesAnAdminPAT(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	f := newAdminPATFixture(t)
	f.srv.ensureRouter()
	param := regexp.MustCompile(`\{[^}]*\}`)
	checked := 0
	err := chi.Walk(f.srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/admin/") {
			return nil
		}
		path := param.ReplaceAllString(route, "x")
		checked++
		rr := doRequestWithBearer(f.srv, method, path, f.pat, nil)
		if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "session_required") {
			t.Errorf("admin PAT %s %s: %d %s, want 403 session_required", method, route, rr.Code, rr.Body.String())
		}
		// AND every route still administers for the admin's sessions, browser
		// and CLI (BUG-1926, Dave day 89: admin needs a session; PATs are the
		// line). The placeholder params and empty bodies may well answer 400
		// or 404: what must never come back is a refusal of the credential or
		// of the role.
		for kind, sess := range map[string]func() *httptest.ResponseRecorder{
			"browser session": func() *httptest.ResponseRecorder { return doRequestWithCookie(f.srv, method, path, nil, f.session) },
			"CLI session":     func() *httptest.ResponseRecorder { return doRequestWithBearer(f.srv, method, path, f.session, nil) },
		} {
			got := sess()
			body := got.Body.String()
			if strings.Contains(body, "session_required") || strings.Contains(body, "Admin access required") {
				t.Errorf("admin %s %s %s: refused %d %s", kind, method, route, got.Code, body)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 20 {
		t.Fatalf("only %d admin routes were walked; the walk is not seeing the router", checked)
	}
	// The admin's own sessions still administer.
	if rr := doRequestWithBearer(f.srv, "GET", "/api/v1/admin/settings", f.session, nil); rr.Code != http.StatusOK {
		t.Fatalf("admin CLI session on /admin/settings: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doRequestWithCookie(f.srv, "GET", "/api/v1/admin/settings", nil, f.session); rr.Code != http.StatusOK {
		t.Fatalf("admin browser session on /admin/settings: %d %s", rr.Code, rr.Body.String())
	}
}

// Admin-create signup is administration too: with a PAT it was the first
// step of minting a second admin.
func TestBUG3361_AdminCreateSignupRefusesAnAdminPAT(t *testing.T) {
	f := newAdminPATFixture(t)
	body := map[string]any{"email": "new-3361@example.com", "name": "New", "password": "a-long-enough-passphrase-3361"}
	rr := doRequestWithBearer(f.srv, "POST", "/api/v1/auth/register", f.pat, body)
	if rr.Code != http.StatusForbidden || errorCode(t, rr) != "session_required" {
		t.Fatalf("admin PAT creating an account: %d %s, want 403 session_required", rr.Code, rr.Body.String())
	}
	if u, _ := f.srv.store.GetUserByEmail("new-3361@example.com"); u != nil {
		t.Fatal("the refused signup created the account")
	}
	if rr := doRequestWithBearer(f.srv, "POST", "/api/v1/auth/register", f.session, body); rr.Code != http.StatusCreated {
		t.Fatalf("admin CLI session creating an account: %d %s, want 201", rr.Code, rr.Body.String())
	}
}

// The self-host escape hatch: PAD_ADMIN_API_TOKENS=allow lets an admin PAT
// administer, and is ignored on Pad Cloud.
func TestBUG3361_EscapeHatchIsSelfHostOnly(t *testing.T) {
	t.Setenv("PAD_ADMIN_API_TOKENS", "allow")
	f := newAdminPATFixture(t)
	if !f.srv.adminAPITokensAllowed {
		t.Fatal("PAD_ADMIN_API_TOKENS=allow was not read at startup")
	}
	if rr := doRequestWithBearer(f.srv, "GET", "/api/v1/admin/settings", f.pat, nil); rr.Code != http.StatusOK {
		t.Fatalf("admin PAT with the escape hatch on: %d %s, want 200", rr.Code, rr.Body.String())
	}
	f.srv.cloudMode = true
	if rr := doRequestWithBearer(f.srv, "GET", "/api/v1/admin/settings", f.pat, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin PAT with the escape hatch on, in cloud mode: %d, want 403", rr.Code)
	}
}
