package events

import (
	"github.com/prometheus/client_golang/prometheus"
)

// PrometheusBusMetrics implements BusMetrics using Prometheus counters,
// gauges, and histograms.
type PrometheusBusMetrics struct {
	published       *prometheus.CounterVec
	delivered       *prometheus.CounterVec
	dropped         *prometheus.CounterVec
	publishDuration *prometheus.HistogramVec
	subscriberCount *prometheus.GaugeVec
}

var _ BusMetrics = (*PrometheusBusMetrics)(nil)

// NewPrometheusBusMetrics creates and registers Prometheus metrics for the
// event bus. The caller should pass a prometheus.Registerer (typically
// prometheus.DefaultRegisterer or a custom registry for tests).
func NewPrometheusBusMetrics(reg prometheus.Registerer) *PrometheusBusMetrics {
	m := &PrometheusBusMetrics{
		published: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "telemetry",
			Subsystem: "event_bus",
			Name:      "published_total",
			Help:      "Total number of events published per topic.",
		}, []string{"topic"}),

		delivered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "telemetry",
			Subsystem: "event_bus",
			Name:      "delivered_total",
			Help:      "Total number of events delivered to subscribers per topic.",
		}, []string{"topic"}),

		dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "telemetry",
			Subsystem: "event_bus",
			Name:      "dropped_total",
			Help:      "Total number of events dropped due to slow subscribers per topic.",
		}, []string{"topic"}),

		publishDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "telemetry",
			Subsystem: "event_bus",
			Name:      "publish_duration_seconds",
			Help:      "Time to fan out a Publish call across all topic subscribers.",
			Buckets:   []float64{0.000001, 0.00001, 0.0001, 0.001, 0.01},
		}, []string{"topic"}),

		subscriberCount: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "telemetry",
			Subsystem: "event_bus",
			Name:      "subscribers",
			Help:      "Current number of active subscribers per topic.",
		}, []string{"topic"}),
	}

	reg.MustRegister(m.published, m.delivered, m.dropped, m.publishDuration, m.subscriberCount)
	return m
}

func (m *PrometheusBusMetrics) IncPublished(topic Topic) {
	m.published.WithLabelValues(string(topic)).Inc()
}

func (m *PrometheusBusMetrics) IncDelivered(topic Topic) {
	m.delivered.WithLabelValues(string(topic)).Inc()
}

func (m *PrometheusBusMetrics) IncDropped(topic Topic) {
	m.dropped.WithLabelValues(string(topic)).Inc()
}

func (m *PrometheusBusMetrics) ObservePublishDuration(topic Topic, seconds float64) {
	m.publishDuration.WithLabelValues(string(topic)).Observe(seconds)
}

func (m *PrometheusBusMetrics) SetSubscriberCount(topic Topic, count int) {
	m.subscriberCount.WithLabelValues(string(topic)).Set(float64(count))
}
