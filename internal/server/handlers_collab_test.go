package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/gorilla/websocket"
)

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
	srv := testServer(t)
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
