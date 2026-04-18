# Observability for Watchtower

Ready-to-scrape dashboards and alerts for operators.

## What Watchtower exports

Enable metrics with `WATCHTOWER_HTTP_API_METRICS=true` + `WATCHTOWER_HTTP_API_TOKEN=<token>`, then scrape `GET /v1/metrics` with an `Authorization: Bearer <token>` header.

| Metric | Type | Meaning |
|---|---|---|
| `watchtower_scans_total` | counter | Poll cycles since the daemon started. |
| `watchtower_scans_skipped` | counter | Poll cycles where another update was still in flight. |
| `watchtower_containers_scanned` | gauge | Containers checked during the most recent poll. |
| `watchtower_containers_updated` | gauge | Containers recreated during the most recent poll. |
| `watchtower_containers_failed` | gauge | Containers that failed to update during the most recent poll. |
| `watchtower_containers_managed` | gauge | Containers with `com.centurylinklabs.watchtower.enable=true`. Current state. |
| `watchtower_containers_excluded` | gauge | Containers with `com.centurylinklabs.watchtower.enable=false` (intentional opt-out). Current state. |
| `watchtower_containers_unmanaged` | gauge | Containers with no enable label at all. Silently skipped under `--label-enable`. Hit `GET /v1/audit` for the names. |
| `watchtower_containers_infrastructure` | gauge | Docker-managed scaffolding containers (`moby/buildkit*`, `docker/desktop-*`, anything with `com.docker.buildx.*` or `com.docker.desktop.*` labels). Tracked separately so they don't inflate the unmanaged bucket every `docker buildx build`. |
| `watchtower_rollbacks_total` | counter | Update rollbacks triggered by `--health-check-gated`. |
| `watchtower_api_requests_total` | counter | HTTP requests to `/v1/*` endpoints. Labels: `endpoint`, `status`. |
| `watchtower_registry_requests_total` | counter | Outbound registry calls. Labels: `host`, `operation` (`challenge`/`token`/`digest`), `outcome` (`success`/`error`/`retried`). |
| `watchtower_registry_retries_total` | counter | Bounded-backoff retry attempts. Labels: `host`. |
| `watchtower_docker_api_errors_total` | counter | Errors from the Docker engine API. Labels: `operation` (`list`/`inspect`/`kill`/`start`/`create`/`remove`/`image_inspect`/`image_remove`/`image_pull`/`rename`/`network_connect`/`network_disconnect`). |
| `watchtower_auth_cache_hits_total` / `watchtower_auth_cache_misses_total` | counter | Bearer-token cache (v1.9+) hit/miss counts. |
| `watchtower_image_fallback_total` | counter | Times `GetContainer` fell back to inspecting by image reference because the source image ID was missing locally. |
| `watchtower_last_scan_timestamp_seconds` | gauge | Unix epoch of the latest completed scan. |
| `watchtower_poll_interval_seconds` | gauge | Configured time between scans, derived from the active schedule at startup. Used by the staleness alert to scale the threshold to the operator's cadence. |
| `watchtower_poll_duration_seconds` | histogram | Wall-clock duration of each scan + update cycle. Buckets: 0.5s → 5m. |

The gauges are reset each poll, so they answer "what did the last run do?" rather than "how many containers exist".

## Prometheus alerts — `prometheus/alerts.yml`

Six rules, deliberately tuned from production use. Earlier drafts of this
file included more noise-heavy alerts (generic skipped-scan counts,
scans-without-updates predictions, API 401 bursts) that turned out to
either fire on expected homelab states or not catch anything actionable in
practice. What's left:

- **`WatchtowerRollbackTriggered`** — any rollback in the last hour. A broken release shipped and got reverted; investigate before the next push.
- **`WatchtowerScansStopped`** — the scheduler hasn't fired in 2× the configured interval. Scales with `watchtower_poll_interval_seconds` so 60 s polls and 12 h polls both alert at a sensible multiple.
- **`WatchtowerFailuresSustained`** — at least one container has been stuck in failure for 30 m. The most actionable alert.
- **`WatchtowerUnmanagedContainersPresent`** (info) — a container without the `enable` label has been visible for over an hour. Usually a new deploy missing a label.
- **`WatchtowerRegistryErrorsSustained`** — outbound registry calls to a given host have been returning errors for 15 m. Distinguishes from the `retried` flake case.
- **`WatchtowerDockerAPIErrorsSustained`** — errors against the Docker socket for 15 m. Usually socket permission drift or daemon load.

Wire into Prometheus:

```yaml
rule_files:
  - /etc/prometheus/watchtower-alerts.yml
```

## Grafana dashboard — `grafana/watchtower-dashboard.json`

Single-page dashboard organised into three rows:

- **Overview** — total/skipped scans, containers per poll, scan rate.
- **Watch status** — managed/excluded/unmanaged donut + stat + history.
- **Reliability & Security** — poll p50/p95, registry outcomes, non-2xx API requests, Docker API error rate, bearer-cache hit ratio, 24h image-fallback count.

Import via **Dashboards → New → Import → Upload JSON file**, pick your Prometheus datasource, and choose the `job` label you scrape watchtower under.

Three dashboard annotations are pre-wired and render as vertical markers on every time-series panel:

- **Rollback** (red) — `changes(watchtower_rollbacks_total[1m]) > 0`. Reverts after failed health-check gating.
- **Watchtower restart** (blue) — `resets(watchtower_scans_total[5m]) > 0`. Daemon restart or redeploy.
- **New unmanaged container** (orange) — `delta(watchtower_containers_unmanaged[5m]) > 0`. A container without the `enable` label appeared.

Toggle individual annotation tracks from the top-right of the Grafana UI if they get noisy.

### Optional Loki integration

The dashboard ships a collapsed "Logs (requires Loki)" row at the bottom with two panels — a warn/error log-rate time-series and a tailing logs explorer — both querying `{container="watchtower"}`. At import time Grafana will prompt for a `DS_LOKI` datasource; pick **Do not save** if you don't run Loki and the rest of the dashboard still works. If you do have Loki scraping Docker container logs (e.g. via Promtail's `docker_sd_configs`), the row becomes extremely useful for correlating metric spikes with the actual log line that caused them.

## Limitations

Upstream metric shape is deliberately minimal — no per-container labels, no registry-level breakdown, no duration histograms. If you want "which container failed" you're going back to logs (and the notification stream: `WATCHTOWER_NOTIFICATIONS_LEVEL=warn`). Richer per-container telemetry is on the fork roadmap.
