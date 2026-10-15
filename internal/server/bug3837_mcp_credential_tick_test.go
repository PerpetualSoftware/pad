package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/oauth"
)

// BUG-3837: a long-lived MCP stream ends with its credential. The transport
// here is a stub that commits an event stream and then holds it until its
// context ends, which is how mcp-go's GET and listen loops leave.

type bug3837Fixture struct {
	srv      *Server
	user     *models.User
	pat      *models.APITokenWithSecret
	access   string
	refresh  string
	clientID string
	reqID    chan string // OAuth request ids seen by the transport
	// emit makes the transport write notification frames until its context
	// ends, one Write per frame, as mcp-go's writeSSEEvent does.
	emit atomic.Bool
}

func newBug3837Fixture(t *testing.T, accessLifespan time.Duration) *bug3837Fixture {
	t.Helper()
	srv := testServer(t)
	srv.SetCloudMode("test-secret")
	srv.mcpCredentialRevalOverride = 5 * time.Millisecond
	f := &bug3837Fixture{srv: srv, reqID: make(chan string, 16)}
	srv.SetMCPTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kind, ref := MCPTokenIdentityFromContext(r.Context()); kind == "oauth" {
			f.reqID <- ref
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for f.emit.Load() {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Millisecond):
			}
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", `{"jsonrpc":"2.0","method":"notifications/message"}`)
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done()
	}), testCanonicalAudience, testAuthServerURL, nil)
	o, err := oauth.NewServer(oauth.Config{
		Store:               srv.store,
		HMACSecret:          bytes32ForTest(),
		AllowedAudience:     testCanonicalAudience,
		AccessTokenLifespan: accessLifespan,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.SetOAuthServer(o)

	user, sessionToken := loginTestUser(t, srv)
	f.user = user
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	tokens := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID,
		"verifier-bug3837-quick-brown-fox-jumps-over-lazy-dog-12")
	f.clientID = clientID
	f.access, _ = tokens["access_token"].(string)
	f.refresh, _ = tokens["refresh_token"].(string)
	if f.access == "" || f.refresh == "" {
		t.Fatalf("missing access or refresh token: %v", tokens)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "BUG-3837"})
	if err != nil {
		t.Fatal(err)
	}
	f.pat, err = srv.store.CreateAPIToken(user.ID, models.APITokenCreate{Name: "bug3837", WorkspaceID: ws.ID}, 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// open starts a long-lived MCP request (a GET stream, or a listen POST) and
// returns a channel closed when its handler returns.
func (f *bug3837Fixture) open(t *testing.T, bearer string, listen bool) <-chan struct{} {
	t.Helper()
	return f.openAs(t, "Bearer", bearer, listen)
}

func (f *bug3837Fixture) openWithScheme(t *testing.T, scheme, bearer string) <-chan struct{} {
	t.Helper()
	return f.openAs(t, scheme, bearer, false)
}

func (f *bug3837Fixture) openAs(t *testing.T, scheme, bearer string, listen bool) <-chan struct{} {
	t.Helper()
	done, _ := f.openRecorded(t, scheme, bearer, listen)
	return done
}

// openRecorded is openAs, and also returns the response recorder. Read its
// body only after done is closed.
func (f *bug3837Fixture) openRecorded(t *testing.T, scheme, bearer string, listen bool) (<-chan struct{}, *deadlineRecorder) {
	t.Helper()
	rec := newDeadlineRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := httptest.NewRequest("GET", "/mcp", nil)
	if listen {
		req = httptest.NewRequest("POST", "/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	req.Header.Set("Authorization", scheme+" "+bearer)
	req.RemoteAddr = "192.0.2.1:1234"
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.srv.ServeHTTP(rec, req)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.srv.admission().heldTotal() == 0 {
		select {
		case <-done:
			t.Fatal("the stream ended before it took a slot")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the stream never took a slot")
		}
		time.Sleep(time.Millisecond)
	}
	return done, rec
}

func (f *bug3837Fixture) assertEnds(t *testing.T, done <-chan struct{}, why string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: the stream is still open", why)
	}
	if held := f.srv.admission().heldTotal(); held != 0 {
		t.Fatalf("%s: slots held after the stream ended = %d, want 0", why, held)
	}
}

func (f *bug3837Fixture) assertStaysOpen(t *testing.T, done <-chan struct{}, ticks int) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("a live credential's stream ended within %d ticks", ticks)
	case <-time.After(time.Duration(ticks) * f.srv.mcpCredentialRevalOverride):
	}
}

func TestMCP_BUG3837_RevokedPATEndsGETAndListen(t *testing.T) {
	for _, listen := range []bool{false, true} {
		name := map[bool]string{false: "GET", true: "listen"}[listen]
		t.Run(name, func(t *testing.T) {
			f := newBug3837Fixture(t, 0)
			done := f.open(t, f.pat.Token, listen)
			f.assertStaysOpen(t, done, 20)
			if err := f.srv.store.DeleteAPITokenScoped(f.pat.ID, f.pat.WorkspaceID); err != nil {
				t.Fatal(err)
			}
			f.assertEnds(t, done, "PAT revoked")
		})
	}
}

func TestMCP_BUG3837_RevokedOAuthGrantEndsGETAndListen(t *testing.T) {
	for _, listen := range []bool{false, true} {
		name := map[bool]string{false: "GET", true: "listen"}[listen]
		t.Run(name, func(t *testing.T) {
			f := newBug3837Fixture(t, 0)
			done := f.open(t, f.access, listen)
			// A live OAuth stream survives its ticks: without the OAuth
			// door it reached the no-kind branch and closed on the first.
			f.assertStaysOpen(t, done, 20)
			if err := f.srv.store.RevokeOAuthGrant(<-f.reqID); err != nil {
				t.Fatal(err)
			}
			f.assertEnds(t, done, "OAuth grant revoked")
		})
	}
}

// The lifespan leaves the fixture's setup seconds of margin before the token
// expires (hunter r1); the stream must be admitted on a live token.
func TestMCP_BUG3837_ExpiredOAuthTokenEndsTheStream(t *testing.T) {
	f := newBug3837Fixture(t, 5*time.Second)
	done := f.open(t, f.access, false)
	f.assertStaysOpen(t, done, 20)
	f.assertEnds(t, done, "OAuth access token expired")
}

// OAuth, because the PAT door already refuses a disabled user's token and
// fosite's introspection does not look at the user at all.
func TestMCP_BUG3837_DisabledUserEndsTheStream(t *testing.T) {
	f := newBug3837Fixture(t, 0)
	done := f.open(t, f.access, false)
	f.assertStaysOpen(t, done, 20)
	// Disabled directly, with the credential left live, so only the user
	// re-check can end the stream.
	if _, err := f.srv.store.DB().Exec(`UPDATE users SET disabled_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), f.user.ID); err != nil {
		t.Fatal(err)
	}
	f.assertEnds(t, done, "user disabled")
}

// A tick that cannot ask (a storage failure under fosite, which it wraps in
// the same outer error as a revoked token) keeps the stream, as the session
// and PAT doors do: closing every OAuth stream on a database blip would be an
// outage, not a revocation.
func TestMCP_BUG3837_OAuthStoreFailureKeepsTheStream(t *testing.T) {
	f := newBug3837Fixture(t, 0)
	done := f.open(t, f.access, false)
	exec := func(q string) {
		t.Helper()
		if _, err := f.srv.store.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`ALTER TABLE oauth_access_tokens RENAME TO oauth_access_tokens_bug3837`)
	f.assertStaysOpen(t, done, 20)
	exec(`ALTER TABLE oauth_access_tokens_bug3837 RENAME TO oauth_access_tokens`)
	if err := f.srv.store.RevokeOAuthGrant(<-f.reqID); err != nil {
		t.Fatal(err)
	}
	f.assertEnds(t, done, "OAuth grant revoked after the store recovered")
}

// A deleted token row (fosite ErrNotFound) ends the stream.
func TestMCP_BUG3837_DeletedOAuthTokenEndsTheStream(t *testing.T) {
	f := newBug3837Fixture(t, 0)
	done := f.open(t, f.access, false)
	f.assertStaysOpen(t, done, 20)
	if _, err := f.srv.store.DB().Exec(`DELETE FROM oauth_access_tokens WHERE request_id = ?`, <-f.reqID); err != nil {
		t.Fatal(err)
	}
	f.assertEnds(t, done, "OAuth access token row deleted")
}

// The bearer scheme is case-insensitive (RFC 6750), and admission accepts
// "bearer <tok>". The tick must read the token the same way (hunter r1). A
// case-sensitive read fails one of two ways: the PAT door finds no token and
// closes a live stream, and fosite rejects the whole header as a malformed
// token, which is "could not ask", so a REVOKED OAuth stream stays open. Each
// subtest therefore checks both halves: open across ticks, then ended by a
// revocation.
func TestMCP_BUG3837_LowercaseBearerIsReadLikeAdmission(t *testing.T) {
	t.Run("pat", func(t *testing.T) {
		f := newBug3837Fixture(t, 0)
		done := f.openWithScheme(t, "bearer", f.pat.Token)
		f.assertStaysOpen(t, done, 20)
		if err := f.srv.store.DeleteAPITokenScoped(f.pat.ID, f.pat.WorkspaceID); err != nil {
			t.Fatal(err)
		}
		f.assertEnds(t, done, "PAT revoked")
	})
	t.Run("oauth", func(t *testing.T) {
		f := newBug3837Fixture(t, 0)
		done := f.openWithScheme(t, "bearer", f.access)
		f.assertStaysOpen(t, done, 20)
		if err := f.srv.store.RevokeOAuthGrant(<-f.reqID); err != nil {
			t.Fatal(err)
		}
		f.assertEnds(t, done, "OAuth grant revoked")
	})
}

// A deleted user ends the stream, not "could not ask": GetUser reports a
// missing row as (nil, nil), and the tick reads nil as gone. Driven directly,
// because deleting a user cascades to its tokens, so the credential door
// would end the stream first and hide this branch.
func TestMCP_BUG3837_DeletedUserIsNotStillValid(t *testing.T) {
	f := newBug3837Fixture(t, 0)
	if u, err := f.srv.store.GetUser("no-such-user"); err != nil || u != nil {
		t.Fatalf("GetUser(missing) = %v, %v; want nil, nil (the premise)", u, err)
	}
	req := httptest.NewRequest("GET", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+f.pat.Token)
	ctx := WithCurrentUser(req.Context(), &models.User{ID: "no-such-user"})
	ctx = context.WithValue(ctx, ctxAuthKind, authKindAPIToken)
	req = req.WithContext(ctx)
	if !f.srv.streamCredentialStillValid(req) {
		t.Fatal("instrument: the PAT itself must still be live")
	}
	if f.srv.mcpStreamPrincipalStillValid(req) {
		t.Fatal("a stream whose user no longer exists is still valid")
	}
}

// The lead's KEEP ruling on rotation (on BUG-3837): a refresh rotation flips
// the old access token inactive, so a live stream opened with it ends at its
// next tick, even though the grant is live. Pinned with its three conditions:
// it ends within one tick of the rotation, it ends between frames (the tick
// only cancels the context, and the transport sees that between writes), and
// the rotated-in token opens a fresh stream that stays open.
func TestMCP_BUG3837_RefreshRotationEndsTheStreamAndTheNewTokenOpensOne(t *testing.T) {
	f := newBug3837Fixture(t, 0)
	// A tick long enough to measure "within one" against, short enough to run.
	const tick = 400 * time.Millisecond
	f.srv.mcpCredentialRevalOverride = tick
	f.emit.Store(true)
	done, rec := f.openRecorded(t, "Bearer", f.access, false)
	// Past the jittered first tick, so the next one is at most a tick away.
	f.assertStaysOpen(t, done, 2)

	code, body := bug3678Refresh(f.srv, f.clientID, f.refresh)
	rotated := time.Now()
	if code != http.StatusOK {
		t.Fatalf("refresh: status %d: %v", code, body)
	}
	newAccess, _ := body["access_token"].(string)
	if newAccess == "" || newAccess == f.access {
		t.Fatalf("refresh minted no new access token: %v", body)
	}
	f.assertEnds(t, done, "access token rotated out by a refresh")
	// One tick, plus scheduling and one introspection's worth of slack.
	if took := time.Since(rotated); took > tick+600*time.Millisecond {
		t.Fatalf("the stream ended %v after the rotation, want within one tick (%v)", took, tick)
	}

	frames := rec.Body.String()
	if n := strings.Count(frames, "event: message\n"); n == 0 {
		t.Fatal("instrument: the transport wrote no frames before the stream ended")
	}
	if !strings.HasSuffix(frames, "\n\n") {
		t.Fatalf("the stream ended mid-frame: body ends %q", frames[max(0, len(frames)-80):])
	}
	for _, frame := range strings.Split(strings.TrimSuffix(frames, "\n\n"), "\n\n") {
		if frame != `event: message`+"\n"+`data: {"jsonrpc":"2.0","method":"notifications/message"}` &&
			!strings.HasPrefix(frame, ":") {
			t.Fatalf("a half-written or foreign frame on the stream: %q", frame)
		}
	}

	f.emit.Store(false)
	fresh := f.open(t, newAccess, false)
	f.assertStaysOpen(t, fresh, 3)
}
