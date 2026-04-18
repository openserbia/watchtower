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
| `watchtower_rollbacks_total` | counter | Update rollbacks triggered by `--health-check-gated`. |
| `watchtower_api_requests_total` | counter | HTTP requests to `/v1/*` endpoints. Labels: `endpoint`, `status`. |
| `watchtower_registry_requests_total` | counter | Outbound registry calls. Labels: `host`, `operation` (`challenge`/`token`/`digest`), `outcome` (`success`/`error`/`retried`). |
| `watchtower_registry_retries_total` | counter | Bounded-backoff retry attempts. Labels: `host`. |
| `watchtower_docker_api_errors_total` | counter | Errors from the Docker engine API. Labels: `operation` (`list`/`inspect`/`kill`/`start`/`create`/`remove`/`image_inspect`/`image_remove`/`image_pull`/`rename`/`network_connect`/`network_disconnect`). |
| `watchtower_auth_cache_hits_total` / `watchtower_auth_cache_misses_total` | counter | Bearer-token cache (v1.9+) hit/miss counts. |
| `watchtower_image_fallback_total` | counter | Times `GetContainer` fell back to inspecting by image reference because the source image ID was missing locally. |
| `watchtower_last_scan_timestamp_seconds` | gauge | Unix epoch of the latest completed scan. |
| `watchtower_poll_duration_seconds` | histogram | Wall-clock duration of each scan + update cycle. Buckets: 0.5s → 5m. |

The gauges are reset each poll, so they answer "what did the last run do?" rather than "how many containers exist".

## Prometheus alerts — `prometheus/alerts.yml`

Four rules, tuned for homelab cadence (hours, not seconds):

- **`WatchtowerScansStopped`** — scheduler wedged, no polls in 2h.
- **`WatchtowerAllScansSkipped`** — HTTP-API-driven updates are blocking scheduled ones; poll interval likely too short.
- **`WatchtowerFailuresSustained`** — at least one container has been stuck in failure for 30m. The most actionable alert.
- **`WatchtowerScansWithoutUpdates`** — scans run but nothing updates for a week. Expected for digest-pinned stacks; suspicious for `:latest`-everywhere.

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

## Limitations

Upstream metric shape is deliberately minimal — no per-container labels, no registry-level breakdown, no duration histograms. If you want "which container failed" you're going back to logs (and the notification stream: `WATCHTOWER_NOTIFICATIONS_LEVEL=warn`). Richer per-container telemetry is on the fork roadmap.
