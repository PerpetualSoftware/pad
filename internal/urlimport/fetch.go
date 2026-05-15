// Package urlimport fetches remote URLs and converts the response into
// markdown that can be inserted into a Pad item. The package is the
// server-side primitive behind the editor's "Insert from URL" toolbar.
//
// Fetcher applies a strict SSRF guard, a per-request timeout, and a
// response-size cap so a malicious or large upstream cannot abuse the
// in-process HTTP client. The default policy blocks loopback, RFC1918,
// CGNAT, IPv4 link-local (including 169.254.169.254 cloud-metadata),
// IPv6 unique local, IPv6 link-local, and the unspecified address.
//
// See PLAN-1467 ("Insert from URL — HTML→Markdown utility in the item
// editor") for the design discussion.
package urlimport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout caps how long Fetch waits for a single upstream request.
const DefaultTimeout = 10 * time.Second

// DefaultMaxBytes caps how many response bytes Fetch reads before
// aborting. 5 MB is a generous ceiling for HTML docs and OpenAPI specs
// without letting a 200 MB PDF or zip-bomb through.
const DefaultMaxBytes int64 = 5 * 1024 * 1024

// DefaultMaxRedirects is the redirect-chain limit applied by Fetch. Every
// hop in the chain is re-validated against the SSRF guard so the initial
// check cannot be bypassed via a 302 to an internal host.
const DefaultMaxRedirects = 5

// DefaultUserAgent is the User-Agent header sent with each fetch. Some
// hosts (Cloudflare, GitHub) serve different content to "default" Go
// clients vs. branded agents, so we identify as Pad.
const DefaultUserAgent = "Pad-URLImport/1.0 (+https://getpad.dev)"

// FetchResult is the outcome of a successful Fetch.
type FetchResult struct {
	// URL is the final resolved URL after any redirects.
	URL string
	// StatusCode is the HTTP status from the upstream.
	StatusCode int
	// ContentType is the raw Content-Type header value (with parameters).
	ContentType string
	// Body is the response body, capped to MaxBytes.
	Body []byte
}

// Fetcher performs SSRF-guarded HTTP GETs.
//
// The zero value is not usable — call NewFetcher. Fields are exported
// only to allow tests to flip AllowLocal; production code should treat
// the struct as opaque.
type Fetcher struct {
	// Transport is the underlying RoundTripper used by the HTTP client.
	// Tests inject a custom transport to point at httptest servers.
	Transport http.RoundTripper
	// Timeout overrides DefaultTimeout when non-zero.
	Timeout time.Duration
	// MaxBytes overrides DefaultMaxBytes when positive.
	MaxBytes int64
	// MaxRedirects overrides DefaultMaxRedirects when positive.
	MaxRedirects int
	// UserAgent overrides DefaultUserAgent when non-empty.
	UserAgent string
	// AllowLocal disables the SSRF guard. INTENDED FOR TESTS ONLY —
	// production handlers must leave this false.
	AllowLocal bool
}

// NewFetcher returns a Fetcher with production defaults applied.
func NewFetcher() *Fetcher {
	return &Fetcher{
		Timeout:      DefaultTimeout,
		MaxBytes:     DefaultMaxBytes,
		MaxRedirects: DefaultMaxRedirects,
		UserAgent:    DefaultUserAgent,
	}
}

// Fetch performs a GET against rawURL, re-validating every redirect hop
// and aborting if the response exceeds the size cap. Returns a wrapped
// error suitable for surfacing to API callers.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*FetchResult, error) {
	if !f.AllowLocal {
		if err := ValidateURL(rawURL); err != nil {
			return nil, err
		}
	}

	maxRedirects := f.MaxRedirects
	if maxRedirects <= 0 {
		maxRedirects = DefaultMaxRedirects
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxBytes := f.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	ua := f.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}

	client := &http.Client{
		Timeout:   timeout,
		Transport: f.Transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("too many redirects (> %d)", maxRedirects)
			}
			if !f.AllowLocal {
				if err := ValidateURL(req.URL.String()); err != nil {
					return fmt.Errorf("redirect to %s blocked: %w", req.URL.Redacted(), err)
				}
			}
			// Strip Authorization on cross-origin redirects — defense in
			// depth even though we never set credentials ourselves.
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization")
				req.Header.Del("Cookie")
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html, application/xhtml+xml, application/xml;q=0.9, application/json;q=0.9, text/plain;q=0.8, */*;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", redactURL(rawURL), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream returned status %d for %s", resp.StatusCode, redactURL(rawURL))
	}

	// Read up to maxBytes+1; one byte over the cap is enough to know
	// the upstream exceeded the limit.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("response exceeds maximum size of %d bytes", maxBytes)
	}

	finalURL := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	return &FetchResult{
		URL:         finalURL,
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
	}, nil
}

// ValidateURL applies the SSRF guard: only http(s) schemes, no embedded
// credentials, hostname must resolve to public IPs only. Returns nil
// when the URL is safe to fetch.
func ValidateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		// ok
	default:
		return fmt.Errorf("unsupported scheme %q: only http and https are allowed", u.Scheme)
	}
	if u.User != nil {
		return errors.New("URLs with embedded credentials are not allowed")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("URL must have a hostname")
	}

	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return fmt.Errorf("URL targets private or reserved IP %s", ip)
		}
		return nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("resolve hostname %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("hostname %q resolved to no addresses", host)
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return fmt.Errorf("hostname %q resolves to private/reserved IP %s", host, ip)
		}
	}
	return nil
}

// isPrivateIP returns true for any IP we refuse to fetch from. This
// includes loopback, RFC1918, IPv4/IPv6 link-local (catches the AWS/GCP/
// Azure cloud-metadata IP 169.254.169.254), IPv6 unique-local, CGNAT,
// and the unspecified address.
func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsPrivate() {
		return true
	}
	// CGNAT (RFC 6598) — not covered by IsPrivate but commonly used on
	// the LAN side of consumer routers / mobile carriers.
	if cgnatCIDR.Contains(ip) {
		return true
	}
	return false
}

// cgnatCIDR is precomputed at init so isPrivateIP stays allocation-free
// and concurrency-safe (no shared map writes).
var cgnatCIDR = func() *net.IPNet {
	_, n, err := net.ParseCIDR("100.64.0.0/10")
	if err != nil {
		panic(fmt.Errorf("urlimport: parse cgnat cidr: %w", err))
	}
	return n
}()

// redactURL hides credentials from log/error output. We already reject
// credential-bearing URLs at validation time, but Go can hand us an
// already-stripped url.URL via redirects — defensive double check.
func redactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Redacted()
}
