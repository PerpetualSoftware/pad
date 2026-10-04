package appfetch

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
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

// codex r1-r3 on U10b: a 2xx means the WHOLE request was sent, and a server
// that answers early and stops reading is a failure, never a delivery.
func TestPoster_SuccessMeansTheWholeBodyWasSent(t *testing.T) {
	body := make([]byte, 4<<20)
	var got atomic.Int64
	done := make(chan struct{}, 1)
	slow, cfg := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Answer first, then read the body slowly to the end.
		w.WriteHeader(http.StatusNoContent)
		w.(http.Flusher).Flush()
		n, _ := io.Copy(io.Discard, &slowReader{r: r.Body})
		got.Store(n)
		done <- struct{}{}
	}))
	p, err := NewPoster([]PrivateOrigin{{Origin: slow.URL, Allowed: []string{"127.0.0.1"}, Webhook: true}}, 30*time.Second, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := p.Post(ctx, slow.URL+"/hooks", body, http.Header{})
	if err != nil || st != http.StatusNoContent {
		t.Fatalf("post: %d %v", st, err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the server never finished reading")
	}
	if got.Load() != int64(len(body)) {
		t.Fatalf("a 2xx with %d of %d body bytes delivered", got.Load(), len(body))
	}

	// A server that answers and stops reading: the write fails.
	quitter, cfg2 := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusNoContent)
		w.(http.Flusher).Flush() // the 204 is on the wire before the close
		if hj, ok := w.(http.Hijacker); ok {
			c, _, _ := hj.Hijack()
			_ = c.Close()
		}
	}))
	p2, err := NewPoster([]PrivateOrigin{{Origin: quitter.URL, Allowed: []string{"127.0.0.1"}, Webhook: true}}, 30*time.Second, cfg2)
	if err != nil {
		t.Fatal(err)
	}
	// Larger than any loopback send buffer, so the write cannot complete
	// into the kernel unread.
	big := make([]byte, 64<<20)
	if st, err := p2.Post(ctx, quitter.URL+"/hooks", big, http.Header{}); err == nil {
		t.Fatalf("a server that stopped reading answered as a delivery: %d", st)
	}
}

type slowReader struct{ r io.Reader }

func (s *slowReader) Read(p []byte) (int, error) {
	time.Sleep(time.Millisecond)
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	return s.r.Read(p)
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

// The certificate is verified: a server the roots do not trust is refused,
// and so is a name the certificate does not cover.
func TestPoster_VerifiesTheCertificate(t *testing.T) {
	srv, cfg := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	entry := []PrivateOrigin{{Origin: srv.URL, Allowed: []string{"127.0.0.1"}, Webhook: true}}

	untrusting, err := NewPoster(entry, 5*time.Second, nil) // system roots only
	if err != nil {
		t.Fatal(err)
	}
	if st, err := untrusting.Post(context.Background(), srv.URL+"/hooks", nil, http.Header{}); err == nil {
		t.Fatalf("a self-signed server was accepted: %d", st)
	}

	// Trusted roots, but a hostname the certificate does not name.
	wrongName := cfg.Clone()
	other := strings.Replace(srv.URL, "127.0.0.1", "localhost.invalid", 1)
	lookupWas := lookupIP
	lookupIP = func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	t.Cleanup(func() { lookupIP = lookupWas })
	pinnedOther := []PrivateOrigin{{Origin: other, Allowed: []string{"127.0.0.1"}, Webhook: true}}
	trusting, err := NewPoster(pinnedOther, 5*time.Second, wrongName)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := trusting.Post(context.Background(), other+"/hooks", nil, http.Header{}); err == nil {
		t.Fatalf("a certificate for another name was accepted: %d", st)
	}
}
