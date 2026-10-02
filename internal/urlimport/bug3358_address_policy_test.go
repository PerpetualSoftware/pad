package urlimport

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
)

// BUG-3358: URL import refused only loopback, RFC 1918, link-local and CGNAT,
// so it could reach 198.18.0.0/15 and other reserved ranges that webhooks
// already refused. Each way to name the address is covered: a literal, a
// hostname whose DNS answer is the address, and a redirect.

func TestBUG3358_LiteralReservedAddressRefused(t *testing.T) {
	if err := ValidateURL("http://198.18.0.1/"); err == nil {
		t.Fatal("ValidateURL accepted http://198.18.0.1/")
	}
	_, err := NewFetcher().Fetch(context.Background(), "http://198.18.0.1/")
	if err == nil || !strings.Contains(err.Error(), "198.18.0.1") {
		t.Fatalf("Fetch of a literal reserved address: got %v, want a refusal naming it", err)
	}
}

// The dialer screens a literal address itself, independent of ValidateURL
// (which the literal test above exits through).
func TestBUG3358_DialerScreensLiteral(t *testing.T) {
	tr := newSafeTransport(false, DefaultTimeout)
	_, err := tr.DialContext(context.Background(), "tcp", "198.18.0.1:80")
	if err == nil || !strings.Contains(err.Error(), "blocked dial to private/reserved IP 198.18.0.1") {
		t.Fatalf("dial of a literal reserved address: got %v, want the dial-time refusal", err)
	}
}

func stubLookup(t *testing.T, answers map[string]string) {
	t.Helper()
	prev := lookupIP
	lookupIP = func(ctx context.Context, network, host string) ([]net.IP, error) {
		if a, ok := answers[host]; ok {
			return []net.IP{net.ParseIP(a)}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	t.Cleanup(func() { lookupIP = prev })
}

func TestBUG3358_ResolvedReservedAddressRefusedAtDial(t *testing.T) {
	stubLookup(t, map[string]string{"bench.test": "198.18.0.1"})
	_, err := NewFetcher().Fetch(context.Background(), "http://bench.test/")
	if err == nil || !strings.Contains(err.Error(), "private/reserved IP 198.18.0.1") {
		t.Fatalf("Fetch of a host resolving to 198.18.0.1: got %v, want the dial-time refusal", err)
	}
}

// redirectFirstHop answers the first request with a redirect and hands every
// other request to the real safe transport, whose dialer applies the policy.
type redirectFirstHop struct {
	from, to string
	next     http.RoundTripper
}

func (r *redirectFirstHop) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == r.from {
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{r.to}},
			Body:       http.NoBody,
			Request:    req,
		}, nil
	}
	return r.next.RoundTrip(req)
}

func TestBUG3358_RedirectToReservedAddressRefused(t *testing.T) {
	stubLookup(t, map[string]string{"bench.test": "198.18.0.1"})
	for _, target := range []string{"http://198.18.0.1/", "http://bench.test/"} {
		f := NewFetcher()
		f.Transport = &redirectFirstHop{from: "start.test", to: target, next: newSafeTransport(false, DefaultTimeout)}
		_, err := f.Fetch(context.Background(), "http://start.test/")
		// The refusal itself, not any error naming the address (a connection
		// failure would name it too): the redirect check's, or the dialer's.
		if err == nil || !(strings.Contains(err.Error(), "redirect to") && strings.Contains(err.Error(), "private or reserved IP 198.18.0.1")) &&
			!strings.Contains(err.Error(), "private/reserved IP 198.18.0.1") {
			t.Fatalf("redirect to %s: got %v, want the policy refusal for 198.18.0.1", target, err)
		}
	}
}
