// Package metrics collects watchtower scan statistics and exports them via
// prometheus counters/gauges consumed by the /v1/metrics API.
package metrics

import (
	"time"

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
	channel          chan *Metric
	scanned          prometheus.Gauge
	updated          prometheus.Gauge
	failed           prometheus.Gauge
	total            prometheus.Counter
	skipped          prometheus.Counter
	rollbacks        prometheus.Counter
	managed          prometheus.Gauge
	excluded         prometheus.Gauge
	unmanaged        prometheus.Gauge
	apiRequests      *prometheus.CounterVec
	registryRequests *prometheus.CounterVec
	registryRetries  *prometheus.CounterVec
	dockerAPIErrors  *prometheus.CounterVec
	authCacheHits    prometheus.Counter
	authCacheMisses  prometheus.Counter
	imageFallback    prometheus.Counter
	lastScanTime     prometheus.Gauge
	pollDuration     prometheus.Histogram
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
		managed: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "watchtower_containers_managed",
			Help: "Containers whose com.centurylinklabs.watchtower.enable label is set to true. Current state at the last classification pass.",
		}),
		excluded: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "watchtower_containers_excluded",
			Help: "Containers whose com.centurylinklabs.watchtower.enable label is set to false (intentional opt-out). Current state at the last classification pass.",
		}),
		unmanaged: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "watchtower_containers_unmanaged",
			Help: "Containers with no com.centurylinklabs.watchtower.enable label at all. Under --label-enable these are silently skipped — indistinguishable from intentional opt-outs. Pair with --audit-unmanaged for log warnings or /v1/audit for per-container detail.",
		}),
		apiRequests: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "watchtower_api_requests_total",
			Help: "HTTP requests to Watchtower's /v1/* endpoints, broken down by endpoint path and response status code. A spike of 401s on /v1/update usually means something is attempting credential stuffing.",
		}, []string{"endpoint", "status"}),
		registryRequests: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "watchtower_registry_requests_total",
			Help: "Outbound requests to container registries, labeled by registry host, logical operation (challenge|token|digest), and outcome (success|error|retried).",
		}, []string{"host", "operation", "outcome"}),
		registryRetries: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "watchtower_registry_retries_total",
			Help: "Bounded-backoff retry attempts (pkg/registry/retry). Zero is healthy; sustained non-zero means a flaky registry.",
		}, []string{"host"}),
		dockerAPIErrors: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "watchtower_docker_api_errors_total",
			Help: "Errors returned by calls into the Docker engine API, labeled by logical operation (list|inspect|kill|start|create|remove|image_inspect|image_remove|image_pull|rename|exec).",
		}, []string{"operation"}),
		authCacheHits: promauto.NewCounter(prometheus.CounterOpts{
			Name: "watchtower_auth_cache_hits_total",
			Help: "Bearer-token cache hits since startup. High hit rate means the v1.9 in-memory cache is sparing the oauth endpoint.",
		}),
		authCacheMisses: promauto.NewCounter(prometheus.CounterOpts{
			Name: "watchtower_auth_cache_misses_total",
			Help: "Bearer-token cache misses since startup. Each miss triggers an oauth exchange.",
		}),
		imageFallback: promauto.NewCounter(prometheus.CounterOpts{
			Name: "watchtower_image_fallback_total",
			Help: "Times Container.GetContainer fell back to inspecting by image reference because the source image ID was missing locally — usually the GC'd-source-image scenario (see upstream#1217).",
		}),
		lastScanTime: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "watchtower_last_scan_timestamp_seconds",
			Help: "Unix timestamp of the most recent completed scan. Staleness alert: (time() - watchtower_last_scan_timestamp_seconds) > expected_interval.",
		}),
		pollDuration: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "watchtower_poll_duration_seconds",
			Help:    "Wall-clock duration of each scan + update cycle. Buckets target homelab cadence (seconds, not sub-second).",
			Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300},
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

// SetAuditCounts updates the managed / excluded / unmanaged container gauges.
// Called from the audit pass so a Grafana panel can show the watch-status
// breakdown without operators having to hit /v1/audit manually.
func SetAuditCounts(managed, excluded, unmanaged int) {
	m := Default()
	m.managed.Set(float64(managed))
	m.excluded.Set(float64(excluded))
	m.unmanaged.Set(float64(unmanaged))
}

// RegisterAPIRequest increments watchtower_api_requests_total. status is the
// HTTP response status code as a decimal string ("200", "401", …) — kept as a
// label so cardinality stays bounded to the codes we actually emit.
func RegisterAPIRequest(endpoint, status string) {
	Default().apiRequests.WithLabelValues(endpoint, status).Inc()
}

// RegisterRegistryRequest increments watchtower_registry_requests_total.
// operation is one of "challenge", "token", "digest" (from pkg/registry);
// outcome is one of "success", "error", "retried".
func RegisterRegistryRequest(host, operation, outcome string) {
	Default().registryRequests.WithLabelValues(host, operation, outcome).Inc()
}

// RegisterRegistryRetry increments watchtower_registry_retries_total.
func RegisterRegistryRetry(host string) {
	Default().registryRetries.WithLabelValues(host).Inc()
}

// RegisterDockerAPIError increments watchtower_docker_api_errors_total for
// a failed call into the Docker engine API.
func RegisterDockerAPIError(operation string) {
	Default().dockerAPIErrors.WithLabelValues(operation).Inc()
}

// RegisterAuthCacheHit increments watchtower_auth_cache_hits_total. Hit rate
// is the primary signal for whether the v1.9 bearer-token cache is earning
// its keep.
func RegisterAuthCacheHit() { Default().authCacheHits.Inc() }

// RegisterAuthCacheMiss increments watchtower_auth_cache_misses_total. Each
// miss triggers an oauth exchange against the registry.
func RegisterAuthCacheMiss() { Default().authCacheMisses.Inc() }

// RegisterImageFallback increments watchtower_image_fallback_total. Fires
// when GetContainer fell back to inspecting by image reference because the
// source image ID was missing locally.
func RegisterImageFallback() { Default().imageFallback.Inc() }

// SetLastScanTimestamp records the completion time of the latest scan cycle.
func SetLastScanTimestamp(t time.Time) {
	Default().lastScanTime.Set(float64(t.Unix()))
}

// ObservePollDuration records the duration of a full scan cycle.
func ObservePollDuration(d time.Duration) {
	Default().pollDuration.Observe(d.Seconds())
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
