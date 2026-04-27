package server

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/xarmian/pad/internal/billing"
)

// AdminBillingStatsResponse is the JSON shape returned by
// GET /api/v1/admin/billing-stats. It merges Stripe-derived metrics from
// pad-cloud (active_subscriptions, MRR, ARR, churn, cancellations) with
// metrics computed from pad's local users table (customers_by_plan,
// new_signups_30d).
//
// CloudUnreachable is true when the pad-cloud sidecar errored out (transport
// failure, 5xx) — in that case the Stripe-derived fields are zero and the
// UI renders a "sidecar unreachable" banner. CloudUnreachable=false +
// StripeConfigured=false means the sidecar is reachable but no Stripe
// account is wired up yet, which the UI surfaces as a placeholder.
type AdminBillingStatsResponse struct {
	StripeConfigured    bool           `json:"stripe_configured"`
	CloudUnreachable    bool           `json:"cloud_unreachable"`
	CustomersByPlan     map[string]int `json:"customers_by_plan"`
	NewSignups30d       int            `json:"new_signups_30d"`
	ActiveSubscriptions int            `json:"active_subscriptions"`
	MRRCents            int64          `json:"mrr_cents"`
	ARRCents            int64          `json:"arr_cents"`
	Currency            string         `json:"currency"`
	ChurnRate30d        float64        `json:"churn_rate_30d"`
	Cancelled30d        int            `json:"cancelled_30d"`
	ComputedAt          time.Time      `json:"computed_at"`
	CacheAgeSeconds     int64          `json:"cache_age_seconds"`
}

// handleAdminBillingStats serves GET /api/v1/admin/billing-stats.
//
// Auth: requireCloudMode (the route group already gates on this) +
// requireAdmin (this handler enforces). Returns 403 to non-admin users.
//
// Local fields (customers_by_plan, new_signups_30d) come straight from the
// users table — same source as /api/v1/admin/stats. Remote fields come from
// pad-cloud's /admin/metrics/billing endpoint via the CloudSidecar interface.
//
// Degradation: if the sidecar is not wired (cloudSidecar == nil) or returns
// an error, the response carries cloud_unreachable=true with the
// Stripe-derived fields zeroed. The endpoint always returns 200 — the UI
// distinguishes "sidecar down" from "Stripe not configured" via the two
// boolean flags rather than HTTP status, so a transient sidecar blip does
// not blank the local-data half of the dashboard.
func (s *Server) handleAdminBillingStats(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	resp := AdminBillingStatsResponse{
		Currency: "usd",
	}

	// Local fields: two scalar SQL queries via store.CountBillingAggregates
	// (per-plan COUNT(*) GROUP BY + a single COUNT(*) for new pro signups).
	// Avoids the ListUsers + per-row TOTP decrypt that would otherwise burn
	// CPU + bandwidth on every admin refresh as the user table grows.
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	agg, err := s.store.CountBillingAggregates(cutoff)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	resp.CustomersByPlan = agg.CustomersByPlan
	resp.NewSignups30d = agg.NewProSignups

	// Remote fields. cloudSidecar is nil when the operator is in cloud mode
	// but hasn't wired PAD_CLOUD_SIDECAR_URL — treat that as unreachable.
	if s.cloudSidecar == nil {
		resp.CloudUnreachable = true
		resp.ComputedAt = time.Now().UTC()
		writeJSON(w, http.StatusOK, resp)
		return
	}

	metrics, err := s.cloudSidecar.GetBillingMetrics()
	if err != nil {
		// Either a transport failure or a non-200 (auth / upstream Stripe).
		// Both degrade to local-only. Log enough to debug a misconfigured
		// pair without leaking internal infra to the operator's browser.
		var sidecarErr *billing.SidecarError
		if errors.As(err, &sidecarErr) {
			slog.Warn("admin/billing-stats: pad-cloud returned non-200, degrading to local-only",
				"status", sidecarErr.Status, "body", sidecarErr.Body)
		} else {
			slog.Warn("admin/billing-stats: pad-cloud unreachable, degrading to local-only",
				"error", err)
		}
		resp.CloudUnreachable = true
		resp.ComputedAt = time.Now().UTC()
		writeJSON(w, http.StatusOK, resp)
		return
	}

	resp.StripeConfigured = metrics.StripeConfigured
	resp.ActiveSubscriptions = metrics.ActiveSubscriptions
	resp.MRRCents = metrics.MRRCents
	resp.ARRCents = metrics.ARRCents
	if metrics.Currency != "" {
		resp.Currency = metrics.Currency
	}
	resp.ChurnRate30d = metrics.ChurnRate30d
	resp.Cancelled30d = metrics.Cancelled30d
	resp.ComputedAt = metrics.ComputedAt
	resp.CacheAgeSeconds = metrics.CacheAgeSeconds

	writeJSON(w, http.StatusOK, resp)
}
