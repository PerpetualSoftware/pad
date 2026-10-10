package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2255 (C110): the admin MCP audit page names people and connections.
// Every resolution path, and an id that no longer resolves left out (the
// page then shows the id).
func TestMCPAuditNames_EveryPath(t *testing.T) {
	s := testStore(t)

	named, err := s.CreateUser(models.UserCreate{Email: "named@example.com", Name: "Named Person", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	unnamed, err := s.CreateUser(models.UserCreate{Email: "unnamed@example.com", Name: "", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}

	pat, err := s.CreateAPIToken(named.ID, models.APITokenCreate{Name: "laptop CLI"}, 30, 365)
	if err != nil {
		t.Fatal(err)
	}

	client, err := s.CreateOAuthClient(models.OAuthClientCreate{
		Name: "Claude Desktop", RedirectURIs: []string{"http://127.0.0.1/cb"},
		GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
		TokenEndpointAuthMethod: "none", Public: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(s.q(`INSERT INTO oauth_refresh_tokens (signature, request_id, requested_at, client_id, subject)
		VALUES (?, ?, ?, ?, ?)`), "sig-live", "req-live", time.Now().UTC().Format(time.RFC3339), client.ID, named.ID); err != nil {
		t.Fatal(err)
	}
	// A second refresh token of the same grant (rotation) must not duplicate.
	if _, err := s.db.Exec(s.q(`INSERT INTO oauth_refresh_tokens (signature, request_id, requested_at, client_id, subject)
		VALUES (?, ?, ?, ?, ?)`), "sig-live-2", "req-live", time.Now().UTC().Format(time.RFC3339), client.ID, named.ID); err != nil {
		t.Fatal(err)
	}
	// Labelled by the user AND still holding a refresh token: the client name wins.
	if err := s.CreateOAuthConnection(OAuthConnection{RequestID: "req-live", UserID: named.ID, Name: "my label"}); err != nil {
		t.Fatal(err)
	}
	// No refresh token survives: the connection label is the fallback.
	if err := s.CreateOAuthConnection(OAuthConnection{RequestID: "req-revoked", UserID: named.ID, Name: "old desktop"}); err != nil {
		t.Fatal(err)
	}

	users, conns, err := s.MCPAuditNames(
		[]string{named.ID, unnamed.ID, "user-gone"},
		[]string{pat.ID, "pat-gone"},
		[]string{"req-live", "req-revoked", "req-gone"},
	)
	if err != nil {
		t.Fatal(err)
	}
	wantUsers := map[string]string{named.ID: "Named Person", unnamed.ID: "unnamed@example.com"}
	wantConns := map[string]string{pat.ID: "laptop CLI", "req-live": "Claude Desktop", "req-revoked": "old desktop"}
	if len(users) != len(wantUsers) {
		t.Errorf("users = %v, want %v", users, wantUsers)
	}
	for k, v := range wantUsers {
		if users[k] != v {
			t.Errorf("users[%s] = %q, want %q", k, users[k], v)
		}
	}
	if len(conns) != len(wantConns) {
		t.Errorf("connections = %v, want %v", conns, wantConns)
	}
	for k, v := range wantConns {
		if conns[k] != v {
			t.Errorf("connections[%s] = %q, want %q", k, conns[k], v)
		}
	}

	// Nothing to resolve is no query and no error.
	if u, c, err := s.MCPAuditNames(nil, nil, nil); err != nil || len(u) != 0 || len(c) != 0 {
		t.Errorf("empty: %v %v %v", u, c, err)
	}
}
