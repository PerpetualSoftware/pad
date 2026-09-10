package server

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// BUG-3007 — a stream must stop delivering when the CREDENTIAL that opened it
// is no longer valid.
//
// Both SSE endpoints revalidate something on a 60s jittered tick and neither
// asks this question. `/api/v1/events` revalidates the connecting user's
// MEMBERSHIP, which a sign-out does not change. `/api/v1/events/stream` is
// stricter — `refreshUser` re-fetches by USER ID and fails closed on a deleted
// or disabled user — but a logout destroys a SESSION, not a USER, so `GetUser`
// still returns a live enabled user and `deny` is never set.
//
// Reproduced by hand against a live instance before these were written: on the
// 9839884b binary a stream opened with a session kept delivering 165s after
// `POST /auth/logout` returned 200 and `GET /auth/me` on the same credential
// answered 401, and one opened with a PAT kept delivering 64s after the token
// was revoked with `DELETE /auth/tokens/{id}` → 204. The PAT legs are not
// assumed from the session legs: a PAT authenticates through `TokenAuth`, a
// different path with scopes and an allowed-workspace set, so "the credential
// is still valid" has a different lookup behind it.
//
// The intervals are shrunk here for the same reason the neighbouring suites
// shrink them: a real minute is not a test.

// streamClosedWithin reports whether the server ended the stream — EOF on the
// body — within d. It reads in a goroutine because a stream that is NOT closed
// simply blocks forever, which is the defect and must not hang the test.
func streamClosedWithin(t *testing.T, body *bufio.Scanner, d time.Duration) bool {
	t.Helper()
	done := make(chan struct{})
	var sawEOF atomic.Bool
	go func() {
		defer close(done)
		for body.Scan() {
			// Drain. Keepalives and events are both fine; only the END matters.
		}
		sawEOF.Store(true)
	}()
	select {
	case <-done:
		return sawEOF.Load()
	case <-time.After(d):
		return false
	}
}

func TestEventsStream_StopsWhenTheSessionIsDestroyed(t *testing.T) {
	prev := sseMembershipRevalInterval
	sseMembershipRevalInterval = 25 * time.Millisecond
	t.Cleanup(func() { sseMembershipRevalInterval = prev })

	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "LivenessWS", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner: %v", err)
	}

	token, err := srv.store.CreateSession(owner.ID, "test", "127.0.0.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events?workspace="+ws.Slug, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// PRECONDITION. Without it, "the stream ended" is also true of a stream
	// that never opened, and the test would pass on a 401.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200 — the connection under test never opened", resp.StatusCode)
	}

	// The session that opened this stream is destroyed. Nothing about the
	// user, the workspace or the membership changes.
	if err := srv.store.DeleteSession(token); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	// CONTROL: the credential really is dead, so a still-live stream cannot be
	// explained by the session having survived.
	if info, err := srv.store.ValidateSession(token); err == nil && info != nil {
		t.Fatal("session still validates after DeleteSession; the test's own premise is broken")
	}

	if !streamClosedWithin(t, bufio.NewScanner(resp.Body), 3*time.Second) {
		t.Fatal("stream still open after its session was destroyed and many revalidation ticks passed")
	}
}

func TestWatchStream_StopsWhenTheSessionIsDestroyed(t *testing.T) {
	prev := watchListRevalInterval
	watchListRevalInterval = 50 * time.Millisecond
	t.Cleanup(func() { watchListRevalInterval = prev })

	srv := testServerWithEvents(t)
	// `/events/stream` 503s without a watch bus, and a 503 would satisfy any
	// assertion about the stream ending. The precondition below catches that,
	// and this is what stops it happening in the first place.
	srv.SetWatchEventsBus(watchevents.New())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "watcher@example.com", Name: "Watcher", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	token, err := srv.store.CreateSession(user.ID, "test", "127.0.0.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200 — the connection under test never opened", resp.StatusCode)
	}

	if err := srv.store.DeleteSession(token); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if info, err := srv.store.ValidateSession(token); err == nil && info != nil {
		t.Fatal("session still validates after DeleteSession; the test's own premise is broken")
	}

	if !streamClosedWithin(t, bufio.NewScanner(resp.Body), 3*time.Second) {
		t.Fatal("watch stream still open after its session was destroyed and many revalidation ticks passed")
	}
}

// The PAT legs. Not a copy of the session legs for tidiness: a PAT
// authenticates through `TokenAuth`, a different middleware path with scopes
// and an allowed-workspace set, so "the credential is still valid" has a
// different lookup behind it and either endpoint could plausibly have failed
// closed on one and not the other. Reproduced by hand before these were
// written; neither does.

// mintPAT returns a live user-scoped token and its id.
func mintPAT(t *testing.T, srv *Server, userID, name string) (secret string, id string) {
	t.Helper()
	tok, err := srv.store.CreateAPIToken(userID, models.APITokenCreate{Name: name}, 90, 365)
	if err != nil {
		t.Fatalf("create PAT: %v", err)
	}
	return tok.Token, tok.ID
}

func TestEventsStream_StopsWhenThePATIsRevoked(t *testing.T) {
	prev := sseMembershipRevalInterval
	sseMembershipRevalInterval = 25 * time.Millisecond
	t.Cleanup(func() { sseMembershipRevalInterval = prev })

	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "patowner@example.com", Name: "Owner", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "PATWS", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner: %v", err)
	}

	secret, id := mintPAT(t, srv, owner.ID, "events-probe")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events?workspace="+ws.Slug, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200 — the connection under test never opened", resp.StatusCode)
	}

	if err := srv.store.DeleteUserAPIToken(id, owner.ID); err != nil {
		t.Fatalf("revoke PAT: %v", err)
	}
	if tok, err := srv.store.ValidateToken(secret); err == nil && tok != nil {
		t.Fatal("PAT still validates after revocation; the test's own premise is broken")
	}

	if !streamClosedWithin(t, bufio.NewScanner(resp.Body), 3*time.Second) {
		t.Fatal("stream still open after its PAT was revoked and many revalidation ticks passed")
	}
}

func TestWatchStream_StopsWhenThePATIsRevoked(t *testing.T) {
	prev := watchListRevalInterval
	watchListRevalInterval = 50 * time.Millisecond
	t.Cleanup(func() { watchListRevalInterval = prev })

	srv := testServerWithEvents(t)
	srv.SetWatchEventsBus(watchevents.New())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "patwatcher@example.com", Name: "Watcher", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	secret, id := mintPAT(t, srv, user.ID, "watch-probe")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200 — the connection under test never opened", resp.StatusCode)
	}

	if err := srv.store.DeleteUserAPIToken(id, user.ID); err != nil {
		t.Fatalf("revoke PAT: %v", err)
	}
	if tok, err := srv.store.ValidateToken(secret); err == nil && tok != nil {
		t.Fatal("PAT still validates after revocation; the test's own premise is broken")
	}

	if !streamClosedWithin(t, bufio.NewScanner(resp.Body), 3*time.Second) {
		t.Fatal("watch stream still open after its PAT was revoked and many revalidation ticks passed")
	}
}

// The collab WebSocket is the third member of this class and the one with
// WRITE access: a credential destroyed after upgrade leaves a connection whose
// per-frame gate still permits mutating item content. `collabRevalidationLoop`
// re-fetches the ITEM each tick and calls `authorizeCollabAccess(r, fresh)`,
// which reads `currentUser(r)` — the principal resolved at UPGRADE time — so it
// catches an item move, a hard-delete, a demotion and a member removal, and
// never re-reads the credential.
//
// Reproduced by hand first: on the 9839884b binary the socket was still open
// 150s after a logout and 100s after a PAT revocation, in both cases with
// `/auth/me` on that credential answering 401, and with a control leg showing
// the endpoint really does authenticate at upgrade (no header -> 401, bogus
// token -> 401).

// joinCollabWorkspace makes userID an owner of the workspace the item lives in.
// `seedCollabFixture` creates an ownerless workspace, so an admin passes on the
// cross-workspace rule while a PAT-authenticated caller does not — the PAT leg
// got a 403 before this existed, and its precondition said so rather than
// letting the test pass for the wrong reason.
func joinCollabWorkspace(t *testing.T, srv *Server, itemID, userID string) {
	t.Helper()
	item, err := srv.store.GetItem(itemID)
	if err != nil || item == nil {
		t.Fatalf("get fixture item: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(item.WorkspaceID, userID, "owner"); err != nil {
		t.Fatalf("add member: %v", err)
	}
}

// collabClosedWithin reports whether the server ended the WS within d.
func collabClosedWithin(t *testing.T, conn *websocket.Conn, d time.Duration) bool {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			// A read deadline means the connection is STILL OPEN and idle,
			// which is the defect. Anything else is the server ending it.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return false
			}
			return true
		}
	}
}

func TestCollab_StopsWhenTheSessionIsDestroyed(t *testing.T) {
	prev := collabMembershipRevalInterval
	collabMembershipRevalInterval = 50 * time.Millisecond
	t.Cleanup(func() { collabMembershipRevalInterval = prev })

	// `testServerWithCollab` rather than `testServer`: without a room manager
	// the endpoint 503s, and a refused handshake would satisfy any assertion
	// about the server closing the socket. The precondition below caught
	// exactly that on the first run.
	srv := testServerWithCollab(t)
	bootstrapFirstUser(t, srv, "collabadmin@test.com", "Admin")

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "collabuser@test.com", Name: "User", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token, err := srv.store.CreateSession(user.ID, "go-test", "127.0.0.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()
	itemID := seedCollabFixture(t, srv, "CollabLiveness")
	joinCollabWorkspace(t, srv, itemID, user.ID)

	cookies := []*http.Cookie{{Name: "pad_session", Value: token}}
	conn, resp, err := dialCollab(t, ts.URL, itemID, cookies, "go-test")
	// PRECONDITION: the connection under test must actually have opened, or
	// "the server closed it" is true of a handshake that was refused.
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial failed (status %d): %v — the connection under test never opened", status, err)
	}
	defer func() { _ = conn.Close() }()

	if err := srv.store.DeleteSession(token); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if info, err := srv.store.ValidateSession(token); err == nil && info != nil {
		t.Fatal("session still validates after DeleteSession; the test's own premise is broken")
	}

	if !collabClosedWithin(t, conn, 3*time.Second) {
		t.Fatal("collab socket still open after its session was destroyed and many revalidation ticks passed")
	}
}

func TestCollab_StopsWhenThePATIsRevoked(t *testing.T) {
	prev := collabMembershipRevalInterval
	collabMembershipRevalInterval = 50 * time.Millisecond
	t.Cleanup(func() { collabMembershipRevalInterval = prev })

	srv := testServerWithCollab(t)
	bootstrapFirstUser(t, srv, "collabadmin2@test.com", "Admin")

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "collabpat@test.com", Name: "User", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	secret, id := mintPAT(t, srv, user.ID, "collab-probe")

	ts := httptest.NewServer(srv)
	defer ts.Close()
	itemID := seedCollabFixture(t, srv, "CollabPATLiveness")
	joinCollabWorkspace(t, srv, itemID, user.ID)

	u, _ := url.Parse(ts.URL)
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+secret)
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	conn, resp, err := dialer.Dial("ws://"+u.Host+"/api/v1/collab/"+itemID+"?schema_version=1", hdr)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial failed (status %d): %v — the connection under test never opened", status, err)
	}
	defer func() { _ = conn.Close() }()

	if err := srv.store.DeleteUserAPIToken(id, user.ID); err != nil {
		t.Fatalf("revoke PAT: %v", err)
	}
	if tok, err := srv.store.ValidateToken(secret); err == nil && tok != nil {
		t.Fatal("PAT still validates after revocation; the test's own premise is broken")
	}

	if !collabClosedWithin(t, conn, 3*time.Second) {
		t.Fatal("collab socket still open after its PAT was revoked and many revalidation ticks passed")
	}
}

// COUNTERFACTUALS. Every test above asserts the server ENDS a connection, and
// all six would pass against a predicate that simply returned false — which
// would close every stream on its first tick and be a far worse defect than the
// one being fixed. These assert the other direction.

func TestEventsStream_StaysOpenWhileTheCredentialIsValid(t *testing.T) {
	prev := sseMembershipRevalInterval
	sseMembershipRevalInterval = 25 * time.Millisecond
	t.Cleanup(func() { sseMembershipRevalInterval = prev })

	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "stay@example.com", Name: "Owner", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "StayWS", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	token, err := srv.store.CreateSession(owner.ID, "test", "127.0.0.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events?workspace="+ws.Slug, nil)
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	// Many ticks at 25ms. Nothing about the credential changes.
	if streamClosedWithin(t, bufio.NewScanner(resp.Body), 1500*time.Millisecond) {
		t.Fatal("stream closed while its session was still valid — the predicate is refusing a live credential")
	}
}

func TestWatchStream_StaysOpenWhileTheCredentialIsValid(t *testing.T) {
	prev := watchListRevalInterval
	watchListRevalInterval = 50 * time.Millisecond
	t.Cleanup(func() { watchListRevalInterval = prev })

	srv := testServerWithEvents(t)
	srv.SetWatchEventsBus(watchevents.New())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "staywatch@example.com", Name: "Watcher", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token, err := srv.store.CreateSession(user.ID, "test", "127.0.0.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events/stream", nil)
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	if streamClosedWithin(t, bufio.NewScanner(resp.Body), 1500*time.Millisecond) {
		t.Fatal("watch stream closed while its session was still valid")
	}
}

func TestCollab_StaysOpenWhileTheCredentialIsValid(t *testing.T) {
	prev := collabMembershipRevalInterval
	collabMembershipRevalInterval = 50 * time.Millisecond
	t.Cleanup(func() { collabMembershipRevalInterval = prev })

	srv := testServerWithCollab(t)
	bootstrapFirstUser(t, srv, "staycollab@test.com", "Admin")
	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "staycollabuser@test.com", Name: "User", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token, err := srv.store.CreateSession(user.ID, "go-test", "127.0.0.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()
	itemID := seedCollabFixture(t, srv, "CollabStay")
	joinCollabWorkspace(t, srv, itemID, user.ID)

	conn, resp, err := dialCollab(t, ts.URL, itemID, []*http.Cookie{{Name: "pad_session", Value: token}}, "go-test")
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial failed (status %d): %v", status, err)
	}
	defer func() { _ = conn.Close() }()

	if collabClosedWithin(t, conn, 1500*time.Millisecond) {
		t.Fatal("collab socket closed while its session was still valid")
	}
}

// The fresh-install / no-auth path. A request carrying NO credential has
// nothing to invalidate, and closing there would turn this fix into an
// availability regression on exactly the deployments least able to diagnose it.
func TestEventsStream_StaysOpenWithNoCredentialAtAll(t *testing.T) {
	prev := sseMembershipRevalInterval
	sseMembershipRevalInterval = 25 * time.Millisecond
	t.Cleanup(func() { sseMembershipRevalInterval = prev })

	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// No users at all: the fresh-install window, where every request is
	// unauthenticated by design.
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "FreshWS"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events?workspace="+ws.Slug, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200 — the fresh-install path must serve this", resp.StatusCode)
	}

	if streamClosedWithin(t, bufio.NewScanner(resp.Body), 1500*time.Millisecond) {
		t.Fatal("stream closed on the fresh-install path, where there is no credential to invalidate")
	}
}

// A CLI session bearer — `padsess_...` in an Authorization header rather than a
// cookie — is a SESSION, not an API token. `TokenAuth` makes that same split at
// middleware_auth.go:107, and getting it wrong here would answer "no such
// token" for a perfectly live session and close every CLI stream on its first
// tick. Both directions, because only the pair pins the branch.
func TestEventsStream_SessionBearerIsValidatedAsASession(t *testing.T) {
	prev := sseMembershipRevalInterval
	sseMembershipRevalInterval = 25 * time.Millisecond
	t.Cleanup(func() { sseMembershipRevalInterval = prev })

	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "cli@example.com", Name: "CLI", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "CLIWS", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	token, err := srv.store.CreateSession(owner.ID, "test", "127.0.0.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if !strings.HasPrefix(token, "padsess_") {
		t.Fatalf("session token has an unexpected shape %q; this test is about the padsess_ branch", token)
	}

	open := func() *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/events?workspace="+ws.Slug, nil)
		req.Header.Set("User-Agent", testSessionUA)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("open stream: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream status = %d, want 200", resp.StatusCode)
		}
		return resp
	}

	// STAYS OPEN while the session is live. This is the leg that fails if the
	// bearer is validated as an API token.
	live := open()
	if streamClosedWithin(t, bufio.NewScanner(live.Body), 1500*time.Millisecond) {
		_ = live.Body.Close()
		t.Fatal("stream with a live session BEARER closed — the padsess_ branch is validating it as a PAT")
	}
	_ = live.Body.Close()

	// STOPS once that same session is destroyed.
	second := open()
	defer func() { _ = second.Body.Close() }()
	if err := srv.store.DeleteSession(token); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if !streamClosedWithin(t, bufio.NewScanner(second.Body), 3*time.Second) {
		t.Fatal("stream with a session BEARER stayed open after that session was destroyed")
	}
}

// The three-valued predicate, unit-tested at the seam. The eleven tests above
// drive it through real connections, which is where the wiring is proved
// (CONVE-19); these pin the two answers a live server is hard to push into on
// demand — a store that cannot answer, and a credential kind nobody taught it.

func TestCredentialLiveness_UnknownKindFailsClosed(t *testing.T) {
	// The lead's ruling, and the reason the default branch is not a fallthrough
	// to "valid": a credential kind nobody taught this predicate about is a
	// connection nobody can revoke, which is BUG-3007 under a new name. A
	// future auth path that forgets to set `ctxAuthKind` must break LOUDLY.
	srv := testServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace=x", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxAuthKind, "some_future_scheme"))

	if got := srv.credentialLiveness(req); got != credentialInvalid {
		t.Fatalf("credentialLiveness for an unknown kind = %v, want credentialInvalid", got)
	}
	if srv.streamCredentialStillValid(req) {
		t.Fatal("an unrecognised credential kind kept the stream open; the default branch must fail closed")
	}
}

func TestCredentialLiveness_NoCredentialIsValid(t *testing.T) {
	// The counterfactual for the branch above: the fresh-install window and the
	// legacy no-auth path carry no credential and have nothing to revoke.
	// Closing them would turn a security fix into an availability regression.
	srv := testServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace=x", nil)

	if got := srv.credentialLiveness(req); got != credentialValid {
		t.Fatalf("credentialLiveness with no credential = %v, want credentialValid", got)
	}
	if !srv.streamCredentialStillValid(req) {
		t.Fatal("a request with no credential was closed; there is nothing there to invalidate")
	}
}

func TestCredentialLiveness_StoreErrorKeepsTheConnection(t *testing.T) {
	// codex round 1, HIGH. The first version of this predicate was a bool, so
	// a store ERROR read as "invalid" and a transient DB blip would have closed
	// every affected stream on the next tick — a fleet-wide reconnect storm for
	// users whose credentials were fine. "Cannot ask" and "definitively gone"
	// are opposite facts and a bool collapses them.
	srv := testServer(t)

	// Close the database underneath the server: every lookup now errors rather
	// than answering "no such session".
	if err := srv.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace=x", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxAuthKind, authKindSessionBearer))
	req.Header.Set("Authorization", "Bearer padsess_"+strings.Repeat("a", 64))

	if got := srv.credentialLiveness(req); got != credentialUnknown {
		t.Fatalf("credentialLiveness with an unreachable store = %v, want credentialUnknown", got)
	}
	if !srv.streamCredentialStillValid(req) {
		t.Fatal("a store error closed the stream; a database blip is not a revocation")
	}
}

func TestValidateTokenForLiveness_DoesNotTouchLastUsed(t *testing.T) {
	// codex round 1, MEDIUM — and a meaning problem as much as a cost one:
	// `last_used_at` is what an operator reads before revoking a token, so a
	// background liveness probe bumping it would make every idle-but-connected
	// token look actively used.
	srv := testServer(t)

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "tokentouch@example.com", Name: "T", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	secret, id := mintPAT(t, srv, user.ID, "touch-probe")

	// PRECONDITION: the ordinary door DOES touch it, or "the liveness door
	// leaves it alone" is true of a column nothing ever writes.
	if _, err := srv.store.ValidateToken(secret); err != nil {
		t.Fatalf("validate: %v", err)
	}
	touched, err := srv.store.ListUserAPITokens(user.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var before *time.Time
	for _, tok := range touched {
		if tok.ID == id {
			before = tok.LastUsedAt
		}
	}
	if before == nil {
		t.Fatal("ValidateToken did not set last_used_at; this test cannot discriminate")
	}

	// The liveness door must leave it exactly where it was.
	time.Sleep(1100 * time.Millisecond) // second-resolution timestamps
	if _, err := srv.store.ValidateTokenForLiveness(secret); err != nil {
		t.Fatalf("liveness validate: %v", err)
	}
	after, err := srv.store.ListUserAPITokens(user.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, tok := range after {
		if tok.ID == id {
			if tok.LastUsedAt == nil || !tok.LastUsedAt.Equal(*before) {
				t.Fatalf("last_used_at moved from %v to %v; the liveness door must not touch it", before, tok.LastUsedAt)
			}
		}
	}
}

func TestCredentialLiveness_AResolvedPrincipalWithoutAKindFailsClosed(t *testing.T) {
	// codex round 2, P1. The empty-kind branch means "no credential on the
	// wire" — the fresh-install window and the legacy no-auth path — and it
	// KEEPS the connection. A request that carries a resolved USER but no kind
	// is a different animal wearing that branch's coat: somebody accepted a
	// credential and did not record which, so the predicate has nothing to
	// re-check and no way to notice. Both MCP accept points were in that state
	// when this was written.
	//
	// The counterfactual is TestCredentialLiveness_NoCredentialIsValid above:
	// same empty kind, no principal, connection KEPT. The two together are what
	// make this branch discriminate rather than just close things.
	srv := testServer(t)

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "nokind@example.com", Name: "N", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace=x", nil)
	req = req.WithContext(WithCurrentUser(req.Context(), user))

	// PRECONDITION: the kind really is absent, or this test proves nothing
	// about the branch it claims to exercise.
	if k := authKind(req); k != "" {
		t.Fatalf("auth kind = %q, want empty; this test cannot discriminate", k)
	}

	if got := srv.credentialLiveness(req); got != credentialInvalid {
		t.Fatalf("credentialLiveness for a principal with no auth kind = %v, want credentialInvalid", got)
	}
	if srv.streamCredentialStillValid(req) {
		t.Fatal("a resolved principal with no auth kind kept the stream open; an accept point that records a user and not its credential must not exempt itself from revalidation")
	}
}

func TestCredentialLiveness_StoreErrorRecoversOnTheNextTick(t *testing.T) {
	// codex round 2, P2. TestCredentialLiveness_StoreErrorKeepsTheConnection
	// closes the whole store, which is a PERMANENT failure — it proves the
	// classification and not the promise the classification is made for, that
	// the tick TRIES AGAIN and the connection comes back under a store that
	// recovers. A permanently-broken store cannot tell those apart.
	//
	// So break the seam transiently instead: rename the table ValidateSession
	// reads, ask, put it back, ask again. Same error path (a failing query, not
	// a closed handle), and it ends.
	//
	// SQLite leg. testServer is hardwired to storetest.NewSQLite, so this one
	// stays SQLite even under `make test-pg` — the Postgres leg below is a
	// separate test, not the same test on another backend (codex round 3).
	credentialLivenessRecovery(t, testServer(t), "sqlite")
}

func TestCredentialLiveness_StoreErrorRecoversOnTheNextTickPostgres(t *testing.T) {
	// The Postgres leg of the test above. The predicate is dialect-independent
	// but the INSTRUMENT is not — `ALTER TABLE ... RENAME TO` and the error a
	// missing table produces are both dialect-specific, and a store error
	// classified as `unknown` on SQLite would be worth nothing if Postgres
	// surfaced it as something else. Skips when PAD_TEST_POSTGRES_URL is unset.
	srv, _ := testServerPostgres(t)
	credentialLivenessRecovery(t, srv, "postgres")
}

// credentialLivenessRecovery drives one store through the outage-and-back
// sequence. Shared so the two legs cannot drift.
func credentialLivenessRecovery(t *testing.T, srv *Server, backend string) {
	t.Helper()

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "transient-" + backend + "@example.com", Name: "T", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token, err := srv.store.CreateSession(user.ID, "test", "127.0.0.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace=x", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxAuthKind, authKindSessionBearer))
	req.Header.Set("Authorization", "Bearer "+token)

	// PRECONDITION: valid BEFORE the outage, or "it recovered" is a claim about
	// a credential that was never good.
	if got := srv.credentialLiveness(req); got != credentialValid {
		t.Fatalf("credentialLiveness before the outage = %v, want credentialValid", got)
	}

	db := srv.store.DB()
	if _, err := db.Exec("ALTER TABLE sessions RENAME TO sessions_outage"); err != nil {
		t.Fatalf("hide sessions table: %v", err)
	}
	// Restore registered IMMEDIATELY, not after the assertions (codex round 3,
	// P2): a t.Fatalf between here and the manual restore would otherwise leave
	// the schema renamed for whatever shares this database. Idempotent, so the
	// manual restore below can still happen first.
	restored := false
	restore := func() {
		if restored {
			return
		}
		if _, err := db.Exec("ALTER TABLE sessions_outage RENAME TO sessions"); err != nil {
			t.Errorf("restore sessions table: %v", err)
			return
		}
		restored = true
	}
	t.Cleanup(restore)

	if got := srv.credentialLiveness(req); got != credentialUnknown {
		t.Fatalf("credentialLiveness during the outage = %v, want credentialUnknown", got)
	}
	if !srv.streamCredentialStillValid(req) {
		t.Fatal("a transient store error closed the stream; a database blip is not a revocation")
	}

	restore()
	if got := srv.credentialLiveness(req); got != credentialValid {
		t.Fatalf("credentialLiveness after the store recovered = %v, want credentialValid; the tick must not latch", got)
	}

	// And the recovered tick still ANSWERS: destroy the session and the same
	// predicate closes. Without this the test would pass on a predicate that
	// returned valid unconditionally after an error.
	if err := srv.store.DeleteSession(token); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if got := srv.credentialLiveness(req); got != credentialInvalid {
		t.Fatalf("credentialLiveness after recovery + revocation = %v, want credentialInvalid", got)
	}
}

func TestMCPBearerAuth_PATPathRecordsTheAPITokenKind(t *testing.T) {
	// codex round 2, P1. The MCP transport is a second accept point: it
	// resolves a principal without going through TokenAuth, so it has to record
	// the credential kind itself or the liveness predicate sees a user it
	// cannot re-check. The PAT branch needs no new door — the credential IS a
	// PAT in the Authorization header — so it records `api_token` and the
	// existing door revalidates it.
	//
	// The OAuth branch deliberately records nothing (no liveness door for an
	// opaque fosite token yet) and is covered by the fail-closed guard in
	// TestCredentialLiveness_AResolvedPrincipalWithoutAKindFailsClosed.
	srv := testServer(t)

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "mcpkind@example.com", Name: "M", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	secret, id := mintPAT(t, srv, user.ID, "mcp-kind")

	var seenKind string
	var seenUser *models.User
	var seenLiveness credentialLiveness
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seenKind = authKind(r)
		seenUser = currentUser(r)
		seenLiveness = srv.credentialLiveness(r)
	})

	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		req.RemoteAddr = "192.0.2.1:1234"
		rr := httptest.NewRecorder()
		srv.MCPBearerAuth(inner).ServeHTTP(rr, req)
		return rr
	}

	// PRECONDITION: the middleware ADMITTED the call. A 401 would leave every
	// assertion below reading the zero value of a handler that never ran.
	if rr := call(); rr.Code != http.StatusOK {
		t.Fatalf("MCPBearerAuth rejected a valid PAT: %d %s", rr.Code, rr.Body.String())
	}
	if seenUser == nil {
		t.Fatal("MCPBearerAuth admitted the call without resolving a principal; this test cannot discriminate")
	}
	if seenKind != authKindAPIToken {
		t.Fatalf("auth kind on an MCP PAT request = %q, want %q", seenKind, authKindAPIToken)
	}
	if seenLiveness != credentialValid {
		t.Fatalf("credentialLiveness on a live MCP PAT request = %v, want credentialValid", seenLiveness)
	}

	// And the recorded kind is the one that makes revocation reachable: revoke
	// the PAT and the same request is now closable. Without this the test would
	// pass on a kind that was merely spelled right.
	if err := srv.store.DeleteUserAPIToken(id, user.ID); err != nil {
		t.Fatalf("revoke PAT: %v", err)
	}
	rr := call()
	if rr.Code == http.StatusOK && seenLiveness != credentialInvalid {
		t.Fatalf("credentialLiveness after the MCP PAT was revoked = %v, want credentialInvalid", seenLiveness)
	}
}
