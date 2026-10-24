package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

func TestChatStreamCookieIgnoresAgentHeader(t *testing.T) {
	f := newChatFixture(t)
	path := f.ensure("rook")
	token, err := f.srv.store.CreateSession(f.owner.ID, "web", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(f.srv)
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cookie := connectWatchStreamWithHeadersAndQuery(ctx, t, h.URL, "", map[string]string{"Cookie": "pad_session=" + token, "X-Pad-Agent": "rook"}, "chat=true&armed=true&access=true")
	bearer := connectWatchStreamWithHeadersAndQuery(ctx, t, h.URL, f.ownerTok, map[string]string{"X-Pad-Agent": "rook"}, "chat=true&armed=true&access=true")
	waitForWatchEvent(t, cookie, time.Second)
	waitForWatchEvent(t, bearer, time.Second)
	f.must(f.request("POST", path+"/messages", "", map[string]any{"body": "hello"}), 201, "send")
	if event := waitForWatchEvent(t, bearer, time.Second); !strings.Contains(event.Data, `"kind":"chat"`) {
		t.Fatal(event)
	}
	// A later access event is a FIFO barrier: the cookie stream must have
	// processed the chat notice before it writes this unrelated event.
	if err := f.srv.watchEvents.Publish(watchevents.Notification{Kind: watchevents.KindWorkspaceAccessChanged, TargetUserID: f.owner.ID}); err != nil {
		t.Fatal(err)
	}
	event := waitForWatchEvent(t, cookie, time.Second)
	if strings.Contains(event.Data, `"kind":"chat"`) {
		t.Fatalf("cookie received agent-addressed chat: %+v", event)
	}
	if !strings.Contains(event.Data, `"kind":"workspace_access_changed"`) {
		t.Fatal(event)
	}
}
