package metrics

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"

	"github.com/PerpetualSoftware/pad/internal/events"

	_ "modernc.org/sqlite"
)

func TestNew(t *testing.T) {
	m := New()
	if m.Registry == nil {
		t.Fatal("Registry should not be nil")
	}
	if m.HTTPRequestsTotal == nil {
		t.Fatal("HTTPRequestsTotal should not be nil")
	}
	if m.HTTPRequestDuration == nil {
		t.Fatal("HTTPRequestDuration should not be nil")
	}
	if m.HTTPResponseSize == nil {
		t.Fatal("HTTPResponseSize should not be nil")
	}
	if m.SSEConnectionsActive == nil {
		t.Fatal("SSEConnectionsActive should not be nil")
	}
	if m.EventBusPublishTotal == nil {
		t.Fatal("EventBusPublishTotal should not be nil")
	}
	if m.EventBusSubscribers == nil {
		t.Fatal("EventBusSubscribers should not be nil")
	}

	// TASK-961: MCP / OAuth metrics constructed alongside the others.
	if m.MCPToolCallsTotal == nil {
		t.Fatal("MCPToolCallsTotal should not be nil")
	}
	if m.MCPToolCallDuration == nil {
		t.Fatal("MCPToolCallDuration should not be nil")
	}
	if m.MCPAuthzDenialsTotal == nil {
		t.Fatal("MCPAuthzDenialsTotal should not be nil")
	}
	if m.MCPActiveSessions == nil {
		t.Fatal("MCPActiveSessions should not be nil")
	}
	if m.OAuthFlowsTotal == nil {
		t.Fatal("OAuthFlowsTotal should not be nil")
	}
	if m.OAuthFlowDuration == nil {
		t.Fatal("OAuthFlowDuration should not be nil")
	}
	if m.OAuthTokenRevocationsTotal == nil {
		t.Fatal("OAuthTokenRevocationsTotal should not be nil")
	}
	if m.OAuthTokenTTLSeconds == nil {
		t.Fatal("OAuthTokenTTLSeconds should not be nil")
	}

	// Verify all metrics can be gathered without error
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}

	// Should have Go runtime + process metrics
	found := false
	for _, f := range families {
		if strings.HasPrefix(f.GetName(), "go_") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("Expected Go runtime metrics to be registered")
	}
}

// TestMCPMetrics_IncrementsAndHistogramBuckets exercises the
// MCP-side metrics surface end-to-end: each metric must accept
// observations and return them through Gather. Verifies the
// histogram bucket choices are sensible (a 100ms call should land
// under the 250ms bucket).
func TestMCPMetrics_IncrementsAndHistogramBuckets(t *testing.T) {
	m := New()

	m.MCPToolCallsTotal.WithLabelValues("user-1", "pad_item", "ok").Inc()
	m.MCPToolCallsTotal.WithLabelValues("user-1", "pad_item", "ok").Inc()
	m.MCPToolCallsTotal.WithLabelValues("user-2", "pad_search", "denied").Inc()

	m.MCPToolCallDuration.WithLabelValues("pad_item").Observe(0.1)
	m.MCPToolCallDuration.WithLabelValues("pad_item").Observe(2.0)

	m.MCPAuthzDenialsTotal.WithLabelValues("audience_mismatch").Inc()
	m.MCPAuthzDenialsTotal.WithLabelValues("rate_limited").Inc()

	m.MCPActiveSessions.Inc()
	m.MCPActiveSessions.Inc()
	m.MCPActiveSessions.Dec()

	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}

	expected := map[string]float64{
		// counter sums
		"pad_mcp_tool_calls_total":           3,
		"pad_mcp_authz_denials_total":        2,
		"pad_mcp_active_sessions":            1,
		"pad_mcp_tool_call_duration_seconds": 2, // sample count
	}
	for _, f := range families {
		want, ok := expected[f.GetName()]
		if !ok {
			continue
		}
		switch f.GetType() {
		case io_prometheus_client.MetricType_COUNTER:
			var sum float64
			for _, m := range f.GetMetric() {
				sum += m.GetCounter().GetValue()
			}
			if sum != want {
				t.Errorf("counter %s: got %v, want %v", f.GetName(), sum, want)
			}
		case io_prometheus_client.MetricType_GAUGE:
			var sum float64
			for _, m := range f.GetMetric() {
				sum += m.GetGauge().GetValue()
			}
			if sum != want {
				t.Errorf("gauge %s: got %v, want %v", f.GetName(), sum, want)
			}
		case io_prometheus_client.MetricType_HISTOGRAM:
			var samples uint64
			for _, m := range f.GetMetric() {
				samples += m.GetHistogram().GetSampleCount()
			}
			if float64(samples) != want {
				t.Errorf("histogram %s sample count: got %v, want %v", f.GetName(), samples, want)
			}
		}
		delete(expected, f.GetName())
	}
	for name := range expected {
		t.Errorf("metric %s not found in gathered families", name)
	}

	// Bucket sanity: the 0.1s observation must land in the 0.25s bucket
	// (or earlier). Walk the histogram metric to confirm.
	for _, f := range families {
		if f.GetName() != "pad_mcp_tool_call_duration_seconds" {
			continue
		}
		for _, mm := range f.GetMetric() {
			h := mm.GetHistogram()
			var hit bool
			for _, b := range h.GetBucket() {
				if b.GetUpperBound() == 0.25 && b.GetCumulativeCount() >= 1 {
					hit = true
					break
				}
			}
			if !hit {
				t.Errorf("histogram bucket 0.25s should have observed the 0.1s sample (buckets: %+v)", h.GetBucket())
			}
		}
	}
}

// TestOAuthMetrics_IncrementsAndHistogramBuckets mirrors the MCP test
// for the OAuth-flow surface. Same shape, different metric set.
func TestOAuthMetrics_IncrementsAndHistogramBuckets(t *testing.T) {
	m := New()

	m.OAuthFlowsTotal.WithLabelValues("started").Inc()
	m.OAuthFlowsTotal.WithLabelValues("completed").Inc()
	m.OAuthFlowsTotal.WithLabelValues("abandoned").Inc()

	m.OAuthFlowDuration.WithLabelValues("authorize").Observe(0.05)
	m.OAuthFlowDuration.WithLabelValues("token").Observe(0.2)

	m.OAuthTokenRevocationsTotal.WithLabelValues("user_initiated").Inc()
	m.OAuthTokenRevocationsTotal.WithLabelValues("rotated").Inc()

	m.OAuthTokenTTLSeconds.Observe(3600) // 1h — natural-expiry
	m.OAuthTokenTTLSeconds.Observe(45)   // sub-minute → caught by the 60s bucket

	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}

	expected := map[string]float64{
		"pad_oauth_flows_total":             3,
		"pad_oauth_token_revocations_total": 2,
		"pad_oauth_flow_duration_seconds":   2, // sample count
		"pad_oauth_token_ttl_seconds":       2, // sample count
	}
	for _, f := range families {
		want, ok := expected[f.GetName()]
		if !ok {
			continue
		}
		switch f.GetType() {
		case io_prometheus_client.MetricType_COUNTER:
			var sum float64
			for _, mm := range f.GetMetric() {
				sum += mm.GetCounter().GetValue()
			}
			if sum != want {
				t.Errorf("counter %s: got %v, want %v", f.GetName(), sum, want)
			}
		case io_prometheus_client.MetricType_HISTOGRAM:
			var samples uint64
			for _, mm := range f.GetMetric() {
				samples += mm.GetHistogram().GetSampleCount()
			}
			if float64(samples) != want {
				t.Errorf("histogram %s sample count: got %v, want %v", f.GetName(), samples, want)
			}
		}
		delete(expected, f.GetName())
	}
	for name := range expected {
		t.Errorf("metric %s not found in gathered families", name)
	}

	// TTL bucket sanity: 60s bucket should have observed the 45s sample.
	for _, f := range families {
		if f.GetName() != "pad_oauth_token_ttl_seconds" {
			continue
		}
		for _, mm := range f.GetMetric() {
			h := mm.GetHistogram()
			var hit bool
			for _, b := range h.GetBucket() {
				if b.GetUpperBound() == 60 && b.GetCumulativeCount() >= 1 {
					hit = true
					break
				}
			}
			if !hit {
				t.Errorf("histogram bucket 60s should have observed the 45s sample (buckets: %+v)", h.GetBucket())
			}
		}
	}
}

// TestRegisterOAuthActiveTokensCollector verifies the callback-driven
// gauge produces a fresh value on each Gather and survives provider
// errors without panicking the registry.
func TestRegisterOAuthActiveTokensCollector(t *testing.T) {
	m := New()

	var count int64 = 7
	m.RegisterOAuthActiveTokensCollector(func() (int64, error) {
		return count, nil
	})

	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}
	got := findGaugeValue(families, "pad_oauth_active_tokens")
	if got != 7 {
		t.Errorf("first scrape: got %v, want 7", got)
	}

	// Mutate the source — next scrape should reflect it.
	count = 12
	families, err = m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}
	got = findGaugeValue(families, "pad_oauth_active_tokens")
	if got != 12 {
		t.Errorf("second scrape: got %v, want 12", got)
	}

	// Nil provider is a no-op (defensive: cmd/pad shouldn't pass nil
	// but a future caller might).
	m2 := New()
	m2.RegisterOAuthActiveTokensCollector(nil)
	if _, err := m2.Registry.Gather(); err != nil {
		t.Fatalf("nil-provider Gather should not error: %v", err)
	}
}

// TestRegisterOAuthActiveTokensCollector_ErrorIsScrapeSafe pins the
// behavior Codex flagged on PR review: a provider error must NOT
// fail Registry.Gather() (which would break the entire /metrics
// scrape via promhttp's default handler). The contract is "skip
// the sample, log the error, let every other metric through."
func TestRegisterOAuthActiveTokensCollector_ErrorIsScrapeSafe(t *testing.T) {
	m := New()
	m.RegisterOAuthActiveTokensCollector(func() (int64, error) {
		return 0, errors.New("simulated DB outage")
	})

	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather must NOT propagate provider errors (would fail entire scrape): %v", err)
	}

	// The active-tokens series should be absent on this scrape.
	if got := findGaugeValue(families, "pad_oauth_active_tokens"); got != -1 {
		t.Errorf("on provider error: expected metric absent (sentinel -1), got %v", got)
	}

	// Other metrics (e.g. Go runtime) MUST still be present — this is
	// the whole point of skipping rather than NewInvalidMetric'ing.
	var sawGoMetric bool
	for _, f := range families {
		if strings.HasPrefix(f.GetName(), "go_") {
			sawGoMetric = true
			break
		}
	}
	if !sawGoMetric {
		t.Error("provider error should not suppress unrelated metrics")
	}
}

// findGaugeValue is a small lookup helper for the OAuth active-tokens
// test. Returns -1 when the metric isn't present so callers can spot
// "missing" distinct from "zero."
func findGaugeValue(families []*io_prometheus_client.MetricFamily, name string) float64 {
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			return m.GetGauge().GetValue()
		}
	}
	return -1
}

func TestRegisterDBCollector(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Failed to open in-memory SQLite: %v", err)
	}
	defer db.Close()

	m := New()
	m.RegisterDBCollector(db)

	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}

	expected := map[string]bool{
		"pad_db_open_connections":            false,
		"pad_db_idle_connections":            false,
		"pad_db_in_use_connections":          false,
		"pad_db_wait_count_total":            false,
		"pad_db_wait_duration_seconds_total": false,
	}

	for _, f := range families {
		if _, ok := expected[f.GetName()]; ok {
			expected[f.GetName()] = true
		}
	}

	for name, found := range expected {
		if !found {
			t.Errorf("Expected metric %q not found in gathered metrics", name)
		}
	}
}

func TestInstrumentedBus_SubscribeUnsubscribe(t *testing.T) {
	inner := events.New()
	m := New()
	bus := NewInstrumentedBus(inner, m)

	// Subscribe to a workspace
	ch := bus.Subscribe("ws-1")
	if ch == nil {
		t.Fatal("Subscribe should return a channel")
	}

	// Check SSE gauge incremented (single total gauge, not per-workspace)
	gauge := getGaugeValue(t, *m.SSEConnectionsActive)
	if gauge != 1 {
		t.Errorf("Expected SSE active connections = 1, got %v", gauge)
	}

	// Subscribe a second connection to same workspace
	ch2 := bus.Subscribe("ws-1")
	gauge = getGaugeValue(t, *m.SSEConnectionsActive)
	if gauge != 2 {
		t.Errorf("Expected SSE active connections = 2, got %v", gauge)
	}

	// Subscribe to a different workspace
	ch3 := bus.Subscribe("ws-2")
	gauge = getGaugeValue(t, *m.SSEConnectionsActive)
	if gauge != 3 {
		t.Errorf("Expected SSE active connections = 3, got %v", gauge)
	}

	// Unsubscribe one from ws-1
	bus.Unsubscribe(ch)
	gauge = getGaugeValue(t, *m.SSEConnectionsActive)
	if gauge != 2 {
		t.Errorf("Expected SSE active connections = 2 after unsubscribe, got %v", gauge)
	}

	// Unsubscribe remaining
	bus.Unsubscribe(ch2)
	bus.Unsubscribe(ch3)
	gauge = getGaugeValue(t, *m.SSEConnectionsActive)
	if gauge != 0 {
		t.Errorf("Expected SSE active connections = 0 after all unsubscribed, got %v", gauge)
	}
}

func TestInstrumentedBus_Publish(t *testing.T) {
	inner := events.New()
	m := New()
	bus := NewInstrumentedBus(inner, m)

	// Subscribe so we can publish
	ch := bus.Subscribe("ws-1")
	defer bus.Unsubscribe(ch)

	bus.Publish(events.Event{Type: "test", WorkspaceID: "ws-1"})
	bus.Publish(events.Event{Type: "test", WorkspaceID: "ws-1"})

	var metric io_prometheus_client.Metric
	if err := (*m.EventBusPublishTotal).Write(&metric); err != nil {
		t.Fatalf("Failed to write publish metric: %v", err)
	}
	if got := metric.GetCounter().GetValue(); got != 2 {
		t.Errorf("Expected publish count = 2, got %v", got)
	}
}

func TestInstrumentedBus_SubscriberCount(t *testing.T) {
	inner := events.New()
	m := New()
	bus := NewInstrumentedBus(inner, m)

	if bus.SubscriberCount() != 0 {
		t.Errorf("Expected 0 subscribers initially")
	}

	ch := bus.Subscribe("ws-1")
	if bus.SubscriberCount() != 1 {
		t.Errorf("Expected 1 subscriber after subscribe")
	}

	bus.Unsubscribe(ch)
	if bus.SubscriberCount() != 0 {
		t.Errorf("Expected 0 subscribers after unsubscribe")
	}
}

// getGaugeValue extracts the current value from a Prometheus Gauge.
func getGaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var metric io_prometheus_client.Metric
	if err := g.Write(&metric); err != nil {
		t.Fatalf("Failed to write gauge metric: %v", err)
	}
	return metric.GetGauge().GetValue()
}
