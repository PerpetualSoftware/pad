package webhooks

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func stubWebhookLookup(t *testing.T, fn func(host string) ([]net.IP, error)) {
	t.Helper()
	was := lookupIP
	lookupIP = func(_ context.Context, _ string, host string) ([]net.IP, error) { return fn(host) }
	t.Cleanup(func() { lookupIP = was })
}

var errDNSTimeout = &net.DNSError{Err: "i/o timeout", Name: "hooks.example.test", IsTimeout: true}

// BUG-3661: a resolver failure at delivery time ended the endpoint's part in
// the event as `permanent`, so DNS recovering never brought a redelivery. It
// is transient now; a policy refusal stays permanent.
func TestBUG3661_ResolverFailureIsTransient(t *testing.T) {
	d := NewDispatcher(newMockStore(nil))
	d.SkipSSRF = false
	d.client.Transport = &stubTransport{respond: func(req *http.Request) *http.Response {
		t.Errorf("nothing should be sent, got %s %s", req.Method, req.URL)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Request: req}
	}}
	hook := models.Webhook{ID: "h", WorkspaceID: "ws", URL: "http://hooks.example.test/x"}

	stubWebhookLookup(t, func(string) ([]net.IP, error) { return nil, errDNSTimeout })
	if got := d.attemptDeliver(hook, []byte("{}")); got != deliveryTransient {
		t.Errorf("a DNS timeout at delivery: result %v, want transient", got)
	}
	stubWebhookLookup(t, func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.0.0.1")}, nil })
	if got := d.attemptDeliver(hook, []byte("{}")); got != deliveryPermanent {
		t.Errorf("control: a hostname resolving to a private address: result %v, want permanent", got)
	}
}

// The same at a redirect hop: a resolver failure there is not a blocked
// redirect.
func TestBUG3661_ResolverFailureAtARedirectIsTransient(t *testing.T) {
	d := NewDispatcher(newMockStore(nil))
	d.SkipSSRF = false
	d.client.Transport = &stubTransport{respond: func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Header:     http.Header{"Location": []string{"http://moved.example.test/x"}},
			Body:       io.NopCloser(strings.NewReader("")), Request: req,
		}
	}}
	stubWebhookLookup(t, func(string) ([]net.IP, error) { return nil, errDNSTimeout })
	hook := models.Webhook{ID: "h", WorkspaceID: "ws", URL: "http://93.184.216.34/hook"}
	if got := d.attemptDeliver(hook, []byte("{}")); got != deliveryTransient {
		t.Errorf("a DNS timeout at a redirect: result %v, want transient", got)
	}

	stubWebhookLookup(t, func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("169.254.169.254")}, nil })
	if got := d.attemptDeliver(hook, []byte("{}")); got != deliveryPermanent {
		t.Errorf("control: a redirect to the metadata address: result %v, want permanent", got)
	}
}

// BUG-3662: a 301/302/303 after the POST became a bodyless GET carrying the
// signature, and a 2xx to it was recorded as delivered. Such a redirect is
// refused; a 307/308 carries the POST, its body and its signature.
func TestBUG3662_RedirectMustKeepMethodAndBody(t *testing.T) {
	body := []byte(`{"event":"item.created"}`)
	for _, tc := range []struct {
		status int
		want   deliveryResult
	}{
		{http.StatusMovedPermanently, deliveryPermanent},
		{http.StatusFound, deliveryPermanent},
		{http.StatusSeeOther, deliveryPermanent},
		{http.StatusTemporaryRedirect, deliverySuccess},
		{http.StatusPermanentRedirect, deliverySuccess},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			d := NewDispatcher(newMockStore(nil))
			d.SkipSSRF = false
			var mu sync.Mutex
			type seen struct {
				method, body, sig string
			}
			var reqs []seen
			d.client.Transport = &stubTransport{respond: func(req *http.Request) *http.Response {
				var b []byte
				if req.Body != nil {
					b, _ = io.ReadAll(req.Body)
				}
				mu.Lock()
				reqs = append(reqs, seen{req.Method, string(b), req.Header.Get("X-Pad-Signature")})
				n := len(reqs)
				mu.Unlock()
				if n == 1 {
					return &http.Response{
						StatusCode: tc.status,
						Header:     http.Header{"Location": []string{"http://93.184.216.34/hook/"}},
						Body:       io.NopCloser(strings.NewReader("")), Request: req,
					}
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Request: req}
			}}
			hook := models.Webhook{ID: "h", WorkspaceID: "ws", URL: "http://93.184.216.34/hook", Secret: "s3cret"}
			if got := d.attemptDeliver(hook, body); got != tc.want {
				t.Fatalf("result %v, want %v (requests: %+v)", got, tc.want, reqs)
			}
			if tc.want == deliverySuccess {
				if len(reqs) != 2 || reqs[1].method != http.MethodPost || reqs[1].body != string(body) || reqs[1].sig == "" || reqs[1].sig != reqs[0].sig {
					t.Errorf("the followed request did not carry the POST, its body and its signature: %+v", reqs)
				}
			} else if len(reqs) != 1 {
				t.Errorf("the method-changing redirect was followed: %+v", reqs)
			}
		})
	}
}

// The error a refused method-changing hop produces is the rejected-redirect
// error, which is what makes the attempt permanent.
func TestBUG3662_CheckRedirectNamesTheRefusal(t *testing.T) {
	d := NewDispatcher(newMockStore(nil))
	orig, _ := http.NewRequest(http.MethodPost, "http://93.184.216.34/hook", strings.NewReader("{}"))
	next, _ := http.NewRequest(http.MethodGet, "http://93.184.216.34/hook/", nil)
	if err := d.checkRedirect(next, []*http.Request{orig}); !errors.Is(err, errRedirectRejected) {
		t.Errorf("checkRedirect = %v, want errRedirectRejected", err)
	}
}
