package metrics

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"

	"github.com/xarmian/pad/internal/events"

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
		"pad_db_open_connections":             false,
		"pad_db_idle_connections":             false,
		"pad_db_in_use_connections":           false,
		"pad_db_wait_count_total":             false,
		"pad_db_wait_duration_seconds_total":  false,
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

	// Check SSE gauge incremented
	gauge := getGaugeValue(t, m.SSEConnectionsActive.WithLabelValues("ws-1"))
	if gauge != 1 {
		t.Errorf("Expected SSE active connections = 1, got %v", gauge)
	}

	// Subscribe a second connection to same workspace
	ch2 := bus.Subscribe("ws-1")
	gauge = getGaugeValue(t, m.SSEConnectionsActive.WithLabelValues("ws-1"))
	if gauge != 2 {
		t.Errorf("Expected SSE active connections = 2, got %v", gauge)
	}

	// Subscribe to a different workspace
	ch3 := bus.Subscribe("ws-2")
	gauge2 := getGaugeValue(t, m.SSEConnectionsActive.WithLabelValues("ws-2"))
	if gauge2 != 1 {
		t.Errorf("Expected SSE active connections for ws-2 = 1, got %v", gauge2)
	}

	// Unsubscribe one from ws-1
	bus.Unsubscribe(ch)
	gauge = getGaugeValue(t, m.SSEConnectionsActive.WithLabelValues("ws-1"))
	if gauge != 1 {
		t.Errorf("Expected SSE active connections = 1 after unsubscribe, got %v", gauge)
	}

	// Unsubscribe remaining
	bus.Unsubscribe(ch2)
	bus.Unsubscribe(ch3)
	gauge = getGaugeValue(t, m.SSEConnectionsActive.WithLabelValues("ws-1"))
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
