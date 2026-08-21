package webhooks

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// mockStore implements WebhookStore for testing.
type mockStore struct {
	mu       sync.Mutex
	hooks    []models.Webhook
	failures map[string]bool // id -> last failed state
	updated  chan string     // signals when UpdateWebhookFailure is called
	listErr  error           // when set, ListWebhooks fails
}

func newMockStore(hooks []models.Webhook) *mockStore {
	return &mockStore{
		hooks:   hooks,
		updated: make(chan string, 10),
	}
}

func (m *mockStore) ListWebhooks(workspaceID string) ([]models.Webhook, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	var result []models.Webhook
	for _, h := range m.hooks {
		if h.WorkspaceID == workspaceID {
			result = append(result, h)
		}
	}
	return result, nil
}

func (m *mockStore) UpdateWebhookFailure(id string, failed bool) error {
	m.mu.Lock()
	if m.failures == nil {
		m.failures = make(map[string]bool)
	}
	m.failures[id] = failed
	m.mu.Unlock()
	m.updated <- id
	return nil
}

func (m *mockStore) waitForUpdate() {
	<-m.updated
}

func TestDispatcher_Dispatch(t *testing.T) {
	var received WebhookPayload
	var receivedSig string
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		receivedSig = r.Header.Get("X-Pad-Signature")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	store := newMockStore([]models.Webhook{
		{
			ID:          "hook-1",
			WorkspaceID: "ws-1",
			URL:         ts.URL,
			Secret:      "test-secret",
			Events:      `["item.created", "item.updated"]`,
			Active:      true,
		},
	})

	d := NewDispatcher(store)
	d.SkipSSRF = true
	d.Dispatch("ws-1", "item.created", map[string]string{"title": "Test Item"})
	store.waitForUpdate()

	mu.Lock()
	defer mu.Unlock()
	if received.Event != "item.created" {
		t.Errorf("expected event 'item.created', got %q", received.Event)
	}
	if received.Workspace != "ws-1" {
		t.Errorf("expected workspace 'ws-1', got %q", received.Workspace)
	}
	if receivedSig == "" {
		t.Error("expected X-Pad-Signature header to be set")
	}
	store.mu.Lock()
	if store.failures["hook-1"] != false {
		t.Errorf("expected success (failed=false), got failed=true")
	}
	store.mu.Unlock()
}

func TestDispatcher_EventFiltering(t *testing.T) {
	callCount := 0
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	store := newMockStore([]models.Webhook{
		{
			ID:          "hook-1",
			WorkspaceID: "ws-1",
			URL:         ts.URL,
			Events:      `["item.created"]`,
			Active:      true,
		},
	})

	d := NewDispatcher(store)
	d.SkipSSRF = true

	// This event should NOT match — no goroutine launched, no store update
	d.Dispatch("ws-1", "item.deleted", map[string]string{"title": "Test"})

	// This event SHOULD match
	d.Dispatch("ws-1", "item.created", map[string]string{"title": "Test"})
	store.waitForUpdate()

	mu.Lock()
	defer mu.Unlock()
	if callCount != 1 {
		t.Errorf("expected 1 delivery, got %d", callCount)
	}
}

func TestDispatcher_WildcardEvent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	store := newMockStore([]models.Webhook{
		{
			ID:          "hook-1",
			WorkspaceID: "ws-1",
			URL:         ts.URL,
			Events:      `["*"]`,
			Active:      true,
		},
	})

	d := NewDispatcher(store)
	d.SkipSSRF = true
	d.Dispatch("ws-1", "item.deleted", map[string]string{"title": "Test"})
	store.waitForUpdate()

	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.failures["hook-1"]; !ok {
		t.Error("expected UpdateWebhookFailure to be called")
	}
	if store.failures["hook-1"] {
		t.Error("expected success, got failure")
	}
}

func TestDispatcher_InactiveWebhookSkipped(t *testing.T) {
	callCount := 0

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	store := newMockStore([]models.Webhook{
		{
			ID:          "hook-1",
			WorkspaceID: "ws-1",
			URL:         ts.URL,
			Events:      `["*"]`,
			Active:      false, // inactive
		},
	})

	d := NewDispatcher(store)
	d.SkipSSRF = true
	d.Dispatch("ws-1", "item.created", map[string]string{"title": "Test"})

	// Since the hook is inactive, no goroutine is launched
	if callCount != 0 {
		t.Errorf("expected 0 deliveries for inactive webhook, got %d", callCount)
	}
}

func TestDispatcher_FailureOnNon2xx(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	store := newMockStore([]models.Webhook{
		{
			ID:          "hook-1",
			WorkspaceID: "ws-1",
			URL:         ts.URL,
			Events:      `["*"]`,
			Active:      true,
		},
	})

	d := NewDispatcher(store)
	d.SkipSSRF = true
	d.Dispatch("ws-1", "item.created", map[string]string{"title": "Test"})
	store.waitForUpdate()

	store.mu.Lock()
	defer store.mu.Unlock()
	if !store.failures["hook-1"] {
		t.Error("expected failure to be recorded for non-2xx response")
	}
}

// stubTransport lets a test drive the delivery client's redirect handling
// without any real network I/O.
type stubTransport struct {
	respond func(*http.Request) *http.Response
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return s.respond(req), nil
}

// TestScreenDialAddr is the dial-time guard that closes the DNS-rebind
// TOCTOU: it validates the resolved ip:port the socket is about to connect
// to, regardless of what the parse-time URL check saw.
func TestScreenDialAddr(t *testing.T) {
	tests := []struct {
		name    string
		address string
		wantErr bool
	}{
		{"public", "93.184.216.34:443", false},
		{"public dns", "8.8.8.8:80", false},
		{"loopback", "127.0.0.1:80", true},
		{"rfc1918", "10.1.2.3:8080", true},
		{"cloud metadata", "169.254.169.254:80", true},
		{"cgnat", "100.64.0.1:443", true},
		{"benchmarking", "198.18.0.1:80", true},
		{"reserved class E", "240.0.0.1:80", true},
		{"broadcast", "255.255.255.255:80", true},
		{"not an ip", "example.com:80", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := screenDialAddr(tt.address)
			if (err != nil) != tt.wantErr {
				t.Errorf("screenDialAddr(%q) error = %v, wantErr = %v", tt.address, err, tt.wantErr)
			}
		})
	}
}

// TestDispatcher_RedirectToInternalBlocked proves a 302 from an allowed
// public endpoint cannot bounce a delivery to an internal IP: the initial
// URL is a public literal (passes parse-time validation), but the stubbed
// transport returns a redirect to the cloud-metadata IP, which CheckRedirect
// must reject so d.client.Do fails and the delivery is recorded as failed.
func TestDispatcher_RedirectToInternalBlocked(t *testing.T) {
	store := newMockStore(nil)
	d := NewDispatcher(store)
	d.SkipSSRF = false
	d.client.Transport = &stubTransport{
		respond: func(req *http.Request) *http.Response {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"http://169.254.169.254/latest/meta-data/"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}
		},
	}

	hook := models.Webhook{ID: "hook-1", WorkspaceID: "ws-1", URL: "http://93.184.216.34/hook"}
	d.deliver(hook, []byte("{}"))
	store.waitForUpdate()

	store.mu.Lock()
	defer store.mu.Unlock()
	if !store.failures["hook-1"] {
		t.Error("expected redirect-to-internal delivery to be recorded as failed")
	}
}

// TestDispatcher_CheckRedirectCap stops runaway redirect chains.
func TestDispatcher_CheckRedirectCap(t *testing.T) {
	d := NewDispatcher(newMockStore(nil))
	req, _ := http.NewRequest(http.MethodGet, "http://93.184.216.34/", nil)
	via := make([]*http.Request, maxWebhookRedirects)
	if err := d.checkRedirect(req, via); err == nil {
		t.Errorf("checkRedirect with %d prior hops: expected error, got nil", maxWebhookRedirects)
	}
}

func TestMatchesEvent(t *testing.T) {
	tests := []struct {
		eventsJSON string
		event      string
		want       bool
	}{
		{`["*"]`, "item.created", true},
		{`["*"]`, "comment.created", true},
		{`["item.created"]`, "item.created", true},
		{`["item.created"]`, "item.deleted", false},
		{`["item.created", "item.updated"]`, "item.updated", true},
		{`["item.created", "item.updated"]`, "item.deleted", false},
		{`invalid`, "item.created", false},
		{`[]`, "item.created", false},
	}

	for _, tt := range tests {
		got := matchesEvent(tt.eventsJSON, tt.event)
		if got != tt.want {
			t.Errorf("matchesEvent(%q, %q) = %v, want %v", tt.eventsJSON, tt.event, got, tt.want)
		}
	}
}

// TestDispatcher_UsesInjectedSpawn verifies deliveries run on the injected
// spawn func (the server wires Server.goAsync here so Stop() can drain them).
func TestDispatcher_UsesInjectedSpawn(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	store := newMockStore([]models.Webhook{
		{ID: "hook-1", WorkspaceID: "ws-1", URL: ts.URL, Events: `["*"]`, Active: true},
	})

	d := NewDispatcher(store)
	d.SkipSSRF = true

	var spawnCount int32
	// Synchronous spawn: run inline so the test is deterministic without
	// channels, while still proving the injected spawner is used.
	d.SetSpawn(func(fn func()) {
		atomic.AddInt32(&spawnCount, 1)
		fn()
	})

	d.Dispatch("ws-1", "item.created", map[string]string{"title": "Test"})

	if got := atomic.LoadInt32(&spawnCount); got != 1 {
		t.Errorf("expected delivery to run on injected spawn once, got %d calls", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failures["hook-1"] {
		t.Error("expected success recorded, got failure")
	}
}

// TestDispatcher_RetriesTransientFailure asserts a 5xx (transient) response
// is retried up to maxDeliveryAttempts and the final failure is recorded.
func TestDispatcher_RetriesTransientFailure(t *testing.T) {
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	store := newMockStore([]models.Webhook{
		{ID: "hook-1", WorkspaceID: "ws-1", URL: ts.URL, Events: `["*"]`, Active: true},
	})

	d := NewDispatcher(store)
	d.SkipSSRF = true
	d.retryBackoff = 0 // no sleeping in tests
	d.SetSpawn(func(fn func()) { fn() })

	d.Dispatch("ws-1", "item.created", map[string]string{"title": "Test"})

	if got := atomic.LoadInt32(&attempts); got != maxDeliveryAttempts {
		t.Errorf("expected %d attempts for transient 5xx, got %d", maxDeliveryAttempts, got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if !store.failures["hook-1"] {
		t.Error("expected failure recorded after exhausting retries")
	}
}

// TestDispatcher_DoesNotRetryPermanentFailure asserts a 4xx (permanent)
// response is attempted exactly once — no retries — and recorded as a failure.
func TestDispatcher_DoesNotRetryPermanentFailure(t *testing.T) {
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()

	store := newMockStore([]models.Webhook{
		{ID: "hook-1", WorkspaceID: "ws-1", URL: ts.URL, Events: `["*"]`, Active: true},
	})

	d := NewDispatcher(store)
	d.SkipSSRF = true
	d.retryBackoff = 0
	d.SetSpawn(func(fn func()) { fn() })

	d.Dispatch("ws-1", "item.created", map[string]string{"title": "Test"})

	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected exactly 1 attempt for permanent 4xx, got %d", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if !store.failures["hook-1"] {
		t.Error("expected failure recorded for permanent 4xx")
	}
}

// TestDispatcher_RetriesThenSucceeds asserts a transient failure that later
// recovers is recorded as success (failed=false).
func TestDispatcher_RetriesThenSucceeds(t *testing.T) {
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable) // transient
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	store := newMockStore([]models.Webhook{
		{ID: "hook-1", WorkspaceID: "ws-1", URL: ts.URL, Events: `["*"]`, Active: true},
	})

	d := NewDispatcher(store)
	d.SkipSSRF = true
	d.retryBackoff = 0
	d.SetSpawn(func(fn func()) { fn() })

	d.Dispatch("ws-1", "item.created", map[string]string{"title": "Test"})

	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("expected 2 attempts (fail then succeed), got %d", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failures["hook-1"] {
		t.Error("expected success recorded after retry recovered")
	}
}

// errRoundTripper returns a fixed error from every RoundTrip and counts calls,
// letting a test drive attemptDeliver's error classification deterministically.
type errRoundTripper struct {
	err   error
	calls int32
}

func (e *errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	atomic.AddInt32(&e.calls, 1)
	return nil, e.err
}

// TestDispatcher_RedirectBlockIsPermanent asserts a redirect rejected by the
// SSRF guard (surfaced via CheckRedirect through client.Do) is classified as
// permanent and attempted exactly once — no retries against an internal target.
func TestDispatcher_RedirectBlockIsPermanent(t *testing.T) {
	store := newMockStore([]models.Webhook{
		{ID: "hook-1", WorkspaceID: "ws-1", URL: "https://example.com/hook", Events: `["*"]`, Active: true},
	})

	d := NewDispatcher(store)
	d.SkipSSRF = true // skip the pre-flight URL check so we exercise the client.Do path
	d.retryBackoff = 0
	d.SetSpawn(func(fn func()) { fn() })

	// Mimic what http.Client.Do surfaces when CheckRedirect blocks a hop:
	// the checkRedirect error wrapped so errors.Is reaches errRedirectRejected.
	rt := &errRoundTripper{err: fmt.Errorf("%w: redirect to 169.254.169.254 blocked", errRedirectRejected)}
	d.client.Transport = rt

	d.Dispatch("ws-1", "item.created", map[string]string{"title": "Test"})

	if got := atomic.LoadInt32(&rt.calls); got != 1 {
		t.Errorf("expected exactly 1 attempt for blocked redirect (permanent), got %d", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if !store.failures["hook-1"] {
		t.Error("expected failure recorded for blocked redirect")
	}
}

// Tests for DeliverEvent — the synchronous delivery seam the outbox drain acks
// on (TASK-2714 requirements 1 and 2).

func deliverTestHook(url, events string) *mockStore {
	return newMockStore([]models.Webhook{{
		ID:          "hook-1",
		WorkspaceID: "ws-1",
		URL:         url,
		Events:      events,
		Active:      true,
	}})
}

// TestDeliverEvent_EnvelopeCarriesIDAndEventTime pins requirement 1 and the
// envelope's two easy-to-get-wrong fields.
//
// The dedupe key is the point: SPEC-3 tells consumers to dedupe on the event
// id, and before the drain nothing put one on the wire. The timestamp is the
// subtle one — it must be the event's OWN occurred_at, because SPEC-3 pins
// time-relative binding predicates to it, so stamping dispatch time would make
// every consumer's notion of when a mutation happened depend on how backed up
// the queue was.
func TestDeliverEvent_EnvelopeCarriesIDAndEventTime(t *testing.T) {
	bodies := make(chan []byte, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	d := NewDispatcher(deliverTestHook(ts.URL, `["*"]`))
	d.SkipSSRF = true

	occurred := "2026-01-02T03:04:05Z"
	out, err := d.DeliverEvent(Delivery{
		WorkspaceID: "ws-1",
		EventID:     "outbox-row-42",
		Event:       "item.created",
		OccurredAt:  occurred,
		Payload:     json.RawMessage(`{"id":"item-9","title":"Embedded verbatim"}`),
	})
	if err != nil {
		t.Fatalf("DeliverEvent: %v", err)
	}
	if out.Matched != 1 || out.Succeeded != 1 {
		t.Fatalf("outcome = %+v, want one matched and one succeeded", out)
	}

	// The body must already be here: DeliverEvent returning before its HTTP
	// request completed is the async shape the drain cannot ack on.
	var raw []byte
	select {
	case raw = <-bodies:
	default:
		t.Fatal("DeliverEvent returned before the endpoint was called — delivery is not synchronous")
	}

	var got WebhookPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode delivered body: %v", err)
	}
	if got.ID != "outbox-row-42" {
		t.Errorf("envelope id = %q, want the outbox row id — consumers dedupe on it", got.ID)
	}
	if got.Event != "item.created" {
		t.Errorf("envelope event = %q, want the canonical name", got.Event)
	}
	if got.Timestamp != occurred {
		t.Errorf("envelope timestamp = %q, want the event's occurred_at %q (not dispatch time)", got.Timestamp, occurred)
	}

	// The payload must be embedded as JSON, not base64'd (what []byte would
	// do) and not double-encoded into a string.
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope data: %v", err)
	}
	if envelope.Data["title"] != "Embedded verbatim" {
		t.Errorf("data = %v, want the outbox payload embedded verbatim", envelope.Data)
	}
}

// TestDeliverEvent_OutcomeDrivesTheAckDecision is the requirement-2 test: the
// drain must be able to tell "rejected us" from "did not answer", because they
// are opposite retry decisions.
//
// Each leg asserts Retryable() rather than just the counts, since that is the
// value the drain actually branches on.
func TestDeliverEvent_OutcomeDrivesTheAckDecision(t *testing.T) {
	t.Run("permanent rejection does not hold the event pending", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer ts.Close()

		d := NewDispatcher(deliverTestHook(ts.URL, `["*"]`))
		d.SkipSSRF = true
		d.retryBackoff = 0

		out, err := d.DeliverEvent(Delivery{WorkspaceID: "ws-1", Event: "item.created", Payload: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatalf("DeliverEvent: %v", err)
		}
		if out.Permanent != 1 || out.Transient != 0 {
			t.Fatalf("outcome = %+v, want one permanent failure and no transient", out)
		}
		if out.Retryable() {
			t.Error("Retryable() = true for a 404 — re-delivering to an endpoint that will reject it again holds up the whole queue")
		}
	})

	t.Run("transient failure holds the event pending", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer ts.Close()

		d := NewDispatcher(deliverTestHook(ts.URL, `["*"]`))
		d.SkipSSRF = true
		d.retryBackoff = 0

		out, err := d.DeliverEvent(Delivery{WorkspaceID: "ws-1", Event: "item.created", Payload: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatalf("DeliverEvent: %v", err)
		}
		if out.Transient != 1 || out.Permanent != 0 {
			t.Fatalf("outcome = %+v, want one transient failure and no permanent", out)
		}
		if !out.Retryable() {
			t.Error("Retryable() = false for a 5xx — the durable retry is the drain's, and it is why the outbox exists")
		}
	})
}

// TestDeliverEvent_NoMatchingHooksIsSuccess pins the case that would otherwise
// wedge every webhook-less workspace.
//
// Zero matched endpoints means nothing is owed, so the event is done. A drain
// that read "nothing succeeded" as "not delivered" would leave every event in
// every workspace without webhooks pending until the retention bound deleted
// it — turning the common case into a permanent backlog.
func TestDeliverEvent_NoMatchingHooksIsSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("endpoint was called for an event it does not subscribe to")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	for _, tc := range []struct {
		name  string
		store *mockStore
	}{
		{"workspace has no webhooks at all", newMockStore(nil)},
		{"webhook subscribes to other events", deliverTestHook(ts.URL, `["comment.created"]`)},
		{"webhook is inactive", newMockStore([]models.Webhook{{
			ID: "hook-1", WorkspaceID: "ws-1", URL: ts.URL, Events: `["*"]`, Active: false,
		}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDispatcher(tc.store)
			d.SkipSSRF = true

			out, err := d.DeliverEvent(Delivery{WorkspaceID: "ws-1", Event: "item.created", Payload: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatalf("DeliverEvent: %v", err)
			}
			if out.Matched != 0 {
				t.Fatalf("Matched = %d, want 0", out.Matched)
			}
			if out.Retryable() {
				t.Error("Retryable() = true with nothing to deliver to — every event in this workspace would back up forever")
			}
		})
	}
}

// TestDeliverEvent_ServerSideFailureIsAnError separates the failure the drain
// must NOT ack from the ones it may.
//
// Listing webhooks failing means nothing was attempted, so the event is still
// owed in full. Returning it in the outcome instead of as an error would let a
// database blip look like "delivered to zero endpoints", which acks.
func TestDeliverEvent_ServerSideFailureIsAnError(t *testing.T) {
	store := newMockStore(nil)
	store.listErr = fmt.Errorf("database unavailable")

	d := NewDispatcher(store)
	d.SkipSSRF = true

	out, err := d.DeliverEvent(Delivery{WorkspaceID: "ws-1", Event: "item.created", Payload: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatalf("DeliverEvent returned nil error on a store failure; outcome %+v would ack an undelivered event", out)
	}
	if !strings.Contains(err.Error(), "database unavailable") {
		t.Errorf("error = %v, want it to carry the store's cause", err)
	}
}
