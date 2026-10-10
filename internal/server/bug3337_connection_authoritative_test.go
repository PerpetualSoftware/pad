package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3337: a grant chain minted before PLAN-1519 carries its allow-list
// in session.Extra, and startup's backfill seeds a connection row from it.
// Connected Apps edits only the row, so when the gate unioned the two, a
// user who narrowed such a connection still had what they removed — and a
// refresh carried Extra forward, so it never went away. With a row present
// the row is authoritative.

// legacyGrant runs a real authorization-code flow, then rewrites the
// chain's stored sessions to the pre-PLAN-1519 shape (allow-list in Extra,
// no connection row) and runs the startup backfill, which is how every
// such chain reaches a live server. It returns the access token and the
// chain's request id.
func legacyGrant(t *testing.T, srv *Server, sess oauthSession, extra []string) (string, string) {
	t.Helper()
	// The canonical resource: a grant without one is refused since TASK-3363
	// phase 2. A legacy chain was minted with the default, which is this.
	tok, code := mintWithResource(t, srv, sess, testCanonicalAudience)
	if code != http.StatusOK {
		t.Fatalf("mint: %d", code)
	}
	db := srv.store.DB()
	var rid string
	if err := db.QueryRow(`SELECT request_id FROM oauth_access_tokens ORDER BY requested_at DESC, rowid DESC LIMIT 1`).Scan(&rid); err != nil {
		t.Fatalf("read request id: %v", err)
	}
	for _, table := range []string{"oauth_access_tokens", "oauth_refresh_tokens"} {
		rows, err := db.Query(`SELECT signature, session_data FROM `+table+` WHERE request_id = ?`, rid)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		type row struct{ sig, data string }
		var all []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.sig, &r.data); err != nil {
				t.Fatalf("scan %s: %v", table, err)
			}
			all = append(all, r)
		}
		rows.Close()
		if len(all) == 0 {
			t.Fatalf("no %s rows for the chain", table)
		}
		for _, r := range all {
			var m map[string]any
			if err := json.Unmarshal([]byte(r.data), &m); err != nil {
				t.Fatalf("decode session: %v", err)
			}
			m["extra"] = map[string]any{"allowed_workspaces": extra}
			b, _ := json.Marshal(m)
			if _, err := db.Exec(`UPDATE `+table+` SET session_data = ? WHERE signature = ?`, string(b), r.sig); err != nil {
				t.Fatalf("rewrite %s: %v", table, err)
			}
		}
	}
	if err := srv.store.DeleteOAuthConnection(rid); err != nil {
		t.Fatalf("drop connection: %v", err)
	}
	if _, err := srv.store.BackfillOAuthConnections(); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if access, err := srv.store.GetOAuthConnectionAccess(rid); err != nil || !access.HasConnection {
		t.Fatalf("backfill left no connection row: %+v %v", access, err)
	}
	return tok, rid
}

// allowListAt presents token to /mcp and returns the workspace allow-list
// the gate stashed (nil = unrestricted).
func allowListAt(t *testing.T, srv *Server, token string) []string {
	t.Helper()
	var got []string
	reached := false
	h := srv.MCPBearerAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		got = TokenAllowedWorkspacesFromContext(r.Context())
	}))
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !reached {
		t.Fatalf("token refused at /mcp: %d %s", rr.Code, rr.Body.String())
	}
	return got
}

func refreshChain(t *testing.T, srv *Server) string {
	t.Helper()
	rr := postOAuthForm(srv, "/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {lastRefresh},
		"client_id":     {lastClientID},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	parseJSON(t, rr, &resp)
	tok, _ := resp["access_token"].(string)
	lastRefresh, _ = resp["refresh_token"].(string)
	if tok == "" {
		t.Fatal("refresh returned no access token")
	}
	return tok
}

func TestBUG3337_ConnectionRowIsAuthoritativeOverLegacyExtra(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	user, sessionToken := loginTestUser(t, srv)
	sess := oauthSession{
		sessionToken: sessionToken,
		csrfTok:      readCSRFFromCookie(t, srv, sessionToken),
		clientID:     registerTestClient(t, srv, "https://app.test/cb"),
	}
	ws1 := mustCreateOwnedWorkspace(t, srv, "Alpha 3337", user)
	ws2 := mustCreateOwnedWorkspace(t, srv, "Beta 3337", user)

	t.Run("wildcard narrowed to one workspace", func(t *testing.T) {
		tok, rid := legacyGrant(t, srv, sess, []string{"*"})
		if got := allowListAt(t, srv, tok); got != nil {
			t.Fatalf("before narrowing: allow-list %v, want unrestricted", got)
		}
		// What Connected Apps does when the user switches to "specific".
		if err := srv.store.SetScopeFlags(rid, true, false, true); err != nil {
			t.Fatalf("SetScopeFlags: %v", err)
		}
		if err := srv.store.AddConnectionWorkspace(rid, ws1.ID, store.AddedByUser); err != nil {
			t.Fatalf("AddConnectionWorkspace: %v", err)
		}
		want := []string{ws1.Slug}
		if got := allowListAt(t, srv, tok); !reflect.DeepEqual(got, want) {
			t.Errorf("after narrowing: allow-list %v, want %v", got, want)
		}
		if got := allowListAt(t, srv, refreshChain(t, srv)); !reflect.DeepEqual(got, want) {
			t.Errorf("after refresh: allow-list %v, want %v", got, want)
		}
	})

	t.Run("workspace removed from an explicit list", func(t *testing.T) {
		tok, rid := legacyGrant(t, srv, sess, []string{ws1.Slug, ws2.Slug})
		both := []string{ws1.Slug, ws2.Slug}
		if got := allowListAt(t, srv, tok); !reflect.DeepEqual(got, both) {
			t.Fatalf("before removal: allow-list %v, want %v", got, both)
		}
		if err := srv.store.RemoveConnectionWorkspace(rid, ws2.ID); err != nil {
			t.Fatalf("RemoveConnectionWorkspace: %v", err)
		}
		want := []string{ws1.Slug}
		if got := allowListAt(t, srv, tok); !reflect.DeepEqual(got, want) {
			t.Errorf("after removal: allow-list %v, want %v", got, want)
		}
		if got := allowListAt(t, srv, refreshChain(t, srv)); !reflect.DeepEqual(got, want) {
			t.Errorf("after refresh: allow-list %v, want %v", got, want)
		}
	})
}
