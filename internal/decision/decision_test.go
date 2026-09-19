package decision

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fake stands up an httptest server speaking the provider's protocol and
// returns a provider pointed at it.
//
// handler receives the decoded request so a test can assert on the WIRE SHAPE
// the provider produced, not merely on what it decoded back — the two are
// different claims and only the first catches an encoder that agrees with its
// own decoder.
type fake struct {
	t        *testing.T
	srv      *httptest.Server
	requests []wireRequest
	// raw bodies as received, for tests asserting on criteria shape where
	// the typed wireRequest would have normalised it away.
	rawBodies []string
	slept     []time.Duration
}

func newFake(t *testing.T, handler func(f *fake, req wireRequest, raw []byte, w http.ResponseWriter)) (*typesafeProvider, *fake) {
	t.Helper()
	f := &fake{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization header = %q, want Bearer test-key", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		// io.ReadAll, not a single Read into a ContentLength-sized slice: a
		// single Read may return fewer bytes, which would truncate the body
		// and make every request-shape assertion below fail for the wrong
		// reason.
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("fake could not read request body: %v", err)
		}
		var req wireRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("fake could not decode request: %v (body %q)", err, string(raw))
		}
		f.requests = append(f.requests, req)
		f.rawBodies = append(f.rawBodies, string(raw))
		handler(f, req, raw, w)
	}))
	t.Cleanup(f.srv.Close)

	p := newTypesafe("test-key", "jev-1.13.0")
	p.setEndpoint(f.srv.URL)
	p.sleep = func(d time.Duration) { f.slept = append(f.slept, d) }
	return p, f
}

func ok(body string) func(*fake, wireRequest, []byte, http.ResponseWriter) {
	return func(_ *fake, _ wireRequest, _ []byte, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}
}

// ---------------------------------------------------------------------------
// Shape round-trip, one test per primitive.
//
// Every response body below is the VERBATIM JSON recorded from
// api.typesafe.ai/v1/systemone against jev-1.13.0 (choice and noul from the
// 760-call eval run, score from this unit's probe — receipts on TASK-3116's
// trail). They are pinned as literals rather than produced by marshalling our
// own wire structs, so a test cannot pass by agreeing with a wrong encoder.
// ---------------------------------------------------------------------------

const recordedChoiceResponse = `{"model":"jev-1.13.0","answers":{"severity":{"type":"choice","choice":"high","confidence":0.63,"probabilities":{"high":0.72,"critical":0.05,"medium":0.22,"low":0.01}}},"usage":{"input_tokens":1138,"output_tokens":165}}`

const recordedNoulResponse = `{"model":"jev-1.13.0","answers":{"needs_human_decision":{"type":"noul","noul":0.92}},"usage":{"input_tokens":1224,"output_tokens":41}}`

const recordedScoreResponse = `{"model":"jev-1.13.0","answers":{"severity":{"type":"score","score":0.14,"confidence":0.88,"legend":{"0":"cosmetic only","1":"annoying but harmless","2":"a real defect in normal use","3":"breaks a core flow","4":"data loss or security exposure"},"probabilities":{"0":0.86,"1":0.14,"2":0.0,"3":0.0,"4":0.0}}},"usage":{"input_tokens":372,"output_tokens":17}}`

func TestAskChoiceRoundTrip(t *testing.T) {
	p, f := newFake(t, ok(recordedChoiceResponse))

	q := Choice("How severe is this bug?", map[string]string{
		"low":      "cosmetic",
		"medium":   "a real defect",
		"high":     "breaks a core flow",
		"critical": "data loss",
	})
	answers, usage, err := p.Ask(context.Background(), "a bug report", map[string]Question{"severity": q})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	// Request shape: choice criteria is an OBJECT of option -> description.
	raw := f.rawBodies[0]
	if !strings.Contains(raw, `"type":"choice"`) {
		t.Errorf("request did not carry type choice: %s", raw)
	}
	if !strings.Contains(raw, `"criteria":{`) {
		t.Errorf("choice criteria must serialise as an object, got: %s", raw)
	}
	if f.requests[0].Model != "jev-1.13.0" {
		t.Errorf("model = %q, want the pinned jev-1.13.0", f.requests[0].Model)
	}

	a := answers["severity"]
	if a.Kind != KindChoice {
		t.Errorf("Kind = %q, want %q", a.Kind, KindChoice)
	}
	if a.Choice != "high" {
		t.Errorf("Choice = %q, want high", a.Choice)
	}
	if a.Confidence == nil {
		t.Fatal("Confidence is nil; a choice answer carries one")
	}
	if *a.Confidence != 0.63 {
		t.Errorf("Confidence = %v, want 0.63", *a.Confidence)
	}
	if got := a.Probabilities["high"]; got != 0.72 {
		t.Errorf("Probabilities[high] = %v, want 0.72", got)
	}
	// Choice probabilities are keyed by OPTION NAME, never by index. A
	// consumer written for score's vocabulary must not find a key here.
	if _, hasIndexKey := a.Probabilities["0"]; hasIndexKey {
		t.Error(`choice probabilities must not be keyed by level index`)
	}
	if a.Legend != nil {
		t.Errorf("Legend = %v, want nil for a choice answer", a.Legend)
	}
	if usage.InputTokens != 1138 || usage.OutputTokens != 165 {
		t.Errorf("Usage = %+v, want 1138/165", usage)
	}
	if usage.StateTruncated {
		t.Error("StateTruncated is true for a short state")
	}
}

func TestAskNoulRoundTripCarriesNoConfidence(t *testing.T) {
	p, f := newFake(t, ok(recordedNoulResponse))

	q := Noul("The item is waiting on a human's judgment.",
		"a human must decide", "an agent can proceed")
	answers, _, err := p.Ask(context.Background(), "an item body", map[string]Question{"needs_human_decision": q})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	raw := f.rawBodies[0]
	if !strings.Contains(raw, `"type":"noul"`) {
		t.Errorf("request did not carry type noul: %s", raw)
	}
	if !strings.Contains(raw, `"true":"a human must decide"`) {
		t.Errorf("noul criteria must carry the true/false pair: %s", raw)
	}

	a := answers["needs_human_decision"]
	if a.Kind != KindNoul {
		t.Errorf("Kind = %q, want %q", a.Kind, KindNoul)
	}
	if a.Noul != 0.92 {
		t.Errorf("Noul = %v, want 0.92", a.Noul)
	}

	// THE point of the pointer. Asserting only "Confidence is nil" would
	// also pass if the field were a float64 that happened to be
	// unset — which is exactly the bug — so assert the CONSEQUENCE a caller
	// would see: the banding default must be the caller's, not 0.0.
	if a.Confidence != nil {
		t.Errorf("Confidence = %v, want nil: a noul answer carries none", *a.Confidence)
	}
	if got := a.ConfidenceOr(1.0); got != 1.0 {
		t.Errorf("ConfidenceOr(1.0) = %v, want the caller's default 1.0; a zero here "+
			"means an absent confidence is being read as minimum confidence", got)
	}
	if a.Probabilities != nil {
		t.Errorf("Probabilities = %v, want nil for a noul answer", a.Probabilities)
	}
}

func TestAskScoreRoundTripAndScoreIsNotALevelIndex(t *testing.T) {
	p, f := newFake(t, ok(recordedScoreResponse))

	levels := []string{
		"cosmetic only", "annoying but harmless", "a real defect in normal use",
		"breaks a core flow", "data loss or security exposure",
	}
	answers, _, err := p.Ask(context.Background(), "a bug report", map[string]Question{
		"severity": Score("Rate how severe this bug is.", levels),
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	// Request shape: score criteria is an ORDERED ARRAY, not a map. This is
	// the one request-shape difference that a map would silently survive
	// locally and fail against the live API.
	raw := f.rawBodies[0]
	if !strings.Contains(raw, `"criteria":["cosmetic only","annoying but harmless"`) {
		t.Errorf("score criteria must serialise as an ordered array in the given order, got: %s", raw)
	}

	a := answers["severity"]
	if a.Kind != KindScore {
		t.Errorf("Kind = %q, want %q", a.Kind, KindScore)
	}
	if a.Score != 0.14 {
		t.Errorf("Score = %v, want 0.14", a.Score)
	}
	if a.Legend["4"] != "data loss or security exposure" {
		t.Errorf("Legend[4] = %q, want the highest level", a.Legend["4"])
	}
	if got := a.Probabilities["0"]; got != 0.86 {
		t.Errorf("Probabilities[0] = %v, want 0.86", got)
	}

	// Score is the probability-weighted MEAN over level indices, so the
	// picked level comes from the argmax and NOT from the number.
	idx, name, okTop := a.TopLevel()
	if !okTop {
		t.Fatal("TopLevel() reported no level for a score answer")
	}
	if idx != 0 || name != "cosmetic only" {
		t.Errorf("TopLevel() = (%d, %q), want (0, cosmetic only)", idx, name)
	}
	// The counterfactual that makes the assertion above mean something: the
	// mass here sits on index 0, and 0.14 rounds to 0 too, so this case
	// alone cannot distinguish argmax from rounding. Assert on a
	// distribution where they DISAGREE.
	skewed := Answer{
		Kind:          KindScore,
		Score:         2.6, // 0.4*2 + 0.6*3
		Legend:        map[string]string{"0": "a", "1": "b", "2": "c", "3": "d"},
		Probabilities: map[string]float64{"0": 0, "1": 0, "2": 0.4, "3": 0.6},
	}
	if idx, name, _ := skewed.TopLevel(); idx != 3 || name != "d" {
		t.Errorf("TopLevel() on a skewed distribution = (%d, %q), want (3, d); "+
			"int-casting Score would give 2 here", idx, name)
	}
}

func TestTopLevelTiesResolveToLowestIndexAndRejectOtherKinds(t *testing.T) {
	tie := Answer{
		Kind:          KindScore,
		Legend:        map[string]string{"0": "low", "1": "high"},
		Probabilities: map[string]float64{"0": 0.5, "1": 0.5},
	}
	// Run repeatedly: a map-order-dependent implementation passes this
	// intermittently, so one iteration would not discriminate.
	for i := 0; i < 50; i++ {
		if idx, name, _ := tie.TopLevel(); idx != 0 || name != "low" {
			t.Fatalf("iteration %d: TopLevel() = (%d, %q), want the lowest index (0, low)", i, idx, name)
		}
	}
	if _, _, okTop := (Answer{Kind: KindChoice, Probabilities: map[string]float64{"a": 1}}).TopLevel(); okTop {
		t.Error("TopLevel() reported ok for a choice answer")
	}
	if _, _, okTop := (Answer{Kind: KindScore}).TopLevel(); okTop {
		t.Error("TopLevel() reported ok for a score answer with no probabilities")
	}
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

func TestOversizedStateSurfacesTypedError(t *testing.T) {
	// The body is the one measured from the live API.
	p, _ := newFake(t, func(_ *fake, _ wireRequest, _ []byte, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":{"error_type":"max_tokens_exceeded"}}`))
	})

	_, _, err := p.Ask(context.Background(), "s", map[string]Question{
		"q": Noul("a statement", "", ""),
	})
	if err == nil {
		t.Fatal("Ask succeeded on a 400 max_tokens_exceeded")
	}
	if !errors.Is(err, ErrMaxTokensExceeded) {
		t.Errorf("errors.Is(err, ErrMaxTokensExceeded) = false for a measured budget refusal; err = %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err is not an *APIError: %v", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400 (the MEASURED status; the contract said 422)", apiErr.StatusCode)
	}
	if apiErr.ErrorType != "max_tokens_exceeded" {
		t.Errorf("ErrorType = %q, want max_tokens_exceeded", apiErr.ErrorType)
	}
}

func Test422SurfacesBodyAndIsNotABudgetError(t *testing.T) {
	const body = `{"detail":[{"loc":["body","questions","q","criteria"],"msg":"at least 2 levels required"}]}`
	p, _ := newFake(t, func(_ *fake, _ wireRequest, _ []byte, w http.ResponseWriter) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(body))
	})

	_, _, err := p.Ask(context.Background(), "s", map[string]Question{"q": Noul("a statement", "", "")})
	if err == nil {
		t.Fatal("Ask succeeded on a 422")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err is not an *APIError: %v", err)
	}
	if apiErr.Body != body {
		t.Errorf("Body = %q, want the provider's body verbatim %q", apiErr.Body, body)
	}
	if !strings.Contains(err.Error(), "at least 2 levels required") {
		t.Errorf("Error() dropped the provider's message, which names the offending question: %q", err.Error())
	}
	// Control leg: 422 must NOT match the budget sentinel, or the typed
	// error above would be meaningless.
	if errors.Is(err, ErrMaxTokensExceeded) {
		t.Error("a 422 validation failure matched ErrMaxTokensExceeded")
	}
}

func TestRetriesTransientThenSucceeds(t *testing.T) {
	calls := 0
	p, f := newFake(t, func(_ *fake, _ wireRequest, _ []byte, w http.ResponseWriter) {
		calls++
		switch calls {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"detail":{"error_type":"rate_limited"}}`))
		case 2:
			w.WriteHeader(529)
			_, _ = w.Write([]byte(`{"detail":{"error_type":"overloaded"}}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(recordedNoulResponse))
		}
	})

	answers, _, err := p.Ask(context.Background(), "s", map[string]Question{
		"needs_human_decision": Noul("a statement", "", ""),
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if calls != 3 {
		t.Errorf("provider was called %d times, want 3 (429, 529, then 200)", calls)
	}
	if answers["needs_human_decision"].Noul != 0.92 {
		t.Errorf("answer not returned after a successful retry: %+v", answers)
	}
	// Assert the backoff GREW, which is what distinguishes exponential
	// backoff from a fixed sleep — the end state (a success) is identical
	// either way.
	if len(f.slept) != 2 {
		t.Fatalf("slept %d times, want 2: %v", len(f.slept), f.slept)
	}
	if f.slept[0] != initialBackoff {
		t.Errorf("first backoff = %v, want %v", f.slept[0], initialBackoff)
	}
	if f.slept[1] <= f.slept[0] {
		t.Errorf("backoff did not grow: %v then %v", f.slept[0], f.slept[1])
	}
}

func TestRetryAfterHeaderOverridesBackoffAndIsBounded(t *testing.T) {
	calls := 0
	p, f := newFake(t, func(_ *fake, _ wireRequest, _ []byte, w http.ResponseWriter) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(recordedNoulResponse))
	})
	if _, _, err := p.Ask(context.Background(), "s", map[string]Question{
		"needs_human_decision": Noul("a statement", "", ""),
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(f.slept) != 1 || f.slept[0] != 3*time.Second {
		t.Errorf("slept %v, want one 3s wait from the Retry-After header", f.slept)
	}

	if got := parseRetryAfter("99999"); got != maxRetryAfter {
		t.Errorf("parseRetryAfter(99999) = %v, want it clamped to %v", got, maxRetryAfter)
	}
	for _, bad := range []string{"", "0", "-5", "Wed, 21 Oct 2015 07:28:00 GMT", "soon"} {
		if got := parseRetryAfter(bad); got != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0 so the normal backoff applies", bad, got)
		}
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	calls := 0
	p, _ := newFake(t, func(_ *fake, _ wireRequest, _ []byte, w http.ResponseWriter) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, _, err := p.Ask(context.Background(), "s", map[string]Question{"q": Noul("a statement", "", "")})
	if err == nil {
		t.Fatal("Ask succeeded against a provider that only ever 429s")
	}
	if calls != maxAttempts {
		t.Errorf("provider was called %d times, want maxAttempts=%d", calls, maxAttempts)
	}
}

func TestPartialAnswerSetIsAnError(t *testing.T) {
	// Two questions asked, one answered. A partial map would leave the
	// caller unable to tell "no opinion" from "half the call failed".
	p, _ := newFake(t, ok(recordedNoulResponse))
	_, _, err := p.Ask(context.Background(), "s", map[string]Question{
		"needs_human_decision": Noul("a statement", "", ""),
		"blocked":              Noul("another statement", "", ""),
	})
	if err == nil {
		t.Fatal("Ask succeeded with one of two questions unanswered")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("error does not name the unanswered question: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Truncation
// ---------------------------------------------------------------------------

func TestOversizedStringStateIsTruncatedAndFlagged(t *testing.T) {
	p, f := newFake(t, ok(recordedNoulResponse))

	// Multi-byte runes so a naive byte cut would produce invalid UTF-8.
	big := strings.Repeat("état ", 40000)
	if len(big) <= int(float64(maxRequestTokens)*charsPerToken) {
		t.Fatalf("test state is not actually over budget (%d chars)", len(big))
	}

	_, usage, err := p.Ask(context.Background(), big, map[string]Question{
		"needs_human_decision": Noul("a statement", "", ""),
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !usage.StateTruncated {
		t.Error("StateTruncated is false after an over-budget state was shortened")
	}

	sent, okStr := f.requests[0].State.(string)
	if !okStr {
		t.Fatalf("state arrived as %T, want string", f.requests[0].State)
	}
	if len(sent) >= len(big) {
		t.Errorf("state was not shortened: sent %d chars of %d", len(sent), len(big))
	}
	if !utf8.ValidString(sent) {
		t.Error("truncated state is not valid UTF-8; the cut ignored rune boundaries")
	}
	if !strings.HasPrefix(big, sent) {
		t.Error("truncated state is not a prefix of the original")
	}
	// The cut must leave room for the questions too, since the measured
	// budget covers state AND questions together.
	if len(sent) > int(float64(maxRequestTokens)*charsPerToken) {
		t.Errorf("truncated state (%d chars) exceeds the whole-request char budget", len(sent))
	}
}

func TestShortStateIsNotTruncated(t *testing.T) {
	p, f := newFake(t, ok(recordedNoulResponse))
	_, usage, err := p.Ask(context.Background(), "a short body", map[string]Question{
		"needs_human_decision": Noul("a statement", "", ""),
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if usage.StateTruncated {
		t.Error("StateTruncated is true for a state well inside the budget")
	}
	if got := f.requests[0].State; got != "a short body" {
		t.Errorf("state = %v, want it passed through verbatim", got)
	}
}

func TestStructuredStateIsNeverTruncated(t *testing.T) {
	// Cutting marshalled JSON at a byte boundary produces invalid JSON, and
	// dropping fields would change what is being asked about. A structured
	// state that does not fit gets the provider's own refusal instead.
	p, f := newFake(t, ok(recordedNoulResponse))
	state := map[string]any{"body": strings.Repeat("x", 200000)}

	_, usage, err := p.Ask(context.Background(), state, map[string]Question{
		"needs_human_decision": Noul("a statement", "", ""),
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if usage.StateTruncated {
		t.Error("StateTruncated is true for a structured state, which is never cut")
	}
	m, okMap := f.requests[0].State.(map[string]any)
	if !okMap {
		t.Fatalf("state arrived as %T, want a JSON object", f.requests[0].State)
	}
	if s, _ := m["body"].(string); len(s) != 200000 {
		t.Errorf("structured state body is %d chars, want the full 200000 — it must not be cut", len(s))
	}
}

// ---------------------------------------------------------------------------
// Question validation
// ---------------------------------------------------------------------------

func TestQuestionValidation(t *testing.T) {
	tests := []struct {
		name    string
		q       Question
		wantErr string
	}{
		{"choice ok", Choice("i", map[string]string{"a": "x", "b": "y"}), ""},
		{"score ok", Score("i", []string{"a", "b"}), ""},
		{"noul ok with criteria", Noul("i", "t", "f"), ""},
		{"noul ok bare", Noul("i", "", ""), ""},

		{"empty instructions", Choice("  ", map[string]string{"a": "x", "b": "y"}), "instructions are empty"},
		{"choice one option", Choice("i", map[string]string{"a": "x"}), "at least 2 options"},
		{"choice empty option name", Choice("i", map[string]string{"": "x", "b": "y"}), "empty option name"},
		{"choice empty description", Choice("i", map[string]string{"a": " ", "b": "y"}), "empty description"},
		{"score one level", Score("i", []string{"a"}), "at least 2 levels"},
		{"score empty level", Score("i", []string{"a", " "}), "level 1 is empty"},
		{"unknown kind", Question{Kind: "guess", Instructions: "i"}, "unknown question kind"},

		// Cross-kind criteria. Each of these is a caller who meant a
		// different constructor; silently dropping the stray field would
		// send a question they did not write.
		{"choice with levels", Question{Kind: KindChoice, Instructions: "i",
			Options: map[string]string{"a": "x", "b": "y"}, Levels: []string{"l1", "l2"}},
			"another kind's criteria"},
		{"score with options", Question{Kind: KindScore, Instructions: "i",
			Levels: []string{"a", "b"}, Options: map[string]string{"o": "d"}},
			"another kind's criteria"},
		{"noul with options", Question{Kind: KindNoul, Instructions: "i",
			Options: map[string]string{"a": "x"}}, "another kind's criteria"},
		{"noul half-described", Question{Kind: KindNoul, Instructions: "i", TrueDesc: "t"},
			"only one of true/false"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.q.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestAskRejectsBadInputBeforeCallingProvider(t *testing.T) {
	called := false
	p, _ := newFake(t, func(_ *fake, _ wireRequest, _ []byte, w http.ResponseWriter) {
		called = true
		_, _ = w.Write([]byte(recordedNoulResponse))
	})

	if _, _, err := p.Ask(context.Background(), "s", nil); err == nil {
		t.Error("Ask succeeded with no questions")
	}
	if _, _, err := p.Ask(context.Background(), "s", map[string]Question{"": Noul("i", "", "")}); err == nil {
		t.Error("Ask succeeded with an empty question key")
	}
	if _, _, err := p.Ask(context.Background(), "s", map[string]Question{
		"q": Choice("i", map[string]string{"only": "one"}),
	}); err == nil {
		t.Error("Ask succeeded with an invalid choice question")
	} else if !strings.Contains(err.Error(), `question "q"`) {
		t.Errorf("error does not name the offending question key: %v", err)
	}

	// The whole point of validating first: no request was ever sent.
	if called {
		t.Error("provider was called despite invalid input")
	}
}
