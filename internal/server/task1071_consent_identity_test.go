package server

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-1071: the consent screen is the trust anchor the RFC 8707 relaxation
// leans on (audienceForAuthorize), so it must say plainly that the resource
// is Pad, name which Pad, list the user's actual workspaces, and not let the
// app's self-chosen name crowd that out or pass as verified.

func registerNamedTestClient(t *testing.T, srv *Server, name, redirectURI string) string {
	t.Helper()
	rr := doRequest(srv, "POST", "/oauth/register", map[string]any{
		"client_name":   name,
		"redirect_uris": []string{redirectURI},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("register %q: %d (body: %s)", name, rr.Code, rr.Body.String())
	}
	var resp map[string]any
	parseJSON(t, rr, &resp)
	id, _ := resp["client_id"].(string)
	if id == "" {
		t.Fatal("register: empty client_id")
	}
	return id
}

func renderConsentFor(t *testing.T, srv *Server, clientID, redirectURI, sessionToken string) string {
	t.Helper()
	q := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {"pad:read"},
		"resource":              {testCanonicalAudience},
		"audience":              {testCanonicalAudience},
		"code_challenge":        {s256Challenge("code-verifier-abc123-must-be-43-to-128-chars-long")},
		"code_challenge_method": {"S256"},
		"state":                 {"task-1071-state"},
	}
	rr := doAuthedRequest(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("consent: %d (location: %s)", rr.Code, rr.Header().Get("Location"))
	}
	return rr.Body.String()
}

// bodyOf is the page after <body>, so CSS and the <title> never satisfy an
// assertion about what the user reads.
func bodyOf(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, "<body>")
	if i < 0 {
		t.Fatal("no <body> in consent page")
	}
	return page[i:]
}

func TestConsentIdentifiesPadAndTheUsersWorkspaces(t *testing.T) {
	t.Parallel()
	srv, _ := oauthEnabledTestServer(t)
	srv.SetBaseURL("https://pad.example.org")
	user, session := loginTestUser(t, srv)
	mustSeedWorkspaceWithRole(t, srv, user.ID, "Alpha Project", "alpha-project", "owner")
	mustSeedWorkspaceWithRole(t, srv, user.ID, "Beta Notes", "beta-notes", "viewer")
	other, err := srv.store.CreateUser(models.UserCreate{Email: "other-1071@example.com", Name: "Other", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	mustSeedWorkspaceWithRole(t, srv, other.ID, "Not Yours", "not-yours", "owner")

	id := registerNamedTestClient(t, srv, "Cursor", "https://app.test/cb")
	page := bodyOf(t, renderConsentFor(t, srv, id, "https://app.test/cb", session))

	for _, want := range []string{
		`<strong>Pad</strong> · pad.example.org`,
		"is requesting access to your Pad workspaces on pad.example.org.",
		"The app chose this name; Pad has not verified it.",
		"you return to <strong>app.test</strong>",
		`value="alpha-project"`,
		"Alpha Project",
		`value="beta-notes"`,
		"Beta Notes",
		// Named under the default "All my workspaces" choice too, where the
		// per-workspace picker is hidden.
		"Your Pad workspaces now: <strong>Alpha Project</strong>, <strong>Beta Notes</strong>.",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("consent page must contain %q", want)
		}
	}
	if strings.Contains(page, "not-yours") || strings.Contains(page, "Not Yours") {
		t.Error("consent page lists a workspace the user is not a member of")
	}
	// Pad is named before the app is.
	if p, c := strings.Index(page, "<strong>Pad</strong>"), strings.Index(page, "Authorize Cursor"); p < 0 || c < 0 || p > c {
		t.Errorf("Pad must be identified above the app's name (pad at %d, app at %d)", p, c)
	}
}

func TestConsentWorkspaceSummaryNamesFiveThenCounts(t *testing.T) {
	t.Parallel()
	srv, _ := oauthEnabledTestServer(t)
	user, session := loginTestUser(t, srv)
	for _, n := range []string{"A1", "A2", "A3", "A4", "A5", "A6", "A7"} {
		mustSeedWorkspaceWithRole(t, srv, user.ID, "WS "+n, "ws-"+strings.ToLower(n), "editor")
	}
	id := registerNamedTestClient(t, srv, "Cursor", "https://app.test/cb")
	page := bodyOf(t, renderConsentFor(t, srv, id, "https://app.test/cb", session))
	want := "Your Pad workspaces now: <strong>WS A1</strong>, <strong>WS A2</strong>, <strong>WS A3</strong>, <strong>WS A4</strong>, <strong>WS A5</strong> and 2 more."
	if !strings.Contains(page, want) {
		t.Errorf("summary must name five and count the rest; want %q", want)
	}
}

func TestConsentResourceHostFallsBackToTheRequestHost(t *testing.T) {
	t.Parallel()
	for _, base := range []string{"", "http://0.0.0.0:7777"} {
		srv, _ := oauthEnabledTestServer(t)
		srv.SetBaseURL(base)
		_, session := loginTestUser(t, srv)
		id := registerNamedTestClient(t, srv, "Cursor", "https://app.test/cb")
		page := bodyOf(t, renderConsentFor(t, srv, id, "https://app.test/cb", session))
		// httptest requests arrive with Host "example.com".
		if !strings.Contains(page, "your Pad workspaces on example.com.") {
			t.Errorf("base %q: consent page must name the request host", base)
		}
	}
}

// The four client-name/logo edge cases the task names.
func TestConsentClientNameEdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("a long name is cut, and Pad's identification still renders", func(t *testing.T) {
		t.Parallel()
		srv, _ := oauthEnabledTestServer(t)
		_, session := loginTestUser(t, srv)
		long := strings.Repeat("Very Long Client Name ", 50)
		id := registerNamedTestClient(t, srv, long, "https://app.test/cb")
		page := bodyOf(t, renderConsentFor(t, srv, id, "https://app.test/cb", session))
		if strings.Contains(page, strings.TrimSpace(long)) {
			t.Error("the full 1100-character name must not render")
		}
		if !strings.Contains(page, "…") {
			t.Error("a cut name must show it was cut")
		}
		if !strings.Contains(page, "your Pad workspaces on") {
			t.Error("Pad identification must survive a long client name")
		}
	})

	t.Run("a name claiming to be Pad is still labelled the app's own claim", func(t *testing.T) {
		t.Parallel()
		srv, _ := oauthEnabledTestServer(t)
		_, session := loginTestUser(t, srv)
		id := registerNamedTestClient(t, srv, "Pad (getpad.dev) official", "https://evil.test/cb")
		page := bodyOf(t, renderConsentFor(t, srv, id, "https://evil.test/cb", session))
		for _, want := range []string{
			"The app chose this name; Pad has not verified it.",
			"you return to <strong>evil.test</strong>",
		} {
			if !strings.Contains(page, want) {
				t.Errorf("must contain %q", want)
			}
		}
	})

	t.Run("markup in the name is escaped, never rendered", func(t *testing.T) {
		t.Parallel()
		srv, _ := oauthEnabledTestServer(t)
		_, session := loginTestUser(t, srv)
		evil := `<script>alert(1)</script><img src=x onerror=alert(2)>`
		id := registerNamedTestClient(t, srv, evil, "https://app.test/cb")
		page := bodyOf(t, renderConsentFor(t, srv, id, "https://app.test/cb", session))
		if strings.Contains(page, "<script>alert(1)") || strings.Contains(page, "<img src=x") {
			t.Error("client name markup must not reach the page unescaped")
		}
		if !strings.Contains(page, html.EscapeString("<script>alert(1)</script>")) {
			t.Error("the escaped name must render")
		}
	})

	t.Run("no logo renders no img, and the card still identifies Pad", func(t *testing.T) {
		t.Parallel()
		srv, _ := oauthEnabledTestServer(t)
		_, session := loginTestUser(t, srv)
		id := registerNamedTestClient(t, srv, "Cursor", "https://app.test/cb")
		page := bodyOf(t, renderConsentFor(t, srv, id, "https://app.test/cb", session))
		if strings.Contains(page, `<img class="logo"`) {
			t.Error("no logo registered, so no logo img")
		}
		if !strings.Contains(page, "your Pad workspaces") {
			t.Error("card must identify Pad without a logo")
		}
	})
}

func TestConsentClientNameCap(t *testing.T) {
	t.Parallel()
	if got := consentClientName("  Claude\n\n\n  Desktop  "); got != "Claude Desktop" {
		t.Errorf("whitespace runs collapse: got %q", got)
	}
	exact := strings.Repeat("é", consentClientNameMax)
	if got := consentClientName(exact); got != exact {
		t.Error("a name at the cap is unchanged")
	}
	got := consentClientName(exact + "x")
	if r := []rune(got); len(r) != consentClientNameMax || r[len(r)-1] != '…' {
		t.Errorf("one rune over the cap is cut to the cap with an ellipsis: %q", got)
	}
}

func TestConsentRedirectTarget(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"https://app.test/cb":             "app.test",
		"http://127.0.0.1:33418/callback": "127.0.0.1:33418",
		"claude://oauth/callback":         "claude://oauth",
		"com.example.app:/oauth/callback": "com.example.app:/oauth/callback",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := consentRedirectTarget(u); got != want {
			t.Errorf("%s: got %q, want %q", raw, got, want)
		}
	}
	if consentRedirectTarget(nil) != "" {
		t.Error("nil URL names nothing")
	}
}

func TestConsentNamesACustomSchemeReturnWithItsScheme(t *testing.T) {
	t.Parallel()
	srv, _ := oauthEnabledTestServer(t)
	_, session := loginTestUser(t, srv)
	id := registerNamedTestClient(t, srv, "Claude", "claude://oauth/callback")
	page := bodyOf(t, renderConsentFor(t, srv, id, "claude://oauth/callback", session))
	if !strings.Contains(page, "you return to <strong>claude://oauth</strong>") {
		t.Error("a custom-scheme return must be named with its scheme")
	}
}
