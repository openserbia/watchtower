// Package metrics collects watchtower scan statistics and exports them via
// prometheus counters/gauges consumed by the /v1/metrics API.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/openserbia/watchtower/pkg/types"
)

const metricChannelBuffer = 10

var metrics *Metrics

// Metric is the data points of a single scan
type Metric struct {
	Scanned int
	Updated int
	Failed  int
}

// Metrics is the handler processing all individual scan metrics
type Metrics struct {
	channel   chan *Metric
	scanned   prometheus.Gauge
	updated   prometheus.Gauge
	failed    prometheus.Gauge
	total     prometheus.Counter
	skipped   prometheus.Counter
	rollbacks prometheus.Counter
}

// NewMetric returns a Metric with the counts taken from the appropriate types.Report fields
func NewMetric(report types.Report) *Metric {
	return &Metric{
		Scanned: len(report.Scanned()),
		// Note: This is for backwards compatibility. ideally, stale containers should be counted separately
		Updated: len(report.Updated()) + len(report.Stale()),
		Failed:  len(report.Failed()),
	}
}

// QueueIsEmpty checks whether any messages are enqueued in the channel
func (metrics *Metrics) QueueIsEmpty() bool {
	return len(metrics.channel) == 0
}

// Register registers metrics for an executed scan
func (metrics *Metrics) Register(metric *Metric) {
	metrics.channel <- metric
}

// Default creates a new metrics handler if none exists, otherwise returns the existing one
func Default() *Metrics {
	if metrics != nil {
		return metrics
	}

	metrics = &Metrics{
		scanned: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "watchtower_containers_scanned",
			Help: "Number of containers scanned for changes by watchtower during the last scan",
		}),
		updated: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "watchtower_containers_updated",
			Help: "Number of containers updated by watchtower during the last scan",
		}),
		failed: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "watchtower_containers_failed",
			Help: "Number of containers where update failed during the last scan",
		}),
		total: promauto.NewCounter(prometheus.CounterOpts{
			Name: "watchtower_scans_total",
			Help: "Number of scans since the watchtower started",
		}),
		skipped: promauto.NewCounter(prometheus.CounterOpts{
			Name: "watchtower_scans_skipped",
			Help: "Number of skipped scans since watchtower started",
		}),
		rollbacks: promauto.NewCounter(prometheus.CounterOpts{
			Name: "watchtower_rollbacks_total",
			Help: "Number of update rollbacks triggered by --health-check-gated since watchtower started. Each increment means a replacement container was created but reverted because it did not become healthy.",
		}),
		channel: make(chan *Metric, metricChannelBuffer),
	}

	go metrics.HandleUpdate(metrics.channel)

	return metrics
}

// RegisterScan fetches a metric handler and enqueues a metric
func RegisterScan(metric *Metric) {
	metrics := Default()
	metrics.Register(metric)
}

// RegisterRollback increments watchtower_rollbacks_total. Called by the health
// gating flow when a replacement container was stopped and the previous image
// was restored.
func RegisterRollback() {
	Default().rollbacks.Inc()
}

// HandleUpdate dequeue the metric channel and processes it
func (metrics *Metrics) HandleUpdate(channel <-chan *Metric) {
	for change := range channel {
		if change == nil {
			// Update was skipped and rescheduled
			metrics.total.Inc()
			metrics.skipped.Inc()
			metrics.scanned.Set(0)
			metrics.updated.Set(0)
			metrics.failed.Set(0)
			continue
		}
		// Update metrics with the new values
		metrics.total.Inc()
		metrics.scanned.Set(float64(change.Scanned))
		metrics.updated.Set(float64(change.Updated))
		metrics.failed.Set(float64(change.Failed))
	}
}
