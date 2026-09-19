package decision

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// One test per finding from review round 1 (backend: opencode/gpt-5.6-luna).
// Each asserts the CONSEQUENCE the finding described, so it fails on the
// pre-fix code rather than merely exercising the new lines.

// Finding 1 — the backoff ignored context cancellation, so a cancelled Ask
// waited out the full wait (up to maxBackoff) before issuing a doomed retry.
//
// The finding names ONE mechanism and it needs two instruments, because the
// obvious single test does not reach the code it claims to. Cancelling during
// the first response makes http.Client.Do fail, so Ask returns from the
// TRANSPORT-error branch having never reached the sleep at all — the first
// version of this test asserted one recorded wait and got zero, which is how
// the substitution showed up. So: this test pins that Ask HONOURS an
// interrupted wait, and TestCtxSleepReturnsEarlyOnCancellation pins that the
// wait actually installed on a real provider IS interruptible. Neither alone
// covers the finding.
func TestAskAbortsWhenTheBackoffIsInterrupted(t *testing.T) {
	sentinel := errors.New("wait interrupted")

	calls := 0
	p, f := newFake(t, func(_ *fake, _ wireRequest, _ []byte, w http.ResponseWriter) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	})
	// Reach the sleep on a live context, then have the wait report that it was
	// interrupted. This is what a cancellation landing inside the backoff
	// looks like from Ask's point of view, without racing a real timer.
	p.sleep = func(_ context.Context, d time.Duration) error {
		f.slept = append(f.slept, d)
		return sentinel
	}

	_, _, err := p.Ask(context.Background(), "s", map[string]Question{"q": Noul("a statement", "", "")})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the interrupted wait's error to propagate", err)
	}
	// One request, one interrupted wait, and no further attempts: the loop
	// must stop rather than proceed into a doomed retry.
	if calls != 1 {
		t.Errorf("provider was called %d times after the wait was interrupted, want 1", calls)
	}
	if len(f.slept) != 1 {
		t.Errorf("slept %v, want exactly one wait before aborting", f.slept)
	}
}

// Finding 1, the in-flight case: a cancellation while the request is on the
// wire must also end the call, and must not be swallowed as a retryable
// transport error.
func TestCancellationDuringTheRequestEndsTheCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	calls := 0
	p, _ := newFake(t, func(_ *fake, _ wireRequest, _ []byte, w http.ResponseWriter) {
		calls++
		cancel()
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, _, err := p.Ask(ctx, "s", map[string]Question{"q": Noul("a statement", "", "")})
	if err == nil {
		t.Fatal("Ask succeeded after its context was cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("provider was called %d times, want 1: a cancelled context must not be "+
			"retried as a transient transport failure", calls)
	}
}

// Finding 1, the production path: the real sleep must return early. The fake
// above can only show that Ask HONOURS an aborting sleep; this shows that the
// sleep actually installed on a real provider aborts.
func TestCtxSleepReturnsEarlyOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := ctxSleep(ctx, 5*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("ctxSleep returned %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Errorf("ctxSleep waited %v on an already-cancelled context, want it to return immediately", elapsed)
	}

	// Control leg: with a live context it really does wait, so the assertion
	// above is not passing because ctxSleep never waits at all.
	start = time.Now()
	if err := ctxSleep(context.Background(), 40*time.Millisecond); err != nil {
		t.Errorf("ctxSleep on a live context returned %v, want nil", err)
	}
	if elapsed = time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("ctxSleep returned after %v for a 40ms wait — it is not waiting at all", elapsed)
	}

	// And newTypesafe must actually install it; a provider left with a
	// non-cancellable sleep would pass every test above.
	if p := newTypesafe("k", "m"); p.sleep == nil {
		t.Error("newTypesafe installed no sleep function")
	} else if err := p.sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("the sleep installed by newTypesafe returned %v on a cancelled context, "+
			"want context.Canceled — it is not cancellable", err)
	}
}

// Finding 2 — a body read that failed partway was discarded, so a caller could
// not tell an absent ErrorType from an unread one.
func TestPartialBodyReadIsCarriedOnTheError(t *testing.T) {
	// Content-Length promises more than the handler writes, so the client's
	// read fails partway with an unexpected EOF.
	srv := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "200")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":{"error_ty`))
	})
	p := newTypesafe("test-key", "jev-1.13.0")
	p.setEndpoint(srv.URL)
	p.sleep = func(context.Context, time.Duration) error { return nil }

	_, _, err := p.Ask(context.Background(), "s", map[string]Question{"q": Noul("a statement", "", "")})
	if err == nil {
		t.Fatal("Ask succeeded on a 400 with a truncated body")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err is not an *APIError: %v", err)
	}
	if apiErr.BodyReadErr == nil {
		t.Error("BodyReadErr is nil after a body read that failed partway; a caller cannot " +
			"tell the missing ErrorType from an unread one")
	}
	if !strings.Contains(err.Error(), "read failed partway") {
		t.Errorf("Error() does not mention the partial read: %q", err.Error())
	}
	// The partial bytes are still the best evidence available, so they are kept.
	if !strings.Contains(apiErr.Body, "error_ty") {
		t.Errorf("Body = %q, want the partial bytes that did arrive", apiErr.Body)
	}
}

// Finding 3 — exhausting retries wrapped a *transientError, which is not an
// *APIError, so errors.As could not reach the status or error type.
func TestExhaustedRetriesStillExposeTheAPIError(t *testing.T) {
	p, _ := newFake(t, func(_ *fake, _ wireRequest, _ []byte, w http.ResponseWriter) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"detail":{"error_type":"rate_limited"}}`))
	})

	_, _, err := p.Ask(context.Background(), "s", map[string]Question{"q": Noul("a statement", "", "")})
	if err == nil {
		t.Fatal("Ask succeeded against a provider that only 429s")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("errors.As(err, **APIError) = false after exhausted retries; the status and "+
			"error type are in the chain and unreachable. err = %v", err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want 429", apiErr.StatusCode)
	}
	if apiErr.ErrorType != "rate_limited" {
		t.Errorf("ErrorType = %q, want rate_limited", apiErr.ErrorType)
	}
	// The wrapper's own message must survive too — the caller needs to know
	// it was a retry wall, not a single refusal.
	if !strings.Contains(err.Error(), "after") {
		t.Errorf("Error() does not say the retries were exhausted: %q", err.Error())
	}
}

// Finding 4 — the truncation budget was measured on RAW string length while
// encoding/json escapes <, > and & to six-byte sequences, so a state of '<'
// characters passed the budget check and then drew max_tokens_exceeded.
func TestTruncationAccountsForJSONEscapeExpansion(t *testing.T) {
	// Establish the premise rather than asserting it from memory: the
	// expansion is real and it is 6x.
	probe, err := json.Marshal(strings.Repeat("<", 1000))
	if err != nil {
		t.Fatalf("marshal probe: %v", err)
	}
	if len(probe) < 6000 {
		t.Fatalf("premise failed: 1000 '<' marshalled to %d bytes, expected ~6002 — "+
			"if encoding/json stopped escaping, this test and the fitState comment are stale",
			len(probe))
	}

	p := newTypesafe("test-key", "jev-1.13.0")
	questions := map[string]wireQuestion{"q": {Type: "noul", Instructions: "i"}}
	budget := int(float64(maxRequestTokens) * charsPerToken)

	for _, tc := range []struct {
		name  string
		state string
	}{
		// Raw length just inside the char budget, marshalled length 6x over:
		// this is the exact case the old code let through untouched.
		{"all escaped", strings.Repeat("<", budget-1000)},
		{"mostly escaped", strings.Repeat("<a", budget/2)},
		{"ampersands", strings.Repeat("&", budget-1000)},
		{"plain over budget", strings.Repeat("a", budget*2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated, err := p.fitState(tc.state, questions)
			if err != nil {
				t.Fatalf("fitState: %v", err)
			}
			if !truncated {
				t.Fatalf("state of %d raw bytes (%d marshalled) was not truncated",
					len(tc.state), marshalledLen(tc.state))
			}
			s, okStr := got.(string)
			if !okStr {
				t.Fatalf("fitState returned %T, want string", got)
			}
			// THE assertion: what goes on the wire must fit the budget.
			if ml := marshalledLen(s); ml > budget {
				t.Errorf("truncated state marshals to %d bytes, over the %d-byte budget — "+
					"the cut was sized on raw length", ml, budget)
			}
			if !utf8.ValidString(s) {
				t.Error("truncated state is not valid UTF-8")
			}
			if !strings.HasPrefix(tc.state, s) {
				t.Error("truncated state is not a prefix of the original")
			}
			// It must not over-truncate to nothing when something fits.
			if len(s) == 0 {
				t.Error("everything was cut; a state this size has room for some prefix")
			}
		})
	}
}

// Finding 4, boundary: when the questions alone consume the budget there is no
// room for any state, and that must be an error or an empty state — never a
// panic or a negative slice index.
func TestFitStateWithNoRoomForState(t *testing.T) {
	p := newTypesafe("test-key", "jev-1.13.0")
	budget := int(float64(maxRequestTokens) * charsPerToken)

	huge := map[string]wireQuestion{"q": {Type: "noul", Instructions: strings.Repeat("i", budget*2)}}
	if _, _, err := p.fitState("some state", huge); err == nil {
		t.Error("fitState succeeded with questions larger than the whole budget, want an error")
	}

	// Just-barely-no-room: the questions fit, but leave almost nothing.
	tight := map[string]wireQuestion{"q": {Type: "noul", Instructions: strings.Repeat("i", budget-600)}}
	got, _, err := p.fitState(strings.Repeat("<", 5000), tight)
	if err != nil {
		t.Fatalf("fitState: %v", err)
	}
	s, _ := got.(string)
	if ml := marshalledLen(s); ml > budget {
		t.Errorf("truncated state marshals to %d, over budget %d", ml, budget)
	}
	if !utf8.ValidString(s) {
		t.Error("truncated state is not valid UTF-8")
	}
}

// Finding 4, the helper's own edge case I found while fixing it: an index at or
// past len(s) must not index out of range.
func TestRuneBoundaryAtOrBeforeEdges(t *testing.T) {
	const s = "aé" // 3 bytes: 'a', then a two-byte rune
	for _, tc := range []struct{ in, want int }{
		{-5, 0}, {0, 0}, {1, 1}, {2, 1}, {3, 3}, {4, 3}, {100, 3},
	} {
		if got := runeBoundaryAtOrBefore(s, tc.in); got != tc.want {
			t.Errorf("runeBoundaryAtOrBefore(%q, %d) = %d, want %d", s, tc.in, got, tc.want)
		}
	}
	if got := runeBoundaryAtOrBefore("", 0); got != 0 {
		t.Errorf("runeBoundaryAtOrBefore(\"\", 0) = %d, want 0", got)
	}
}

// Finding 5 — an answer whose kind differs from its question's was accepted,
// and fromWire then left the requested kind's member at a plausible zero.
func TestAnswerKindMustMatchTheQuestion(t *testing.T) {
	// A noul question answered with a score. Noul would read 0.0 — "almost
	// certainly false" — which the provider never said.
	p, _ := newFake(t, ok(`{"model":"jev-1.13.0","answers":{"needs_human":{"type":"score","score":0,"confidence":0.9,"probabilities":{"0":1}}},"usage":{"input_tokens":10,"output_tokens":2}}`))

	_, _, err := p.Ask(context.Background(), "s", map[string]Question{
		"needs_human": Noul("a statement", "", ""),
	})
	if err == nil {
		t.Fatal("Ask accepted a score answer to a noul question; the caller would have read " +
			"Noul == 0.0, a confident 'false' the provider never said")
	}
	if !strings.Contains(err.Error(), "needs_human") {
		t.Errorf("error does not name the question: %v", err)
	}
	if !strings.Contains(err.Error(), string(KindNoul)) || !strings.Contains(err.Error(), string(KindScore)) {
		t.Errorf("error does not name both the asked and answered kinds: %v", err)
	}

	// Control leg: the matching kind still succeeds, so the check is not
	// simply refusing everything.
	p2, _ := newFake(t, ok(recordedNoulResponse))
	if _, _, err := p2.Ask(context.Background(), "s", map[string]Question{
		"needs_human_decision": Noul("a statement", "", ""),
	}); err != nil {
		t.Errorf("a correctly-typed answer was refused: %v", err)
	}
}

// Finding 6 — TopLevel ordered only on the parsed index, so distinct keys
// sharing an index ("1" and "01"), or several unparseable keys sharing the
// sentinel, were tie-broken by Go's randomised map iteration order.
func TestTopLevelIsDeterministicForDegenerateKeys(t *testing.T) {
	cases := []struct {
		name     string
		answer   Answer
		wantName string
	}{
		{
			// Same numeric index, different key strings, equal probability.
			name: "numerically duplicate keys",
			answer: Answer{Kind: KindScore,
				Legend:        map[string]string{"1": "one", "01": "oh-one"},
				Probabilities: map[string]float64{"1": 0.5, "01": 0.5}},
			wantName: "oh-one", // key "01" sorts before "1"
		},
		{
			// Several unparseable keys all collapse to the same sentinel.
			name: "unparseable keys",
			answer: Answer{Kind: KindScore,
				Legend:        map[string]string{"zz": "zed", "aa": "ay", "mm": "em"},
				Probabilities: map[string]float64{"zz": 0.4, "aa": 0.4, "mm": 0.4}},
			wantName: "ay", // "aa" sorts first among the tied sentinels
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Repeat: a map-order-dependent implementation passes
			// intermittently, so a single iteration discriminates nothing.
			for i := 0; i < 200; i++ {
				_, name, okTop := tc.answer.TopLevel()
				if !okTop {
					t.Fatalf("iteration %d: TopLevel reported not-ok", i)
				}
				if name != tc.wantName {
					t.Fatalf("iteration %d: TopLevel name = %q, want a stable %q — the tie-break "+
						"is decided by map iteration order", i, name, tc.wantName)
				}
			}
		})
	}

	// A real index still beats an unparseable key, so the sentinel does not
	// accidentally win by sorting first.
	mixed := Answer{Kind: KindScore,
		Legend:        map[string]string{"0": "zero", "junk": "junk"},
		Probabilities: map[string]float64{"0": 0.5, "junk": 0.5}}
	for i := 0; i < 50; i++ {
		if idx, name, _ := mixed.TopLevel(); idx != 0 || name != "zero" {
			t.Fatalf("iteration %d: TopLevel = (%d,%q), want the real index (0,\"zero\") to win "+
				"the tie against an unparseable key", i, idx, name)
		}
	}
}

// newRawServer is for the cases needing control of the raw response — here, a
// Content-Length that lies — which the shared fake cannot express.
func newRawServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}
