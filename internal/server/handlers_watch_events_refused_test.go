package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// TestWatchStreamIsRefusedWhenTheInstanceHasNoSubscription is BUG-2800's
// binding test, driven through a REAL watchevents.RedisBus rather than a double
// (CONVE-19): the bus's refusal and this handler's mapping of it are two
// claims, and only an end-to-end request vouches for both at once.
//
// The instance is put in the state the bug is about the way production reaches
// it: Redis rejects the constructor's SUBSCRIBE (BUG-2799), so the bus holds no
// subscription to the watch channel and nothing published anywhere can reach a
// subscriber here. Before the fix both paths answered 200 and held a stream
// open that carried nothing. Each assertion names what that leaves behind
// (CONVE-12): a 200, an event-stream body, a held admission slot.
func TestWatchStreamIsRefusedWhenTheInstanceHasNoSubscription(t *testing.T) {
	mr := miniredis.RunT(t)
	var rejected atomic.Int64
	mr.Server().SetPreHook(func(p *server.Peer, cmd string, _ ...string) bool {
		if !strings.EqualFold(cmd, "SUBSCRIBE") {
			return false
		}
		rejected.Add(1)
		p.WriteError("NOPERM User default has no permissions to access the channel")
		return true
	})
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	bus := watchevents.NewRedisBus(client)
	t.Cleanup(bus.Close)
	if rejected.Load() == 0 {
		t.Fatal("the bus's SUBSCRIBE was never rejected; this test could not have discriminated")
	}

	srv := testServerWithWatchEvents(t)
	_, _, tok, _ := setupWatchTestUser(t, srv)
	srv.SetWatchEventsBus(bus)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	for _, tc := range []struct {
		name        string
		lastEventID string
	}{
		{"fresh connection takes Subscribe", ""},
		{"resume takes SubscribeAndReplaySince", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/events/stream", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+tok.Token)
			if tc.lastEventID != "" {
				req.Header.Set("Last-Event-ID", tc.lastEventID)
			}
			resp, err := isolatedTestClient().Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: the stream was admitted on an instance that cannot deliver to it", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
				t.Fatalf("Content-Type = %q on a refusal: the SSE headers were set before the subscribe was decided", ct)
			}
			if got := resp.Header.Get("Retry-After"); got != "5" {
				t.Errorf("Retry-After = %q, want %q (the activity stream's value for the same refusal)", got, "5")
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode refusal body: %v", err)
			}
			if body.Error.Code != "subscription_failed" {
				t.Errorf("error code = %q, want subscription_failed (the activity stream's code, so a client handles both one way)", body.Error.Code)
			}
			if held := srv.admission().heldTotal(); held != 0 {
				t.Errorf("admission slots held after a refusal = %d, want 0", held)
			}
		})
	}
}
