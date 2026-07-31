package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newCopyTestClient points a Client at ts with a HOME that has no saved
// credentials, so the test never picks up the developer's real token.
func newCopyTestClient(t *testing.T, ts *httptest.Server) *Client {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return NewClientFromURL(ts.URL)
}

// ── DR-13: the mutating copy is attempted EXACTLY ONCE ───────────────────
//
// These are the guard the task asks for: a test, not a comment. If someone
// later installs a retrying RoundTripper on the shared client, swaps
// copyHTTPClient() for c.httpClient, or "simplifies" postCopyJSON's opaque
// reader back to a *bytes.Reader, one of these fails.

func TestCopyItem_500CopyFailedIsAttemptedOnce(t *testing.T) {
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/copy") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"copy_failed","message":"the copy may or may not have committed"}}`))
	}))
	defer ts.Close()

	c := newCopyTestClient(t, ts)
	_, _, err := c.CopyItem("ws", "TASK-1", ItemCopyRequest{TargetWorkspace: "b", TargetCollection: "tasks"})
	if err == nil {
		t.Fatal("expected an error from a 500 copy_failed")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("the mutating copy must be attempted exactly once; server saw %d attempts", got)
	}
	if !CopyOutcomeUnknown(err) {
		t.Errorf("500 copy_failed must be reported as an unknown outcome; got %v", err)
	}
}

// TestCopyItem_MutatingRequestIsNotReplayable pins DR-13 mechanism 2
// directly: net/http replays a request whose connection died before any
// byte was written, and it can only do that when Request.GetBody is set.
//
// This is asserted on the constructed request rather than over a socket on
// purpose. The transport's nothing-written path needs the client to lose a
// race against an idle-connection close, so a network-level test would pass
// whether or not GetBody were nil — it would look like a guard and be none.
// The preflight case is included as the control: the difference between the
// two calls has to be deliberate and visible, not an accident.
func TestCopyItem_MutatingRequestIsNotReplayable(t *testing.T) {
	c := &Client{baseURL: "http://example.invalid/api/v1", httpClient: &http.Client{}}
	body := []byte(`{"target_workspace":"b","target_collection":"tasks","archive_source":false}`)

	mutating, err := c.newCopyRequest("/workspaces/a/items/TASK-1/copy", body, true)
	if err != nil {
		t.Fatalf("newCopyRequest(mutating): %v", err)
	}
	if mutating.GetBody != nil {
		t.Error("the mutating copy's request must not carry GetBody — that is what lets net/http replay it")
	}
	if mutating.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", mutating.ContentLength, len(body))
	}
	if mutating.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", mutating.Header.Get("Content-Type"))
	}

	preflight, err := c.newCopyRequest("/workspaces/a/items/TASK-1/copy/preflight", body, false)
	if err != nil {
		t.Fatalf("newCopyRequest(preflight): %v", err)
	}
	if preflight.GetBody == nil {
		t.Error("the read-only preflight should stay replayable; if it does not, the mutating assertion above proves nothing")
	}
}

// TestCopyItem_LostResponseIsAttemptedOnce covers the transport-failure
// path: the request reaches the server and the RESPONSE is lost, so the
// outcome is genuinely unknown. The CLI must send it once and must not
// claim to know what happened.
//
// Scope, stated so nobody mistakes this for more than it is (Codex round
// 3): this is NOT the net/http nothing-written replay case. That one needs
// the connection to die BEFORE the request bytes go out, on a pooled
// connection, which is a race no test can schedule — and the copy now runs
// on its own transport with its own empty pool, so it cannot arise here at
// all. The replay guarantee itself is pinned by
// TestCopyItem_MutatingRequestIsNotReplayable.
func TestCopyItem_LostResponseIsAttemptedOnce(t *testing.T) {
	var copyAttempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&copyAttempts, 1)
		// Take the connection and drop it without answering.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer ts.Close()

	c := newCopyTestClient(t, ts)
	_, _, err := c.CopyItem("ws", "TASK-1", ItemCopyRequest{TargetWorkspace: "b", TargetCollection: "tasks"})
	if err == nil {
		t.Fatal("expected a transport error when the connection dies")
	}
	if got := atomic.LoadInt32(&copyAttempts); got != 1 {
		t.Fatalf("the copy request must not be replayed; server saw %d attempts", got)
	}
	if !CopyOutcomeUnknown(err) {
		t.Errorf("a lost connection leaves the outcome unknown; got %v", err)
	}
	if !errors.Is(err, ErrCopyOutcomeUnknown) {
		t.Errorf("expected ErrCopyOutcomeUnknown in the chain; got %v", err)
	}
}

// TestCopyItem_RedirectIsNotFollowed — a 307/308 would re-send the POST
// body at a new URL. That is a retry wearing a hat.
func TestCopyItem_RedirectIsNotFollowed(t *testing.T) {
	var copyAttempts, elsewhereAttempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/elsewhere") {
			atomic.AddInt32(&elsewhereAttempts, 1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		atomic.AddInt32(&copyAttempts, 1)
		http.Redirect(w, r, "/api/v1/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer ts.Close()

	c := newCopyTestClient(t, ts)
	_, _, err := c.CopyItem("ws", "TASK-1", ItemCopyRequest{TargetWorkspace: "b", TargetCollection: "tasks"})
	if err == nil {
		t.Fatal("expected an error rather than a followed redirect")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("error should name the redirect; got %v", err)
	}
	if got := atomic.LoadInt32(&elsewhereAttempts); got != 0 {
		t.Fatalf("the POST body must not be re-sent at the redirect target; got %d requests", got)
	}
	if got := atomic.LoadInt32(&copyAttempts); got != 1 {
		t.Fatalf("expected exactly one copy attempt; got %d", got)
	}
	// A redirect tells us nothing about whether anything committed, but it
	// is not the ambiguous-outcome class either: the server answered.
	if CopyOutcomeUnknown(err) {
		t.Errorf("a redirect is a misconfiguration, not an unknown copy outcome")
	}
}

// TestCopyItem_MutatingClientIsNotTheSharedOne pins mechanism 1: even if
// the shared client is swapped for something with retry behaviour, the
// copy gets its own.
func TestCopyItem_MutatingClientIsNotTheSharedOne(t *testing.T) {
	c := &Client{httpClient: &http.Client{}}
	got := c.copyHTTPClient()
	if got == c.httpClient {
		t.Fatal("the mutating copy must not run on the shared http.Client")
	}
	if got.Transport == c.httpClient.Transport {
		t.Fatal("the mutating copy must not inherit the shared client's RoundTripper")
	}
	if got.CheckRedirect == nil {
		t.Fatal("the mutating copy's client must refuse redirects")
	}
	if err := got.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect must return ErrUseLastResponse; got %v", err)
	}
	if got.Timeout != copyRequestTimeout {
		t.Fatalf("copy timeout = %v, want %v", got.Timeout, copyRequestTimeout)
	}
}

// retryingTransport is the hazard DR-13 names, in the shape it actually
// takes in Go: a RoundTripper wrapper, not an http.Client setting. A
// dedicated *http.Client would inherit it through Transport.
type retryingTransport struct {
	base    http.RoundTripper
	tries   int
	roundTr int32
}

func (rt *retryingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	atomic.AddInt32(&rt.roundTr, 1)
	var resp *http.Response
	var err error
	for i := 0; i < rt.tries; i++ {
		if req.GetBody != nil && i > 0 {
			b, gerr := req.GetBody()
			if gerr != nil {
				return nil, gerr
			}
			req.Body = b
		}
		resp, err = rt.base.RoundTrip(req)
		if err == nil && resp.StatusCode < 500 {
			return resp, nil
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
	return resp, err
}

// TestCopyItem_RetryingSharedTransportIsNotInherited is Codex round 1's P1.
// A dedicated http.Client is not sufficient on its own: retry behaviour
// lives in the RoundTripper, and inheriting c.httpClient.Transport would
// have inherited the retry with it.
func TestCopyItem_RetryingSharedTransportIsNotInherited(t *testing.T) {
	var serverHits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&serverHits, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"copy_failed","message":"unknown outcome"}}`))
	}))
	defer ts.Close()

	c := newCopyTestClient(t, ts)
	rt := &retryingTransport{base: http.DefaultTransport, tries: 3}
	c.httpClient.Transport = rt

	_, _, err := c.CopyItem("ws", "TASK-1", ItemCopyRequest{TargetWorkspace: "b", TargetCollection: "tasks"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := atomic.LoadInt32(&serverHits); got != 1 {
		t.Fatalf("a retrying transport on the SHARED client must not reach the copy; server saw %d requests", got)
	}
	if got := atomic.LoadInt32(&rt.roundTr); got != 0 {
		t.Fatalf("the mutating copy must not run through the shared transport at all; it was entered %d times", got)
	}

	// Control: the read-only preflight DOES use the shared transport, so
	// the assertion above is about the copy's isolation and not about the
	// wrapper being inert.
	if _, _, err := c.CopyItemPreflight("ws", "TASK-1", ItemCopyRequest{TargetWorkspace: "b", TargetCollection: "tasks"}); err == nil {
		t.Fatal("expected the preflight to surface the 500 too")
	}
	if got := atomic.LoadInt32(&rt.roundTr); got == 0 {
		t.Fatal("the preflight should have gone through the shared transport; the control proves nothing otherwise")
	}
}

// copyTransport must preserve a plain *http.Transport's configuration —
// dropping proxy/TLS settings would turn DR-13 compliance into a
// connection bug.
func TestCopyTransport_ClonesAPlainTransportButNotAWrapper(t *testing.T) {
	shared := &http.Transport{MaxIdleConnsPerHost: 42, DisableCompression: true}
	c := &Client{httpClient: &http.Client{Transport: shared}}

	got, ok := c.copyTransport().(*http.Transport)
	if !ok {
		t.Fatalf("expected an *http.Transport; got %T", c.copyTransport())
	}
	if got == shared {
		t.Error("the copy must not share the connection pool it was cloned from")
	}
	if got.MaxIdleConnsPerHost != 42 || !got.DisableCompression {
		t.Errorf("clone lost configuration: %+v", got)
	}

	// A wrapper is discarded outright — see copyTransport's doc comment for
	// the trade-off this encodes.
	c.httpClient.Transport = &retryingTransport{base: shared, tries: 2}
	if _, ok := c.copyTransport().(*http.Transport); !ok {
		t.Error("a wrapping RoundTripper must be replaced by a plain transport")
	}

	// A nil Transport means net/http's default, and must still be cloned
	// rather than shared.
	c.httpClient.Transport = nil
	def, ok := c.copyTransport().(*http.Transport)
	if !ok {
		t.Fatal("nil Transport should yield a plain *http.Transport")
	}
	if def == http.DefaultTransport {
		t.Error("must not hand out DefaultTransport itself")
	}
}

// The three outcome classes are mutually exclusive, and each one drives a
// different thing the CLI is allowed to tell the user:
//
//	unknown   — may or may not have committed; check the destination
//	committed — definitely committed; do NOT re-run
//	neither   — a refusal made before any write; safe to fix and re-run
func TestCopyOutcomeClassification(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		wantUnknown   bool
		wantCommitted bool
	}{
		{"nil", nil, false, false},
		{"copy_failed", &APIError{Code: "copy_failed", Message: "x"}, true, false},
		{"validation_error", &APIError{Code: "validation_error", Message: "x"}, false, false},
		{"plan_limit_exceeded", &APIError{Code: "plan_limit_exceeded", Message: "x"}, false, false},
		{"conflict", &APIError{Code: "conflict", Message: "x"}, false, false},
		{"transport", fmt.Errorf("%w: request failed: EOF", ErrCopyOutcomeUnknown), true, false},
		{"undecodable 2xx", fmt.Errorf("%w: decoding the response: x", ErrCopyCommitted), false, true},
		{"unrelated", errors.New("boom"), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CopyOutcomeUnknown(tc.err); got != tc.wantUnknown {
				t.Errorf("CopyOutcomeUnknown(%v) = %v, want %v", tc.err, got, tc.wantUnknown)
			}
			if got := CopyCommitted(tc.err); got != tc.wantCommitted {
				t.Errorf("CopyCommitted(%v) = %v, want %v", tc.err, got, tc.wantCommitted)
			}
			if tc.wantUnknown && tc.wantCommitted {
				t.Fatal("fixture claims both classes; they are mutually exclusive")
			}
		})
	}
}

// A body that dies mid-read after a 2xx header is the same class as an
// undecodable one: the server already committed.
func TestCopyItem_TruncatedSuccessBodyIsCommittedNotUnknown(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "500")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"source":`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer ts.Close()

	c := newCopyTestClient(t, ts)
	_, _, err := c.CopyItem("ws", "TASK-1", ItemCopyRequest{TargetWorkspace: "b", TargetCollection: "tasks"})
	if err == nil {
		t.Fatal("expected an error from a truncated body")
	}
	if CopyOutcomeUnknown(err) {
		t.Errorf("a 2xx header already arrived; the copy committed. got %v", err)
	}
	if !CopyCommitted(err) {
		t.Errorf("expected the committed-but-unreported class; got %v", err)
	}
}

// A 4xx is a refusal made BEFORE anything was written, so it must be
// distinguishable from the ambiguous class — and it must not be retried
// either.
func TestCopyItem_4xxIsARefusalNotAnUnknownOutcome(t *testing.T) {
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"validation_error","message":"priority is required"}}`))
	}))
	defer ts.Close()

	c := newCopyTestClient(t, ts)
	_, _, err := c.CopyItem("ws", "TASK-1", ItemCopyRequest{TargetWorkspace: "b", TargetCollection: "tasks"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *APIError; got %v", err)
	}
	if apiErr.Code != "validation_error" || apiErr.Message != "priority is required" {
		t.Errorf("unexpected APIError %+v", apiErr)
	}
	if CopyOutcomeUnknown(err) {
		t.Error("a 400 is a refusal, not an unknown outcome")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("expected exactly one attempt; got %d", got)
	}
}

// ── request/response fidelity ────────────────────────────────────────────

// Codex round 6. A hostile ref must not be able to re-route the one call
// in this CLI that mutates two workspaces.
func TestCopyItemPath_EscapesEachSegmentOnce(t *testing.T) {
	// The ordinary case is byte-identical to bare concatenation — that is
	// what makes the escaping free.
	if got, want := itemCopyPath("pad-web", "TASK-5"), "/workspaces/pad-web/items/TASK-5/copy"; got != want {
		t.Errorf("itemCopyPath = %q, want %q", got, want)
	}
	for _, ref := range []string{"../../admin", "a/b", "a?x=1", "a#frag", "a%2Fb"} {
		got := itemCopyPath("ws", ref)
		if strings.Count(got, "/") != 5 {
			t.Errorf("itemCopyPath(%q) = %q — a hostile ref changed the path shape", ref, got)
		}
		for _, bad := range []string{"?", "#"} {
			if strings.Contains(got, bad) {
				t.Errorf("itemCopyPath(%q) = %q — %q survived unescaped", ref, got, bad)
			}
		}
	}
}

// The escaped path must survive to the wire, not just to the string.
func TestCopyItem_HostileRefDoesNotEscapeTheRoute(t *testing.T) {
	var seen string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.EscapedPath()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	c := newCopyTestClient(t, ts)
	_, _, err := c.CopyItem("ws", "../../admin", ItemCopyRequest{TargetWorkspace: "b", TargetCollection: "tasks"})
	if err != nil {
		t.Fatalf("CopyItem: %v", err)
	}
	if want := "/api/v1/workspaces/ws/items/..%2F..%2Fadmin/copy"; seen != want {
		t.Errorf("server saw %q, want %q", seen, want)
	}
}

func TestCopyItem_RequestShapeIsTheDocumentedOne(t *testing.T) {
	var body []byte
	var contentType string
	var contentLength int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		body = buf.Bytes()
		contentType = r.Header.Get("Content-Type")
		contentLength = r.ContentLength
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"source":{},"destination":{},"warnings":{"dropped_fields":[]}}`))
	}))
	defer ts.Close()

	c := newCopyTestClient(t, ts)
	_, _, err := c.CopyItem("ws", "TASK-1", ItemCopyRequest{
		TargetWorkspace:  "pad-web",
		TargetCollection: "tasks",
		FieldOverrides:   map[string]any{"priority": "high"},
		ArchiveSource:    true,
	})
	if err != nil {
		t.Fatalf("CopyItem: %v", err)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q", contentType)
	}
	// The opaque reader defeats net/http's length sniffing, so Content-Length
	// is restored by hand. A regression there would make the server read a
	// chunked body it does not expect.
	if contentLength != int64(len(body)) {
		t.Errorf("Content-Length = %d, body is %d bytes", contentLength, len(body))
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	want := map[string]any{
		"target_workspace":  "pad-web",
		"target_collection": "tasks",
		"field_overrides":   map[string]any{"priority": "high"},
		"archive_source":    true,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("request body =\n %v\nwant\n %v", got, want)
	}
}

// The preflight and the copy take a BYTE-IDENTICAL body. That is the
// server's stated contract and the reason a client can preview then commit
// without rebuilding the request.
func TestCopyPreflightAndCopySendIdenticalBodies(t *testing.T) {
	bodies := map[string][]byte{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		if strings.HasSuffix(r.URL.Path, "/copy/preflight") {
			bodies["preflight"] = buf.Bytes()
			_, _ = w.Write([]byte(`{"valid":true}`))
			return
		}
		bodies["copy"] = buf.Bytes()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	c := newCopyTestClient(t, ts)
	req := ItemCopyRequest{
		TargetWorkspace:  "pad-web",
		TargetCollection: "tasks",
		FieldOverrides:   map[string]any{"priority": "high", "points": 3.0},
		ArchiveSource:    true,
	}
	if _, _, err := c.CopyItemPreflight("ws", "TASK-1", req); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if _, _, err := c.CopyItem("ws", "TASK-1", req); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if !bytes.Equal(bodies["preflight"], bodies["copy"]) {
		t.Errorf("bodies differ:\npreflight: %s\ncopy:      %s", bodies["preflight"], bodies["copy"])
	}
}

// The raw bytes handed to --format json must be the SERVER's, unmodelled
// fields and full int64 precision included.
func TestCopyItemPreflight_ReturnsServerBytesVerbatim(t *testing.T) {
	const payload = `{"valid":true,"archive_source":false,` +
		`"fields":{"carried":[],"dropped":[],"needs_value":[]},` +
		`"warnings":{"attachment_bytes":9007199254740993,"outgoing_links":{},"incoming_links":{}},` +
		`"a_field_this_cli_does_not_model":"kept"}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer ts.Close()

	c := newCopyTestClient(t, ts)
	pre, raw, err := c.CopyItemPreflight("ws", "TASK-1", ItemCopyRequest{TargetWorkspace: "b", TargetCollection: "tasks"})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if string(raw) != payload {
		t.Errorf("raw bytes were altered.\n got: %s\nwant: %s", raw, payload)
	}
	if !pre.Valid {
		t.Error("decoded preflight lost valid=true")
	}
	if pre.Warnings.AttachmentBytes != 9007199254740993 {
		t.Errorf("int64 attachment_bytes decoded as %d", pre.Warnings.AttachmentBytes)
	}
}

func TestPrintRawJSON_PreservesShapeAndPrecision(t *testing.T) {
	const payload = `{"warnings":{"attachment_bytes":9007199254740993},"unmodelled":{"z":1,"a":2}}`
	var buf bytes.Buffer
	if err := PrintRawJSON(&buf, json.RawMessage(payload)); err != nil {
		t.Fatalf("PrintRawJSON: %v", err)
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Error("output should end with a newline")
	}
	// json.Indent is lexical: the big integer survives as a literal and the
	// unmodelled object keeps its original key order.
	if !strings.Contains(out, "9007199254740993") {
		t.Errorf("large integer lost precision:\n%s", out)
	}
	if strings.Index(out, `"z"`) > strings.Index(out, `"a"`) {
		t.Errorf("key order was not preserved:\n%s", out)
	}
	// Compacting the output must reproduce the input byte-for-byte.
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(out)); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if compact.String() != payload {
		t.Errorf("round trip changed the document.\n got: %s\nwant: %s", compact.String(), payload)
	}
}

func TestPrintRawJSON_NonJSONIsEmittedVerbatim(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintRawJSON(&buf, json.RawMessage("not json")); err != nil {
		t.Fatalf("PrintRawJSON: %v", err)
	}
	if buf.String() != "not json\n" {
		t.Errorf("got %q", buf.String())
	}
}

// A 2xx whose body will not decode means the copy COMMITTED — that is a
// known outcome, and must not be reported as ambiguous or the user will be
// told to go hunting for something that is definitely there.
func TestCopyItem_UndecodableSuccessIsNotAmbiguous(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"item": "not-an-object"}`))
	}))
	defer ts.Close()

	c := newCopyTestClient(t, ts)
	_, raw, err := c.CopyItem("ws", "TASK-1", ItemCopyRequest{TargetWorkspace: "b", TargetCollection: "tasks"})
	if err == nil {
		t.Fatal("expected a decode error")
	}
	if CopyOutcomeUnknown(err) {
		t.Error("a 2xx means the copy committed; the outcome is known")
	}
	if !CopyCommitted(err) {
		t.Errorf("an undecodable 2xx must be classified as committed-but-unreported; got %v", err)
	}
	if !errors.Is(err, ErrCopyCommitted) {
		t.Errorf("expected ErrCopyCommitted in the chain; got %v", err)
	}
	if len(raw) == 0 {
		t.Error("the raw bytes should still be returned so a caller can show them")
	}
}
