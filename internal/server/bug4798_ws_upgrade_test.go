package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// BUG-4798: gorilla/websocket v1.5.4-0.20240701034025-d67f41855da4 updates
// handshake / subprotocol negotiation alongside the crypto/rand mask key fix.
// collabUpgrader advertises no subprotocols, so an upgrade succeeds both when
// the client offers none and when it offers one, and the 101 response carries
// no Sec-WebSocket-Protocol header in either case.
func TestBUG4798_CollabUpgradeNegotiatesNoSubprotocol(t *testing.T) {
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	itemID := seedCollabFixture(t, srv, "BUG4798Subprotocol")
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	wsURL := "ws://" + u.Host + "/api/v1/collab/" + itemID

	cases := []struct {
		name         string
		subprotocols []string
	}{
		{name: "no_subprotocol"},
		{name: "client_offers_subprotocol", subprotocols: []string{"anything"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dialer := &websocket.Dialer{
				HandshakeTimeout: 3 * time.Second,
				Subprotocols:     tc.subprotocols,
			}
			conn, resp, err := dialer.Dial(wsURL, nil)
			if resp != nil && resp.Body != nil {
				defer resp.Body.Close()
			}
			if err != nil {
				status := ""
				if resp != nil {
					status = resp.Status
				}
				t.Fatalf("dial: %v (%s)", err, status)
			}
			defer func() {
				_ = conn.WriteControl(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"),
					time.Now().Add(1*time.Second),
				)
				_ = conn.Close()
			}()

			if resp.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
			}
			if got := resp.Header.Values("Sec-WebSocket-Protocol"); len(got) != 0 {
				t.Fatalf("Sec-WebSocket-Protocol = %q, want absent", got)
			}
		})
	}
}
