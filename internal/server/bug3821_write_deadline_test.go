package server

import (
	"bufio"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// BUG-3821: every response that is not a stream gets a rolling per-write
// deadline, so a client that stops reading cannot pin its handler, and one
// that reads slowly but steadily still gets the whole body.
//
// Through the real router and a real TCP connection, because the deadline
// reaches the socket only through every middleware writer in between, and
// because the placement under the compressor is the router's.

const bug3821Timeout = 300 * time.Millisecond

// bug3821Server serves srv, with a write timeout of timeout, on a real
// listener whose accepted connections get sndbuf, and reports each request's handler return on the returned channel
// (the path, once the handler has returned).
func bug3821Server(t *testing.T, srv *Server, timeout time.Duration, sndbuf int) (*httptest.Server, <-chan string) {
	t.Helper()
	srv.responseWriteTimeoutOverride = timeout
	returned := make(chan string, 16)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.ServeHTTP(w, r)
		returned <- r.URL.Path
	}))
	ts.Config.ConnState = func(c net.Conn, st http.ConnState) {
		if st == http.StateNew {
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetWriteBuffer(sndbuf)
			}
		}
	}
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, returned
}

// bug3821Dial opens a raw connection to ts with the given receive buffer.
func bug3821Dial(t *testing.T, ts *httptest.Server, rcvbuf int) net.Conn {
	t.Helper()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(rcvbuf)
	}
	return conn
}

// stallAfterHead sends a GET for path, with any extra header lines, and reads
// the response head only, then never reads again. The connection stays open,
// so the handler's return cannot come from the client going away.
func stallAfterHead(t *testing.T, ts *httptest.Server, path, extra string) {
	t.Helper()
	conn := bug3821Dial(t, ts, 4096)
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\nAccept-Encoding: gzip\r\n%s\r\n", path, extra)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReaderSize(conn, 16)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(status, " 200 ") {
		t.Fatalf("status line %q, want 200", strings.TrimSpace(status))
	}
}

// awaitReturn requires path's handler to return within the deadline plus
// slack, while its client is still connected and not reading.
func awaitReturn(t *testing.T, returned <-chan string, path string) {
	t.Helper()
	const slack = 5 * time.Second
	timer := time.NewTimer(bug3821Timeout + slack)
	defer timer.Stop()
	for {
		select {
		case p := <-returned:
			if p == path {
				return
			}
		case <-timer.C:
			t.Fatalf("GET %s: the handler had not returned %v after its client stopped reading (write timeout %v): "+
				"a blocked response write pins it", path, bug3821Timeout+slack, bug3821Timeout)
		}
	}
}

// bigExportWorkspace makes a workspace whose JSON export is n x 768 KB of
// hex text, which gzips about 2:1, so the compressed body is large too.
func bigExportWorkspace(t *testing.T, srv *Server, n int) string {
	t.Helper()
	slug := createWSForTest(t, srv)
	for i := 0; i < n; i++ {
		raw := make([]byte, 384<<10)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		createItem(t, srv, slug, "docs", map[string]interface{}{
			"title":   fmt.Sprintf("Bulk %d", i),
			"content": hex.EncodeToString(raw),
		})
	}
	return slug
}

// The attachment GET is the unauthenticated share-link shape. A client must not
// be able to opt it out of the deadline by sending an Upgrade header (codex r4).
func TestBUG3821_StalledReaderOfAnAttachmentReleasesItsHandler(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, extra string }{
		{"plain", ""},
		{"with an Upgrade header", "Upgrade: websocket\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stalledAttachmentReader(t, tc.extra)
		})
	}
}

func stalledAttachmentReader(t *testing.T, extra string) {
	srv, slug := testServerWithAttachments(t)
	raw := make([]byte, 2<<20)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	rr := doMultipartUpload(srv, slug, "blob.txt", []byte(hex.EncodeToString(raw)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	var att struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &att); err != nil {
		t.Fatal(err)
	}
	ts, returned := bug3821Server(t, srv, bug3821Timeout, 4096)
	path := "/api/v1/workspaces/" + slug + "/attachments/" + att.ID
	stallAfterHead(t, ts, path, extra)
	awaitReturn(t, returned, path)
}

func TestBUG3821_StalledReaderOfTheJSONExportReleasesItsHandler(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := bigExportWorkspace(t, srv, 6)
	ts, returned := bug3821Server(t, srv, bug3821Timeout, 4096)
	path := "/api/v1/workspaces/" + slug + "/export"
	stallAfterHead(t, ts, path, "")
	awaitReturn(t, returned, path)
}

// The deadline is rolling: a client that takes far longer than the timeout to
// read the whole body, but never stalls for that long, gets all of it. The
// export goes out gzipped and writeJSON hands the compressor the whole
// document in ONE Write, so this also pins the placement: a deadline armed
// above the compressor is armed once for that Write and expires mid-body.
// Buffers stay above loopback's 64KB MSS, so a reading client is not slowed by
// TCP's persist timer instead of by the test. The timeout is 1s, not
// bug3821Timeout: alongside the whole package's parallel tests, the reader
// can go unscheduled for longer than 300ms, which cut the body off once in a
// full run.
func TestBUG3821_SteadyReaderOfALongCompressedBodyCompletes(t *testing.T) {
	t.Parallel()
	const timeout = time.Second
	srv := testServer(t)
	const items = 12
	slug := bigExportWorkspace(t, srv, items)
	ts, _ := bug3821Server(t, srv, timeout, 128<<10)
	conn := bug3821Dial(t, ts, 128<<10)
	path := "/api/v1/workspaces/" + slug + "/export"
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\nAccept-Encoding: gzip\r\nConnection: close\r\n\r\n", path)

	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	slow := &throttledReader{r: conn, chunk: 16 << 10, pause: 20 * time.Millisecond}
	resp, err := http.ReadResponse(bufio.NewReaderSize(slow, 32<<10), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("status %d, Content-Encoding %q: want a 200 gzip response", resp.StatusCode, resp.Header.Get("Content-Encoding"))
	}
	start := time.Now()
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip header: %v", err)
	}
	var export struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(gz).Decode(&export); err != nil {
		t.Fatalf("after %v of steady reading the body was cut off (write timeout %v): %v",
			time.Since(start).Round(time.Millisecond), timeout, err)
	}
	if _, err := io.Copy(io.Discard, gz); err != nil {
		t.Fatalf("after %v the gzip stream did not end cleanly: %v", time.Since(start).Round(time.Millisecond), err)
	}
	// The control: the read must have outlasted the timeout several times
	// over, or it proves nothing about rolling.
	if took := time.Since(start); took < 3*timeout {
		t.Fatalf("the body drained in %v, under 3x the %v write timeout: grow the workspace or slow the reader", took, timeout)
	}
	if len(export.Items) != items {
		t.Errorf("export carried %d items, want %d", len(export.Items), items)
	}
}

// throttledReader reads at most chunk bytes per call, pausing before each.
type throttledReader struct {
	r     io.Reader
	chunk int
	pause time.Duration
}

func (s *throttledReader) Read(p []byte) (int, error) {
	time.Sleep(s.pause)
	if len(p) > s.chunk {
		p = p[:s.chunk]
	}
	return s.r.Read(p)
}

// The middleware never clears the deadline it leaves on a connection, because
// net/http does, after each response and on hijack. The next two tests hold
// net/http to each clear (codex r4: one test could not tell them apart).

// After a response: a keep-alive connection's next request starts with no
// deadline. The second request goes to an exempt route the middleware never
// arms, and writes after the first response's deadline would have expired.
func TestBUG3821_AKeepAliveConnectionStartsItsNextRequestWithNoDeadline(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	srv.ensureRouter()
	srv.router.Get("/__bug3821/bounded", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "bounded\n")
	})
	srv.router.Get("/api/v1/collab/__bug3821", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "exempt\n")
	})
	ts, _ := bug3821Server(t, srv, bug3821Timeout, 1<<20)
	conn := bug3821Dial(t, ts, 1<<20)
	br := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	get := func(path string) string {
		t.Helper()
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("GET %s on the reused connection: %v: a write deadline outlived the previous response", path, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("GET %s body: %v", path, err)
		}
		return string(body)
	}
	if got := get("/__bug3821/bounded"); got != "bounded\n" {
		t.Fatalf("first response %q", got)
	}
	time.Sleep(2 * bug3821Timeout)
	if !ownsWriteDeadline(httptest.NewRequest("GET", "/api/v1/collab/__bug3821", nil)) {
		t.Fatal("the second route is not exempt, so the middleware would arm it and hide a leftover deadline")
	}
	if got := get("/api/v1/collab/__bug3821"); got != "exempt\n" {
		t.Fatalf("second response %q, want %q", got, "exempt\n")
	}
}

// On hijack: whatever the middleware armed is gone, so an upgraded connection
// can write long after the response timeout. The route is not exempt, so the
// handler gets the middleware's writer, and it hijacks by type assertion, as
// gorilla/websocket does (codex r7).
func TestBUG3821_AHijackedConnectionCarriesNoDeadline(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	srv.ensureRouter()
	srv.router.Get("/__bug3821/upgrade", func(w http.ResponseWriter, r *http.Request) {
		h, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("the response writer (%T) does not implement http.Hijacker", w)
			http.Error(w, "no hijacker", http.StatusInternalServerError)
			return
		}
		conn, rw, err := h.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		time.Sleep(2 * bug3821Timeout)
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: test\r\nConnection: Upgrade\r\n\r\n")
		if err := rw.Flush(); err != nil {
			t.Errorf("the hijacked connection's write, %v after the hijack, failed: %v", 2*bug3821Timeout, err)
		}
	})
	ts, _ := bug3821Server(t, srv, bug3821Timeout, 1<<20)
	conn := bug3821Dial(t, ts, 1<<20)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "GET /__bug3821/upgrade HTTP/1.1\r\nHost: x\r\nUpgrade: test\r\nConnection: Upgrade\r\n\r\n")
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(status, " 101 ") {
		t.Fatalf("status line %q (%v), want 101: the deadline armed before the hijack outlived it", status, err)
	}
}

// A handler that writes a small body and then keeps working: the body sits in
// net/http's buffer and reaches the socket only after the handler returns, so
// the deadline armed by that Write has long expired by then unless the
// middleware re-arms on the way out.
func TestBUG3821_ABufferedBodyIsFlushedAfterALongHandler(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	srv.ensureRouter()
	srv.router.Get("/__bug3821/slow", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "done\n")
		time.Sleep(3 * bug3821Timeout)
	})
	ts, _ := bug3821Server(t, srv, bug3821Timeout, 1<<20)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(ts.URL + "/__bug3821/slow")
	if err != nil {
		t.Fatalf("GET: %v: the buffered body was flushed under the deadline its Write armed, %v before", err, 3*bug3821Timeout)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "done\n" {
		t.Fatalf("body %q (%v), want %q", body, err, "done\n")
	}
}

// The predicate reads the escaped path chi routes on. Each exempt route is
// recognised as written; a decoded spelling of some other route that merely
// looks like one is not (codex r5: an encoded item ref decoding to an
// attachment-download path), and neither is a path that only shares a prefix.
func TestBUG3821_TheExemptionMatchesTheRoutedPathOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		method, target string
		exempt         bool
	}{
		{"GET", "/api/v1/events?workspace=w", true},
		{"GET", "/api/v1/events/stream", true},
		{"POST", "/mcp", true},
		{"GET", "/api/v1/collab/0b6f2a8e-1111-2222-3333-444455556666", true},
		{"GET", appAPIPrefix + "/workspaces/w/attachments/a1/content", true},
		{"HEAD", appAPIPrefix + "/workspaces/w/attachments/a1/content", true},
		// An app item ref whose decoded form looks like the download route.
		{"GET", appAPIPrefix + "/workspaces/w/items/x%2Fattachments%2Fa1%2Fcontent", false},
		// Shares the events prefix, is not an events route.
		{"GET", "/api/v1/events-anything", false},
		{"GET", "/api/v1/workspaces/w/attachments/a1", false},
		{"GET", "/api/v1/s/tok/attachments/a1", false},
	} {
		r := httptest.NewRequest(tc.method, tc.target, nil)
		if got := ownsWriteDeadline(r); got != tc.exempt {
			t.Errorf("%s %s: ownsWriteDeadline = %v, want %v", tc.method, tc.target, got, tc.exempt)
		}
	}
}
