// Package metrics provides shared Prometheus helpers.
// Each service can register ConsumerMetrics to automatically instrument
// NATS handler throughput and latency via the WithMetrics Consumer option.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Handler returns the default Prometheus HTTP handler.
func Handler() http.Handler {
	return promhttp.Handler()
}

// MustRegister registers collectors against the default registry and panics
// on conflict. Wraps prometheus.MustRegister for convenience.
func MustRegister(cs ...prometheus.Collector) {
	prometheus.MustRegister(cs...)
}

// ConsumerMetrics holds Prometheus instruments for a NATS consumer.
// Create once with NewConsumerMetrics, then pass to the Consumer via WithMetrics.
type ConsumerMetrics struct {
	MessagesTotal   *prometheus.CounterVec
	HandlerDuration *prometheus.HistogramVec
	ReconnectsTotal prometheus.Counter
}

// NewConsumerMetrics creates and registers ConsumerMetrics for the given service.
// It panics if any metric name collides with an already-registered collector.
func NewConsumerMetrics(service string) *ConsumerMetrics {
	labels := prometheus.Labels{"service": service}

	messagesTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "nats_messages_total",
		Help:        "Total NATS messages processed, partitioned by action and status code.",
		ConstLabels: labels,
	}, []string{"action", "status"})

	handlerDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "nats_handler_duration_seconds",
		Help:        "NATS handler execution time in seconds.",
		ConstLabels: labels,
		Buckets:     prometheus.DefBuckets,
	}, []string{"action"})

	reconnectsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name:        "nats_reconnects_total",
		Help:        "Total NATS broker reconnect attempts.",
		ConstLabels: labels,
	})

	prometheus.MustRegister(messagesTotal, handlerDuration, reconnectsTotal)

	return &ConsumerMetrics{
		MessagesTotal:   messagesTotal,
		HandlerDuration: handlerDuration,
		ReconnectsTotal: reconnectsTotal,
	}
}
