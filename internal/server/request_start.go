package server

import (
	"context"
	"github.com/PerpetualSoftware/pad/internal/store"
	"net/http"
	"time"
)

type requestStartKey struct{}

// withRequestStart records when the request reached this process, as the first
// middleware, so a deadline can be counted from arrival rather than from the
// moment a handler happens to start its work (TASK-4278).
func withRequestStart(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), requestStartKey{}, time.Now())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requestStartFrom returns the arrival time withRequestStart recorded, or now
// for a request that did not pass through it (a handler called directly).
func requestStartFrom(ctx context.Context) time.Time {
	if t, ok := ctx.Value(requestStartKey{}).(time.Time); ok {
		return t
	}
	return time.Now()
}

// Account deletion's time budget (TASK-4278). The whole request, the Stripe
// cancel before the transaction included, must finish or give a clean
// retryable answer before any hop in front of the server gives up on it:
//
//   - Pad Cloud's inner nginx: proxy_read_timeout 60s on `location /`
//     (pad-cloud nginx-router.conf:267);
//   - this server's WriteTimeout, which is unset (0) today.
//
// accountDeleteTxBudget is counted from request arrival. Inside it the
// transaction's statement and lock timeouts are min(45s, time left), 45s being
// the most that fits under the 60s hop with margin and 2s over the 43s a
// workspace purge was measured to hold the rows a deletion needs.
const (
	accountDeleteTxBudget       = 50 * time.Second
	accountDeleteStmtTimeout    = store.AccountDeleteTxTimeout
	accountDeleteLockTimeout    = store.AccountDeleteTxTimeout
	accountDeletePostCommitSlop = 5 * time.Second
)
