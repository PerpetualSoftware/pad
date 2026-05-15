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
	"sync"
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
// only to allow tests to flip AllowLocal and inject a Transport;
// production code should treat the struct as opaque after creation.
//
// Fetcher is safe for concurrent use. The default safe-transport is
// lazily built on first Fetch and reused for subsequent calls so
// keep-alive connections aren't leaked.
type Fetcher struct {
	// Transport is the underlying RoundTripper used by the HTTP client.
	// Tests inject a custom transport to point at httptest servers.
	// When nil, Fetcher builds and caches a safe default.
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

	// safeOnce + safeTransport memoize the default transport so we
	// don't leak per-call *http.Transport instances with their own
	// idle-connection pools.
	safeOnce      sync.Once
	safeTransport *http.Transport
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
//
// The SSRF guard runs in two layers:
//
//  1. ValidateURL — fast-fail pre-flight on scheme, credentials,
//     hostname presence, and IP-literal targets. Does NOT do DNS
//     resolution; see (2).
//  2. A custom dialer that resolves the hostname once and checks every
//     returned IP at dial-time. This is the canonical guard against
//     DNS rebinding: any hostname-lookup happens inside the dialer and
//     the result is reused as the dial target, so the IP we vetted is
//     the IP we connect to.
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

	transport := f.Transport
	if transport == nil {
		transport = f.defaultTransport(timeout)
	}

	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
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

// ValidateURL is the pre-flight SSRF guard: it rejects non-http(s)
// schemes, URLs with embedded credentials, missing hostnames, and
// IP-literal hosts in private/reserved ranges. It deliberately does
// NOT do DNS resolution — that happens once at dial-time inside the
// fetcher's transport (see newSafeTransport), where the resolved IP
// is both checked and reused as the dial target. Doing DNS here would
// open a TOCTOU window allowing DNS rebinding: a malicious server
// returns a public IP for the validation lookup and a private IP for
// the actual fetch.
//
// Callers that just want a quick "is this URL syntactically safe?"
// check (e.g. UI input validation) can call ValidateURL standalone.
// Callers that fetch must use Fetcher, which adds the dial-time check.
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
	}
	return nil
}

// defaultTransport lazily builds and memoizes the package's safe
// transport. Reusing one transport across Fetch calls keeps the
// keep-alive pool bounded and the FD usage flat under load. AllowLocal
// is captured on first use — callers that want to flip it after the
// first Fetch should construct a new Fetcher instead.
func (f *Fetcher) defaultTransport(timeout time.Duration) *http.Transport {
	f.safeOnce.Do(func() {
		f.safeTransport = newSafeTransport(f.AllowLocal, timeout)
	})
	return f.safeTransport
}

// newSafeTransport returns an *http.Transport whose DialContext resolves
// each hostname inside the dialer and validates every returned IP. Only
// IPs that pass isPrivateIP are dialed, and the connection is made to
// the resolved IP directly (no second lookup) so the validated IP is
// the dial target.
//
// HTTP/HTTPS proxies are deliberately NOT honored: when a proxy is in
// use the client connects to the proxy host and the target hostname is
// never resolved by our dialer, which would silently bypass the SSRF
// guard. Operators who need an outbound proxy can wire their own
// trusted transport into Fetcher.Transport instead of relying on
// HTTP_PROXY/HTTPS_PROXY env vars.
//
// allowLocal=true (tests only) bypasses the IP check so httptest servers
// on 127.0.0.1 are reachable.
func newSafeTransport(allowLocal bool, timeout time.Duration) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}
	resolver := net.DefaultResolver
	return &http.Transport{
		// Proxy intentionally nil — see function docstring.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("urlimport: split host/port %q: %w", addr, err)
			}

			// IP literal — already vetted by ValidateURL. Dial directly.
			if ip := net.ParseIP(host); ip != nil {
				if !allowLocal && isPrivateIP(ip) {
					return nil, fmt.Errorf("urlimport: blocked dial to private/reserved IP %s", ip)
				}
				return dialer.DialContext(ctx, network, addr)
			}

			// Hostname — resolve once, validate every IP, dial the first
			// allowed one. This single resolution is the dial target.
			ips, err := resolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("urlimport: resolve %q: %w", host, err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("urlimport: hostname %q resolved to no addresses", host)
			}
			var lastErr error
			for _, ip := range ips {
				if !allowLocal && isPrivateIP(ip) {
					lastErr = fmt.Errorf("urlimport: %q resolves to private/reserved IP %s", host, ip)
					continue
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("urlimport: no usable address for %q", host)
		},
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
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
