// Package metrics collects watchtower scan statistics and exports them via
// prometheus counters/gauges consumed by the /v1/metrics API.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/openserbia/watchtower/pkg/types"
)

const (
	metricChannelBuffer = 10
	// labelOperation is the shared Prometheus label key for the registry/docker
	// API metric vectors.
	labelOperation = "operation"
)

var metrics *Metrics

// Metric is the data points of a single scan
type Metric struct {
	Scanned int
	Updated int
	Failed  int
}

// Metrics is the handler processing all individual scan metrics
type Metrics struct {
	channel                 chan *Metric
	scanned                 prometheus.Gauge
	updated                 prometheus.Gauge
	failed                  prometheus.Gauge
	total                   prometheus.Counter
	skipped                 prometheus.Counter
	rollbacks               prometheus.Counter
	promotionAborts         prometheus.Counter
	managed                 prometheus.Gauge
	excluded                prometheus.Gauge
	unmanaged               prometheus.Gauge
	infrastructure          prometheus.Gauge
	apiRequests             *prometheus.CounterVec
	registryRequests        *prometheus.CounterVec
	registryRetries         *prometheus.CounterVec
	dockerAPIErrors         *prometheus.CounterVec
	dockerAPIRetries        *prometheus.CounterVec
	authCacheHits           prometheus.Counter
	authCacheMisses         prometheus.Counter
	imageFallback           prometheus.Counter
	strandedInitDeps        prometheus.Counter
	strandedInitDepsCurrent prometheus.Gauge
	lastScanTime            prometheus.Gauge
	pollInterval            prometheus.Gauge
	pollDuration            prometheus.Histogram
	inCooldown              prometheus.Gauge
	eventsReceived          *prometheus.CounterVec
	eventsTriggered         prometheus.Counter
	eventsReconnects        prometheus.Counter
}

// NewMetric returns a Metric with the counts taken from the appropriate types.Report fields.
//
// report may be nil: actions.Update returns a nil report on its error paths
// (e.g. the Docker daemon was unreachable mid-scan and ListContainers failed),
// and the scheduled/HTTP/event callers feed that result straight in here. Treat
// a nil report as an empty scan rather than dereferencing it — a transient
// daemon blip must not panic the cron goroutine and crash the whole daemon.
func NewMetric(report types.Report) *Metric {
	if report == nil {
		return &Metric{}
	}

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
			Help: "Number of update rollbacks triggered by --health-check-gated since watchtower started. Each increment means a replacement container was created but reverted because it did not become healthy. Distinct from watchtower_promotion_aborts_total, where the replacement WAS healthy but the old container could not be retired.",
		}),
		promotionAborts: promauto.NewCounter(prometheus.CounterOpts{
			Name: "watchtower_promotion_aborts_total",
			Help: "Number of blue-green cutovers that brought up a healthy replacement (\"green\") but could not retire the old container, since watchtower started. Unlike a rollback the new image IS live — green keeps a temporary name and the old container lingers — so this is tracked separately from watchtower_rollbacks_total. Non-zero means a cutover needs manual reconciliation: `docker compose up -d --force-recreate <service>`.",
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
			Help: "Containers with no com.centurylinklabs.watchtower.enable label at all. Under --label-enable these are silently skipped — indistinguishable from intentional opt-outs. Pair with --audit-unmanaged for log warnings or /v1/audit for per-container detail. Excludes Docker-managed infrastructure (buildkit etc.), which lives in watchtower_containers_infrastructure.",
		}),
		infrastructure: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "watchtower_containers_infrastructure",
			Help: "Docker-managed infrastructure containers (moby/buildkit builders, Docker Desktop internals). Not user workloads — tracked separately so they don't inflate the unmanaged bucket every `docker buildx build` invocation.",
		}),
		apiRequests: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "watchtower_api_requests_total",
			Help: "HTTP requests to Watchtower's /v1/* endpoints, broken down by endpoint path and response status code. A spike of 401s on /v1/update usually means something is attempting credential stuffing.",
		}, []string{"endpoint", "status"}),
		registryRequests: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "watchtower_registry_requests_total",
			Help: "Outbound requests to container registries, labeled by registry host, logical operation (challenge|token|digest), and outcome (success|error|retried).",
		}, []string{"host", labelOperation, "outcome"}),
		registryRetries: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "watchtower_registry_retries_total",
			Help: "Bounded-backoff retry attempts (pkg/registry/retry). Zero is healthy; sustained non-zero means a flaky registry.",
		}, []string{"host"}),
		dockerAPIErrors: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "watchtower_docker_api_errors_total",
			Help: "Errors returned by calls into the Docker engine API, labeled by logical operation (list|inspect|kill|start|create|remove|image_inspect|image_remove|image_pull|rename|exec).",
		}, []string{labelOperation}),
		dockerAPIRetries: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "watchtower_docker_api_retries_total",
			Help: "Bounded-backoff retry attempts against the Docker engine API, labeled by logical operation. Zero is healthy; sustained non-zero means the daemon is flaky or restarting during polls.",
		}, []string{labelOperation}),
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
		strandedInitDeps: promauto.NewCounter(prometheus.CounterOpts{
			Name: "watchtower_stranded_init_deps_total",
			Help: "Times --rerun-init-deps found a stale compose-managed target with no declared service_completed_successfully deps while its project still held one-shot init siblings (migrate/pg-ready) — the signature of a com.docker.compose.depends_on label dropped by a prior blue-green cutover. Non-zero means a service ran new code against an un-migrated schema; redeploy it via compose to restore the label. This is a CUMULATIVE counter (per-detection history for rate/debugging); alert on the watchtower_stranded_init_deps gauge instead so the alert resolves when the target is re-armed.",
		}),
		strandedInitDepsCurrent: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "watchtower_stranded_init_deps",
			Help: "Number of compose-managed targets CURRENTLY stranded: empty com.docker.compose.depends_on while their project still holds a one-shot init sibling (migrate/pg-ready) and no no-init-deps opt-out. Recomputed every scan, so it drops to 0 on the first scan after the operator re-arms with `docker compose up -d --force-recreate <service>`. Unlike the _total counter this is a live gauge — alert on `> 0` for a NON-RESOLVING 'new code on un-migrated schema' signal that clears exactly when the label is restored.",
		}),
		lastScanTime: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "watchtower_last_scan_timestamp_seconds",
			Help: "Unix timestamp of the most recent completed scan. Staleness alert: (time() - watchtower_last_scan_timestamp_seconds) > expected_interval.",
		}),
		pollInterval: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "watchtower_poll_interval_seconds",
			Help: "Configured time between scans, derived from the active schedule. Lets alerts scale staleness thresholds to the actual cadence instead of hardcoding a window (long-cadence deployments like 12h otherwise false-alarm).",
		}),
		pollDuration: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "watchtower_poll_duration_seconds",
			Help:    "Wall-clock duration of each scan + update cycle. Buckets target homelab cadence (seconds, not sub-second).",
			Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		}),
		inCooldown: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "watchtower_containers_in_cooldown",
			Help: "Containers with a pending --image-cooldown: a new digest has been detected but the supply-chain cooldown window hasn't elapsed yet. Non-zero is expected right after an image author pushes a new tag; a stuck non-zero value usually means the author keeps re-pushing.",
		}),
		eventsReceived: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "watchtower_events_received_total",
			Help: "Docker engine events received by --watch-docker-events, labeled by action (tag, load). A flat-zero counter under load usually means the event stream reconnected silently — cross-check watchtower_events_reconnects_total.",
		}, []string{"action"}),
		eventsTriggered: promauto.NewCounter(prometheus.CounterOpts{
			Name: "watchtower_events_triggered_scans_total",
			Help: "Targeted scans triggered by --watch-docker-events after the debounce window. Expected to be strictly lower than watchtower_events_received_total (bursty rebuilds collapse into a single scan).",
		}),
		eventsReconnects: promauto.NewCounter(prometheus.CounterOpts{
			Name: "watchtower_events_reconnects_total",
			Help: "Times the Docker event stream was re-established after an error. Non-zero is normal (daemon restarts, network blips); a rapidly climbing counter means the stream is flapping.",
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

// RegisterPromotionAbort increments watchtower_promotion_aborts_total. Called by
// the blue-green path when the replacement ("green") container came up healthy
// but the old container could not be stopped (e.g. a transient Docker API error
// survived retries). Unlike a rollback the new image is live — green keeps a
// temporary name and the old container lingers until manual reconciliation — so
// it is counted separately from watchtower_rollbacks_total.
func RegisterPromotionAbort() {
	Default().promotionAborts.Inc()
}

// SetAuditCounts updates the managed / excluded / unmanaged / infrastructure
// container gauges. Called from the audit pass so a Grafana panel can show the
// watch-status breakdown without operators having to hit /v1/audit manually.
func SetAuditCounts(managed, excluded, unmanaged, infrastructure int) {
	m := Default()
	m.managed.Set(float64(managed))
	m.excluded.Set(float64(excluded))
	m.unmanaged.Set(float64(unmanaged))
	m.infrastructure.Set(float64(infrastructure))
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

// RegisterDockerAPIRetry increments watchtower_docker_api_retries_total. One
// increment per retry attempt (not per failed call), so a 3-attempt run that
// eventually succeeds adds 2.
func RegisterDockerAPIRetry(operation string) {
	Default().dockerAPIRetries.WithLabelValues(operation).Inc()
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

// RegisterStrandedInitDeps increments watchtower_stranded_init_deps_total. Fires
// when --rerun-init-deps detects a stale compose target whose depends_on label
// was stripped (typically by a prior blue-green cutover) yet whose project still
// holds one-shot init siblings — migrations would otherwise be silently skipped.
func RegisterStrandedInitDeps() { Default().strandedInitDeps.Inc() }

// SetStrandedInitDeps publishes the current count of stranded init-deps targets
// to the watchtower_stranded_init_deps gauge. Called once per scan so the value
// reflects live state: it stays >0 while a target's com.docker.compose.depends_on
// label is missing and drops to 0 the first scan after it is re-armed, letting
// the alert resolve exactly on remediation. Companion to the RegisterStrandedInitDeps
// history counter.
func SetStrandedInitDeps(n int) { Default().strandedInitDepsCurrent.Set(float64(n)) }

// SetLastScanTimestamp records the completion time of the latest scan cycle.
func SetLastScanTimestamp(t time.Time) {
	Default().lastScanTime.Set(float64(t.Unix()))
}

// SetPollInterval records the configured seconds between scheduled scans,
// derived from the active cron expression at startup.
func SetPollInterval(d time.Duration) {
	Default().pollInterval.Set(d.Seconds())
}

// ObservePollDuration records the duration of a full scan cycle.
func ObservePollDuration(d time.Duration) {
	Default().pollDuration.Observe(d.Seconds())
}

// SetContainersInCooldown reports how many containers are currently awaiting
// their --image-cooldown window to elapse.
func SetContainersInCooldown(count int) {
	Default().inCooldown.Set(float64(count))
}

// RegisterEventReceived increments watchtower_events_received_total for one
// image event observed on the Docker engine event stream.
func RegisterEventReceived(action string) {
	Default().eventsReceived.WithLabelValues(action).Inc()
}

// RegisterEventTriggeredScan increments watchtower_events_triggered_scans_total
// each time a debounced burst of events fires a targeted scan.
func RegisterEventTriggeredScan() { Default().eventsTriggered.Inc() }

// RegisterEventReconnect increments watchtower_events_reconnects_total after
// the event stream has been re-established following a transient failure.
func RegisterEventReconnect() { Default().eventsReconnects.Inc() }

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
