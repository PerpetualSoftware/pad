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
	"sync"
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

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// Fetcher fetches under the policy. Build one per install attempt with New.
type Fetcher struct {
	client  *http.Client
	private map[string][]*net.IPNet // origin -> pinned nets, fetch-flagged only
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
// screened. Its private destinations are the admin entries flagged Webhook,
// and an entry with a Path admits only URLs under that path.
type Poster struct {
	f     *Fetcher
	paths map[string]string // origin -> required path prefix, webhook entries with one
}

// NewPoster builds a Poster. private is the admin list (nil on Cloud).
// timeout is a ceiling; the caller's context deadline is the real bound.
func NewPoster(private []PrivateOrigin, timeout time.Duration, tlsConfig *tls.Config) (*Poster, error) {
	f, err := newFetcher(private, func(p PrivateOrigin) bool { return p.Webhook }, timeout, tlsConfig)
	if err != nil {
		return nil, err
	}
	p := &Poster{f: f, paths: map[string]string{}}
	for _, e := range private {
		if !e.Webhook || e.Path == "" {
			continue
		}
		origin, err := appmanifest.NormalizeOrigin(e.Origin)
		if err != nil {
			return nil, err
		}
		p.paths[origin] = e.Path
	}
	return p, nil
}

// Post sends body to rawURL with header and returns the status. The response
// body is drained up to a small bound and discarded. An error wrapping
// ErrRefused is the policy's (permanent); any other error is the network's.
func (p *Poster) Post(ctx context.Context, rawURL string, body []byte, header http.Header) (int, error) {
	return p.post(ctx, rawURL, bytes.NewReader(body), int64(len(body)), header)
}

func (p *Poster) post(ctx context.Context, rawURL string, body io.Reader, size int64, header http.Header) (int, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return 0, refused("%q is not an https URL", rawURL)
	}
	origin, err := appmanifest.NormalizeOrigin("https://" + u.Host)
	if err != nil {
		return 0, refused("%q: %v", rawURL, err)
	}
	if prefix, ok := p.paths[origin]; ok && !underPath(u, prefix) {
		return 0, refused("%s is outside the path %s allowed for %s", u.EscapedPath(), prefix, origin)
	}
	// The transport closes the request body when it has finished writing it
	// (or given up). Post does not return before that: a server may answer
	// while the body is still being written, and the caller's fence must not
	// end while a byte can still go out (codex r1 on U10b).
	rb := &closeSignal{Reader: body, closed: make(chan struct{})}
	reqCtx, cancel := context.WithCancel(context.WithValue(ctx, originKey{}, origin))
	defer func() {
		cancel()
		<-rb.closed
	}()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, u.String(), rb)
	if err != nil {
		rb.Close()
		return 0, refused("%v", err)
	}
	req.ContentLength = size
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := p.f.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
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

// closeSignal is a request body that reports when the transport closed it.
type closeSignal struct {
	io.Reader
	once   sync.Once
	closed chan struct{}
}

func (c *closeSignal) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
