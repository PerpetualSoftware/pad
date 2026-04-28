package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/PerpetualSoftware/pad/internal/metrics"
)

// MetricsMiddleware returns a chi middleware that records Prometheus metrics
// for every HTTP request: count, duration, and response size.
//
// Path labels use chi's RoutePattern (e.g. "/api/v1/workspaces/{slug}/items/{itemSlug}")
// instead of actual URL paths, keeping label cardinality bounded.
func MetricsMiddleware(m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := chimiddleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			duration := time.Since(start).Seconds()
			status := strconv.Itoa(ww.Status())
			route := chi.RouteContext(r.Context()).RoutePattern()
			if route == "" {
				route = "unmatched"
			}
			method := r.Method

			m.HTTPRequestsTotal.WithLabelValues(method, route, status).Inc()
			m.HTTPRequestDuration.WithLabelValues(method, route, status).Observe(duration)
			m.HTTPResponseSize.WithLabelValues(method, route, status).Observe(float64(ww.BytesWritten()))
		})
	}
}
