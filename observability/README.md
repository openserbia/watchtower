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

Single-page dashboard: total scans, skipped scans, last-poll stats, scan rate, containers-per-poll over time. Import via **Dashboards → New → Import → Upload JSON file**, pick your Prometheus datasource, and choose the `job` label you scrape watchtower under.

## Limitations

Upstream metric shape is deliberately minimal — no per-container labels, no registry-level breakdown, no duration histograms. If you want "which container failed" you're going back to logs (and the notification stream: `WATCHTOWER_NOTIFICATIONS_LEVEL=warn`). Richer per-container telemetry is on the fork roadmap.
