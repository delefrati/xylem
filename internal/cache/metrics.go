package cache

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics are a tier's Prometheus metrics (09 §Caching tiers): hits, misses,
// cached bytes and in-flight upstream requests, labeled with the tier kind.
// It implements Observer.
type Metrics struct {
	reg      *prometheus.Registry
	hits     prometheus.Counter
	misses   prometheus.Counter
	inflight prometheus.Gauge
	labels   prometheus.Labels
}

// NewMetrics returns the metrics of a tier of the given kind, in a new
// registry.
func NewMetrics(kind string) *Metrics {
	labels := prometheus.Labels{"tier": kind}
	m := &Metrics{
		reg:    prometheus.NewRegistry(),
		labels: labels,
		hits: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "xylem_cache_hits_total", Help: "Objects served from this tier's memory or disk cache.", ConstLabels: labels,
		}),
		misses: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "xylem_cache_misses_total", Help: "Objects this tier had to fetch from upstream.", ConstLabels: labels,
		}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "xylem_upstream_inflight", Help: "Upstream requests in flight.", ConstLabels: labels,
		}),
	}
	m.reg.MustRegister(m.hits, m.misses, m.inflight)
	return m
}

// Watch adds the cache size gauges of tier (memory and disk).
func (m *Metrics) Watch(t *Tier) {
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "xylem_cache_bytes", Help: "Bytes held by the cache.", ConstLabels: merge(m.labels, "level", "memory"),
	}, func() float64 {
		_, b := t.mem.Stats()
		return float64(b)
	}))
	if t.cfg.Disk != nil {
		m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "xylem_cache_bytes", Help: "Bytes held by the cache.", ConstLabels: merge(m.labels, "level", "disk"),
		}, func() float64 {
			_, b := t.cfg.Disk.Stats()
			return float64(b)
		}))
	}
}

func merge(l prometheus.Labels, k, v string) prometheus.Labels {
	out := prometheus.Labels{k: v}
	for a, b := range l {
		out[a] = b
	}
	return out
}

// Registry returns the metrics registry (for extra collectors).
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Handler serves the metrics.
func (m *Metrics) Handler() http.Handler { return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}) }

// Hit implements Observer.
func (m *Metrics) Hit(n int) { m.hits.Add(float64(n)) }

// Miss implements Observer.
func (m *Metrics) Miss(n int) { m.misses.Add(float64(n)) }

// UpstreamInflight implements Observer.
func (m *Metrics) UpstreamInflight(delta int) { m.inflight.Add(float64(delta)) }
