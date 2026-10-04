package appfetch

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TASK-3408 (U10b): the app webhook Poster.

func TestPoster_PolicyAndPrivateOrigins(t *testing.T) {
	var redirected atomic.Int32
	srv, cfg := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hooks/in":
			w.WriteHeader(http.StatusNoContent)
		case "/moved":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		case "/elsewhere":
			redirected.Add(1)
		}
	}))
	webhookEntry := []PrivateOrigin{{Origin: srv.URL, Allowed: []string{"127.0.0.1"}, Webhook: true, Path: "/hooks/"}}

	p, err := NewPoster(webhookEntry, 5*time.Second, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if st, err := p.Post(ctx, srv.URL+"/hooks/in", []byte("{}"), http.Header{}); err != nil || st != http.StatusNoContent {
		t.Fatalf("pinned webhook origin: %d %v", st, err)
	}
	// Outside the entry's path.
	if _, err := p.Post(ctx, srv.URL+"/moved", nil, http.Header{}); !errors.Is(err, ErrRefused) {
		t.Fatalf("outside the path: %v, want refused", err)
	}
	// Not https.
	if _, err := p.Post(ctx, "http://127.0.0.1/hooks/in", nil, http.Header{}); !errors.Is(err, ErrRefused) {
		t.Fatalf("http: %v, want refused", err)
	}

	// A fetch-only entry does not open the origin to webhooks.
	fetchOnly, err := NewPoster(pinnedTo(srv), 5*time.Second, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetchOnly.Post(ctx, srv.URL+"/hooks/in", nil, http.Header{}); err == nil {
		t.Fatal("a fetch-only private entry let a webhook reach a loopback address")
	}

	// A redirect is returned, never followed.
	noPath, err := NewPoster([]PrivateOrigin{{Origin: srv.URL, Allowed: []string{"127.0.0.1"}, Webhook: true}}, 5*time.Second, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := noPath.Post(ctx, srv.URL+"/moved", nil, http.Header{}); err != nil || st != http.StatusFound {
		t.Fatalf("redirect: %d %v, want the 302 itself", st, err)
	}
	if redirected.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

// codex r1/r2 on U10b: a server can answer while the body (or the
// transport's buffered flush) is still going out. Once Post returns, no byte
// may be written: the caller's in-flight record ends then.
func TestPoster_NoWriteAfterReturn(t *testing.T) {
	srv, cfg := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		w.(http.Flusher).Flush()
		// Keep reading, slowly, so the client keeps writing.
		buf := make([]byte, 4096)
		for i := 0; i < 50; i++ {
			if _, err := r.Body.Read(buf); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}))
	p, err := NewPoster([]PrivateOrigin{{Origin: srv.URL, Allowed: []string{"127.0.0.1"}, Webhook: true}}, 10*time.Second, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var returned atomic.Bool
	var writes, late atomic.Int32
	onFencedWrite = func(err error) {
		writes.Add(1)
		if err != nil {
			// The write the seal cut short: make it end late enough that
			// a seal which did not wait for it would return first.
			time.Sleep(50 * time.Millisecond)
		}
		if returned.Load() {
			late.Add(1)
		}
	}
	t.Cleanup(func() { onFencedWrite = nil })
	body := make([]byte, 8<<20)
	for i := 0; i < 3; i++ {
		if _, err := p.Post(context.Background(), srv.URL+"/hooks", body, http.Header{}); err != nil {
			t.Logf("post: %v", err)
		}
		returned.Store(true)
		time.Sleep(300 * time.Millisecond)
		if n := late.Load(); n != 0 {
			t.Fatalf("round %d: %d writes still running when Post returned", i, n)
		}
		returned.Store(false)
	}
	if writes.Load() == 0 {
		t.Fatal("no write went through the fence: the connection was not wrapped")
	}
}

func TestPoster_DuplicateEntriesKeepEveryPath(t *testing.T) {
	srv, cfg := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	entries := []PrivateOrigin{
		{Origin: srv.URL, Allowed: []string{"127.0.0.1"}, Webhook: true, Path: "/hooks/a"},
		{Origin: srv.URL, Allowed: []string{"127.0.0.1"}, Webhook: true, Path: "/hooks/b"},
	}
	p, err := NewPoster(entries, 5*time.Second, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/hooks/a", "/hooks/b/x"} {
		if st, err := p.Post(context.Background(), srv.URL+path, nil, http.Header{}); err != nil || st != 204 {
			t.Errorf("%s: %d %v, want 204", path, st, err)
		}
	}
	if _, err := p.Post(context.Background(), srv.URL+"/hooks/c", nil, http.Header{}); !errors.Is(err, ErrRefused) {
		t.Errorf("/hooks/c: %v, want refused", err)
	}
}

func TestPoster_PathCannotEscapeItsPrefix(t *testing.T) {
	srv, cfg := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	p, err := NewPoster([]PrivateOrigin{{Origin: srv.URL, Allowed: []string{"127.0.0.1"}, Webhook: true, Path: "/hooks"}}, 5*time.Second, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/hooks/../admin", "/hooks/./x", "/hooks/%2e%2e/admin", "/hooks%2fadmin", "/hooksevil", "/hook", "/admin"} {
		if _, err := p.Post(context.Background(), srv.URL+path, nil, http.Header{}); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v, want refused", path, err)
		}
	}
	for _, path := range []string{"/hooks", "/hooks/in", "/hooks/a/b"} {
		if st, err := p.Post(context.Background(), srv.URL+path, nil, http.Header{}); err != nil || st != 204 {
			t.Errorf("%s: %d %v, want 204", path, st, err)
		}
	}
}
