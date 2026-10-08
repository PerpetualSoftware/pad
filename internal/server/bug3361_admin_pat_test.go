package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
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
	// memberSession is a NON-admin's browser session: the control that makes
	// the admin-session leg of the walk mean something (BUG-1926 codex r1).
	memberSession string
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
	if _, err := srv.store.CreateUser(models.UserCreate{Email: "member-3361@example.com", Name: "Member", Password: "correct-horse-battery-staple"}); err != nil {
		t.Fatal(err)
	}
	return adminPATFixture{
		srv:           srv,
		pat:           pat.Token,
		session:       loginUser(t, srv, "admin-3361@example.com", "correct-horse-battery-staple"),
		memberSession: loginUser(t, srv, "member-3361@example.com", "correct-horse-battery-staple"),
	}
}

// Every route the router registers under /api/v1/admin refuses an admin's
// PAT, so an admin route added later is refused by construction and by test.
func TestBUG3361_EveryAdminRouteRefusesAnAdminPAT(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	f := newAdminPATFixture(t)
	f.srv.ensureRouter()
	param := regexp.MustCompile(`\{[^}]*\}`)
	checked := 0
	var cloudGated []string
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
		// of the role. The CONTROL makes that mean something (codex r1): a
		// non-admin's session on the very same request IS refused for its role,
		// so the role check runs before whatever 400/404 the admin then meets,
		// and the admin's request got past it.
		ctl := doRequestWithCookie(f.srv, method, path, nil, f.memberSession)
		if ctl.Code == http.StatusNotFound && route != "/api/v1/admin/" {
			// Behind requireCloudMode: 404 for everyone off Cloud, so neither
			// leg can mean anything here. Proven on a cloud-mode server below.
			cloudGated = append(cloudGated, method+" "+route)
			return nil
		}
		if ctl.Code != http.StatusForbidden {
			t.Errorf("CONTROL %s %s: a non-admin session got %d %s, not a role refusal, so the admin leg proves nothing here",
				method, route, ctl.Code, ctl.Body.String())
		}
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
	sort.Strings(cloudGated)
	if got, want := strings.Join(cloudGated, ","), strings.Join(cloudOnlyAdminRoutes, ","); got != want {
		t.Errorf("routes answering 404 to everyone off Cloud:\n got  %s\n want %s\n(a route that 404s for another reason has no proof in this walk)", got, want)
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

// cloudOnlyAdminRoutes are the /admin routes behind requireCloudMode, sorted:
// the sidecar's billing calls (the cloud secret, or a platform admin as the
// alternative) and the billing dashboard.
var cloudOnlyAdminRoutes = []string{
	"GET /api/v1/admin/billing-stats",
	"GET /api/v1/admin/user-by-customer",
	"POST /api/v1/admin/payment-failed",
	"POST /api/v1/admin/plan",
	"POST /api/v1/admin/stripe-customer-id",
	"POST /api/v1/admin/stripe-event-processed",
	"POST /api/v1/admin/stripe-event-unmark",
}

// On Pad Cloud the billing routes take the cloud secret OR a platform admin.
// The admin alternative is a session's, never a PAT's (BUG-1926 / BUG-3361):
// a non-admin session without the secret is refused (the CONTROL), an admin's
// browser and CLI sessions get past it, and an admin PAT is refused.
func TestBUG1926_CloudBillingRoutesTakeAnAdminSessionNotAnAdminPAT(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	f := newAdminPATFixture(t)
	f.srv.cloudMode = true
	f.srv.cloudSecrets = []string{"cloud-secret-1926"}
	for _, mr := range cloudOnlyAdminRoutes {
		method, path, _ := strings.Cut(mr, " ")
		// The POST handlers decode the body (where the secret travels) before
		// they look at the caller, so an empty OBJECT, not a nil body, makes the
		// secret-or-admin decision the first thing that can refuse.
		var body interface{}
		if method == http.MethodPost {
			body = map[string]any{}
		}
		if pat := doRequestWithBearer(f.srv, method, path, f.pat, nil); pat.Code != http.StatusForbidden || !strings.Contains(pat.Body.String(), "session_required") {
			t.Errorf("admin PAT %s: %d %s, want 403 session_required", mr, pat.Code, pat.Body.String())
		}
		if ctl := doRequestWithCookie(f.srv, method, path, body, f.memberSession); ctl.Code != http.StatusForbidden {
			t.Errorf("CONTROL %s: a non-admin session without the secret got %d %s, want 403", mr, ctl.Code, ctl.Body.String())
		}
		for kind, got := range map[string]*httptest.ResponseRecorder{
			"browser session": doRequestWithCookie(f.srv, method, path, body, f.session),
			"CLI session":     doRequestWithBearer(f.srv, method, path, f.session, body),
		} {
			if got.Code == http.StatusForbidden {
				t.Errorf("admin %s %s: refused %d %s", kind, mr, got.Code, got.Body.String())
			}
		}
	}
}
