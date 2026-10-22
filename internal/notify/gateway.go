// Package notify is the client for the push gateway (pad-cloud's
// POST /internal/push/send, contract v1 on TASK-3939). It knows nothing about
// the store: the dispatcher hands it fully built messages.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MaxBatch is the gateway's per-request message limit.
const MaxBatch = 100

// maxResponseBytes caps what is read from a gateway response. A 100-message
// result is a few KB; anything near this is not the gateway.
const maxResponseBytes = 1 << 20

// Per-message statuses.
const (
	StatusSent         = "sent"
	StatusInvalidToken = "invalid_token"
	StatusRetry        = "retry"
	StatusRejected     = "rejected"
)

var (
	// ErrBadRequest means the gateway refused the request itself (400), or
	// this client refused to build an invalid one. A pad-side bug: the batch
	// is skipped, not retried.
	ErrBadRequest = errors.New("push gateway: bad request")
	// ErrUnavailable means no usable answer: a transport error or any non-200
	// other than 400 (403 wrong secret, 503 no FCM credentials, a redirect).
	// Every message in the batch is a retry.
	ErrUnavailable = errors.New("push gateway: unavailable")
)

// Message is one push to one device.
type Message struct {
	ID       string `json:"id"`
	Platform string `json:"platform"`
	Token    string `json:"token"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	Data     Data   `json:"data"`
}

// Result is the gateway's verdict on one message.
type Result struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// Gateway sends batches to the push gateway.
type Gateway struct {
	URL    string
	Secret string
	HTTP   *http.Client
}

// SendTimeout is the HTTP client's whole-request timeout, above the gateway's
// own 20s deadline. The dispatcher's claim lease must exceed it by a margin
// (internal/server/push_dispatch.go).
const SendTimeout = 25 * time.Second

// NewGateway returns a Gateway with a client that does not follow redirects:
// the secret rides in a header, and a redirect would carry it to wherever the
// answer points.
func NewGateway(url, secret string) *Gateway {
	return &Gateway{
		URL: url, Secret: secret,
		HTTP: &http.Client{
			Timeout: SendTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Send posts msgs (1 to MaxBatch, unique ids) and returns one Result per
// message, in order. On a 200 whose results do not line up with the request
// (wrong length, a wrong or reordered id, unreadable JSON) every message
// comes back retry, since nothing in the answer can be trusted to belong to a
// device; an unknown status is a retry for that message. Otherwise the error
// is ErrBadRequest or ErrUnavailable. Neither the secret nor a token ever
// appears in an error: transport errors carry the URL, so they are replaced
// by the sentinel.
func (g *Gateway) Send(ctx context.Context, msgs []Message) ([]Result, error) {
	if len(msgs) == 0 || len(msgs) > MaxBatch {
		return nil, fmt.Errorf("%w: %d messages", ErrBadRequest, len(msgs))
	}
	seen := make(map[string]struct{}, len(msgs))
	for _, m := range msgs {
		if _, dup := seen[m.ID]; dup {
			return nil, fmt.Errorf("%w: duplicate message id", ErrBadRequest)
		}
		seen[m.ID] = struct{}{}
	}
	payload, err := json.Marshal(struct {
		Messages []Message `json:"messages"`
	}{msgs})
	if err != nil {
		return nil, fmt.Errorf("%w: encode", ErrBadRequest)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: build request", ErrUnavailable)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cloud-Secret", g.Secret)
	client := g.HTTP
	if client == nil {
		client = NewGateway(g.URL, g.Secret).HTTP
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
		}
		return nil, fmt.Errorf("%w: transport error", ErrUnavailable)
	}
	// Drain what is left (bounded) so the connection can be reused (TASK-3939 r1-H3).
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		resp.Body.Close()
	}()
	switch {
	case resp.StatusCode == http.StatusBadRequest:
		return nil, ErrBadRequest
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read response", ErrUnavailable)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("%w: response too large", ErrUnavailable)
	}
	var out struct {
		Results []Result `json:"results"`
	}
	if json.Unmarshal(body, &out) != nil {
		return allRetry(msgs), nil
	}
	return validateResults(msgs, out.Results), nil
}

// validateResults pins the answer to the request: the same length and the
// same ids in the same order, or the whole batch is retry. A status outside
// the four known ones is a retry for its message.
func validateResults(msgs []Message, got []Result) []Result {
	if len(got) != len(msgs) {
		return allRetry(msgs)
	}
	out := make([]Result, len(got))
	for i, r := range got {
		if r.ID != msgs[i].ID {
			return allRetry(msgs)
		}
		switch r.Status {
		case StatusSent, StatusInvalidToken, StatusRetry, StatusRejected:
			out[i] = r
		default:
			out[i] = Result{ID: r.ID, Status: StatusRetry}
		}
	}
	return out
}

func allRetry(msgs []Message) []Result {
	out := make([]Result, len(msgs))
	for i, m := range msgs {
		out[i] = Result{ID: m.ID, Status: StatusRetry}
	}
	return out
}
