package urlimport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		// Schemes
		{"http public", "http://example.com/foo", false},
		{"https public", "https://example.com/foo", false},
		{"file scheme rejected", "file:///etc/passwd", true},
		{"ftp scheme rejected", "ftp://example.com/foo", true},
		{"javascript scheme rejected", "javascript:alert(1)", true},
		{"gopher scheme rejected", "gopher://example.com", true},

		// Credentials
		{"basic auth rejected", "https://user:pass@example.com/foo", true},
		{"user-only rejected", "https://user@example.com/foo", true},

		// Hostname
		{"no host rejected", "https:///foo", true},
		{"empty host rejected", "https://", true},
		{"malformed url rejected", "://bad-url", true},

		// IPv4 private ranges
		{"loopback 127.0.0.1", "http://127.0.0.1/foo", true},
		{"loopback range 127.5.5.5", "http://127.5.5.5/foo", true},
		{"rfc1918 10.x", "http://10.0.0.1/foo", true},
		{"rfc1918 172.16.x", "http://172.16.0.1/foo", true},
		{"rfc1918 172.31.x boundary", "http://172.31.255.255/foo", true},
		{"rfc1918 192.168.x", "http://192.168.1.1/foo", true},

		// IPv4 link-local + cloud metadata
		{"link-local 169.254.x", "http://169.254.1.1/foo", true},
		{"AWS metadata 169.254.169.254", "http://169.254.169.254/latest/meta-data/", true},

		// IPv4 special / CGNAT
		{"unspecified 0.0.0.0", "http://0.0.0.0/foo", true},
		{"cgnat 100.64.x", "http://100.64.0.1/foo", true},
		{"cgnat 100.127.x boundary", "http://100.127.255.255/foo", true},

		// IPv6
		{"ipv6 loopback ::1", "http://[::1]/foo", true},
		{"ipv6 unspecified ::", "http://[::]/foo", true},
		{"ipv6 link-local fe80::", "http://[fe80::1]/foo", true},
		{"ipv6 unique-local fc00::", "http://[fc00::1]/foo", true},
		{"ipv6 unique-local fd00::", "http://[fd00::1]/foo", true},

		// Boundary: a public IPv4 (1.1.1.1 — Cloudflare DNS, well-known public)
		{"public ipv4 1.1.1.1", "http://1.1.1.1/foo", false},

		// Hostname resolution — these depend on DNS being available; we
		// only test parse-level cases here and rely on the IP-literal
		// tests above for guard correctness. example.com is RFC2606 and
		// always resolves to a public IP when DNS works; if the test
		// environment has no DNS the test will skip via t.Skip below.
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateURL(tc.url)
			gotErr := err != nil
			if gotErr != tc.wantErr {
				t.Fatalf("ValidateURL(%q): err = %v, wantErr = %v", tc.url, err, tc.wantErr)
			}
		})
	}
}

func TestFetch_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><body>hello</body></html>")
	}))
	defer srv.Close()

	f := NewFetcher()
	f.AllowLocal = true // httptest binds to 127.0.0.1
	res, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if !strings.Contains(res.ContentType, "text/html") {
		t.Fatalf("content-type = %q, want text/html prefix", res.ContentType)
	}
	if !strings.Contains(string(res.Body), "hello") {
		t.Fatalf("body = %q, want to contain 'hello'", string(res.Body))
	}
}

func TestFetch_SSRFRejected(t *testing.T) {
	// Loopback URL goes straight through ValidateURL — no need for a
	// server. AllowLocal stays false (the production default).
	f := NewFetcher()
	_, err := f.Fetch(context.Background(), "http://127.0.0.1:1/")
	if err == nil {
		t.Fatal("Fetch: expected SSRF rejection, got nil")
	}
}

func TestFetch_SizeCap(t *testing.T) {
	// Server streams 2 MB but the cap is 1 KB.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(200)
		buf := strings.Repeat("A", 1024)
		for i := 0; i < 2048; i++ {
			if _, err := io.WriteString(w, buf); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	f := NewFetcher()
	f.AllowLocal = true
	f.MaxBytes = 1024
	_, err := f.Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("Fetch: expected size-cap error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("error = %v, want 'exceeds maximum size'", err)
	}
}

func TestFetch_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	f := NewFetcher()
	f.AllowLocal = true
	f.Timeout = 100 * time.Millisecond
	_, err := f.Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("Fetch: expected timeout error, got nil")
	}
}

func TestFetch_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", 404)
	}))
	defer srv.Close()

	f := NewFetcher()
	f.AllowLocal = true
	_, err := f.Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("Fetch: expected error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %v, want to mention status 404", err)
	}
}

func TestFetch_RedirectRevalidated(t *testing.T) {
	// Initial URL is a public IP literal (1.1.1.1) so ValidateURL passes,
	// but a stubbed transport returns a 302 → 127.0.0.1. The CheckRedirect
	// closure must re-run ValidateURL and abort the redirect chain.
	stub := &stubTransport{
		respond: func(req *http.Request) *http.Response {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"http://127.0.0.1/internal"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}
		},
	}
	f := NewFetcher()
	f.AllowLocal = false
	f.Transport = stub
	_, err := f.Fetch(context.Background(), "http://1.1.1.1/foo")
	if err == nil {
		t.Fatal("Fetch: expected redirect-to-loopback rejection, got nil")
	}
	if !strings.Contains(err.Error(), "redirect") && !strings.Contains(err.Error(), "private") {
		t.Fatalf("error = %v, want to mention redirect/private", err)
	}
}

type stubTransport struct {
	respond func(*http.Request) *http.Response
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return s.respond(req), nil
}

func TestFetch_ContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	f := NewFetcher()
	f.AllowLocal = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling Fetch
	_, err := f.Fetch(ctx, srv.URL)
	if err == nil {
		t.Fatal("Fetch: expected context-cancel error, got nil")
	}
}
