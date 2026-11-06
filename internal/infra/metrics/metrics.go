// Package metrics defines the Prometheus metrics farvardin exports.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the collectors. A nil *Metrics is valid and records
// nothing, so tests and callers without metrics need no checks.
type Metrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	writes   *prometheus.CounterVec
	rejected *prometheus.CounterVec
	photos   *prometheus.CounterVec
	objects  *prometheus.GaugeVec
}

// New registers the collectors on a fresh registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})) //nolint:exhaustruct // defaults

	m := &Metrics{
		registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{ //nolint:exhaustruct // only name and help
			Name: "farvardin_requests_total", Help: "HTTP requests by method and status code.",
		}, []string{"method", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{ //nolint:exhaustruct // default buckets
			Name: "farvardin_request_duration_seconds", Help: "HTTP request duration by method.",
		}, []string{"method"}),
		writes: prometheus.NewCounterVec(prometheus.CounterOpts{ //nolint:exhaustruct // only name and help
			Name: "farvardin_writes_total", Help: "Objects written, by collection kind and operation.",
		}, []string{"kind", "op"}),
		rejected: prometheus.NewCounterVec(prometheus.CounterOpts{ //nolint:exhaustruct // only name and help
			Name: "farvardin_rejected_writes_total", Help: "Writes refused, by reason.",
		}, []string{"reason"}),
		photos: prometheus.NewCounterVec(prometheus.CounterOpts{ //nolint:exhaustruct // only name and help
			Name: "farvardin_photo_actions_total", Help: "Photo policy actions on incoming cards (scaled, guarded, stabilized).",
		}, []string{"action"}),
		objects: prometheus.NewGaugeVec(prometheus.GaugeOpts{ //nolint:exhaustruct // only name and help
			Name: "farvardin_collection_objects", Help: "Objects per collection at its last snapshot.",
		}, []string{"kind", "collection"}),
	}

	reg.MustRegister(m.requests, m.duration, m.writes, m.rejected, m.photos, m.objects)

	return m
}

// Handler serves /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}) //nolint:exhaustruct // defaults
}

// Request records one HTTP request.
func (m *Metrics) Request(method string, code int, d time.Duration) {
	if m == nil {
		return
	}

	m.requests.WithLabelValues(method, strconv.Itoa(code)).Inc()
	m.duration.WithLabelValues(method).Observe(d.Seconds())
}

// Write records a stored object.
func (m *Metrics) Write(kind, op string) {
	if m != nil {
		m.writes.WithLabelValues(kind, op).Inc()
	}
}

// Rejected records a refused write.
func (m *Metrics) Rejected(reason string) {
	if m != nil {
		m.rejected.WithLabelValues(reason).Inc()
	}
}

// Photo records a photo policy action.
func (m *Metrics) Photo(action string) {
	if m != nil && action != "" {
		m.photos.WithLabelValues(action).Inc()
	}
}

// Objects sets the object count of a collection.
func (m *Metrics) Objects(kind, collection string, n int) {
	if m != nil {
		m.objects.WithLabelValues(kind, collection).Set(float64(n))
	}
}
