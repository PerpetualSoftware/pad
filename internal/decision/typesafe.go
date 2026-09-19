package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// defaultTypesafeEndpoint is typesafe.ai's System One endpoint.
const defaultTypesafeEndpoint = "https://api.typesafe.ai/v1/systemone"

// Token budget for one request.
//
// maxRequestTokens is MEASURED against jev-1.13.0, not taken from the vendor
// docs, which state no limit. Bisecting on the API's own
// usage.input_tokens: 32,476 tokens was ACCEPTED and the next step up
// (~33,600) was refused with HTTP 400 max_tokens_exceeded. The budget counts
// state AND questions together — input_tokens includes the questions — so a
// large question set leaves less room for state. 32,000 sits under the
// measured accepted figure with ~476 tokens of headroom, and independently
// matches the Cloudflare model card's published context window, which is the
// only published number and is therefore unlikely to be reduced below this.
// Receipts, including the full bisection, are on TASK-3116's trail.
const maxRequestTokens = 32000

// charsPerToken converts characters to an estimated token count for
// truncation.
//
// It is a deliberate UNDER-estimate of characters per token, which makes the
// resulting token estimate an OVER-estimate and truncation conservative. The
// bisection above measured 2.80 chars/token on number-dense filler; ordinary
// English prose runs nearer 4. Dense content — base64, hashes, minified JSON —
// tokenizes worse than either, so this cannot be a guarantee: it is a cheap
// bound that keeps normal item content well inside the budget. The real
// backstop for pathological input is the provider's own refusal, surfaced as
// [ErrMaxTokensExceeded] rather than swallowed, so a caller can shrink its
// state instead of reading a silent truncation.
const charsPerToken = 2.5

// Retry policy for transient provider failures.
const (
	maxAttempts      = 4
	initialBackoff   = 500 * time.Millisecond
	maxBackoff       = 8 * time.Second
	maxRetryAfter    = 30 * time.Second
	requestTimeout   = 60 * time.Second
	maxErrBodyLength = 2048
)

// ErrMaxTokensExceeded reports that the provider refused the request because
// state plus questions exceeded its token budget.
//
// It is distinguished from ordinary validation failures because the remedy is
// different and mechanical: send less state. The provider signals it with
// HTTP 400 and a machine-readable body ({"detail":{"error_type":
// "max_tokens_exceeded"}}), so this does not rest on matching prose.
var ErrMaxTokensExceeded = errors.New("decision: provider token budget exceeded")

// APIError is a non-retryable error response from the provider. The body is
// carried verbatim (bounded) because the provider's validation messages name
// the offending question, which is the only way a caller can fix the call.
type APIError struct {
	StatusCode int
	// ErrorType is the provider's machine-readable code from
	// detail.error_type, when present.
	ErrorType string
	// Body is the response body, truncated to a bounded length.
	Body string
	// BodyReadErr is set when reading the response body failed partway.
	// Body then holds whatever arrived before the failure, and callers must
	// not treat it as the provider's complete message — in particular a
	// missing ErrorType may mean "could not read" rather than "not that
	// error". Keeping the read failure instead of discarding it is the
	// difference between a caller knowing the evidence is partial and
	// silently trusting a truncated body.
	BodyReadErr error
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "decision: provider returned %d", e.StatusCode)
	if e.ErrorType != "" {
		fmt.Fprintf(&b, " (%s)", e.ErrorType)
	}
	fmt.Fprintf(&b, ": %s", e.Body)
	if e.BodyReadErr != nil {
		fmt.Fprintf(&b, " [response body read failed partway: %v]", e.BodyReadErr)
	}
	return b.String()
}

// Is lets errors.Is(err, ErrMaxTokensExceeded) match the budget refusal
// without the caller inspecting the status code or the body.
func (e *APIError) Is(target error) bool {
	return target == ErrMaxTokensExceeded && e.ErrorType == "max_tokens_exceeded"
}

// typesafeProvider is the typesafe.ai Jev backend.
type typesafeProvider struct {
	apiKey   string
	model    string
	endpoint string
	client   *http.Client
	// sleep waits for d or until ctx is done, whichever comes first,
	// returning ctx.Err() in the latter case. Tests replace it to record the
	// requested durations without spending wall clock.
	//
	// It takes a context because a plain time.Sleep here made a cancelled
	// Ask keep waiting out the full backoff — up to maxBackoff — before
	// issuing a request that could only fail. Cancellation has to be able to
	// interrupt the WAIT, not merely be noticed after it.
	sleep func(ctx context.Context, d time.Duration) error
}

func newTypesafe(apiKey, model string) *typesafeProvider {
	return &typesafeProvider{
		apiKey:   apiKey,
		model:    model,
		endpoint: defaultTypesafeEndpoint,
		client:   &http.Client{Timeout: requestTimeout},
		sleep:    ctxSleep,
	}
}

// ctxSleep waits for d, or returns early with ctx.Err() if the context is done
// first.
func ctxSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (p *typesafeProvider) Name() string  { return ProviderTypesafe }
func (p *typesafeProvider) Model() string { return p.model }

// setEndpoint points the provider at a different base URL. Intended for tests
// standing up an httptest server; production callers leave the default.
func (p *typesafeProvider) setEndpoint(u string) { p.endpoint = u }

// wireQuestion is one question as the provider expects it. Criteria is
// `any` because its shape depends on the kind: a map for choice and noul, an
// ordered array for score.
type wireQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type wireRequest struct {
	Model     string                  `json:"model"`
	State     any                     `json:"state"`
	Questions map[string]wireQuestion `json:"questions"`
}

// wireAnswer is one answer as the provider returns it. Confidence is a
// pointer so a noul answer — which carries none — stays distinguishable from
// one whose confidence really is zero.
type wireAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Score         float64            `json:"score"`
	Noul          float64            `json:"noul"`
	Legend        map[string]string  `json:"legend"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    *float64           `json:"confidence"`
}

type wireUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type wireResponse struct {
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
	Usage   wireUsage             `json:"usage"`
}

// wireError is the provider's error envelope. detail is `any` because the
// provider returns an object for the cases measured here and the shape is
// undocumented; errorTypeOf digs the code out without depending on it.
type wireError struct {
	Detail any `json:"detail"`
}

// Ask implements [Provider].
func (p *typesafeProvider) Ask(ctx context.Context, state any, questions map[string]Question) (map[string]Answer, Usage, error) {
	if len(questions) == 0 {
		return nil, Usage{}, errors.New("decision: no questions asked")
	}

	wire := make(map[string]wireQuestion, len(questions))
	for key, q := range questions {
		if strings.TrimSpace(key) == "" {
			return nil, Usage{}, errors.New("decision: question key is empty")
		}
		if err := q.Validate(); err != nil {
			return nil, Usage{}, fmt.Errorf("question %q: %w", key, err)
		}
		wire[key] = toWire(q)
	}

	state, truncated, err := p.fitState(state, wire)
	if err != nil {
		return nil, Usage{}, err
	}

	body, err := json.Marshal(wireRequest{Model: p.model, State: state, Questions: wire})
	if err != nil {
		return nil, Usage{}, fmt.Errorf("decision: marshal request: %w", err)
	}

	resp, err := p.post(ctx, body)
	if err != nil {
		return nil, Usage{}, err
	}

	answers := make(map[string]Answer, len(resp.Answers))
	for key, wa := range resp.Answers {
		answers[key] = fromWire(wa)
	}
	// A provider that answered a subset would leave the caller unable to
	// tell a missing answer from one it forgot to ask for.
	//
	// The KIND check beside it matters for the same reason the Confidence
	// pointer does. fromWire populates only the members the ANSWERED kind
	// uses, so a noul question answered with {"type":"score","score":0}
	// yields an Answer whose Noul is 0 — a perfectly plausible "almost
	// certainly false" that the provider never said. Without this, a
	// mismatched kind is indistinguishable from a confident answer.
	for key, q := range questions {
		a, ok := answers[key]
		if !ok {
			return nil, Usage{}, fmt.Errorf("decision: provider returned no answer for question %q", key)
		}
		if a.Kind != q.Kind {
			return nil, Usage{}, fmt.Errorf(
				"decision: question %q is a %s but the provider answered with a %s",
				key, q.Kind, a.Kind)
		}
	}

	return answers, Usage{
		InputTokens:    resp.Usage.InputTokens,
		OutputTokens:   resp.Usage.OutputTokens,
		StateTruncated: truncated,
	}, nil
}

// toWire converts a question to its provider representation. Criteria shape
// is per kind: option->description for choice, an ordered array for score,
// and a true/false pair (omitted when both are empty) for noul.
func toWire(q Question) wireQuestion {
	w := wireQuestion{Type: string(q.Kind), Instructions: q.Instructions}
	switch q.Kind {
	case KindChoice:
		w.Criteria = q.Options
	case KindScore:
		w.Criteria = q.Levels
	case KindNoul:
		if q.TrueDesc != "" || q.FalseDesc != "" {
			w.Criteria = map[string]string{"true": q.TrueDesc, "false": q.FalseDesc}
		}
	}
	return w
}

// fromWire converts a provider answer, keeping only the members that the
// answered kind actually populates. Copying a stray Score onto a choice
// answer would hand a consumer a number the provider never meant.
func fromWire(w wireAnswer) Answer {
	a := Answer{Kind: Kind(w.Type)}
	switch a.Kind {
	case KindChoice:
		a.Choice = w.Choice
		a.Probabilities = w.Probabilities
		a.Confidence = w.Confidence
	case KindScore:
		a.Score = w.Score
		a.Legend = w.Legend
		a.Probabilities = w.Probabilities
		a.Confidence = w.Confidence
	case KindNoul:
		a.Noul = w.Noul
	default:
		// An unknown kind is carried through rather than dropped: the
		// caller can see the Kind it does not recognise, which is more
		// useful than an empty answer.
		a.Choice = w.Choice
		a.Score = w.Score
		a.Noul = w.Noul
		a.Legend = w.Legend
		a.Probabilities = w.Probabilities
		a.Confidence = w.Confidence
	}
	return a
}

// fitState shortens the state, if needed, so that state plus questions fit
// the measured token budget. It reports whether it truncated.
//
// Only a STRING state is truncated. Cutting a marshalled struct or map at a
// byte boundary would produce invalid JSON, and silently dropping fields
// would change what the questions are being asked ABOUT; a caller with a
// structured state that does not fit gets the provider's own refusal, which
// names the problem correctly.
func (p *typesafeProvider) fitState(state any, questions map[string]wireQuestion) (any, bool, error) {
	qb, err := json.Marshal(questions)
	if err != nil {
		return nil, false, fmt.Errorf("decision: marshal questions: %w", err)
	}
	budgetChars := int(float64(maxRequestTokens) * charsPerToken)
	// Reserve the questions' size plus a small allowance for the envelope
	// (model name, JSON punctuation).
	remaining := budgetChars - len(qb) - 512
	if remaining < 0 {
		return nil, false, fmt.Errorf("decision: questions alone exceed the %d-token budget", maxRequestTokens)
	}

	s, ok := state.(string)
	if !ok || marshalledLen(s) <= remaining {
		return state, false, nil
	}

	// The budget is measured against the MARSHALLED length, not the raw
	// string length, because encoding/json escapes <, > and & to six-byte
	// \uXXXX sequences: a state of 80,000 '<' characters is 80,000 raw bytes
	// and 480,000 on the wire (measured ratio exactly 6.00). Sizing the cut
	// on raw length let such a state sail past the budget check and come back
	// as max_tokens_exceeded, which is the very refusal truncation exists to
	// avoid.
	//
	// Expansion is not uniform, so one division cannot place the cut. Start
	// from the raw budget — marshalled length is never less than raw length,
	// so that is a valid upper bound — then shrink by the expansion ratio
	// actually observed at the current cut until it fits. `cut` strictly
	// decreases every iteration, so this terminates.
	cut := runeBoundaryAtOrBefore(s, min(remaining, len(s)))
	for cut > 0 {
		got := marshalledLen(s[:cut])
		if got <= remaining {
			return s[:cut], true, nil
		}
		// Scale by the observed ratio, with a margin so a pathological
		// distribution still converges, and always make progress.
		next := int(float64(cut) * (float64(remaining) / float64(got)) * 0.95)
		if next >= cut {
			next = cut - 1
		}
		cut = runeBoundaryAtOrBefore(s, next)
	}
	// Nothing of the state fits beside the questions.
	return "", true, nil
}

// marshalledLen is the number of bytes this string occupies as a JSON value,
// which is what the provider's token budget is actually charged against.
func marshalledLen(s string) int {
	b, err := json.Marshal(s)
	if err != nil {
		// json.Marshal cannot fail for a string; fall back to a
		// deliberately pessimistic bound rather than reporting a small one.
		return len(s) * 6
	}
	return len(b)
}

// runeBoundaryAtOrBefore returns the largest index <= i that begins a UTF-8
// sequence, so a cut there leaves valid UTF-8.
func runeBoundaryAtOrBefore(s string, i int) int {
	if i < 0 {
		return 0
	}
	// len(s) is a valid cut point (the whole string) and indexing s[len(s)]
	// would panic, so return early rather than clamping into the loop.
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !isRuneStart(s[i]) {
		i--
	}
	return i
}

// isRuneStart reports whether b can begin a UTF-8 sequence — i.e. it is not a
// continuation byte.
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// post sends the request, retrying transient failures with exponential
// backoff, and returns the decoded response.
func (p *typesafeProvider) post(ctx context.Context, body []byte) (*wireResponse, error) {
	backoff := initialBackoff
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			wait := backoff
			if ra := retryAfterOf(lastErr); ra > 0 {
				wait = ra
			}
			// The wait itself is cancellable; a cancelled context must not
			// have to outlast the backoff before anyone notices.
			if err := p.sleep(ctx, wait); err != nil {
				return nil, err
			}
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("decision: create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.apiKey)

		resp, err := p.client.Do(req)
		if err != nil {
			// A transport error may be transient; keep retrying, but never
			// past a cancelled context.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("decision: call provider: %w", err)
			continue
		}

		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		status := resp.StatusCode
		resp.Body.Close()

		switch {
		case status == http.StatusTooManyRequests || status == 529:
			// Retryable by the provider's own contract.
			lastErr = &transientError{APIError: apiErrorFrom(status, raw, readErr), retryAfter: retryAfter}
			continue
		case status >= 500:
			lastErr = &transientError{APIError: apiErrorFrom(status, raw, readErr)}
			continue
		case status >= 400:
			// Not retryable: the request itself is the problem. The body is
			// surfaced verbatim, and max_tokens_exceeded is matchable via
			// errors.Is without parsing prose. A body read that failed
			// partway is carried on the error rather than dropped, so a
			// caller can tell a missing ErrorType from an unread one.
			apiErr := apiErrorFrom(status, raw, readErr)
			return nil, &apiErr
		}

		if readErr != nil {
			return nil, fmt.Errorf("decision: read provider response: %w", readErr)
		}
		var out wireResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("decision: decode provider response: %w", err)
		}
		if len(out.Answers) == 0 {
			return nil, errors.New("decision: provider returned no answers")
		}
		return &out, nil
	}

	if lastErr == nil {
		lastErr = errors.New("decision: provider retries exhausted")
	}
	return nil, fmt.Errorf("decision: provider failed after %d attempts: %w", maxAttempts, lastErr)
}

// transientError wraps a retryable provider response so the retry loop can
// carry a Retry-After hint without widening APIError.
type transientError struct {
	APIError
	retryAfter time.Duration
}

// Unwrap exposes the embedded APIError to errors.As.
//
// Without it, exhausted retries produce an error chain of
// fmt.Errorf -> *transientError and stop: errors.As(err, **APIError) returns
// FALSE, because *transientError is a different type and value-embedding does
// not make it one. A caller that retried four times into a 429 wall would then
// have no way to read the status or the error type off the failure — the
// information is in the chain and unreachable.
func (t *transientError) Unwrap() error { return &t.APIError }

// apiErrorFrom builds an APIError from a response, keeping a partial-read
// failure rather than discarding it.
func apiErrorFrom(status int, raw []byte, readErr error) APIError {
	return APIError{
		StatusCode:  status,
		ErrorType:   errorTypeOf(raw),
		Body:        boundedBody(raw),
		BodyReadErr: readErr,
	}
}

func retryAfterOf(err error) time.Duration {
	var t *transientError
	if errors.As(err, &t) {
		return t.retryAfter
	}
	return 0
}

// parseRetryAfter reads a Retry-After header in delta-seconds form, bounded
// so a hostile or mistaken value cannot park a request indefinitely.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	d := time.Duration(secs) * time.Second
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

// errorTypeOf extracts detail.error_type from an error body, returning "" if
// the body is not that shape. It tolerates detail being a string or absent,
// since only the object form is measured and the rest is undocumented.
func errorTypeOf(raw []byte) string {
	var we wireError
	if err := json.Unmarshal(raw, &we); err != nil {
		return ""
	}
	m, ok := we.Detail.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["error_type"].(string)
	return s
}

func boundedBody(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > maxErrBodyLength {
		return s[:maxErrBodyLength] + "...(truncated)"
	}
	return s
}

// levelIndex parses a score probability/legend key ("0", "1", ...) into its
// integer index. A key that is not an integer sorts last rather than
// panicking, so an unexpected vocabulary degrades instead of crashing.
func levelIndex(k string) int {
	n, err := strconv.Atoi(k)
	if err != nil {
		return 1 << 30
	}
	return n
}
