package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	io_prometheus_client "github.com/prometheus/client_model/go"

	"github.com/xarmian/pad/internal/metrics"
)

func TestMetricsMiddleware_RecordsRequestMetrics(t *testing.T) {
	m := metrics.New()

	r := chi.NewRouter()
	r.Use(MetricsMiddleware(m))
	r.Get("/api/v1/items/{slug}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})

	req := httptest.NewRequest("GET", "/api/v1/items/my-item", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}

	// Check request count
	counter, err := m.HTTPRequestsTotal.GetMetricWithLabelValues("GET", "/api/v1/items/{slug}", "200")
	if err != nil {
		t.Fatalf("Failed to get counter: %v", err)
	}
	var metric io_prometheus_client.Metric
	if err := counter.Write(&metric); err != nil {
		t.Fatalf("Failed to write counter: %v", err)
	}
	if got := metric.GetCounter().GetValue(); got != 1 {
		t.Errorf("Expected request count = 1, got %v", got)
	}

	// Check duration and response size via the registry gather
	families, gatherErr := m.Registry.Gather()
	if gatherErr != nil {
		t.Fatalf("Failed to gather metrics: %v", gatherErr)
	}

	var durationCount uint64
	var responseSizeSum float64
	for _, f := range families {
		switch f.GetName() {
		case "pad_http_request_duration_seconds":
			for _, fm := range f.GetMetric() {
				durationCount += fm.GetHistogram().GetSampleCount()
			}
		case "pad_http_response_size_bytes":
			for _, fm := range f.GetMetric() {
				responseSizeSum += fm.GetHistogram().GetSampleSum()
			}
		}
	}

	if durationCount != 1 {
		t.Errorf("Expected 1 observation in duration histogram, got %d", durationCount)
	}
	if responseSizeSum <= 0 {
		t.Errorf("Expected response size > 0, got %v", responseSizeSum)
	}
}

func TestMetricsMiddleware_UsesRoutePattern(t *testing.T) {
	m := metrics.New()

	r := chi.NewRouter()
	r.Use(MetricsMiddleware(m))
	r.Get("/api/v1/workspaces/{ws}/items/{slug}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Hit with different slugs — should all map to the same route pattern
	for _, path := range []string{
		"/api/v1/workspaces/my-ws/items/item-1",
		"/api/v1/workspaces/other-ws/items/item-2",
		"/api/v1/workspaces/third/items/item-3",
	} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
	}

	// All 3 requests should be under the same route pattern label
	counter, err := m.HTTPRequestsTotal.GetMetricWithLabelValues("GET", "/api/v1/workspaces/{ws}/items/{slug}", "200")
	if err != nil {
		t.Fatalf("Failed to get counter: %v", err)
	}
	var metric io_prometheus_client.Metric
	if err := counter.Write(&metric); err != nil {
		t.Fatalf("Failed to write counter: %v", err)
	}
	if got := metric.GetCounter().GetValue(); got != 3 {
		t.Errorf("Expected 3 requests under route pattern, got %v", got)
	}
}

func TestMetricsMiddleware_UnmatchedRoute(t *testing.T) {
	m := metrics.New()

	r := chi.NewRouter()
	r.Use(MetricsMiddleware(m))
	r.Get("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Hit a route that doesn't exist
	req := httptest.NewRequest("GET", "/nonexistent", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// chi returns 404 for unmatched routes; the middleware should label it "unmatched"
	counter, err := m.HTTPRequestsTotal.GetMetricWithLabelValues("GET", "unmatched", "404")
	if err != nil {
		t.Fatalf("Failed to get counter: %v", err)
	}
	var metric io_prometheus_client.Metric
	if err := counter.Write(&metric); err != nil {
		t.Fatalf("Failed to write counter: %v", err)
	}
	if got := metric.GetCounter().GetValue(); got != 1 {
		t.Errorf("Expected 1 unmatched request, got %v", got)
	}
}
