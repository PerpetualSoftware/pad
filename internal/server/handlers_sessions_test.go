package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// testServerWithPresence gives a server both the watch bus and the
// presence registry — the production pairing (cmd_server.go wires them
// on adjacent lines), since presence only ever fills from stream
// connections.
func testServerWithPresence(t *testing.T) *Server {
	t.Helper()
	srv := testServerWithWatchEvents(t)
	srv.SetSessionPresence(NewMemorySessionPresence())
	return srv
}

func getSessions(t *testing.T, baseURL, token string) (int, sessionsResponse) {
	t.Helper()
	req, _ := http.NewRequest("GET", baseURL+"/api/v1/sessions", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var out sessionsResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode sessions response: %v", err)
		}
	}
	return resp.StatusCode, out
}

func TestListSessions_RequiresAuth(t *testing.T) {
	t.Parallel()
	srv := testServerWithPresence(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	if status, _ := getSessions(t, ts.URL, ""); status != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", status)
	}
}

// TestListSessions_UnavailableWithoutRegistry pins the deliberate
// choice in handleListSessions: a server with no registry answers 503,
// NOT 200-with-an-empty-list. "I can't tell" and "nobody is listening"
// must not look the same to the UI — collapsing them is precisely the
// dishonesty this feature exists to remove.
func TestListSessions_UnavailableWithoutRegistry(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t) // no SetSessionPresence
	_, _, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	if status, _ := getSessions(t, ts.URL, tok.Token); status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without a presence registry, got %d", status)
	}
}

func TestListSessions_EmptyWhenNothingConnected(t *testing.T) {
	t.Parallel()
	srv := testServerWithPresence(t)
	_, _, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	status, body := getSessions(t, ts.URL, tok.Token)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if body.Count != 0 || len(body.Sessions) != 0 {
		t.Fatalf("expected an empty session list, got count=%d sessions=%+v", body.Count, body.Sessions)
	}
}

// TestListSessions_IsNotCacheable pins the Cache-Control header (codex
// round 2, P2). The response is both per-user sensitive and short-lived
// by nature, so a cached copy is wrong in two independent ways — see
// handleListSessions.
func TestListSessions_IsNotCacheable(t *testing.T) {
	t.Parallel()
	srv := testServerWithPresence(t)
	_, _, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("expected Cache-Control %q, got %q", "private, no-store", got)
	}
}

// TestListSessions_ReflectsAnOpenStream is the end-to-end shape of the
// slice: open a stream, see it; drop the stream, stop seeing it. The
// disconnect half is the one that matters — a leaked entry makes the UI
// promise a listener that is gone.
func TestListSessions_ReflectsAnOpenStream(t *testing.T) {
	t.Parallel()
	srv := testServerWithPresence(t)
	_, _, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	// Wait for "connected" rather than polling blind: it proves the
	// handler reached the point where registration has happened, so the
	// assertion below isn't racing the handshake.
	if ev := waitForWatchEvent(t, ch, 3*time.Second); ev.Type != "connected" {
		t.Fatalf("expected 'connected', got %q", ev.Type)
	}

	status, body := getSessions(t, ts.URL, tok.Token)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if body.Count != 1 || len(body.Sessions) != 1 {
		t.Fatalf("expected exactly 1 live session, got count=%d sessions=%+v", body.Count, body.Sessions)
	}
	if body.Sessions[0].ID == "" {
		t.Fatal("live session has an empty id")
	}
	if body.Sessions[0].ConnectedAt.IsZero() {
		t.Fatal("live session has a zero connected_at")
	}

	cancel()
	// The deregistration happens in the handler's defer once it notices
	// ctx.Done, which is asynchronous with the client-side cancel.
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, body = getSessions(t, ts.URL, tok.Token)
		if body.Count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session still listed %v after disconnect: %+v", 3*time.Second, body.Sessions)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestListSessions_IsSelfScoped guards the consent boundary: one user's
// open stream must never appear in another user's list. There is no
// admin view and no ?user_id= for the same reason.
func TestListSessions_IsSelfScoped(t *testing.T) {
	t.Parallel()
	srv := testServerWithPresence(t)
	_, _, tokA, _ := setupWatchTestUser(t, srv)
	// Not a second setupWatchTestUser call: that helper hardcodes
	// watch-test@example.com, so calling it twice on one server collides
	// on the unique email. User B needs no workspace membership here —
	// the presence list is user-scoped and never consults workspaces.
	userB, err := srv.store.CreateUser(models.UserCreate{
		Email:    "sessions-test-b@example.com",
		Name:     "Sessions Tester B",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	tokB, err := srv.store.CreateAPIToken(userB.ID, models.APITokenCreate{Name: "sessions-test-b"}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := connectWatchStream(ctx, t, ts.URL, tokA.Token)
	if ev := waitForWatchEvent(t, ch, 3*time.Second); ev.Type != "connected" {
		t.Fatalf("expected 'connected', got %q", ev.Type)
	}

	if _, body := getSessions(t, ts.URL, tokA.Token); body.Count != 1 {
		t.Fatalf("user A should see their own session, got count=%d", body.Count)
	}
	if _, body := getSessions(t, ts.URL, tokB.Token); body.Count != 0 {
		t.Fatalf("user B must not see user A's session, got count=%d sessions=%+v", body.Count, body.Sessions)
	}
}
