package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/gorilla/websocket"
)

// BUG-1308. Collab WebSocket dials used to draw on the general per-user API
// bucket (burst 60), so 100 dials from one user got 60 through, and a user's
// own dials and REST calls refused each other. They now draw on their own
// bucket, and how many sockets a principal may HOLD is a separate gate.

// collabDial sends one collab dial through the whole router (RateLimit
// included) as a plain GET. The item does not exist, so a dial that passes
// the limiter is answered 404 by the handler; 429 means the limiter refused.
func collabDialStatus(srv *Server) int {
	req := httptest.NewRequest("GET", "/api/v1/collab/00000000-0000-0000-0000-000000000000", nil)
	req.RemoteAddr = "192.0.2.7:1234"
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr.Code
}

func restStatus(srv *Server) int {
	req := httptest.NewRequest("GET", "/api/v1/workspaces", nil)
	req.RemoteAddr = "192.0.2.7:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr.Code
}

// The discriminating assertion is the SECOND loop: after a user has spent
// every collab dial token, their REST calls still have the whole API burst.
// Before the fix the dials had already spent it.
func TestCollabDialsDoNotSpendTheAPIBucket(t *testing.T) {
	srv := testServer(t)
	burst := srv.rateLimiters.CollabDial.config.Burst
	if burst != 50 || srv.rateLimiters.CollabDial.config.Rate != 5 {
		t.Fatalf("collab dial bucket = %v/s burst %d, want 5/s burst 50 (BUG-1308 checkpoint 2)",
			srv.rateLimiters.CollabDial.config.Rate, burst)
	}

	refused := 0
	for i := 0; i < burst+10; i++ {
		switch code := collabDialStatus(srv); code {
		case http.StatusNotFound:
		case http.StatusTooManyRequests:
			refused++
		default:
			t.Fatalf("collab dial %d: unexpected status %d", i, code)
		}
	}
	if refused == 0 {
		t.Fatalf("%d dials against a burst of %d: none refused, so the collab bucket is not applied", burst+10, burst)
	}

	apiBurst := srv.rateLimiters.API.config.Burst
	for i := 0; i < apiBurst; i++ {
		if code := restStatus(srv); code == http.StatusTooManyRequests {
			t.Fatalf("REST call %d of the API burst of %d was refused after collab dials: the dials drew on the API bucket", i+1, apiBurst)
		}
	}
}

// And the reverse: a user who has spent their API burst can still dial.
func TestRESTCallsDoNotSpendTheCollabDialBucket(t *testing.T) {
	srv := testServer(t)
	for i := 0; i < srv.rateLimiters.API.config.Burst+20; i++ {
		restStatus(srv)
	}
	if code := restStatus(srv); code != http.StatusTooManyRequests {
		t.Fatalf("precondition: the API bucket should be spent, got %d", code)
	}
	if code := collabDialStatus(srv); code != http.StatusNotFound {
		t.Fatalf("a collab dial after the API burst was spent answered %d, want it to reach the handler (404)", code)
	}
}

// Only an upgrade request is a dial. A plain request under the prefix is an
// ordinary API request and pays the API bucket, so it cannot be used to spend
// the dial budget in place of the API one, or to escape the API one.
func TestNonUpgradeCollabRequestPaysTheAPIBucket(t *testing.T) {
	srv := testServer(t)
	plain := func() int {
		req := httptest.NewRequest("GET", "/api/v1/collab/00000000-0000-0000-0000-000000000000", nil)
		req.RemoteAddr = "192.0.2.7:1234"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr.Code
	}
	apiBurst := srv.rateLimiters.API.config.Burst
	refused := 0
	for i := 0; i < apiBurst+10; i++ {
		if plain() == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Fatalf("%d plain requests under /api/v1/collab/ against an API burst of %d: none refused, so they are not paying the API bucket", apiBurst+10, apiBurst)
	}
	if code := collabDialStatus(srv); code != http.StatusNotFound {
		t.Fatalf("a real dial after plain requests spent the API bucket answered %d; the dial bucket should be untouched", code)
	}
}

// The socket gate: a principal holding PAD_COLLAB_MAX_PER_USER sockets is
// refused another with 429 collab_limit_exceeded and a Retry-After, and gets
// its slot back when a socket closes. Fresh-install mode, so the principal is
// the item's workspace (streamPrincipal).
func TestCollabSocketCapPerPrincipal(t *testing.T) {
	srv := testServerWithCollab(t)
	srv.SetCollabLimits(2)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	itemA := seedCollabFixture(t, srv, "CapA")
	itemB := seedCollabFixture(t, srv, "CapB")

	var held []*websocket.Conn
	for i := 0; i < 2; i++ {
		conn, resp, err := dialCollab(t, ts.URL, itemA, nil, "")
		if err != nil {
			t.Fatalf("dial %d under the cap: %v (%v)", i, err, resp)
		}
		held = append(held, conn)
	}
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()

	_, resp, err := dialCollab(t, ts.URL, itemA, nil, "")
	if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third socket for one principal: want 429, got err=%v resp=%v", err, resp)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if derr := json.NewDecoder(resp.Body).Decode(&body); derr != nil || body.Error.Code != "collab_limit_exceeded" {
		t.Errorf("refusal code = %q (%v), want collab_limit_exceeded", body.Error.Code, derr)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Errorf("refusal carries no Retry-After")
	}

	// Another principal is not affected by this one's sockets.
	other, resp, err := dialCollab(t, ts.URL, itemB, nil, "")
	if err != nil {
		t.Fatalf("a different principal was refused by another's cap: %v (%v)", err, resp)
	}
	other.Close()

	// Closing one socket gives its slot back.
	_ = held[0].WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"), time.Now().Add(time.Second))
	held[0].Close()
	held = held[1:]
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, _, err := dialCollab(t, ts.URL, itemA, nil, "")
		if err == nil {
			held = append(held, conn)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the slot was never released after a socket closed: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Two authenticated users in ONE workspace hold separate caps: the principal
// is the user, not the workspace. A key that fell back to the workspace for
// everyone would pass the fresh-install test above (its items sit in two
// workspaces) and fail here.
func TestCollabSocketCapIsPerUserWithinAWorkspace(t *testing.T) {
	srv := testServerWithCollab(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	srv.SetCollabLimits(1)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	itemID := seedCollabFixture(t, srv, "SharedWS")
	item, err := srv.store.GetItem(itemID)
	if err != nil || item == nil {
		t.Fatalf("GetItem: %v", err)
	}
	session := func(email string) []*http.Cookie {
		u, err := srv.store.CreateUser(models.UserCreate{Email: email, Name: email, Password: "correct-horse-battery-staple", Role: "member"})
		if err != nil {
			t.Fatalf("CreateUser %s: %v", email, err)
		}
		if err := srv.store.AddWorkspaceMember(item.WorkspaceID, u.ID, "editor"); err != nil {
			t.Fatalf("AddWorkspaceMember %s: %v", email, err)
		}
		tok, err := srv.store.CreateSession(u.ID, "go-test", "127.0.0.1", "go-test", 24*time.Hour)
		if err != nil {
			t.Fatalf("CreateSession %s: %v", email, err)
		}
		return []*http.Cookie{{Name: "pad_session", Value: tok}}
	}
	alice, bob := session("alice@test.com"), session("bob@test.com")

	a1, resp, err := dialCollab(t, ts.URL, itemID, alice, "go-test")
	if err != nil {
		t.Fatalf("alice's first socket: %v (%v)", err, resp)
	}
	defer a1.Close()
	if _, resp, err := dialCollab(t, ts.URL, itemID, alice, "go-test"); err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("alice's second socket at a cap of 1: want 429, got err=%v resp=%v", err, resp)
	}
	b1, resp, err := dialCollab(t, ts.URL, itemID, bob, "go-test")
	if err != nil {
		t.Fatalf("bob was refused by alice's cap in the same workspace: %v (%v)", err, resp)
	}
	b1.Close()
}
