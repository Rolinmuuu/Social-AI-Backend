// Package metrics exposes the event pipeline to Prometheus: outbox publish results and lag,
// consumer throughput, retries and dead letters. These are the numbers to watch when
// looking for the bottleneck between "post saved" and "post in followers' feeds".
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	outboxPublished = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "outbox_published_total",
		Help: "Events published by the outbox relay.",
	}, []string{"topic"})
	outboxFailed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "outbox_publish_failures_total",
		Help: "Failed publish attempts; dead=true means the record was parked.",
	}, []string{"topic", "dead"})
	outboxLag = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "outbox_publish_lag_seconds",
		Help:    "Time from the business write to a successful publish by the relay.",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 300},
	}, []string{"topic"})

	consumed = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "consumer_handle_seconds",
		Help:    "Time to handle one event, including retries.",
		Buckets: prometheus.DefBuckets,
	}, []string{"topic"})
	retried = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "consumer_retries_total",
		Help: "Handler attempts that failed and were retried.",
	}, []string{"topic"})
	deadLettered = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "consumer_dead_letters_total",
		Help: "Events moved to the dead-letter topic.",
	}, []string{"topic"})
)

// Outbox implements outbox.Metrics.
type Outbox struct{}

func (Outbox) Published(topic string) { outboxPublished.WithLabelValues(topic).Inc() }
func (Outbox) Failed(topic string, dead bool) {
	d := "false"
	if dead {
		d = "true"
	}
	outboxFailed.WithLabelValues(topic, d).Inc()
}
func (Outbox) Lag(topic string, seconds float64) { outboxLag.WithLabelValues(topic).Observe(seconds) }

// Consumer implements consumer.Metrics.
type Consumer struct{}

func (Consumer) Processed(topic string, d time.Duration) {
	consumed.WithLabelValues(topic).Observe(d.Seconds())
}
func (Consumer) Retried(topic string)      { retried.WithLabelValues(topic).Inc() }
func (Consumer) DeadLettered(topic string) { deadLettered.WithLabelValues(topic).Inc() }
