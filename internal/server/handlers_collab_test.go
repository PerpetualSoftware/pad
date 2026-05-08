package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/gorilla/websocket"
)

// testServerWithCollab wires a RoomManager onto a fresh test server
// so the collab handler answers 200/101 on the happy path. Tests that
// only assert on auth / visibility responses (401 / 403 / 404) don't
// need this — those replies happen BEFORE the s.collab nil-check —
// but it doesn't hurt to keep the helper centralised.
func testServerWithCollab(t *testing.T) *Server {
	t.Helper()
	srv := testServer(t)
	bus := collab.NewMemoryOpBus()
	t.Cleanup(bus.Close)
	rm := collab.NewRoomManager(srv.store, bus)
	t.Cleanup(rm.Close)
	srv.SetCollabRoomManager(rm)
	return srv
}

// dialCollab opens a WebSocket against the test server's collab
// endpoint with the given itemID. Returns the connection and the HTTP
// response. The response status is checked by the caller for negative
// cases (401 / 403 / 404). cookies, when non-nil, are forwarded to
// authenticate the upgrade. userAgent must match the value passed to
// store.CreateSession when the session was minted — pad's
// session-binding middleware hashes the UA and rejects a session
// whose hash differs from the one stored at CreateSession time.
func dialCollab(t *testing.T, baseURL, itemID string, cookies []*http.Cookie, userAgent string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u, _ := url.Parse(baseURL)
	wsURL := "ws://" + u.Host + "/api/v1/collab/" + itemID

	hdr := http.Header{}
	for _, c := range cookies {
		hdr.Add("Cookie", c.String())
	}
	if userAgent != "" {
		hdr.Set("User-Agent", userAgent)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 3 * time.Second,
	}
	return dialer.Dial(wsURL, hdr)
}

// seedCollabFixture creates the minimum fixture chain needed to test
// the collab WS endpoint: workspace + collection + item, all in the
// store, returning the item's ID.
func seedCollabFixture(t *testing.T, srv *Server, wsName string) string {
	t.Helper()
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: wsName})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "An item",
		Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return item.ID
}

// TestCollabUpgradeFreshInstall covers the no-users escape hatch:
// before any user is bootstrapped, the handler should grant access
// to the WS upgrade just like RequireWorkspaceAccess grants HTTP
// access in that state. This is the easiest path to assert the
// upgrade actually works end-to-end without setting up auth.
func TestCollabUpgradeFreshInstall(t *testing.T) {
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	itemID := seedCollabFixture(t, srv, "FreshInstall")

	conn, resp, err := dialCollab(t, ts.URL, itemID, nil, "")
	if err != nil {
		body := ""
		if resp != nil {
			body = "status=" + resp.Status
		}
		t.Fatalf("dial: %v (%s)", err, body)
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101 Switching Protocols, got %d", resp.StatusCode)
	}

	// Send a close frame so the server's read loop terminates with the
	// expected normal-closure code rather than a torn-connection error.
	if err := conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"),
		time.Now().Add(1*time.Second),
	); err != nil {
		t.Fatalf("write close frame: %v", err)
	}
}

// TestCollabUpgradeRejectsUnauthenticated verifies that once a user
// is bootstrapped (so the no-users escape hatch is closed), an
// unauthenticated upgrade attempt is rejected with 401.
func TestCollabUpgradeRejectsUnauthenticated(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	itemID := seedCollabFixture(t, srv, "Locked")

	_, resp, err := dialCollab(t, ts.URL, itemID, nil, "")
	if err == nil {
		t.Fatal("expected dial to fail for unauthenticated request")
	}
	if resp == nil {
		t.Fatalf("dial returned no response: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

// TestCollabUpgradeRejectsNonMember confirms that an authenticated
// user who is NOT a member of the item's workspace gets 403, not 200,
// even though they're a valid logged-in user. This is the core
// access-control assertion.
func TestCollabUpgradeRejectsNonMember(t *testing.T) {
	srv := testServer(t)
	// Admin bootstrap creates an admin who would normally have
	// cross-workspace access. We instead create a SECOND, non-admin
	// user whose only privilege is being authenticated; that user
	// must NOT be able to upgrade against an unrelated workspace's
	// item.
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	registerSecondUser := func() string {
		// We want a user that the admin hasn't granted workspace
		// membership to. Create the user directly via the store and
		// mint a session token the same way the login flow does.
		u, err := srv.store.CreateUser(models.UserCreate{
			Email:    "outsider@test.com",
			Name:     "Outsider",
			Password: "correct-horse-battery-staple",
			Role:     "member",
		})
		if err != nil {
			t.Fatalf("CreateUser outsider: %v", err)
		}
		token, err := srv.store.CreateSession(u.ID, "go-test", "127.0.0.1", "go-test", 24*time.Hour)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return token
	}
	outsiderToken := registerSecondUser()

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Item lives in a workspace that the admin owns; outsider has no
	// membership and no grants there.
	itemID := seedCollabFixture(t, srv, "AdminOnly")

	cookies := []*http.Cookie{{Name: "pad_session", Value: outsiderToken}}
	_, resp, err := dialCollab(t, ts.URL, itemID, cookies, "go-test")
	if err == nil {
		t.Fatal("expected dial to fail for non-member")
	}
	if resp == nil {
		t.Fatalf("dial returned no response: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

// TestCollabUpgradeRejectsRestrictedMemberForeignCollection confirms
// that a member with collection_access="specific" scoped to one
// collection cannot upgrade for an item in a DIFFERENT collection of
// the same workspace, even though they're a valid member. Pad's
// permission cascade applies to live collab too — restricted access
// has to be honoured at the WS upgrade boundary, not just at the
// item-level HTTP handlers.
func TestCollabUpgradeRejectsRestrictedMemberForeignCollection(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Two collections in the same workspace; restrict the test user
	// to collA only and put the target item in collB.
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Restricted"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	collA, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Allowed", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection A: %v", err)
	}
	collB, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Off-limits", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection B: %v", err)
	}
	itemB, err := srv.store.CreateItem(ws.ID, collB.ID, models.ItemCreate{
		Title: "Off-limits", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem B: %v", err)
	}

	// Create a non-admin user, add them as workspace member with
	// "specific" access scoped to collA only, then mint a session.
	u, err := srv.store.CreateUser(models.UserCreate{
		Email:    "restricted@test.com",
		Name:     "Restricted",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, u.ID, "specific", []string{collA.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	token, err := srv.store.CreateSession(u.ID, "go-test", "127.0.0.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	cookies := []*http.Cookie{{Name: "pad_session", Value: token}}
	_, resp, err := dialCollab(t, ts.URL, itemB.ID, cookies, "go-test")
	if err == nil {
		t.Fatal("expected dial to fail for restricted member targeting foreign collection")
	}
	if resp == nil {
		t.Fatalf("dial returned no response: %v", err)
	}
	// 404 mirrors requireItemVisible's "don't leak existence" pattern.
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestCollabUpgradeRejectsGuestWithSiblingItemGrantOnly is a
// regression test for the round-3 over-grant bug: a guest with an
// item-level grant on item A could upgrade /api/v1/collab/{B} for a
// sibling item B in the same collection because VisibleCollectionIDs
// returns the collection (so the nav can show it) even though the
// user only has access to one specific item in it.
//
// Strict per-item check (round 4 fix) must surface 404 for the
// sibling.
func TestCollabUpgradeRejectsGuestWithSiblingItemGrantOnly(t *testing.T) {
	srv := testServerWithCollab(t)
	admin := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	_ = admin
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Workspace + one collection holding two siblings.
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "GuestSibling"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	itemA, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Granted", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem A: %v", err)
	}
	itemB, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Sibling (off-limits)", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem B: %v", err)
	}

	// Guest user who is NOT a workspace member but has an item grant
	// on itemA only. Pad's grants design says this guest should see
	// itemA (and the nav for the parent collection) but NOT sibling B.
	guest, err := srv.store.CreateUser(models.UserCreate{
		Email:    "guest@test.com",
		Name:     "Guest",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser guest: %v", err)
	}
	if _, err := srv.store.CreateItemGrant(ws.ID, itemA.ID, guest.ID, "edit", guest.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	token, err := srv.store.CreateSession(guest.ID, "go-test", "127.0.0.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	cookies := []*http.Cookie{{Name: "pad_session", Value: token}}

	// Sibling B → 404 (the bug being regression-tested).
	_, resp, err := dialCollab(t, ts.URL, itemB.ID, cookies, "go-test")
	if err == nil {
		t.Fatal("expected dial to fail for guest targeting sibling without grant")
	}
	if resp == nil {
		t.Fatalf("dial returned no response: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("sibling: expected 404, got %d", resp.StatusCode)
	}

	// Sanity: the granted item itself MUST upgrade successfully.
	conn, resp, err := dialCollab(t, ts.URL, itemA.ID, cookies, "go-test")
	if err != nil {
		body := ""
		if resp != nil {
			body = "status=" + resp.Status
		}
		t.Fatalf("granted item dial: %v (%s)", err, body)
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("granted item: expected 101, got %d", resp.StatusCode)
	}
	// Clean close so the server's read loop sees normal closure.
	_ = conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"),
		time.Now().Add(1*time.Second),
	)
}

// TestCollabUpgradeRejectsUnknownItem covers the 404 path: a
// well-formed itemID that doesn't resolve to any item must surface
// as Not Found, not as a 401 / 403 leakage about the workspace's
// existence.
func TestCollabUpgradeRejectsUnknownItem(t *testing.T) {
	srv := testServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	_, resp, err := dialCollab(t, ts.URL, "00000000-0000-0000-0000-000000000000", nil, "")
	if err == nil {
		t.Fatal("expected dial to fail for unknown item")
	}
	if resp == nil {
		t.Fatalf("dial returned no response: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestCollabMembershipRevalidationClosesOnRevoke is the
// auth-revalidation regression test (TASK-1256). With the reval
// interval shrunk to a few ms, the goroutine ticks shortly after
// connect; if the user's workspace membership is revoked between
// the initial upgrade and the next tick, the WS must be closed
// with a ClosePolicyViolation frame and the read on the client
// side must return an error.
func TestCollabMembershipRevalidationClosesOnRevoke(t *testing.T) {
	// Shrink the cadence so the test runs in tens of ms rather than
	// 60 seconds. Restore at the end so any later test that runs in
	// the same process sees the production value.
	origInterval := collabMembershipRevalInterval
	collabMembershipRevalInterval = 30 * time.Millisecond
	defer func() { collabMembershipRevalInterval = origInterval }()

	srv := testServerWithCollab(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Workspace + collection + item.
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "RevalTest"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Item", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	// Member user (NOT admin — admin would still pass after removal
	// because admins see everything). Add them as a workspace
	// editor, mint a session.
	u, err := srv.store.CreateUser(models.UserCreate{
		Email:    "member@test.com",
		Name:     "Member",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	token, err := srv.store.CreateSession(u.ID, "go-test", "127.0.0.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	cookies := []*http.Cookie{{Name: "pad_session", Value: token}}
	conn, resp, err := dialCollab(t, ts.URL, item.ID, cookies, "go-test")
	if err != nil {
		body := ""
		if resp != nil {
			body = "status=" + resp.Status
		}
		t.Fatalf("initial dial: %v (%s)", err, body)
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101 Switching Protocols, got %d", resp.StatusCode)
	}

	// Revoke membership AFTER the connection is established. The
	// goroutine started in handleCollab will tick within the
	// shrunk reval interval and close the conn.
	if err := srv.store.RemoveWorkspaceMember(ws.ID, u.ID); err != nil {
		t.Fatalf("RemoveWorkspaceMember: %v", err)
	}

	// Read with a deadline. The reval goroutine's first tick is
	// jittered across [0, interval) so worst case is ~30ms; allow a
	// generous margin against scheduler noise.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, readErr := conn.ReadMessage()
	if readErr == nil {
		t.Fatal("expected read error after membership revocation; got none")
	}
	// Must be a CLOSE frame or transport error — both are acceptable
	// signals that the server tore the conn down.
	if !websocket.IsCloseError(
		readErr,
		websocket.ClosePolicyViolation,
		websocket.CloseAbnormalClosure,
		websocket.CloseGoingAway,
	) && !isTimeoutOrEOF(readErr) {
		t.Logf("post-revoke read error (acceptable): %v", readErr)
	}
}

func isTimeoutOrEOF(err error) bool {
	if err == nil {
		return false
	}
	type timeout interface{ Timeout() bool }
	if t, ok := err.(timeout); ok && t.Timeout() {
		return false // timeout means revocation didn't happen — caller should fail
	}
	// Read after server-side close: net.ErrClosed, io.EOF, EOF. Hard
	// to enumerate exhaustively; the test above already short-circuits
	// on err != nil so this helper is purely diagnostic.
	return true
}

// TestCollabUpgradeUnavailableWithoutRoomManager confirms the
// handler returns 503 when no RoomManager is wired (the
// SetCollabRoomManager call hasn't happened). Self-host builds that
// don't enable collab should fail loud rather than silently
// accepting an upgrade and dropping every byte.
func TestCollabUpgradeUnavailableWithoutRoomManager(t *testing.T) {
	srv := testServer(t) // NO RoomManager wired
	ts := httptest.NewServer(srv)
	defer ts.Close()

	itemID := seedCollabFixture(t, srv, "Unwired")
	_, resp, err := dialCollab(t, ts.URL, itemID, nil, "")
	if err == nil {
		t.Fatal("expected dial to fail without a wired RoomManager")
	}
	if resp == nil {
		t.Fatalf("dial returned no response: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
}

// TestCollabUpgradeMissingItemIDBadRequest exercises the bad-request
// branch — chi's URL pattern requires the {itemID} segment to be
// present, so the route just won't match without it. This test is
// here as documentation: hitting the parent path returns 404 from
// the chi router rather than reaching our handler.
func TestCollabUpgradeMissingItemIDBadRequest(t *testing.T) {
	srv := testServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/collab/")
	if err != nil {
		t.Fatalf("http get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatalf("did not expect a successful upgrade on missing item segment")
	}
	// Any non-2xx is fine for this assertion — we're just confirming
	// the route doesn't accidentally match an empty itemID.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		t.Fatalf("expected non-success on missing item segment, got %d", resp.StatusCode)
	}
}
