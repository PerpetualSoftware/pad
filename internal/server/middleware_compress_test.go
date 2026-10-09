package server

import (
	"bufio"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TASK-2225: responses are gzipped for a client that accepts it, except
// streams, upgrades, ranges and responses that carry credentials.

var bigJSON = `{"items":[` + strings.Repeat(`{"id":"00000000-0000-0000-0000-000000000000","title":"a row"},`, 200) + `{}]}`

func jsonHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, bigJSON)
	})
}

func get(t *testing.T, h http.Handler, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func TestCompressGzipsJSONForAClientThatAcceptsIt(t *testing.T) {
	h := CompressResponses(jsonHandler())
	resp := get(t, h, "/api/v1/workspaces/w/items-index", map[string]string{"Accept-Encoding": "gzip"})
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding %q; want gzip", resp.Header.Get("Content-Encoding"))
	}
	if !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
		t.Errorf("Vary %q; a cache must key on Accept-Encoding", resp.Header.Get("Vary"))
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if string(body) != bigJSON {
		t.Fatal("the gzipped body does not decode to what the handler wrote")
	}

	plain := get(t, h, "/api/v1/workspaces/w/items-index", nil)
	if plain.Header.Get("Content-Encoding") != "" {
		t.Errorf("compressed for a client that did not ask: %q", plain.Header.Get("Content-Encoding"))
	}
}

func TestCompressLeavesSkippedResponsesAlone(t *testing.T) {
	h := CompressResponses(jsonHandler())
	for _, c := range []struct {
		path string
		hdr  map[string]string
	}{
		{"/api/v1/events", nil},
		{"/api/v1/events/stream", nil},
		{"/mcp", nil},
		{"/api/v1/collab/item-1", map[string]string{"Upgrade": "websocket"}},
		{"/api/v1/workspaces/w/items/x", map[string]string{"Upgrade": "websocket"}},
		{"/api/v1/workspaces/w/attachments/a", map[string]string{"Range": "bytes=0-99"}},
		{"/api/v1/auth/login", nil},
		{"/api/v1/auth/tokens", nil},
		{"/api/v1/workspaces/w/tokens", nil},
		{"/oauth/token", nil},
		{"/api/v1/workspaces/w/claim-code", nil},
		{"/api/v1/workspaces/w/members", nil},
	} {
		hdr := map[string]string{"Accept-Encoding": "gzip"}
		for k, v := range c.hdr {
			hdr[k] = v
		}
		resp := get(t, h, c.path, hdr)
		if ce := resp.Header.Get("Content-Encoding"); ce != "" {
			t.Errorf("%s %v: compressed (%s); it must go out as written", c.path, c.hdr, ce)
		}
	}
}

func TestCompressSkipsTypesNotWorthCompressing(t *testing.T) {
	h := CompressResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte(strings.Repeat("\x89PNG", 500)))
	}))
	if ce := get(t, h, "/api/v1/workspaces/w/attachments/a", map[string]string{"Accept-Encoding": "gzip"}).Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("an image was compressed (%s)", ce)
	}
}

// An SSE event reaches the client when the handler flushes it, not when the
// stream ends: a compressor holding it in its window would starve the client.
func TestCompressDoesNotBufferAnSSEStream(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(CompressResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
	})))
	defer srv.Close()
	defer close(release)

	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/events/stream", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	tr := &http.Transport{DisableCompression: true}
	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("the SSE stream was compressed (%s)", ce)
	}
	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		if l != "data: first\n" {
			t.Fatalf("first line %q", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the flushed event never arrived while the stream was open")
	}
}

func TestCompressIsWiredIntoTheServer(t *testing.T) {
	srv := testServer(t)
	req := httptest.NewRequest("GET", "/api/v1/health", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("GET /api/v1/health with Accept-Encoding: gzip answered Content-Encoding %q", rec.Header().Get("Content-Encoding"))
	}
}

// A write's response is never compressed: every secret the API mints comes
// back from one (codex r1). Nor a HEAD's: its Content-Length is the answer
// (codex r3).
func TestCompressLeavesWriteResponsesAlone(t *testing.T) {
	h := CompressResponses(jsonHandler())
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "HEAD"} {
		req := httptest.NewRequest(m, "/api/v1/workspaces/w/members/invite", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if ce := rec.Header().Get("Content-Encoding"); ce != "" {
			t.Errorf("%s: compressed (%s)", m, ce)
		}
	}
}

// A compressed response that flushes (the account export flushes after each
// workspace) still delivers each flushed part while the handler runs: chi's
// writer flushes the gzip stream and then the connection.
func TestCompressedStreamStillDeliversFlushes(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(CompressResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"first":true}`+"\n")
		w.(http.Flusher).Flush()
		<-release
	})))
	defer srv.Close()
	defer close(release)

	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/me/export", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding %q; want gzip", resp.Header.Get("Content-Encoding"))
	}
	line := make(chan string, 1)
	go func() {
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			line <- "error: " + err.Error()
			return
		}
		l, _ := bufio.NewReader(zr).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		if l != `{"first":true}`+"\n" {
			t.Fatalf("first line %q", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the flushed part never arrived while the response was open")
	}
}
