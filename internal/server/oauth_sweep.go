package server

import (
	"log/slog"
	"time"

	"github.com/PerpetualSoftware/pad/internal/oauth"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// The OAuth expiry sweep's retention (BUG-3301). A row is deleted only when
// its requested_at is older than its token's lifespan plus
// oauthSweepGrace. A row's requested_at is its own issuance, and fosite
// sets its expiry to that instant plus the lifespan (rounded to the
// second), so the cutoff is always at least a day past the moment the
// token stopped being usable. No live chain, and no /oauth/token
// exchange racing the sweep, can need a row the sweep deletes: an
// exchange that could still succeed presents a token younger than its
// lifespan.
//
// Per table:
//
//   - refresh: the refresh lifespan (30 d by default). Rotation leaves each
//     old refresh row inactive, and fosite checks "inactive" BEFORE
//     "expired" (flow_refresh.go), so a replayed rotated token revokes the
//     whole family. Keeping the row for as long as its token could have
//     been valid keeps that detection for every token that could still
//     matter. Past it, a replay answers invalid_grant without revoking;
//     the token was expired anyway, so the attacker gains nothing.
//   - access: the access lifespan (1 h). An inactive and a missing access
//     row both reject, and fosite never revokes a family on access-token
//     reuse, so nothing reads one past its expiry.
//   - authorization codes: the refresh lifespan, not the code's 15 min. A
//     reused code revokes the chain it produced
//     (flow_authorize_code_token.go), and a grant keeps exactly one code
//     row, so keeping it for the chain's lifetime costs one row per grant.
//   - PKCE: the code lifespan. A successful exchange deletes the row; what
//     is left belongs to abandoned authorizations, useless once the code
//     has expired.
//
// Audit does not need these rows: mcp_audit_log records the chain's
// request_id, with no foreign key to a token row.
//
// The grace absorbs fosite's rounding and any clock skew, with margin; a
// day is far more than either needs, and costs one extra day of rows.
const oauthSweepGrace = 24 * time.Hour

// Batching (BUG-3301), measured on SQLite with 50,000 aged refresh rows
// while a second goroutine wrote an access-token row every 5 ms (the write
// /oauth/token does):
//
//   - One 500-row DELETE: p50 10.4 ms, p95 17.8 ms, max 19.0 ms.
//   - Back to back with no pause, the writer lost the lock to the next batch
//     again and again while SQLite's busy handler backed off: one write
//     waited 1.55 s (the whole sweep), and in a second run p99 was 753 ms.
//   - With oauthSweepPause between batches: write max 52 ms and 44 ms over
//     two runs (p99 40 ms, p50 about 7 ms), and the sweep took 5-6 s.
//
// The cap bounds a tick at 200 x 500 = 100,000 rows per table, about 6 s
// of wall time, far past the measured growth of 48 rows a day per active
// client; a backlog (an install that ran unswept for months) drains over a
// few hourly ticks.
const (
	oauthSweepBatchSize  = 500
	oauthSweepMaxBatches = 200
	oauthSweepPause      = 20 * time.Millisecond
)

// oauthSweepCutoffs derives the per-table cutoffs from the running OAuth
// server's lifespans, or the package defaults when OAuth is not
// constructed (an install that served OAuth before and no longer does
// still has rows to sweep).
func (s *Server) oauthSweepCutoffs(now time.Time) store.OAuthSweepCutoffs {
	l := oauth.DefaultLifespans()
	if s.oauthServer != nil {
		l = s.oauthServer.Lifespans()
	}
	before := func(lifespan time.Duration) time.Time {
		return now.Add(-(lifespan + oauthSweepGrace))
	}
	return store.OAuthSweepCutoffs{
		AccessTokens:       before(l.AccessToken),
		RefreshTokens:      before(l.RefreshToken),
		AuthorizationCodes: before(l.RefreshToken),
		PKCERequests:       before(l.AuthorizeCode),
	}
}

// sweepExpiredOAuthRows is the token reaper's OAuth step.
func (s *Server) sweepExpiredOAuthRows() {
	res, err := s.store.SweepExpiredOAuthRows(s.oauthSweepCutoffs(time.Now()), oauthSweepBatchSize, oauthSweepMaxBatches, oauthSweepPause)
	if err != nil {
		slog.Warn("token reaper: sweep expired OAuth rows failed", "error", err, "deleted", res.Total())
		return
	}
	if res.Total() == 0 {
		return
	}
	slog.Info("token reaper: swept expired OAuth rows",
		"access_tokens", res.AccessTokens, "refresh_tokens", res.RefreshTokens,
		"authorization_codes", res.AuthorizationCodes, "pkce_requests", res.PKCERequests,
		"capped", res.Capped)
}
