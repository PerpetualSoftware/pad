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
