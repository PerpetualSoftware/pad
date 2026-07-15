package server

import (
	"bytes"
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
	// jittered across [0, interval) — worst case is ~30ms — so
	// 2 seconds is wildly generous. A timeout here means the WS is
	// still open, which is the bug being regression-tested.
	//
	// Drain initial control frames (the post-replay op_log_cursor
	// from TASK-1319) until either an error surfaces (the close
	// we want) or the deadline trips. Without the drain the very
	// first read returns the cursor TextMessage and the test would
	// false-pass on a still-open connection.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var readErr error
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			readErr = err
			break
		}
		// A control/data frame slipped through before the close —
		// keep reading until we see the close.
	}
	if readErr == nil {
		t.Fatal("expected read error after membership revocation; got none (WS still open)")
	}
	if isTimeout(readErr) {
		t.Fatalf("read timed out before WS was closed — revocation not honoured: %v", readErr)
	}
	// We sent ClosePolicyViolation. Accept either an explicit
	// close frame with that code OR a transport-level error
	// (gorilla returns io.ErrUnexpectedEOF / net.ErrClosed when
	// the peer closes the underlying TCP conn before sending a
	// close frame; either way the server-side teardown happened).
	if websocket.IsCloseError(readErr, websocket.ClosePolicyViolation) {
		// Best case: client got the typed reason.
		return
	}
	// Generic close / EOF / connection-reset: also a pass — the
	// server CLOSED us, just without a clean frame depending on
	// timing. The test's assertion is "the WS is no longer
	// readable", which holds.
	t.Logf("post-revoke read error: %v", readErr)
}

func isTimeout(err error) bool {
	type timeout interface{ Timeout() bool }
	if t, ok := err.(timeout); ok {
		return t.Timeout()
	}
	return false
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

// TestCollabUpgradeRejectsSchemaVersionMismatch is the TASK-1268
// negative-handshake regression: a client announcing a different
// SCHEMA_VERSION than the server runs must be 400'd before the WS
// upgrade. Admitting a mismatched client and letting it stamp ops
// onto the op-log would corrupt the rebuild flow's mismatch
// detection (the server's stamp is supposed to mark each op-log
// row's era).
func TestCollabUpgradeRejectsSchemaVersionMismatch(t *testing.T) {
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	itemID := seedCollabFixture(t, srv, "SchemaMismatch")

	// Server's RoomManager is at "1" (DefaultSchemaVersion); send "9"
	// to force a mismatch. We hit the HTTP path directly rather than
	// through dialCollab so we can read the JSON error body.
	resp, err := http.Get(ts.URL + "/api/v1/collab/" + itemID + "?schema_version=9")
	if err != nil {
		t.Fatalf("http get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on schema mismatch, got %d", resp.StatusCode)
	}
	// We deliberately don't lock to the exact JSON shape (which
	// could shift if the writeError envelope changes) — the
	// status code is the load-bearing assertion.
}

// TestCollabUpgradeAcceptsExplicitMatchingSchemaVersion confirms the
// happy path of the handshake: a client that sends a schema_version
// matching the server's value is upgraded normally.
func TestCollabUpgradeAcceptsExplicitMatchingSchemaVersion(t *testing.T) {
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	itemID := seedCollabFixture(t, srv, "SchemaMatch")

	// Server's RoomManager runs at "1" (DefaultSchemaVersion). The
	// client announces "1" too — must be accepted.
	u, _ := url.Parse(ts.URL)
	wsURL := "ws://" + u.Host + "/api/v1/collab/" + itemID + "?schema_version=1"
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	conn, resp, err := dialer.Dial(wsURL, nil)
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
	if err := conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"),
		time.Now().Add(1*time.Second),
	); err != nil {
		t.Fatalf("write close: %v", err)
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

// readNextBinaryFrame reads WebSocket frames from conn, skipping any
// TextMessage control frames (op_log_cursor, force_refresh), until it
// gets a BinaryMessage or the deadline passes. Returns (data, true)
// on a binary frame and (nil, false) on timeout / connection error.
func readNextBinaryFrame(t *testing.T, conn *websocket.Conn, within time.Duration) ([]byte, bool) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(within))
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return nil, false
		}
		if mt == websocket.BinaryMessage {
			return data, true
		}
		// Text control frame — skip and keep reading until the deadline.
	}
}

// waitForOpLogRows polls the item's op-log until it holds exactly want
// rows or the deadline passes. Fails immediately if the count exceeds
// want (an unexpected extra persist — e.g. a read-only frame leaking
// through). Returns the final snapshot.
func waitForOpLogRows(t *testing.T, srv *Server, itemID string, want int, within time.Duration) []models.YjsUpdate {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		rows, err := srv.store.LoadYjsUpdatesSince(itemID, 0)
		if err != nil {
			t.Fatalf("LoadYjsUpdatesSince: %v", err)
		}
		if len(rows) > want {
			t.Fatalf("op-log has %d rows, want %d (unexpected extra persist)", len(rows), want)
		}
		if len(rows) == want {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d op-log row(s); have %d", want, len(rows))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCollabViewerIsReadOnly is the TASK-265 regression: a workspace
// VIEWER admitted to a collab room is a READ-ONLY participant. It
// keeps live view + presence (still receives broadcasts) but its
// inbound Yjs sync frames are dropped — never persisted to
// item_yjs_updates, never rebroadcast — while a co-present EDITOR's
// frames ARE persisted and broadcast. This closes the viewer-write
// bypass: the collab WS GET is mounted outside RequireWorkspaceAccess,
// so before this fix a viewer's frames persisted and got canonicalized
// into items.content on a co-present editor's authorized flush.
func TestCollabViewerIsReadOnly(t *testing.T) {
	srv := testServerWithCollab(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "ViewerReadOnly"})
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
		Title: "Shared doc", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	// Two members: one editor (may write), one viewer (read-only).
	mkMember := func(email, role string) []*http.Cookie {
		u, err := srv.store.CreateUser(models.UserCreate{
			Email: email, Name: role, Password: "correct-horse-battery-staple", Role: "member",
		})
		if err != nil {
			t.Fatalf("CreateUser %s: %v", email, err)
		}
		if err := srv.store.AddWorkspaceMember(ws.ID, u.ID, role); err != nil {
			t.Fatalf("AddWorkspaceMember %s: %v", role, err)
		}
		token, err := srv.store.CreateSession(u.ID, "go-test", "127.0.0.1", "go-test", 24*time.Hour)
		if err != nil {
			t.Fatalf("CreateSession %s: %v", email, err)
		}
		return []*http.Cookie{{Name: "pad_session", Value: token}}
	}
	editorCookies := mkMember("editor@test.com", "editor")
	viewerCookies := mkMember("viewer@test.com", "viewer")

	// Both members upgrade successfully — the viewer is admitted as a
	// read-only participant, NOT rejected (live-view must stay intact).
	editorConn, resp, err := dialCollab(t, ts.URL, item.ID, editorCookies, "go-test")
	if err != nil {
		body := ""
		if resp != nil {
			body = "status=" + resp.Status
		}
		t.Fatalf("editor dial: %v (%s)", err, body)
	}
	defer editorConn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("editor: expected 101, got %d", resp.StatusCode)
	}
	// Drain the editor's initial post-replay op_log_cursor so we know
	// its Join has completed (subscribed + readLoop running) before we
	// send. At connect time nothing else writes, so the first frame is
	// the text cursor.
	if _, _, derr := editorConn.ReadMessage(); derr != nil {
		t.Fatalf("editor initial frame: %v", derr)
	}

	viewerConn, resp, err := dialCollab(t, ts.URL, item.ID, viewerCookies, "go-test")
	if err != nil {
		body := ""
		if resp != nil {
			body = "status=" + resp.Status
		}
		t.Fatalf("viewer dial (should be admitted read-only, not rejected): %v (%s)", err, body)
	}
	defer viewerConn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("viewer: expected 101 (read-only admission), got %d", resp.StatusCode)
	}

	// (c) EDITOR frame IS persisted + broadcast. Yjs sync frame: first
	// byte 0 = sync (the dumb relay keys on data[0]; the remaining
	// bytes needn't be a real Y update for the relay path).
	editorFrame := []byte{0x00, 0xE1, 0xE2}
	if err := editorConn.WriteMessage(websocket.BinaryMessage, editorFrame); err != nil {
		t.Fatalf("editor write: %v", err)
	}
	rows := waitForOpLogRows(t, srv, item.ID, 1, 3*time.Second)
	if !bytes.Equal(rows[0].UpdateData, editorFrame) {
		t.Fatalf("persisted row = %x, want editor frame %x", rows[0].UpdateData, editorFrame)
	}

	// (b) The VIEWER receives the editor's broadcast — read-only
	// participant, live view intact.
	got, ok := readNextBinaryFrame(t, viewerConn, 3*time.Second)
	if !ok {
		t.Fatal("viewer did not receive the editor's broadcast frame (live view broken)")
	}
	if !bytes.Equal(got, editorFrame) {
		t.Fatalf("viewer received %x, want editor frame %x", got, editorFrame)
	}

	// (a) VIEWER sync frame is dropped: not broadcast, not persisted.
	// The viewer sends a sync frame V (must be dropped) then an
	// awareness frame A (presence — still relayed). The viewer's
	// readLoop is single-threaded and the bus is FIFO, so if the
	// editor receives A we KNOW V was already processed (and dropped).
	viewerSync := []byte{0x00, 0x11, 0x12}      // sync — must be dropped
	viewerAwareness := []byte{0x01, 0xAA, 0xBB} // awareness — must relay
	if err := viewerConn.WriteMessage(websocket.BinaryMessage, viewerSync); err != nil {
		t.Fatalf("viewer sync write: %v", err)
	}
	if err := viewerConn.WriteMessage(websocket.BinaryMessage, viewerAwareness); err != nil {
		t.Fatalf("viewer awareness write: %v", err)
	}

	// The next binary the editor receives must be the awareness frame,
	// NOT the viewer's dropped sync frame.
	edGot, ok := readNextBinaryFrame(t, editorConn, 3*time.Second)
	if !ok {
		t.Fatal("editor did not receive the viewer's awareness (presence) frame")
	}
	if bytes.Equal(edGot, viewerSync) || edGot[0] == 0x00 {
		t.Fatalf("editor received the viewer's dropped SYNC frame %x — read-only gate leaked", edGot)
	}
	if !bytes.Equal(edGot, viewerAwareness) {
		t.Fatalf("editor received unexpected frame %x, want awareness %x", edGot, viewerAwareness)
	}

	// Op-log still holds exactly the editor's single row: the viewer's
	// sync frame was NOT persisted (barrier reached above).
	final, err := srv.store.LoadYjsUpdatesSince(item.ID, 0)
	if err != nil {
		t.Fatalf("LoadYjsUpdatesSince: %v", err)
	}
	if len(final) != 1 {
		t.Fatalf("op-log has %d rows after viewer write, want 1 (viewer frame must not persist)", len(final))
	}
	if !bytes.Equal(final[0].UpdateData, editorFrame) {
		t.Fatalf("op-log row = %x, want only the editor frame %x", final[0].UpdateData, editorFrame)
	}
}

// TestAuthorizeCollabAccessCanWrite unit-tests the write-permission
// half of the collab authorization decision (TASK-265): a workspace
// editor resolves canWrite=true, a viewer canWrite=false — both are
// admitted (no error), mirroring requireEditPermission on the REST
// path.
func TestAuthorizeCollabAccessCanWrite(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "CanWrite"})
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

	mkUser := func(email, role string) *models.User {
		u, err := srv.store.CreateUser(models.UserCreate{
			Email: email, Name: role, Password: "correct-horse-battery-staple", Role: "member",
		})
		if err != nil {
			t.Fatalf("CreateUser %s: %v", email, err)
		}
		if err := srv.store.AddWorkspaceMember(ws.ID, u.ID, role); err != nil {
			t.Fatalf("AddWorkspaceMember %s: %v", role, err)
		}
		return u
	}
	editor := mkUser("editor2@test.com", "editor")
	viewer := mkUser("viewer2@test.com", "viewer")

	// Editor member who ALSO holds an incidental item-level `view`
	// grant. ResolveUserPermission resolves the item grant ("view")
	// BEFORE membership role, so computing canWrite purely from it
	// would wrongly demote this legitimate editor to read-only. The
	// role short-circuit (mirroring requireEditPermission) must win.
	editorWithViewGrant := mkUser("editorgrant@test.com", "editor")
	if _, err := srv.store.CreateItemGrant(ws.ID, item.ID, editorWithViewGrant.ID, "view", editorWithViewGrant.ID); err != nil {
		t.Fatalf("CreateItemGrant (editor view): %v", err)
	}

	// Viewer member who holds an item-level `edit` grant — the grant
	// must OVERRIDE the insufficient base role (also mirrors REST).
	viewerWithEditGrant := mkUser("viewergrant@test.com", "viewer")
	if _, err := srv.store.CreateItemGrant(ws.ID, item.ID, viewerWithEditGrant.ID, "edit", viewerWithEditGrant.ID); err != nil {
		t.Fatalf("CreateItemGrant (viewer edit): %v", err)
	}

	check := func(name string, u *models.User, wantWrite bool) {
		// Cookie-session-shaped request (no bearer header) so the
		// caller is treated as an interactive session, not a token.
		req := httptest.NewRequest(http.MethodGet, "/api/v1/collab/"+item.ID, nil)
		req = req.WithContext(WithCurrentUser(req.Context(), u))
		access, err := srv.authorizeCollabAccess(req, item)
		if err != nil {
			t.Fatalf("%s: authorizeCollabAccess returned error (should be admitted): %v", name, err)
		}
		if access.canWrite != wantWrite {
			t.Fatalf("%s: canWrite = %v, want %v", name, access.canWrite, wantWrite)
		}
	}
	check("editor", editor, true)
	check("viewer", viewer, false)
	check("editor+incidental view grant", editorWithViewGrant, true)
	check("viewer+edit grant override", viewerWithEditGrant, true)
}

// TestCollabDemotionMakesConnReadOnly exercises the mid-session
// demotion path (TASK-265): an editor connected to a collab room who
// is demoted to viewer must become read-only WITHOUT a reconnect —
// the periodic revalidation recomputes canWrite and pushes it via
// SetConnWritable, and the room's readLoop then drops the (now
// read-only) conn's inbound sync frames. Also implicitly covers the
// startup-ordering fix: revalidation only starts after registration,
// so the SetConnWritable can find the conn.
func TestCollabDemotionMakesConnReadOnly(t *testing.T) {
	origInterval := collabMembershipRevalInterval
	collabMembershipRevalInterval = 25 * time.Millisecond
	defer func() { collabMembershipRevalInterval = origInterval }()

	srv := testServerWithCollab(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Demotion"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Doc", Fields: `{}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	u, err := srv.store.CreateUser(models.UserCreate{
		Email: "demote@test.com", Name: "Demote", Password: "correct-horse-battery-staple", Role: "member",
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
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101, got %d", resp.StatusCode)
	}

	// While an editor: a sync frame persists.
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte{0x00, 0xE1}); err != nil {
		t.Fatalf("editor write: %v", err)
	}
	waitForOpLogRows(t, srv, item.ID, 1, 3*time.Second)

	// Demote to viewer. The revalidation loop (25ms cadence) recomputes
	// canWrite=false and pushes it via SetConnWritable.
	if err := srv.store.UpdateWorkspaceMemberRole(ws.ID, u.ID, "viewer"); err != nil {
		t.Fatalf("UpdateWorkspaceMemberRole: %v", err)
	}

	// Let the demotion propagate: several reval cadences (25ms) is
	// ample for the loop to observe the viewer role and apply
	// canWrite=false to the live conn. 500ms = ~20 ticks.
	time.Sleep(500 * time.Millisecond)

	// A post-demotion sync frame must now be dropped, not persisted.
	// Send it, then watch the op-log for a further window: if the gate
	// leaked, the frame would persist (in-process, sub-ms) and push the
	// count to 2 well within the window.
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte{0x00, 0x22}); err != nil {
		t.Fatalf("post-demotion write: %v", err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		rows, err := srv.store.LoadYjsUpdatesSince(item.ID, 0)
		if err != nil {
			t.Fatalf("LoadYjsUpdatesSince: %v", err)
		}
		if len(rows) > 1 {
			t.Fatalf("post-demotion frame persisted (%d rows) — demoted editor still writable", len(rows))
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Final confirmation: still exactly the single editor-era row.
	rows, err := srv.store.LoadYjsUpdatesSince(item.ID, 0)
	if err != nil {
		t.Fatalf("LoadYjsUpdatesSince: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("op-log has %d rows, want 1 (only the pre-demotion editor frame)", len(rows))
	}
}
