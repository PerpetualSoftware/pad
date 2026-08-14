package cli

import (
	"context"
	"testing"
)

// TestNewWatchEventsStreamRequest_AnnouncesIdentity covers PLAN-2558
// S2's client half: the session identity has to reach the server as
// request headers, because the presence registry fills from the stream
// connection itself.
func TestNewWatchEventsStreamRequest_AnnouncesIdentity(t *testing.T) {
	t.Parallel()

	c := NewClientFromURL("http://127.0.0.1:0")
	req, err := c.NewWatchEventsStreamRequest(context.Background(), "", StreamSessionIdentity{
		Label: "docapp",
		PID:   4242,
	})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	if got := req.Header.Get("X-Pad-Session-Label"); got != "docapp" {
		t.Fatalf("label header = %q, want %q", got, "docapp")
	}
	if got := req.Header.Get("X-Pad-Session-Pid"); got != "4242" {
		t.Fatalf("pid header = %q, want %q", got, "4242")
	}
	// The stream's pre-existing contract must survive the new parameter.
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept header = %q, want text/event-stream", got)
	}
}

// TestNewWatchEventsStreamRequest_OmitsUnsetIdentity pins absence
// rather than emptiness. Sending `X-Pad-Session-Label: ""` would make
// every unannounced client look like a client that announced nothing
// useful — indistinguishable at the server, but noise on the wire and
// an invitation for a future reader to treat "present but empty" as a
// meaningful state.
func TestNewWatchEventsStreamRequest_OmitsUnsetIdentity(t *testing.T) {
	t.Parallel()

	c := NewClientFromURL("http://127.0.0.1:0")
	req, err := c.NewWatchEventsStreamRequest(context.Background(), "", StreamSessionIdentity{})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	if _, ok := req.Header["X-Pad-Session-Label"]; ok {
		t.Fatal("expected no label header at all for a zero identity")
	}
	if _, ok := req.Header["X-Pad-Session-Pid"]; ok {
		t.Fatal("expected no pid header at all for a zero identity")
	}
}
