package crawler

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eshaan2418/trawl/frontier"
)

// Metrics are Prometheus collectors for a crawl. A nil *Metrics disables them.
type Metrics struct {
	fetches       *prometheus.CounterVec
	bytes         prometheus.Counter
	duration      prometheus.Histogram
	errors        prometheus.Counter
	robotsBlocked prometheus.Counter
	dropped       prometheus.Counter
	queued        prometheus.Gauge
	inFlight      prometheus.Gauge
	hosts         prometheus.Gauge
}

// NewMetrics registers the crawler's collectors with reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		fetches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "trawl_fetches_total", Help: "Page fetches by HTTP status code.",
		}, []string{"code"}),
		bytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "trawl_body_bytes_total", Help: "Response body bytes received.",
		}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "trawl_fetch_duration_seconds", Help: "Time to fetch a page (headers and body).",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 16),
		}),
		errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "trawl_fetch_errors_total", Help: "Network errors and timeouts.",
		}),
		robotsBlocked: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "trawl_robots_blocked_total", Help: "URLs skipped because robots.txt disallows them.",
		}),
		dropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "trawl_frontier_dropped_total", Help: "URLs dropped because the frontier was full.",
		}),
		queued:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "trawl_frontier_queued", Help: "URLs waiting."}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{Name: "trawl_frontier_in_flight", Help: "Fetches in progress."}),
		hosts:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "trawl_frontier_hosts", Help: "Hosts seen."}),
	}
	reg.MustRegister(m.fetches, m.bytes, m.duration, m.errors, m.robotsBlocked, m.dropped, m.queued, m.inFlight, m.hosts)
	return m
}

// The counter methods are nil-safe: a crawl without metrics passes a nil *Metrics. (Passing
// m.field as an argument would dereference nil before the method could check.)
func (m *Metrics) incErrors() {
	if m != nil {
		m.errors.Inc()
	}
}

func (m *Metrics) incRobotsBlocked() {
	if m != nil {
		m.robotsBlocked.Inc()
	}
}

func (m *Metrics) incDropped() {
	if m != nil {
		m.dropped.Inc()
	}
}

func (m *Metrics) observeFetch(code, n int, d time.Duration) {
	if m == nil {
		return
	}
	m.fetches.WithLabelValues(strconv.Itoa(code)).Inc()
	m.bytes.Add(float64(n))
	m.duration.Observe(d.Seconds())
}

func (m *Metrics) observeFrontier(s frontier.Stats) {
	if m == nil {
		return
	}
	m.queued.Set(float64(s.Queued))
	m.inFlight.Set(float64(s.InFlight))
	m.hosts.Set(float64(s.Hosts))
}
