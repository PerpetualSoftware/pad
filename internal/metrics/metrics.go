// Package metrics provides Prometheus instrumentation for the Pad server.
// It uses a custom registry (not the global default) for test isolation
// and explicit control over exposed metrics.
package metrics

import (
	"database/sql"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics holds all Prometheus collectors and the custom registry.
type Metrics struct {
	Registry *prometheus.Registry

	// HTTP request metrics
	HTTPRequestsTotal   *prometheus.CounterVec
	HTTPRequestDuration *prometheus.HistogramVec
	HTTPResponseSize    *prometheus.HistogramVec

	// SSE connection metrics (single gauge to avoid unbounded label cardinality)
	SSEConnectionsActive *prometheus.Gauge

	// EventBus metrics
	EventBusPublishTotal *prometheus.Counter
	EventBusSubscribers  *prometheus.Gauge
}

// New creates a new Metrics instance with a custom registry and registers
// all application metrics plus Go runtime and process collectors.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	// Go runtime + process collectors (goroutines, memory, GC, file descriptors)
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	httpRequestsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_http_requests_total",
		Help: "Total number of HTTP requests by method, route, and status code.",
	}, []string{"method", "route", "status"})

	httpRequestDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pad_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds by method, route, and status code.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route", "status"})

	httpResponseSize := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pad_http_response_size_bytes",
		Help:    "HTTP response size in bytes by method, route, and status code.",
		Buckets: prometheus.ExponentialBuckets(100, 10, 7), // 100B to 100MB
	}, []string{"method", "route", "status"})

	sseConnectionsActive := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pad_sse_connections_active",
		Help: "Total number of active SSE connections.",
	})

	eventBusPublishTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_eventbus_publish_total",
		Help: "Total number of events published to the event bus.",
	})

	eventBusSubscribers := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pad_eventbus_subscribers",
		Help: "Current number of event bus subscribers.",
	})

	reg.MustRegister(
		httpRequestsTotal,
		httpRequestDuration,
		httpResponseSize,
		sseConnectionsActive,
		eventBusPublishTotal,
		eventBusSubscribers,
	)

	return &Metrics{
		Registry:             reg,
		HTTPRequestsTotal:    httpRequestsTotal,
		HTTPRequestDuration:  httpRequestDuration,
		HTTPResponseSize:     httpResponseSize,
		SSEConnectionsActive: &sseConnectionsActive,
		EventBusPublishTotal: &eventBusPublishTotal,
		EventBusSubscribers:  &eventBusSubscribers,
	}
}

// RegisterDBCollector registers a callback-based collector that exposes
// database connection pool statistics on each Prometheus scrape.
// This is preferred over a periodic goroutine: zero overhead between
// scrapes and always fresh data.
func (m *Metrics) RegisterDBCollector(db *sql.DB) {
	m.Registry.MustRegister(&dbStatsCollector{db: db})
}

// dbStatsCollector implements prometheus.Collector using db.Stats() callbacks.
type dbStatsCollector struct {
	db *sql.DB
}

var (
	dbOpenDesc = prometheus.NewDesc(
		"pad_db_open_connections",
		"Number of open database connections.",
		nil, nil,
	)
	dbIdleDesc = prometheus.NewDesc(
		"pad_db_idle_connections",
		"Number of idle database connections.",
		nil, nil,
	)
	dbInUseDesc = prometheus.NewDesc(
		"pad_db_in_use_connections",
		"Number of in-use database connections.",
		nil, nil,
	)
	dbWaitCountDesc = prometheus.NewDesc(
		"pad_db_wait_count_total",
		"Total number of connections waited for.",
		nil, nil,
	)
	dbWaitDurationDesc = prometheus.NewDesc(
		"pad_db_wait_duration_seconds_total",
		"Total time blocked waiting for a new connection.",
		nil, nil,
	)
)

func (c *dbStatsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- dbOpenDesc
	ch <- dbIdleDesc
	ch <- dbInUseDesc
	ch <- dbWaitCountDesc
	ch <- dbWaitDurationDesc
}

func (c *dbStatsCollector) Collect(ch chan<- prometheus.Metric) {
	stats := c.db.Stats()
	ch <- prometheus.MustNewConstMetric(dbOpenDesc, prometheus.GaugeValue, float64(stats.OpenConnections))
	ch <- prometheus.MustNewConstMetric(dbIdleDesc, prometheus.GaugeValue, float64(stats.Idle))
	ch <- prometheus.MustNewConstMetric(dbInUseDesc, prometheus.GaugeValue, float64(stats.InUse))
	ch <- prometheus.MustNewConstMetric(dbWaitCountDesc, prometheus.CounterValue, float64(stats.WaitCount))
	ch <- prometheus.MustNewConstMetric(dbWaitDurationDesc, prometheus.CounterValue, stats.WaitDuration.Seconds())
}
