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
