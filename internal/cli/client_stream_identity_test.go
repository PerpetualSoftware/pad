package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestNewWatchEventsStreamRequest_UnsendableLabelStillConnects is the
// regression for the failure Codex round 1 found on this PR, and the
// reason it is asserted by DOING THE ROUND TRIP rather than by
// inspecting the header: the bug was never about the header's contents.
// Unix directory names may contain newlines ("doc\napp" is a legal
// directory), Go's http.Client refuses to SEND a header value holding
// one, and Do returns an error before anything reaches the server. In
// the monitor that looks exactly like an unreachable padd, so its retry
// loop backs off and tries again — forever, printing nothing, with the
// user simply never receiving notifications. The server cannot defend
// against a request that was never transmitted.
//
// A test that only checked the header value would pass against the
// broken version too, since http.Header.Set stores anything; only
// attempting the request distinguishes them.
func TestNewWatchEventsStreamRequest_UnsendableLabelStillConnects(t *testing.T) {
	t.Parallel()

	var gotLabel string
	var sawRequest bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		gotLabel = r.Header.Get("X-Pad-Session-Label")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClientFromURL(srv.URL)
	req, err := c.NewWatchEventsStreamRequest(context.Background(), "", StreamSessionIdentity{
		Label: "doc\napp",
		PID:   4242,
	})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request was not sendable — the monitor would retry forever and deliver nothing: %v", err)
	}
	defer resp.Body.Close()

	if !sawRequest {
		t.Fatal("server never saw the request")
	}
	if gotLabel != "docapp" {
		t.Fatalf("label = %q, want the control byte dropped (%q)", gotLabel, "docapp")
	}
}

// TestHeaderSafeLabel covers the pieces the round trip above can't show
// individually.
func TestHeaderSafeLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ordinary", "docapp", "docapp"},
		{"newline dropped", "doc\napp", "docapp"},
		{"carriage return dropped", "doc\rapp", "docapp"},
		{"tab dropped", "doc\tapp", "docapp"},
		{"spaces survive — legal in a header value", "my project", "my project"},
		{"surrounding space trimmed", "  docapp  ", "docapp"},
		{"control-only becomes empty, so no header is sent at all", "\n\t\x00", ""},
		{"non-ascii printable survives", "проект", "проект"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := headerSafeLabel(tc.in); got != tc.want {
				t.Fatalf("headerSafeLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestHeaderSafeLabel_BoundsLength(t *testing.T) {
	t.Parallel()

	got := headerSafeLabel(strings.Repeat("é", maxHeaderLabelLen+50))
	if n := len([]rune(got)); n != maxHeaderLabelLen {
		t.Fatalf("got %d runes, want %d", n, maxHeaderLabelLen)
	}
}
