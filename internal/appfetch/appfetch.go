// Package appfetch is the fetch policy for app manifests and companion-pack
// artifacts (SPEC-6, DOC-3371 §2 step 1 and §9):
//   - https only, and never a redirect: a 3xx is a failure, not followed;
//   - no proxy from the environment;
//   - a size cap per response, enforced while reading;
//   - every connection screened at DIAL time against the address actually
//     dialed: a non-public address (netpolicy.Blocked) is refused unless the
//     URL's exact origin is on the instance admin's private-origin list with
//     the fetch flag, and then only an address inside that entry's pinned
//     IPs or CIDRs is dialed. Never a hostname alone, never a skip switch.
//
// Content hashes are the caller's check (the manifest declares them).
package appfetch

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/appmanifest"
	"github.com/PerpetualSoftware/pad/internal/netpolicy"
)

// PrivateOrigin is one entry of the admin's private-destination list (§9):
// an exact origin, the addresses it may resolve to, and what it may be used
// for. Only Fetch matters here; Webhook is A5's.
type PrivateOrigin struct {
	Origin  string   `json:"origin"`
	Allowed []string `json:"allowed"` // IPs or CIDRs
	Fetch   bool     `json:"fetch"`
	Webhook bool     `json:"webhook"`
	Path    string   `json:"path,omitempty"` // webhook only
}

// ErrRefused is any fetch the policy refuses: scheme, redirect, status,
// address or size. The wrapped message says which.
var ErrRefused = errors.New("app fetch refused")

// ErrNotSent marks a Poster.Post failure that happened before any byte of
// the request was written (dial, TLS handshake), so a delivery ledger does
// not count a request the endpoint never received (codex r3 on U10c). A
// policy refusal (ErrRefused) is also unsent.
var ErrNotSent = errors.New("request not sent")

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// Fetcher fetches under the policy. Build one per install attempt with New.
type Fetcher struct {
	client  *http.Client
	private map[string][]*net.IPNet // origin -> pinned nets, fetch-flagged only
	tls     *tls.Config             // nil: the system roots
	dialer  *net.Dialer
}

// lookupIP is the resolver; a variable so tests can resolve without DNS.
var lookupIP = net.DefaultResolver.LookupIP

// New builds a Fetcher. private is the admin list (self-host only; Cloud
// passes nil, so no private destination is ever reachable). tlsConfig is nil
// in production; tests pass one that trusts their server.
func New(private []PrivateOrigin, timeout time.Duration, tlsConfig *tls.Config) (*Fetcher, error) {
	return newFetcher(private, func(p PrivateOrigin) bool { return p.Fetch }, timeout, tlsConfig)
}

// newFetcher builds the client with the private entries use selects.
func newFetcher(private []PrivateOrigin, use func(PrivateOrigin) bool, timeout time.Duration, tlsConfig *tls.Config) (*Fetcher, error) {
	f := &Fetcher{private: map[string][]*net.IPNet{}}
	for i, p := range private {
		if !use(p) {
			continue
		}
		origin, err := appmanifest.NormalizeOrigin(p.Origin)
		if err != nil {
			return nil, fmt.Errorf("private origin %d: %v", i, err)
		}
		if len(p.Allowed) == 0 {
			return nil, fmt.Errorf("private origin %s: no pinned addresses", origin)
		}
		for _, a := range p.Allowed {
			n, err := parseNet(a)
			if err != nil {
				return nil, fmt.Errorf("private origin %s: %v", origin, err)
			}
			f.private[origin] = append(f.private[origin], n)
		}
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	f.tls, f.dialer = tlsConfig, dialer
	transport := &http.Transport{
		Proxy:                 nil,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return f.dial(ctx, dialer, network, addr)
		},
	}
	f.client = &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return f, nil
}

func parseNet(s string) (*net.IPNet, error) {
	if strings.Contains(s, "/") {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("bad CIDR %q", s)
		}
		return n, nil
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("bad IP %q", s)
	}
	bits := 128
	if ip.To4() != nil {
		ip, bits = ip.To4(), 32
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, nil
}

type originKey struct{}

// dial screens the address actually dialed. The request's origin rides in the
// context, because the transport sees only host:port.
func (f *Fetcher) dial(ctx context.Context, d *net.Dialer, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, refused("bad address %q", addr)
	}
	origin, _ := ctx.Value(originKey{}).(string)
	pinned := f.private[origin]
	if only, ok := ctx.Value(pinsKey{}).([]*net.IPNet); ok {
		// The caller narrowed the pins to the entries that admit this
		// request (Poster: entries whose path matches).
		pinned = only
	}

	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		ips, err = lookupIP(ctx, "ip", host)
		if err != nil {
			return nil, refused("resolve %s: %v", host, err)
		}
	}
	var lastErr error = refused("%s resolves to no permitted address", host)
	for _, ip := range ips {
		if !permitted(ip, pinned) {
			continue
		}
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// permitted: a pinned origin may dial only its pinned addresses (public or
// not); any other origin may dial only public addresses.
func permitted(ip net.IP, pinned []*net.IPNet) bool {
	if len(pinned) > 0 {
		for _, n := range pinned {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
	return !netpolicy.Blocked(ip)
}

// Get fetches rawURL and returns at most maxBytes of body. Anything else
// (not https, a redirect, a non-200 status, a longer body, a refused
// address) is an error wrapping ErrRefused or a transport error.
func (f *Fetcher) Get(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, refused("%q is not an https URL", rawURL)
	}
	origin, err := appmanifest.NormalizeOrigin("https://" + u.Host)
	if err != nil {
		return nil, refused("%q: %v", rawURL, err)
	}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, originKey{}, origin), http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, refused("%v", err)
	}
	req.Header.Set("Accept", "application/json, text/markdown, text/plain, */*")
	req.Header.Set("User-Agent", "Pad-App-Installer/1")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, refused("%s redirected (%d); redirects are never followed", rawURL, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, refused("%s answered %d", rawURL, resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return nil, refused("%s is larger than %d bytes", rawURL, maxBytes)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, refused("%s is larger than %d bytes", rawURL, maxBytes)
	}
	return body, nil
}

// Poster sends app webhooks under the same policy (DOC-3371 §5, §9; TASK-3408
// U10b): https only, never a redirect (a 3xx is returned as a status, which
// the caller treats as a failed attempt), no environment proxy, every dial
// screened. Its private destinations are the admin entries flagged Webhook;
// an entry with a Path admits only URLs under that path.
//
// It does NOT use net/http's Transport. The transport may hand back a
// response while it is still writing the request, and stops writing on its
// own schedule. A webhook's in-flight record must end only when no byte can
// still go out, and a 2xx must mean the whole request was sent (codex r1-r3
// on U10b). So Post writes the whole request itself, on its own connection,
// then reads the response, then closes the connection, all on the caller's
// goroutine: when it returns, nothing else can write.
type Poster struct {
	f       *Fetcher
	entries map[string][]posterEntry // origin -> its webhook entries
	timeout time.Duration            // ceiling on one Post, under the caller's deadline
}

// posterEntry is one webhook-flagged admin entry: its pinned addresses and
// the path it admits ("" = any). Kept per ENTRY, never merged per origin:
// two entries for one origin admit their own path at their own addresses
// only (codex r4 on U10b).
type posterEntry struct {
	nets []*net.IPNet
	path string
}

type pinsKey struct{}

// posterHandshakeTimeout bounds the TLS handshake, as the fetch Transport's
// TLSHandshakeTimeout does.
const posterHandshakeTimeout = 10 * time.Second

// NewPoster builds a Poster. private is the admin list (nil on Cloud).
// The caller's context deadline bounds each Post.
func NewPoster(private []PrivateOrigin, timeout time.Duration, tlsConfig *tls.Config) (*Poster, error) {
	f, err := newFetcher(private, func(p PrivateOrigin) bool { return p.Webhook }, timeout, tlsConfig)
	if err != nil {
		return nil, err
	}
	p := &Poster{f: f, entries: map[string][]posterEntry{}, timeout: timeout}
	for _, e := range private {
		if !e.Webhook {
			continue
		}
		origin, err := appmanifest.NormalizeOrigin(e.Origin)
		if err != nil {
			return nil, err
		}
		pe := posterEntry{path: e.Path}
		for _, a := range e.Allowed {
			n, err := parseNet(a)
			if err != nil {
				return nil, fmt.Errorf("private origin %s: %v", origin, err)
			}
			pe.nets = append(pe.nets, n)
		}
		// Every entry is kept: duplicate entries for one origin add paths
		// (codex r2), each with its own addresses (codex r4).
		p.entries[origin] = append(p.entries[origin], pe)
	}
	return p, nil
}

// postResponseMax bounds the response head and body Post reads.
const postResponseMax = 64 << 10

// Post sends body to rawURL with header and returns the status, having
// written the WHOLE request before reading any response. A server that stops
// reading early makes the write fail, which is an error here, never a
// success. An error wrapping ErrRefused is the policy's (permanent); any
// other error is the network's.
func (p *Poster) Post(ctx context.Context, rawURL string, body []byte, header http.Header) (int, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return 0, refused("%q is not an https URL", rawURL)
	}
	origin, err := appmanifest.NormalizeOrigin("https://" + u.Host)
	if err != nil {
		return 0, refused("%q: %v", rawURL, err)
	}
	var pins []*net.IPNet
	if entries, ok := p.entries[origin]; ok {
		for _, e := range entries {
			if e.path == "" || underPath(u, e.path) {
				pins = append(pins, e.nets...)
			}
		}
		if len(pins) == 0 {
			return 0, refused("%s is outside the paths allowed for %s", u.EscapedPath(), origin)
		}
		ctx = context.WithValue(ctx, pinsKey{}, pins)
	}
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	// f.dial screens every resolved address and dials the screened IP
	// itself, so nothing is resolved between the screen and the connect.
	raw, err := p.f.dial(context.WithValue(ctx, originKey{}, origin), p.f.dialer, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	// The system roots unless a test supplies its own; the hostname is
	// verified against the certificate (ServerName); never InsecureSkipVerify.
	cfg := &tls.Config{}
	if p.f.tls != nil {
		cfg = p.f.tls.Clone()
	}
	cfg.ServerName = u.Hostname()
	cfg.InsecureSkipVerify = false
	cfg.MinVersion = tls.VersionTLS12
	cfg.NextProtos = []string{"http/1.1"}
	conn := tls.Client(raw, cfg)
	// Closed before Post returns, on every path. Every read and write below
	// runs on this goroutine, so once Post returns nothing can write. The
	// AfterFunc only moves the deadline, to unblock a read or write when ctx
	// ends; it never writes.
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	hctx, hcancel := context.WithTimeout(ctx, posterHandshakeTimeout)
	err = conn.HandshakeContext(hctx)
	hcancel()
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, refused("%v", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Close = true // one request per connection
	// Count what reached the connection: a write that fails before its
	// first byte (a reset or the deadline right after the handshake) sent
	// nothing (codex r4 on U10c).
	cw := &countingWriter{w: conn}
	bw := bufio.NewWriter(cw)
	if err := req.Write(bw); err != nil {
		return 0, unsentIfNothingWritten(cw, err)
	}
	if err := bw.Flush(); err != nil {
		return 0, unsentIfNothingWritten(cw, err)
	}
	// Interim 1xx answers (100 Continue, 103 Early Hints) precede the final
	// one and are skipped, as net/http's Transport does (codex r4 on U10b);
	// they share the one 64 KiB bound. 101 is a protocol switch, never a
	// webhook answer.
	br := bufio.NewReader(io.LimitReader(conn, postResponseMax))
	for {
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			return 0, err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusSwitchingProtocols {
			return 0, refused("%s answered 101 Switching Protocols", u.Redacted())
		}
		if resp.StatusCode >= 100 && resp.StatusCode < 200 {
			continue
		}
		return resp.StatusCode, nil
	}
}

// underPath reports whether u's path lies under prefix, by whole segments.
// A path carrying dot segments or percent-encoding is refused outright: a
// server may normalise "/hooks/../admin" or "/hooks/%2e%2e/admin" to a path
// outside the prefix that a string comparison accepted (codex r1 on U10b).
func underPath(u *url.URL, prefix string) bool {
	if u.RawPath != "" || strings.Contains(u.EscapedPath(), "%") {
		return false
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(u.Path, prefix)
	}
	return u.Path == prefix || strings.HasPrefix(u.Path, prefix+"/")
}

// countingWriter counts the bytes its writer accepted.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func unsentIfNothingWritten(cw *countingWriter, err error) error {
	if cw.n == 0 {
		return fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	return err
}
