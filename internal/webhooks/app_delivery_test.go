package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/appfetch"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3408 (U10b): the app delivery path and the shared backoff.

type fakeAdmitter struct {
	mu       sync.Mutex
	refuse   bool
	fail     error
	admitted []string
	ended    []string
	// onAdmit runs inside AdmitAppDelivery (to flip state between attempts).
	onAdmit func(n int)
}

func (a *fakeAdmitter) AdmitAppDelivery(webhookID, event, collectionID, occurredAt, deliveryID string) (*AppAdmission, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.onAdmit != nil {
		a.onAdmit(len(a.admitted))
	}
	if a.fail != nil {
		return nil, a.fail
	}
	if a.refuse {
		return nil, fmt.Errorf("%w: install_disabling", ErrAppDeliveryRefused)
	}
	a.admitted = append(a.admitted, deliveryID)
	return &AppAdmission{URL: "https://portal.example/hooks", Secret: "padwh_test", InstallID: "inst-1"}, nil
}

func (a *fakeAdmitter) EndAppDelivery(deliveryID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ended = append(a.ended, deliveryID)
	return nil
}

type fakePoster struct {
	mu       sync.Mutex
	statuses []int
	err      error
	calls    []http.Header
	bodies   [][]byte
	// deadline is the remaining time on the request context at each call.
	deadlines []time.Duration
}

func (p *fakePoster) Post(ctx context.Context, rawURL string, body []byte, header http.Header) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, header)
	p.bodies = append(p.bodies, body)
	if dl, ok := ctx.Deadline(); ok {
		p.deadlines = append(p.deadlines, time.Until(dl))
	}
	if p.err != nil {
		return 0, p.err
	}
	st := http.StatusOK
	if len(p.statuses) > 0 {
		st = p.statuses[0]
		if len(p.statuses) > 1 {
			p.statuses = p.statuses[1:]
		}
	}
	return st, nil
}

func newAppTestDispatcher() (*Dispatcher, *[]time.Duration) {
	d := NewDispatcher(newMockStore(nil))
	var waits []time.Duration
	d.wait = func(ctx context.Context, dur time.Duration) bool {
		waits = append(waits, dur)
		return ctx.Err() == nil
	}
	d.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	return d, &waits
}

var appDV = AppDelivery{WebhookID: "wh-1", Event: "item.created", CollectionID: "coll-1", Body: []byte(`{"event":"item.created"}`)}

func TestAppDelivery_SignedAndAdmittedPerAttempt(t *testing.T) {
	d, waits := newAppTestDispatcher()
	adm := &fakeAdmitter{}
	p := &fakePoster{statuses: []int{503, 502, 204}}
	if got, _ := d.DeliverAppEvent(adm, p, appDV); got != AppDelivered {
		t.Fatalf("result %s, want delivered", got)
	}
	if len(adm.admitted) != 3 || len(adm.ended) != 3 {
		t.Fatalf("admitted %d, ended %d; want every attempt admitted and ended", len(adm.admitted), len(adm.ended))
	}
	if adm.admitted[0] == adm.admitted[1] {
		t.Fatal("two attempts shared an in-flight id")
	}
	if fmt.Sprint(*waits) != fmt.Sprint([]time.Duration{500 * time.Millisecond, time.Second}) {
		t.Fatalf("backoff schedule %v", *waits)
	}
	h := p.calls[0]
	if h.Get(AppInstallHeader) != "inst-1" || h.Get("X-Pad-Signature") != "" {
		t.Fatalf("headers %v", h)
	}
	mac := hmac.New(sha256.New, []byte("padwh_test"))
	mac.Write([]byte("1800000000." + string(appDV.Body)))
	if want := "t=1800000000,v1=" + hex.EncodeToString(mac.Sum(nil)); h.Get(AppSignatureHeader) != want {
		t.Fatalf("signature %q, want %q", h.Get(AppSignatureHeader), want)
	}
	for _, dl := range p.deadlines {
		if dl <= 0 || dl > deliveryTimeout {
			t.Fatalf("attempt deadline %v, want within the %v delivery timeout", dl, deliveryTimeout)
		}
	}
	if len(p.deadlines) != 3 {
		t.Fatal("an attempt had no deadline")
	}
}

func TestAppDelivery_Outcomes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		adm      *fakeAdmitter
		poster   *fakePoster
		want     AppResult
		attempts int
	}{
		{"refused sends nothing", &fakeAdmitter{refuse: true}, &fakePoster{}, AppRefused, 0},
		{"store error defers", &fakeAdmitter{fail: errors.New("db down")}, &fakePoster{}, AppDeferred, 0},
		{"3xx is permanent, never retried", &fakeAdmitter{}, &fakePoster{statuses: []int{302}}, AppPermanent, 1},
		{"4xx is permanent", &fakeAdmitter{}, &fakePoster{statuses: []int{410}}, AppPermanent, 1},
		{"policy refusal is permanent", &fakeAdmitter{}, &fakePoster{err: fmt.Errorf("%w: not https", appfetch.ErrRefused)}, AppPermanent, 1},
		{"network error is transient, retried", &fakeAdmitter{}, &fakePoster{err: errors.New("connection reset")}, AppTransient, 3},
		{"5xx exhausts", &fakeAdmitter{}, &fakePoster{statuses: []int{500}}, AppTransient, 3},
		// TASK-3409: the receiver asking us to come back is transient.
		{"408 is transient, retried", &fakeAdmitter{}, &fakePoster{statuses: []int{408}}, AppTransient, 3},
		{"425 is transient, retried", &fakeAdmitter{}, &fakePoster{statuses: []int{425}}, AppTransient, 3},
		{"429 is transient, retried", &fakeAdmitter{}, &fakePoster{statuses: []int{429}}, AppTransient, 3},
		{"400 stays permanent", &fakeAdmitter{}, &fakePoster{statuses: []int{400}}, AppPermanent, 1},
		{"404 stays permanent", &fakeAdmitter{}, &fakePoster{statuses: []int{404}}, AppPermanent, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newAppTestDispatcher()
			if got, _ := d.DeliverAppEvent(tc.adm, tc.poster, appDV); got != tc.want {
				t.Fatalf("result %s, want %s", got, tc.want)
			}
			if len(tc.poster.calls) != tc.attempts {
				t.Fatalf("%d requests, want %d", len(tc.poster.calls), tc.attempts)
			}
			if len(tc.adm.admitted) != len(tc.adm.ended) {
				t.Fatalf("admitted %d, ended %d", len(tc.adm.admitted), len(tc.adm.ended))
			}
		})
	}
}

// A disable between attempts: the next attempt is refused, not sent.
func TestAppDelivery_DisableBetweenAttemptsStopsTheRetry(t *testing.T) {
	d, _ := newAppTestDispatcher()
	adm := &fakeAdmitter{}
	adm.onAdmit = func(n int) { adm.refuse = n >= 1 }
	p := &fakePoster{statuses: []int{503}}
	if got, _ := d.DeliverAppEvent(adm, p, appDV); got != AppRefused {
		t.Fatalf("result %s, want refused", got)
	}
	if len(p.calls) != 1 {
		t.Fatalf("%d requests after the install was disabled, want 1", len(p.calls))
	}
}

// A stopped dispatcher ends a pending backoff without another attempt, on
// both paths.
func TestBackoff_StopEndsTheWait(t *testing.T) {
	d, _ := newAppTestDispatcher()
	ctx, cancel := context.WithCancel(context.Background())
	d.SetContext(ctx)
	adm := &fakeAdmitter{}
	adm.onAdmit = func(int) { cancel() }
	p := &fakePoster{statuses: []int{503}}
	d.wait = timerWait
	d.retryBackoff = time.Hour
	start := time.Now()
	if got, _ := d.DeliverAppEvent(adm, p, appDV); got != AppTransient {
		t.Fatalf("result %s, want transient (still owed)", got)
	}
	if len(p.calls) > 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("%d requests in %v: the stop did not end the wait", len(p.calls), time.Since(start))
	}

	// The owner path: same.
	octx, ocancel := context.WithCancel(context.Background())
	var hits int
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		ocancel()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	store := newMockStore([]models.Webhook{{ID: "o1", WorkspaceID: "ws", URL: ts.URL, Events: `["*"]`, Active: true}})
	od := NewDispatcher(store)
	od.SkipSSRF = true
	od.SetContext(octx)
	od.retryBackoff = time.Hour
	start = time.Now()
	out, err := od.DeliverEvent(Delivery{WorkspaceID: "ws", EventID: "e1", Event: "item.created", OccurredAt: "x", Payload: []byte(`{}`)})
	if err != nil || !out.Retryable() {
		t.Fatalf("owner outcome %+v (%v), want still owed", out, err)
	}
	if hits != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("owner path: %d requests in %v", hits, time.Since(start))
	}
}

// Parity: the owner path keeps its attempt count and its 500 ms x n schedule.
func TestBackoff_OwnerScheduleUnchanged(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	store := newMockStore([]models.Webhook{{ID: "o1", WorkspaceID: "ws", URL: ts.URL, Events: `["*"]`, Active: true}})
	d := NewDispatcher(store)
	d.SkipSSRF = true
	var waits []time.Duration
	d.wait = func(_ context.Context, dur time.Duration) bool { waits = append(waits, dur); return true }
	if _, err := d.DeliverEvent(Delivery{WorkspaceID: "ws", EventID: "e1", Event: "item.created", OccurredAt: "x", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(waits) != fmt.Sprint([]time.Duration{500 * time.Millisecond, time.Second}) {
		t.Fatalf("owner backoff schedule %v, want [500ms 1s] (3 attempts)", waits)
	}
}

func TestSignAppPayload_Format(t *testing.T) {
	got := SignAppPayload("k", time.Unix(42, 0), []byte("b"))
	if !strings.HasPrefix(got, "t=42,v1=") || len(got) != len("t=42,v1=")+64 {
		t.Fatalf("signature %q", got)
	}
}

// The attempt's deadline starts BEFORE admission (§5): time spent admitting
// comes out of the attempt, never out of the in-flight record's margin.
func TestAppDelivery_DeadlineStartsBeforeAdmission(t *testing.T) {
	d, _ := newAppTestDispatcher()
	adm := &fakeAdmitter{onAdmit: func(int) { time.Sleep(300 * time.Millisecond) }}
	p := &fakePoster{}
	if got, _ := d.DeliverAppEvent(adm, p, appDV); got != AppDelivered {
		t.Fatalf("result %s", got)
	}
	if len(p.deadlines) != 1 || p.deadlines[0] > deliveryTimeout-250*time.Millisecond {
		t.Fatalf("remaining %v after a 300ms admission, want at most %v", p.deadlines, deliveryTimeout-250*time.Millisecond)
	}
}

// A stopped dispatcher admits and sends nothing, even on the first attempt.
func TestAppDelivery_StoppedDispatcherSendsNothing(t *testing.T) {
	d, _ := newAppTestDispatcher()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.SetContext(ctx)
	adm := &fakeAdmitter{}
	p := &fakePoster{}
	if got, _ := d.DeliverAppEvent(adm, p, appDV); got != AppTransient {
		t.Fatalf("result %s, want transient (still owed)", got)
	}
	if len(adm.admitted) != 0 || len(p.calls) != 0 {
		t.Fatalf("a stopped dispatcher admitted %d and sent %d", len(adm.admitted), len(p.calls))
	}
}

// codex r3 on U10c: a request that failed before any byte went out (dial,
// handshake, policy) is not counted as sent.
func TestAppDelivery_UnsentFailuresAreNotCounted(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want AppResult
		sent int
	}{
		{"dial failure", fmt.Errorf("%w: connection refused", appfetch.ErrNotSent), AppTransient, 0},
		{"policy refusal", fmt.Errorf("%w: not https", appfetch.ErrRefused), AppPermanent, 0},
		{"failed mid-request", errors.New("connection reset by peer"), AppTransient, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newAppTestDispatcher()
			got, sent := d.DeliverAppEvent(&fakeAdmitter{}, &fakePoster{err: tc.err}, appDV)
			if got != tc.want || sent != tc.sent {
				t.Fatalf("got %s, %d sent; want %s, %d", got, sent, tc.want, tc.sent)
			}
		})
	}
}
