package appfetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// tlsServer serves h over TLS on 127.0.0.1 and returns its origin plus a
// TLS config trusting it.
func tlsServer(t *testing.T, h http.Handler) (*httptest.Server, *tls.Config) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, &tls.Config{RootCAs: pool}
}

func pinnedTo(srv *httptest.Server) []PrivateOrigin {
	return []PrivateOrigin{{Origin: srv.URL, Allowed: []string{"127.0.0.1"}, Fetch: true}}
}

func TestGet_PinnedPrivateOriginFetches(t *testing.T) {
	srv, cfg := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("manifest")) }))
	f, err := New(pinnedTo(srv), 5*time.Second, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.Get(context.Background(), srv.URL+"/.well-known/pad-app.json", 100)
	if err != nil || string(got) != "manifest" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// Without the admin entry, the same loopback server is refused at dial time.
func TestGet_PrivateAddressRefusedWithoutAnEntry(t *testing.T) {
	srv, cfg := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) }))
	for name, list := range map[string][]PrivateOrigin{
		"no list":             nil,
		"entry without fetch": {{Origin: srv.URL, Allowed: []string{"127.0.0.1"}, Fetch: false, Webhook: true}},
		"another origin":      {{Origin: "https://other.example", Allowed: []string{"127.0.0.1"}, Fetch: true}},
	} {
		t.Run(name, func(t *testing.T) {
			f, err := New(list, 5*time.Second, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Get(context.Background(), srv.URL+"/m", 100); !errors.Is(err, ErrRefused) {
				t.Fatalf("got %v, want ErrRefused", err)
			}
		})
	}
}

// A pinned origin dials ONLY its pinned addresses, even public ones it
// resolves to elsewhere.
func TestGet_PinnedOriginOutsideItsPinsIsRefused(t *testing.T) {
	srv, cfg := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) }))
	f, err := New([]PrivateOrigin{{Origin: srv.URL, Allowed: []string{"10.9.9.9/32"}, Fetch: true}}, 5*time.Second, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get(context.Background(), srv.URL+"/m", 100); !errors.Is(err, ErrRefused) {
		t.Fatalf("got %v", err)
	}
}

// A hostname resolving to a private address is refused at dial: DNS cannot
// launder an address the literal check would refuse.
func TestGet_HostnameResolvingPrivateIsRefused(t *testing.T) {
	srv, cfg := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) }))
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	orig := lookupIP
	lookupIP = func(ctx context.Context, network, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	t.Cleanup(func() { lookupIP = orig })
	f, err := New(nil, 5*time.Second, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get(context.Background(), "https://app.example:"+port+"/m", 100); !errors.Is(err, ErrRefused) {
		t.Fatalf("got %v", err)
	}
}

func TestGet_RedirectStatusAndSizeAreRefused(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) })
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(strings.Repeat("x", 101))) })
	mux.HandleFunc("/big-chunked", func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for i := 0; i < 11; i++ {
			_, _ = w.Write([]byte(strings.Repeat("y", 10)))
			fl.Flush()
		}
	})
	srv, cfg := tlsServer(t, mux)
	f, err := New(pinnedTo(srv), 5*time.Second, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/redirect", "/missing", "/big", "/big-chunked"} {
		if _, err := f.Get(context.Background(), srv.URL+p, 100); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: got %v, want ErrRefused", p, err)
		}
	}
	if got, err := f.Get(context.Background(), srv.URL+"/ok", 100); err != nil || string(got) != "ok" {
		t.Errorf("/ok: %q %v", got, err)
	}
}

func TestGet_OnlyHTTPS(t *testing.T) {
	f, err := New(nil, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"http://example.com/m", "ftp://example.com/m", "https://user:pw@example.com/m", "/relative"} {
		if _, err := f.Get(context.Background(), u, 10); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: got %v", u, err)
		}
	}
}

func TestNew_RejectsBadEntries(t *testing.T) {
	for _, p := range []PrivateOrigin{
		{Origin: "http://x.example", Allowed: []string{"10.0.0.1"}, Fetch: true},
		{Origin: "https://x.example", Fetch: true},
		{Origin: "https://x.example", Allowed: []string{"not-an-ip"}, Fetch: true},
	} {
		if _, err := New([]PrivateOrigin{p}, time.Second, nil); err == nil {
			t.Errorf("accepted %+v", p)
		}
	}
}
