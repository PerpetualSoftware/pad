package server

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3184, both halves.
//
// (1) ONE DOOR. `read tar entry` was the one importBundle error return that
// dropped the workspace it had already minted, so the handler neither kept it
// (owner attached, named in the 400) nor removed it: a live workspace with no
// member row, invisible to its importer, holding its slug. Every tr.Next error
// after pad-export.json reached it. The legs below put the failure exactly at
// a tar HEADER after manifest.json, three ways, on both backends, and assert
// the KEEP door's outcome (TASK-896 keeps a mid-stream partial; which way that
// should go is TASK-896's open question, not this unit's).
//
// (2) THE TIMEOUT. The server-wide ReadTimeout counted server time between
// body reads; the import route now arms a per-Read deadline. Driven over a
// real listener, because a ResponseRecorder has no connection to time out.

// splitAfterManifest re-packs a real bundle so its gzip stream is
// sync-flushed at the first tar header AFTER attachments/manifest.json.
// prefix alone decodes to pad-export.json and the manifest; the next thing
// the server asks for is a header.
func splitAfterManifest(t *testing.T, realBundle []byte) (prefix, rest []byte) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(realBundle))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	split := -1
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read entry: %v", err)
		}
		names = append(names, hdr.Name)
		if err := tw.WriteHeader(&tar.Header{Name: hdr.Name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("write body: %v", err)
		}
		if hdr.Name == "attachments/manifest.json" {
			if err := tw.Flush(); err != nil { // pads the entry; the next header starts here
				t.Fatalf("flush: %v", err)
			}
			split = raw.Len()
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if split < 0 || split == raw.Len() {
		t.Fatalf("need an entry after the manifest to split before, got %v", names)
	}
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	if _, err := gw.Write(raw.Bytes()[:split]); err != nil {
		t.Fatalf("gzip prefix: %v", err)
	}
	if err := gw.Flush(); err != nil {
		t.Fatalf("gzip flush: %v", err)
	}
	n := out.Len()
	if _, err := gw.Write(raw.Bytes()[split:]); err != nil {
		t.Fatalf("gzip rest: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	all := out.Bytes()
	return append([]byte(nil), all[:n]...), append([]byte(nil), all[n:]...)
}

// failingAfter yields data, then err: the server-side view of a client that
// sent part of the body and went away.
type failingAfter struct {
	data *bytes.Reader
	err  error
}

func (f *failingAfter) Read(p []byte) (int, error) {
	if f.data.Len() > 0 {
		return f.data.Read(p)
	}
	return 0, f.err
}

func importBodyAs(srv *Server, name string, body io.Reader, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name="+name, body)
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "192.0.2.1:1234"
	const csrf = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: token})
	req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: csrf})
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// assertKeptThroughTheDoor is the KEEP door's outcome, and each clause is what
// the dropped-workspace arm did NOT do: the 400 names the slug, the workspace
// is in its importer's list, and the importer can delete it.
func assertKeptThroughTheDoor(t *testing.T, srv *Server, userID, token, slug string, rr *httptest.ResponseRecorder, wantCause string) {
	t.Helper()
	body := rr.Body.String()
	if rr.Code != http.StatusBadRequest || !strings.Contains(body, "import_failed") {
		t.Fatalf("want 400 import_failed, got %d: %s", rr.Code, body)
	}
	if !strings.Contains(body, wantCause) {
		t.Errorf("the 400 should carry the cause %q: %s", wantCause, body)
	}
	if !strings.Contains(body, `\"`+slug+`\"`) || !strings.Contains(body, "kept") {
		t.Errorf("the 400 must name the kept workspace %q: %s", slug, body)
	}
	live, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || live == nil {
		t.Fatalf("workspace %q is not live after a post-mint failure (TASK-896 keeps it): %v", slug, err)
	}
	mine, err := srv.store.GetUserWorkspaces(userID)
	if err != nil {
		t.Fatalf("GetUserWorkspaces: %v", err)
	}
	found := false
	for _, w := range mine {
		found = found || w.ID == live.ID
	}
	if !found {
		t.Errorf("workspace %q is live but not in its importer's list: no owner row, the husk this fixes", slug)
	}
	if rr := doRequestWithCookie(srv, "DELETE", "/api/v1/workspaces/"+slug, nil, token); rr.Code != http.StatusNoContent {
		t.Errorf("the importer cannot delete %q: %d %s", slug, rr.Code, rr.Body.String())
	}
}

func TestImportBundle_BUG3184_TarNextFailureGoesThroughTheKeepDoor_SQLite(t *testing.T) {
	tarNextFailureGoesThroughTheKeepDoor(t, store.DriverSQLite)
}

func TestImportBundle_BUG3184_TarNextFailureGoesThroughTheKeepDoor_Postgres(t *testing.T) {
	tarNextFailureGoesThroughTheKeepDoor(t, store.DriverPostgres)
}

func tarNextFailureGoesThroughTheKeepDoor(t *testing.T, driver store.DriverType) {
	prefix, rest := splitAfterManifest(t, realBundleWithBlob(t))

	t.Run("corrupt gzip", func(t *testing.T) {
		srv := attachmentsServerOn(t, driver)
		u, tok := memberImporter(t, srv)
		bad := append([]byte(nil), rest...)
		for i := range bad {
			bad[i] ^= 0xA5
		}
		rr := importBodyAs(srv, "corrupt", bytes.NewReader(append(append([]byte(nil), prefix...), bad...)), tok)
		assertKeptThroughTheDoor(t, srv, u.ID, tok, "corrupt", rr, "read tar entry")
	})

	t.Run("body cap", func(t *testing.T) {
		srv := attachmentsServerOn(t, driver)
		u, tok := memberImporter(t, srv)
		srv.importBundleMaxBytes = int64(len(prefix))
		rr := importBodyAs(srv, "capped", bytes.NewReader(append(append([]byte(nil), prefix...), rest...)), tok)
		assertKeptThroughTheDoor(t, srv, u.ID, tok, "capped", rr, "request body too large")
	})

	t.Run("client went away", func(t *testing.T) {
		srv := attachmentsServerOn(t, driver)
		u, tok := memberImporter(t, srv)
		rr := importBodyAs(srv, "dropped", &failingAfter{data: bytes.NewReader(prefix), err: io.ErrUnexpectedEOF}, tok)
		assertKeptThroughTheDoor(t, srv, u.ID, tok, "dropped", rr, "unexpected EOF")
	})
}

// --- (2) the per-Read deadline, over a real listener ------------------------

const liveCSRF = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// liveServer serves srv on a real listener with the given server-wide
// ReadTimeout (Listen's 15s, scaled down).
func liveServer(t *testing.T, srv *Server, readTimeout time.Duration) string {
	t.Helper()
	ts := httptest.NewUnstartedServer(srv)
	ts.Config.ReadTimeout = readTimeout
	ts.Start()
	t.Cleanup(ts.Close)
	return ts.Listener.Addr().String()
}

// sendImport writes one import request on conn: headers, then each piece
// with gap between them. keepAlive leaves the connection open for another
// request. Returns the status line and body.
func sendImport(t *testing.T, conn net.Conn, br *bufio.Reader, token, name, contentType string, pieces [][]byte, gap time.Duration, keepAlive bool) (string, string) {
	t.Helper()
	total := 0
	for _, p := range pieces {
		total += len(p)
	}
	connHdr := "close"
	if keepAlive {
		connHdr = "keep-alive"
	}
	head := fmt.Sprintf("POST /api/v1/workspaces/import?name=%s HTTP/1.1\r\nHost: pad.test\r\n"+
		"Content-Type: %s\r\nContent-Length: %d\r\n"+
		"Cookie: pad_session=%s; pad_csrf=%s\r\nX-CSRF-Token: %s\r\nConnection: %s\r\n\r\n",
		name, contentType, total, token, liveCSRF, liveCSRF, connHdr)
	if _, err := conn.Write([]byte(head)); err != nil {
		t.Fatalf("write head: %v", err)
	}
	for i, p := range pieces {
		if i > 0 {
			time.Sleep(gap)
		}
		if _, err := conn.Write(p); err != nil {
			break // the server may already have answered and closed
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.Status, string(b)
}

// liveImport sends headers and prefix, stalls, then sends rest, over a fresh
// connection.
func liveImport(t *testing.T, srv *Server, readTimeout time.Duration, token, name string, prefix, rest []byte, stall time.Duration) (string, string) {
	t.Helper()
	conn, err := net.Dial("tcp", liveServer(t, srv, readTimeout))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	return sendImport(t, conn, bufio.NewReader(conn), token, name, "application/gzip", [][]byte{prefix, rest}, stall, false)
}

// thirds splits b into three pieces.
func thirds(b []byte) [][]byte {
	n := len(b) / 3
	return [][]byte{b[:n], b[n : 2*n], b[2*n:]}
}

// The stall is LONGER than the server-wide ReadTimeout and SHORTER than the
// import's idle window. Before the fix this answered 400 "i/o timeout".
func TestImportBundle_BUG3184_ServerTimeBetweenReadsDoesNotCount(t *testing.T) {
	prefix, rest := splitAfterManifest(t, realBundleWithBlob(t))
	srv := attachmentsServerOn(t, store.DriverSQLite)
	_, tok := memberImporter(t, srv)
	srv.importReadIdle = 3 * time.Second

	status, body := liveImport(t, srv, 300*time.Millisecond, tok, "patient", prefix, rest, time.Second)
	if !strings.HasPrefix(status, "201") {
		t.Fatalf("an import stalled 1s under a 300ms server-wide ReadTimeout and a 3s per-Read window must succeed, got %s: %s", status, body)
	}
}

// The deadline is armed PER READ, not once: a client whose every gap is inside
// the idle window but whose whole body takes longer than it still succeeds. A
// single longer deadline (the fix this is not) fails this leg and passes the
// one above.
func TestImportBundle_BUG3184_TheWindowRestartsOnEveryRead(t *testing.T) {
	prefix, rest := splitAfterManifest(t, realBundleWithBlob(t))
	body := append(append([]byte(nil), prefix...), rest...)
	srv := attachmentsServerOn(t, store.DriverSQLite)
	_, tok := memberImporter(t, srv)
	srv.importReadIdle = time.Second

	conn, err := net.Dial("tcp", liveServer(t, srv, 300*time.Millisecond))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	status, resp := sendImport(t, conn, bufio.NewReader(conn), tok, "steady", "application/gzip", thirds(body), 700*time.Millisecond, false)
	if !strings.HasPrefix(status, "201") {
		t.Fatalf("a body sent in three pieces 700ms apart (1.4s in all) under a 1s per-Read window must succeed, got %s: %s", status, resp)
	}
}

// The JSON body shape reads under the same deadline: the wrap sits above the
// Content-Type dispatch.
func TestImportJSON_BUG3184_ReadsUnderThePerReadDeadline(t *testing.T) {
	src, srcSlug := testServerWithAttachments(t)
	rr := doRequest(src, "GET", "/api/v1/workspaces/"+srcSlug+"/export", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.Bytes()
	srv := attachmentsServerOn(t, store.DriverSQLite)
	_, tok := memberImporter(t, srv)
	srv.importReadIdle = 3 * time.Second

	conn, err := net.Dial("tcp", liveServer(t, srv, 300*time.Millisecond))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	status, resp := sendImport(t, conn, bufio.NewReader(conn), tok, "jsonpatient", "application/json", [][]byte{body[:len(body)/2], body[len(body)/2:]}, time.Second, false)
	if !strings.HasPrefix(status, "201") {
		t.Fatalf("a JSON import stalled 1s under a 300ms server-wide ReadTimeout and a 3s per-Read window must succeed, got %s: %s", status, resp)
	}
}

// The deadline an import arms does not outlive its request: net/http re-arms
// the connection for the next request on a keep-alive connection. Measured
// here rather than argued from net/http's source, because a leak would fail
// an UNRELATED request on the same connection.
func TestImportBundle_BUG3184_DeadlineDoesNotLeakToTheNextRequest(t *testing.T) {
	prefix, rest := splitAfterManifest(t, realBundleWithBlob(t))
	body := append(append([]byte(nil), prefix...), rest...)
	srv := attachmentsServerOn(t, store.DriverSQLite)
	_, tok := memberImporter(t, srv)
	srv.importReadIdle = 500 * time.Millisecond

	conn, err := net.Dial("tcp", liveServer(t, srv, time.Minute))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	if status, resp := sendImport(t, conn, br, tok, "first", "application/gzip", [][]byte{body}, 0, true); !strings.HasPrefix(status, "201") {
		t.Fatalf("first import: %s %s", status, resp)
	}
	time.Sleep(time.Second) // past the import's last per-Read deadline
	if status, resp := sendImport(t, conn, br, tok, "second", "application/gzip", [][]byte{body}, 0, false); !strings.HasPrefix(status, "201") {
		t.Fatalf("a second request on the same keep-alive connection, after the first import's deadline passed, must not time out: %s %s", status, resp)
	}
}

// The deadline still bounds the client: a stall longer than the idle window
// fails, and the ceiling bounds a client that never stalls that long.
func TestImportBundle_BUG3184_TheDeadlineStillBoundsTheClient(t *testing.T) {
	prefix, rest := splitAfterManifest(t, realBundleWithBlob(t))

	t.Run("idle window", func(t *testing.T) {
		srv := attachmentsServerOn(t, store.DriverSQLite)
		_, tok := memberImporter(t, srv)
		srv.importReadIdle = 500 * time.Millisecond
		status, body := liveImport(t, srv, time.Minute, tok, "idle", prefix, rest, 1500*time.Millisecond)
		if !strings.HasPrefix(status, "400") || !strings.Contains(body, "i/o timeout") {
			t.Fatalf("a 1.5s stall against a 500ms per-Read window must time out, got %s: %s", status, body)
		}
	})

	t.Run("ceiling", func(t *testing.T) {
		srv := attachmentsServerOn(t, store.DriverSQLite)
		_, tok := memberImporter(t, srv)
		srv.importReadIdle = time.Minute
		srv.importReadCeiling = 500 * time.Millisecond
		status, body := liveImport(t, srv, time.Minute, tok, "ceiling", prefix, rest, 1500*time.Millisecond)
		if !strings.HasPrefix(status, "400") || !strings.Contains(body, "i/o timeout") {
			t.Fatalf("a read past the 500ms ceiling must time out, got %s: %s", status, body)
		}
	})
}

// A writer without a connection keeps the body and the server-wide deadline:
// the fallback is today's behaviour, never a failed import.
func TestWithImportReadDeadline_UnsupportedWriterLeavesTheBodyAlone(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest("POST", "/", strings.NewReader("x"))
	orig := req.Body
	srv.withImportReadDeadline(httptest.NewRecorder(), req)
	if req.Body != orig {
		t.Fatalf("the body was wrapped although the writer cannot take a deadline")
	}
	if _, err := io.ReadAll(req.Body); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read: %v", err)
	}
}
