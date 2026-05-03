package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	io_prometheus_client "github.com/prometheus/client_model/go"

	"github.com/PerpetualSoftware/pad/internal/metrics"
)

// Tests for the MCP/OAuth metric emit helpers wired in PLAN-943 TASK-961.
// We unit-test the helper methods rather than the full middleware
// pipelines because:
//
//   - The audit middleware needs a started writer + store; covering the
//     metric side via the helper avoids re-importing all that test scaffolding.
//   - The helpers ARE the metric contract — every emit path goes through
//     them, so verifying their behaviour pins the visible metrics shape.

// TestRecordMCPCallMetrics_HappyPath verifies the standard tool-call
// fan-out: counter +1 per call, latency histogram observation, no
// session-gauge change for ordinary tool calls.
func TestRecordMCPCallMetrics_HappyPath(t *testing.T) {
	s := &Server{metrics: metrics.New()}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)

	s.recordMCPCallMetrics("pad_item", "ok", "user-1", 50*time.Millisecond, r, http.StatusOK)
	s.recordMCPCallMetrics("pad_item", "ok", "user-1", 100*time.Millisecond, r, http.StatusOK)

	got := counterValue(t, s.metrics.MCPToolCallsTotal.WithLabelValues("user-1", "pad_item", "ok"))
	if got != 2 {
		t.Errorf("MCPToolCallsTotal: got %v, want 2", got)
	}

	// Histogram should have 2 observations under the pad_item label.
	families, _ := s.metrics.Registry.Gather()
	var samples uint64
	for _, f := range families {
		if f.GetName() != "pad_mcp_tool_call_duration_seconds" {
			continue
		}
		for _, mm := range f.GetMetric() {
			samples += mm.GetHistogram().GetSampleCount()
		}
	}
	if samples != 2 {
		t.Errorf("MCPToolCallDuration sample count: got %d, want 2", samples)
	}

	// Active sessions gauge unchanged (tool != initialize, method !=
	// DELETE).
	if got := gaugeValueOrZero(t, s.metrics.MCPActiveSessions); got != 0 {
		t.Errorf("MCPActiveSessions: got %v, want 0", got)
	}
}

// TestRecordMCPCallMetrics_DoesNotTouchSessionGauge pins the
// TASK-1120 invariant: recordMCPCallMetrics must NOT mutate the
// active-sessions gauge directly. The gauge is now owned by the
// session tracker (middleware_mcp_session.go) keyed on
// Mcp-Session-Id, so the audit-side helper deliberately drops the
// initialize/DELETE accounting it used to do. If a future refactor
// re-adds gauge updates here, the resulting double-counting would
// silently corrupt the dashboard — this test is the canary.
func TestRecordMCPCallMetrics_DoesNotTouchSessionGauge(t *testing.T) {
	s := &Server{metrics: metrics.New()}
	post := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	del := httptest.NewRequest(http.MethodDelete, "/mcp", nil)

	s.recordMCPCallMetrics("initialize", "ok", "u", time.Millisecond, post, http.StatusOK)
	s.recordMCPCallMetrics("initialize", "ok", "u", time.Millisecond, post, http.StatusOK)
	s.recordMCPCallMetrics("(unknown)", "ok", "u", time.Millisecond, del, http.StatusOK)
	s.recordMCPCallMetrics("initialize", "error", "u", time.Millisecond, post, http.StatusInternalServerError)

	if got := gaugeValueOrZero(t, s.metrics.MCPActiveSessions); got != 0 {
		t.Errorf("recordMCPCallMetrics must not touch MCPActiveSessions; got %v", got)
	}
}

// TestRecordMCPCallMetrics_NilMetrics verifies the helper is a clean
// no-op when metrics aren't wired (selfhost / tests that don't build
// a registry). No panic is the only assertion that matters.
func TestRecordMCPCallMetrics_NilMetrics(t *testing.T) {
	s := &Server{metrics: nil}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	s.recordMCPCallMetrics("pad_item", "ok", "u", time.Second, r, http.StatusOK)
}

// TestRecordMCPTierMismatch covers the dispatcher-side observer added
// in TASK-1119: every call increments
// pad_mcp_authz_denials_total{reason="tier_mismatch"} unconditionally
// (no MCP-origin gate — the dispatcher is by construction MCP-only).
// Also verifies nil-metrics safety.
func TestRecordMCPTierMismatch(t *testing.T) {
	s := &Server{metrics: metrics.New()}

	s.RecordMCPTierMismatch(http.MethodPost, "/api/v1/workspaces/x/items")
	s.RecordMCPTierMismatch(http.MethodDelete, "/api/v1/workspaces/x/items/y")

	got := counterValue(t, s.metrics.MCPAuthzDenialsTotal.WithLabelValues("tier_mismatch"))
	if got != 2 {
		t.Errorf("MCPAuthzDenialsTotal{tier_mismatch}: got %v, want 2", got)
	}

	// Other reasons untouched — sanity check the wiring didn't fan out.
	for _, r := range []string{"audience_mismatch", "rate_limited", "workspace_not_in_allowlist", "not_a_member"} {
		if got := counterValue(t, s.metrics.MCPAuthzDenialsTotal.WithLabelValues(r)); got != 0 {
			t.Errorf("MCPAuthzDenialsTotal{%s}: got %v, want 0 (untouched)", r, got)
		}
	}

	// Nil metrics → no panic.
	s2 := &Server{metrics: nil}
	s2.RecordMCPTierMismatch(http.MethodPost, "/whatever")
}

// TestRecordMCPAuthzDenial_GatedOnMCPOrigin verifies the discriminator:
// the counter only increments when the request context carries an MCP
// token identity (set by MCPBearerAuth). Non-MCP requests must be
// silently skipped so /api/v1 traffic doesn't pollute the MCP-specific
// counter.
func TestRecordMCPAuthzDenial_GatedOnMCPOrigin(t *testing.T) {
	s := &Server{metrics: metrics.New()}

	// Request without MCP identity → no-op.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/foo/items", nil)
	s.recordMCPAuthzDenial(r, "not_a_member")
	if got := counterValue(t, s.metrics.MCPAuthzDenialsTotal.WithLabelValues("not_a_member")); got != 0 {
		t.Errorf("non-MCP request must not increment counter, got %v", got)
	}

	// Request with MCP identity → counter increments.
	ctx := WithMCPTokenIdentity(context.Background(), "oauth", "req-123")
	r = httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/foo/items", nil).WithContext(ctx)
	s.recordMCPAuthzDenial(r, "not_a_member")
	if got := counterValue(t, s.metrics.MCPAuthzDenialsTotal.WithLabelValues("not_a_member")); got != 1 {
		t.Errorf("MCP-origin request: got %v, want 1", got)
	}
}

// TestObserveOAuthFlowDuration covers the OAuth handler-side
// duration helper. Verifies the histogram captures the elapsed time
// under the right stage label.
func TestObserveOAuthFlowDuration(t *testing.T) {
	s := &Server{metrics: metrics.New()}

	// Sleep is too racy for CI; just observe a fixed-past start.
	start := time.Now().Add(-100 * time.Millisecond)
	s.observeOAuthFlowDuration("authorize", start)

	families, _ := s.metrics.Registry.Gather()
	var samples uint64
	var found bool
	for _, f := range families {
		if f.GetName() != "pad_oauth_flow_duration_seconds" {
			continue
		}
		for _, mm := range f.GetMetric() {
			// Locate the metric for our stage label.
			for _, lp := range mm.GetLabel() {
				if lp.GetName() == "stage" && lp.GetValue() == "authorize" {
					samples += mm.GetHistogram().GetSampleCount()
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("OAuthFlowDuration histogram missing stage=authorize series")
	}
	if samples != 1 {
		t.Errorf("OAuthFlowDuration{authorize} sample count: got %d, want 1", samples)
	}
}

// TestRecordOAuthFlow covers the OAuth-flow stage counter.
func TestRecordOAuthFlow(t *testing.T) {
	s := &Server{metrics: metrics.New()}
	s.recordOAuthFlow("started")
	s.recordOAuthFlow("started")
	s.recordOAuthFlow("completed")
	s.recordOAuthFlow("abandoned")
	s.recordOAuthFlow("failed")

	cases := map[string]float64{
		"started":   2,
		"completed": 1,
		"abandoned": 1,
		"failed":    1,
	}
	for stage, want := range cases {
		got := counterValue(t, s.metrics.OAuthFlowsTotal.WithLabelValues(stage))
		if got != want {
			t.Errorf("OAuthFlowsTotal{%s}: got %v, want %v", stage, got, want)
		}
	}
}

// counterValue is a tiny helper that pulls the float value out of a
// Prometheus counter.
func counterValue(t *testing.T, c interface {
	Write(*io_prometheus_client.Metric) error
}) float64 {
	t.Helper()
	var m io_prometheus_client.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("counter Write: %v", err)
	}
	return m.GetCounter().GetValue()
}

// gaugeValueOrZero pulls the gauge value or returns 0 when the
// underlying metric hasn't been touched yet.
func gaugeValueOrZero(t *testing.T, g interface {
	Write(*io_prometheus_client.Metric) error
}) float64 {
	t.Helper()
	var m io_prometheus_client.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("gauge Write: %v", err)
	}
	return m.GetGauge().GetValue()
}
