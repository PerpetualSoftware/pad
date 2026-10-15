package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/mark3labs/mcp-go/mcp"
	mcpgo "github.com/mark3labs/mcp-go/server"
)

// BUG-3835: the MCP transport's long-lived streams get a comment keepalive
// and take a slot in the shared stream admission gate. Built on BUG-3808's
// fixture: a real mcp-go transport behind the real router, over real TCP.

func (f *mcpStallFixture) userID(t *testing.T) string {
	t.Helper()
	u, err := f.srv.store.GetUserByEmail("mcp-stall@example.com")
	if err != nil || u == nil {
		t.Fatalf("fixture user: %v", err)
	}
	return u.ID
}

func waitHeld(t *testing.T, srv *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for srv.admission().heldTotal() != want {
		if time.Now().After(deadline) {
			t.Fatalf("admission slots held = %d, want %d", srv.admission().heldTotal(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An IDLE GET stream whose client stopped reading ends, and frees its slot.
// Nothing is pumped: only the keepalive writes, so without it no write ever
// happens and #2182's deadline never fires (the bug). Release on
// write-deadline death.
func TestMCP_BUG3835_IdleStalledGETEndsAndReleasesItsSlot(t *testing.T) {
	f := newMCPStallFixture(t)
	ts, wf := stalledStreamServer(t, f.srv)
	f.openGET(t, ts.URL, true, wf)
	assertSlotReleased(t, f.srv, wf)
	if f.unregistered.Load() == 0 {
		t.Fatal("the slot was released but the GET stream's session is still registered")
	}
}

// CONTROL: a reading client on an idle stream receives the comment, keeps
// its stream, and frees the slot when it closes the connection. Release on
// client close.
func TestMCP_BUG3835_ReadingIdleClientGetsKeepalives(t *testing.T) {
	f := newMCPStallFixture(t)
	f.srv.sseKeepaliveOverride = 20 * time.Millisecond
	ts := httptest.NewServer(f.srv)
	t.Cleanup(ts.Close)
	conn, br := f.openGET(t, ts.URL, false, nil)
	waitHeld(t, f.srv, 1)

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := 0
	for got < 3 {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v (after %d keepalives)", err, got)
		}
		if line == ": keepalive\n" {
			got++
		}
	}
	if n := f.unregistered.Load(); n != 0 {
		t.Fatalf("a reading client's idle stream ended (unregistered=%d)", n)
	}
	conn.Close()
	waitHeld(t, f.srv, 0)
}

// Over the budget, a GET is a plain 429 sent before any SSE byte, and the
// transport never runs: no session is registered.
func TestMCP_BUG3835_GETOverTheBudgetIsAPlain429(t *testing.T) {
	f := newMCPStallFixture(t)
	f.srv.admission().setLimits(0, 1)
	ts := httptest.NewServer(f.srv)
	t.Cleanup(ts.Close)
	f.openGET(t, ts.URL, false, nil)
	waitHeld(t, f.srv, 1)

	req, _ := http.NewRequest("GET", ts.URL+"/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, body := doRefusable(t, req)
	assertPlainStreamRefusal(t, resp, body)
	if n := f.registered.Load(); n != 1 {
		t.Errorf("sessions registered = %d, want 1: the refused GET reached the transport", n)
	}
}

// doRefusable sends a request that should be refused. A response that is not
// a refusal may be a live event stream, which never reaches EOF, so its body
// is not read: the test fails at once instead of hanging.
func doRefusable(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d (Content-Type %q), want 429: the stream was served", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func assertPlainStreamRefusal(t *testing.T, resp *http.Response, body []byte) {
	t.Helper()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429; body %s", resp.StatusCode, body)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "5" {
		t.Errorf("Retry-After %q, want 5", ra)
	}
	if ct := resp.Header.Get("Content-Type"); isEventStream(ct) {
		t.Errorf("Content-Type %q: the refusal was committed as an event stream", ct)
	}
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error.Code != "sse_limit_exceeded" {
		t.Errorf("body %q, want a JSON error with code sse_limit_exceeded", body)
	}
}

// A subscriptions/listen POST over the budget is refused the same way, before
// the transport runs, so it leaves no session, subscription or goroutine.
func TestMCP_BUG3835_RefusedListenLeavesNothing(t *testing.T) {
	f := newMCPStallFixture(t)
	f.srv.admission().setLimits(0, 1)
	release, refusal := f.srv.admission().acquire(f.userID(t))
	if refusal != admissionRefusalNone {
		t.Fatal(refusal)
	}
	defer release()
	ts := httptest.NewServer(f.srv)
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":7,"method":"subscriptions/listen","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, body := doRefusable(t, req)
	assertPlainStreamRefusal(t, resp, body)
	if n := f.registered.Load(); n != 0 {
		t.Errorf("sessions registered = %d, want 0", n)
	}
	if held := f.srv.admission().heldTotal(); held != 1 {
		t.Errorf("slots held = %d, want 1 (only the test's own)", held)
	}
}

// Listen detection decodes exactly as mcp-go does, so no spelling of the body
// mcp-go would serve as a listen can skip the budget, and nothing else is
// counted. The reference decode is handlePost's own struct shape.
func TestMCP_BUG3835_ListenDetectionMatchesMCPGo(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	cases := []struct {
		name string
		body string
	}{
		{"plain", `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen"}`},
		{"method after a large params", `{"jsonrpc":"2.0","id":1,"params":{"pad":"` + big + `"},"method":"subscriptions/listen"}`},
		{"upper-case key", `{"jsonrpc":"2.0","id":1,"METHOD":"subscriptions/listen"}`},
		{"mixed-case key", `{"jsonrpc":"2.0","id":1,"Method":"subscriptions/listen"}`},
		{"duplicate keys, listen last", `{"jsonrpc":"2.0","id":1,"method":"tools/call","method":"subscriptions/listen"}`},
		{"duplicate keys, listen first", `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","method":"tools/call"}`},
		{"array", `[{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen"}]`},
		{"tools/call", `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`},
		{"not json", `subscriptions/listen`},
	}
	for _, c := range cases {
		var ref struct {
			Method mcp.MCPMethod `json:"method,omitempty"`
		}
		want := json.Unmarshal([]byte(c.body), &ref) == nil && ref.Method == mcp.MethodSubscriptionsListen
		if got := jsonRPCHasMethod([]byte(c.body), string(mcp.MethodSubscriptionsListen)); got != want {
			t.Errorf("%s: detected listen = %v, mcp-go's decode says %v", c.name, got, want)
		}
	}
	// The cases that must count, spelled out rather than only mirrored.
	for _, body := range []string{cases[1].body, cases[2].body, cases[4].body} {
		if !jsonRPCHasMethod([]byte(body), string(mcp.MethodSubscriptionsListen)) {
			t.Errorf("a listen was not detected: %.80s", body)
		}
	}
	// The scan replays the body unchanged to the transport.
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(cases[1].body))
	if !mcpLongLivedRequest(r) {
		t.Fatal("mcpLongLivedRequest missed the listen")
	}
	replayed, _ := io.ReadAll(r.Body)
	if string(replayed) != cases[1].body {
		t.Fatal("the scanned body was not replayed intact")
	}
}

// An array body is not counted, which is safe only because mcp-go refuses it
// outright: nothing is served and no stream is held.
func TestMCP_BUG3835_ArrayBodyIsRejectedByTheTransport(t *testing.T) {
	f := newMCPStallFixture(t)
	ts := httptest.NewServer(f.srv)
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(
		`[{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen"}]`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+f.token)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("the array body was held open (or failed): %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK || isEventStream(resp.Header.Get("Content-Type")) {
		t.Fatalf("mcp-go served an array body: %d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if n := f.registered.Load(); n != 0 {
		t.Errorf("sessions registered = %d, want 0", n)
	}
}

// Every credential /mcp accepts yields a non-empty principal: an empty one
// would skip the per-user bound. /mcp accepts a PAT or an OAuth access token
// (MCPBearerAuth); a session is not a bearer it takes, and an app principal's
// credential is refused as unknown. With the user's one slot held, each must
// be refused per user; once released, each is served.
func TestMCP_BUG3835_EveryAuthPathHasAPrincipal(t *testing.T) {
	srv, transport := mcpAndOAuthEnabledTestServer(t)
	srv.admission().setLimits(0, 1)
	user, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	tokens := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID,
		"verifier-bug3835-quick-brown-fox-jumps-over-lazy-dog-12")
	access, _ := tokens["access_token"].(string)
	if access == "" {
		t.Fatalf("missing access token: %v", tokens)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "BUG-3835"})
	if err != nil {
		t.Fatal(err)
	}
	pat, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{Name: "bug3835", WorkspaceID: ws.ID}, 30, 0)
	if err != nil {
		t.Fatal(err)
	}

	for name, bearer := range map[string]string{"pat": pat.Token, "oauth": access} {
		t.Run(name, func(t *testing.T) {
			get := func() *deadlineRecorder {
				req := httptest.NewRequest("GET", "/mcp", nil)
				req.Header.Set("Authorization", "Bearer "+bearer)
				req.RemoteAddr = "192.0.2.1:1234"
				rr := newDeadlineRecorder()
				srv.ServeHTTP(rr, req)
				return rr
			}
			release, refusal := srv.admission().acquire(user.ID)
			if refusal != admissionRefusalNone {
				t.Fatal(refusal)
			}
			transport.Called = false
			if rr := get(); rr.Code != http.StatusTooManyRequests {
				t.Fatalf("with the user's slot held: %d %s, want 429 per user: the principal was not the user", rr.Code, rr.Body.String())
			}
			if transport.Called {
				t.Fatal("the refused GET reached the transport")
			}
			release()
			if rr := get(); rr.Code != http.StatusOK || !transport.Called {
				t.Fatalf("with the slot free: %d %s, want the transport served", rr.Code, rr.Body.String())
			}
			// Released on handler exit.
			if held := srv.admission().heldTotal(); held != 0 {
				t.Fatalf("slots held after the handler returned = %d, want 0", held)
			}
		})
	}
}

// A tools/call POST that mcp-go upgrades to SSE mid-call takes no slot, gets
// keepalives while the call runs, and nothing follows its final event.
func TestMCP_BUG3835_ToolCallSSEUpgradeEndsWithItsResponse(t *testing.T) {
	f := newMCPStallFixture(t)
	f.srv.sseKeepaliveOverride = 5 * time.Millisecond
	f.srv.admission().setLimits(0, 1)
	heldDuringCall := make(chan int, 1)
	f.mcp.AddTool(mcp.NewTool("slow"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_ = mcpgo.ServerFromContext(ctx).SendNotificationToClient(ctx, "notifications/message", map[string]any{"data": "working"})
		time.Sleep(100 * time.Millisecond)
		heldDuringCall <- f.srv.admission().heldTotal()
		return mcp.NewToolResultText("done"), nil
	})
	ts := httptest.NewServer(f.srv)
	t.Cleanup(ts.Close)

	post := func(sid, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+f.token)
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	init := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	io.Copy(io.Discard, init.Body)
	init.Body.Close()
	sid := init.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("initialize gave no session id (%d)", init.StatusCode)
	}
	ack := post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	io.Copy(io.Discard, ack.Body)
	ack.Body.Close()

	resp := post(sid, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"slow"}}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !isEventStream(resp.Header.Get("Content-Type")) {
		t.Fatalf("instrument: the tool call was not upgraded to SSE (%s): %s", resp.Header.Get("Content-Type"), body)
	}
	if held := <-heldDuringCall; held != 0 {
		t.Errorf("slots held during an SSE-upgraded tool call = %d, want 0", held)
	}
	if !strings.Contains(string(body), ": keepalive\n\n") {
		t.Errorf("instrument: no keepalive during a 100ms call at a 5ms cadence: %s", body)
	}
	// The last frame is the response; no keepalive after it.
	frames := strings.Split(strings.TrimSuffix(string(body), "\n\n"), "\n\n")
	last := frames[len(frames)-1]
	if !strings.HasPrefix(last, "event: message\ndata: ") || !strings.Contains(last, `"id":2`) {
		t.Fatalf("the stream's last frame is %q, want the tool call's response", last)
	}
}

// Unit: the writer never sends a keepalive before the response is committed
// as an event stream, after a final event, or after the handler returned.
func TestMCP_BUG3835_KeepaliveOnlyInsideALiveEventStream(t *testing.T) {
	srv := testServer(t)
	open := func(contentType string) (*mcpStreamWriter, *deadlineRecorder) {
		rr := newDeadlineRecorder()
		stream, err := srv.openSSEStream(rr)
		if err != nil {
			t.Fatal(err)
		}
		mw := newMCPStreamWriter(stream)
		mw.Header().Set("Content-Type", contentType)
		return mw, rr
	}

	// A JSON response is never an event stream.
	mw, _ := open("application/json")
	mw.WriteHeader(http.StatusOK)
	select {
	case <-mw.committed:
		t.Fatal("a JSON response was treated as an event stream")
	default:
	}

	// After the final event: nothing.
	mw, rr := open("text/event-stream")
	mw.WriteHeader(http.StatusOK)
	if !mw.keepalive() {
		t.Fatal("a live event stream refused a keepalive")
	}
	_, _ = mw.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\"}\n\n"))
	if !mw.keepalive() {
		t.Fatal("a notification ended the stream")
	}
	_, _ = mw.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n"))
	before := rr.Body.Len()
	if mw.keepalive() || rr.Body.Len() != before {
		t.Fatal("a keepalive followed the final event")
	}

	// After the handler returned: nothing.
	mw, rr = open("text/event-stream")
	mw.WriteHeader(http.StatusOK)
	mw.end()
	before = rr.Body.Len()
	if mw.keepalive() || rr.Body.Len() != before {
		t.Fatal("a keepalive followed the handler's return")
	}
}

// A transport that panics after committing an event stream still stops the
// keepalive before mcpBoundWrites unwinds (hunter r2): chi's Recoverer
// swallows the panic, so a keepalive left running would write to a response
// whose handler has returned.
func TestMCP_BUG3835_PanickingTransportStopsTheKeepalive(t *testing.T) {
	srv := testServer(t)
	srv.sseKeepaliveOverride = time.Millisecond
	h := srv.mcpBoundWrites(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(10 * time.Millisecond) // let keepalives flow
		panic(http.ErrAbortHandler)
	}))
	rr := newDeadlineRecorder()
	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(rr, httptest.NewRequest("DELETE", "/mcp", nil))
	}()
	if !strings.Contains(rr.Body.String(), ": keepalive") {
		t.Fatal("instrument: no keepalive was written before the panic")
	}
	n := rr.Body.Len()
	time.Sleep(20 * time.Millisecond)
	if rr.Body.Len() != n {
		t.Fatal("a keepalive was written after the panicking handler unwound")
	}
}
