package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestWatchEventsStream_OAuthShapedTokenCannotAuthenticate is the probe
// receipt for BUG-2752, which claimed that an OAuth token whose consent
// was narrowed to a subset of workspaces could receive notifications
// from workspaces outside that subset over GET /api/v1/events/stream.
//
// The claim's premise was correct as far as it went: the allow-list gate
// really does live inside RequireWorkspaceAccess (middleware_auth.go,
// tokenAllowedWorkspaceMatches), that middleware really does early-return
// when there is no {slug} URL param, this route really is registered
// without one, and neither watchNotificationVisible nor
// computeWatchAccessVisibility consults TokenAllowedWorkspaceSet.
//
// The claim was still WRONG, because it never asked whether an
// allow-listed token can reach this route at all. It cannot:
//
//   - The allow-list is stashed at exactly one site — handleMCPOAuthAuth
//     in middleware_mcp_auth.go — and MCPBearerAuth is mounted ONLY on
//     /mcp (handlers_mcp.go). The PAT branch never stashes one.
//   - /api/v1/* runs TokenAuth instead, which accepts a `padsess_` CLI
//     session token or a `pad_`-prefixed API token of exact length and
//     rejects everything else on FORMAT, before any lookup.
//   - A fosite-issued OAuth access token is base64 of an HMAC and never
//     carries the `pad_` prefix — MCPBearerAuth's own branch-selection
//     comment depends on that same fact to tell the two token kinds
//     apart cheaply.
//
// So the consent scope is not unenforced on this route; the credential
// that carries a consent scope cannot authenticate to this route.
//
// WHY THIS TEST EXISTS RATHER THAN JUST A CLOSING COMMENT: the
// refutation rests entirely on that format gate. If /api/v1/* ever
// learns to accept OAuth access tokens — a plausible future, since the
// remote MCP surface and the REST surface keep converging — the leak
// BUG-2752 described becomes real on the same day, with nothing else
// changing and no reviewer necessarily connecting the two. This test
// fails at that moment.
func TestWatchEventsStream_OAuthShapedTokenCannotAuthenticate(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug := createWSWithCollections(t, srv)

	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "oauth-scope-probe@example.com", Name: "Probe", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	pat, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{Name: "oauth-scope-probe"}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// OAuth-shaped bearer: base64-of-HMAC shape, no `pad_` prefix. This is
	// what a fosite-issued access token looks like to TokenAuth, and the
	// only credential kind that can carry a workspace allow-list.
	const oauthShaped = "MTIzNDU2Nzg5MC1hYmNkZWZnaGlqa2xtbm9wcXJzdHV2d3h5ejEyMzQ1Njc4OTA"
	if strings.HasPrefix(oauthShaped, "pad_") {
		t.Fatal("probe fixture is wrong: an OAuth-shaped token must not carry the PAT prefix")
	}

	newStreamReq := func(t *testing.T, token string) *http.Request {
		t.Helper()
		req, err := http.NewRequest("GET", "/api/v1/events/stream", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return req
	}

	// THE PROBE LEG. An OAuth-shaped credential is refused on format,
	// so it never reaches the stream handler and its (absent) allow-list
	// filtering is moot.
	t.Run("oauth shaped token is refused", func(t *testing.T) {
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, newStreamReq(t, oauthShaped))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for an OAuth-shaped bearer on /api/v1/events/stream, got %d: %s",
				rr.Code, rr.Body.String())
		}
	})

	// THE NEGATIVE CONTROL, and the reason the leg above means anything.
	// Without it, a route that 401s every caller — a broken harness, a
	// renamed path, a middleware ordering change — would produce the
	// same green and read as a refutation. A real PAT must get through
	// to the same URL.
	t.Run("real PAT reaches the stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req := newStreamReq(t, pat.Token).WithContext(ctx)
		rr := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			defer close(done)
			srv.ServeHTTP(rr, req)
		}()

		// The handler holds the connection open; the ctx timeout closes
		// it. Either way, what matters is that it did NOT 401.
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Fatal("stream handler did not return after context expiry")
		}
		if rr.Code == http.StatusUnauthorized {
			t.Fatalf("negative control failed: a valid PAT was refused (%d) — "+
				"the probe leg's 401 proves nothing about token format: %s",
				rr.Code, rr.Body.String())
		}
	})
}
