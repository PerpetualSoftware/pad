package appfetch

import (
	"context"
	"errors"
	"io"
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

// slowBody reads in small, slow chunks and records any read that is still
// running, or starts, after the caller marked Post as returned.
type slowBody struct {
	left     int
	returned atomic.Bool
	late     atomic.Int32
}

func (b *slowBody) Read(p []byte) (int, error) {
	if b.returned.Load() {
		b.late.Add(1)
	}
	if b.left == 0 {
		return 0, io.EOF
	}
	time.Sleep(15 * time.Millisecond)
	n := min(len(p), 4096, b.left)
	b.left -= n
	if b.returned.Load() {
		b.late.Add(1)
	}
	return n, nil
}

// codex r1 on U10b: a server can answer before the body is written. Post
// must not return while the transport can still read (and so write) the
// body, or the caller's in-flight record ends with bytes still going out.
func TestPoster_ReturnsOnlyAfterTheBodyIsDone(t *testing.T) {
	srv, cfg := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent) // answer without reading the body
	}))
	p, err := NewPoster([]PrivateOrigin{{Origin: srv.URL, Allowed: []string{"127.0.0.1"}, Webhook: true}}, 10*time.Second, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		b := &slowBody{left: 4 << 20}
		if _, err := p.post(context.Background(), srv.URL+"/hooks", b, int64(b.left), http.Header{}); err != nil {
			t.Logf("post: %v", err) // the early answer may surface as a write error; either is fine
		}
		b.returned.Store(true)
		time.Sleep(100 * time.Millisecond)
		if n := b.late.Load(); n != 0 {
			t.Fatalf("round %d: %d body reads after Post returned", i, n)
		}
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
