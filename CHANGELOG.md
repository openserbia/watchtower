# Changelog

All notable changes to this fork are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Cross-references with a leading `upstream#` link to issues on
[containrrr/watchtower](https://github.com/containrrr/watchtower/issues) that
this fork has addressed (upstream archived in late 2024 without shipping a fix).

## [Unreleased]

## [1.11.1] - 2026-04-18

### Added
- **`infrastructure` audit bucket** — containers matching Docker-managed
  scaffolding (image prefixes `moby/buildkit*` / `docker/desktop-*`, label
  prefixes `com.docker.buildx.*` / `com.docker.desktop.*`) are now
  classified as `infrastructure` instead of `unmanaged`. Silences the
  recurring audit warning every `docker buildx build` caused by the
  ephemeral `buildx_buildkit_*` container. Exposed via:
  - New `watchtower_containers_infrastructure` Prometheus gauge.
  - New `"status": "infrastructure"` entries in `GET /v1/audit`.
  - New fourth slice on the Grafana watch-status donut + stacked history.
  - New `Container.IsInfrastructure()` method on the `types.Container`
    interface for programmatic use.

## [1.11.0] - 2026-04-18

### Added
- **`--http-api-audit`** flag (env: `WATCHTOWER_HTTP_API_AUDIT`) + new
  `GET /v1/audit` endpoint that returns a JSON report of every container
  classified as `managed` / `excluded` / `unmanaged`. Pull-model alternative
  to the log-based `--audit-unmanaged` for scripts, dashboards, and
  post-deploy verification. Token-gated like `/v1/update`.
- **Three new Prometheus gauges** — `watchtower_containers_managed`,
  `watchtower_containers_excluded`, `watchtower_containers_unmanaged` —
  published every poll regardless of whether any audit flag is set, so the
  Grafana dashboard shows the watch-status breakdown at a glance. Dashboard
  (`observability/grafana/watchtower-dashboard.json`) adds a donut, a
  stat-with-threshold for unmanaged, and a stacked history panel. Alerts
  add `WatchtowerUnmanagedContainersPresent` (info, >1 h).
- **`watchtower_poll_interval_seconds`** gauge — configured scan cadence
  derived from the active schedule at startup. Replaces the hardcoded 2 h
  window in the `WatchtowerScansStopped` alert with
  `(time() - last_scan) > 2 × poll_interval`, so long-cadence deployments
  (e.g. `@every 12h`) no longer false-alarm.

### Changed
- **Observability artifacts** (`observability/`) aligned with production
  tuning. Alerts trimmed to the six that have proven actionable
  (`WatchtowerRollbackTriggered`, `WatchtowerScansStopped`,
  `WatchtowerFailuresSustained`, `WatchtowerUnmanagedContainersPresent`,
  `WatchtowerRegistryErrorsSustained`,
  `WatchtowerDockerAPIErrorsSustained`); noise-heavy candidates
  (`WatchtowerAllScansSkipped`, `WatchtowerScansWithoutUpdates`,
  `WatchtowerAPIUnauthorizedBurst`) dropped. Descriptions tightened to
  single-line operational prose with `humanizeDuration` templating.
- Dashboard gains a collapsed "Logs (requires Loki)" row with two panels
  (warn/error log rate + logs explorer, both querying
  `{container="watchtower"}`). Uses a new `DS_LOKI` datasource variable
  so operators without Loki can pick "Do not save" at import time and the
  rest of the dashboard keeps working.
- **Reliability / security observability.** Seven new metrics:
  `watchtower_api_requests_total{endpoint, status}`,
  `watchtower_registry_requests_total{host, operation, outcome}`,
  `watchtower_registry_retries_total{host}`,
  `watchtower_docker_api_errors_total{operation}`,
  `watchtower_auth_cache_hits_total` / `watchtower_auth_cache_misses_total`,
  `watchtower_image_fallback_total`,
  `watchtower_last_scan_timestamp_seconds`, and
  `watchtower_poll_duration_seconds` (histogram). Dashboard gets a
  "Reliability & Security" row with poll-duration p50/p95, registry-outcome
  rate, non-2xx API request breakdown, Docker API error rate, bearer-cache
  hit ratio, and 24 h image-fallback count. Alerts add
  `WatchtowerRegistryErrorsSustained`, `WatchtowerAPIUnauthorizedBurst`,
  and `WatchtowerDockerAPIErrorsSustained`.
- **`--http-api-metrics-no-auth`** flag (env:
  `WATCHTOWER_HTTP_API_METRICS_NO_AUTH`). Exposes `/v1/metrics` without
  bearer-token auth, matching Prometheus convention for trusted-network
  scraping. `/v1/update` remains token-gated unconditionally. When only the
  (public) metrics endpoint is enabled, `--http-api-token` is no longer
  required to start the daemon.

- `--audit-unmanaged` is no longer spammy. The audit warns about each
  unlabeled container the first time it appears (startup baseline) and then
  stays silent on subsequent polls unless the set changes — a new unlabeled
  container shows up, or a previously-unlabeled one gets labeled or removed.
  Same signal, orders of magnitude less log noise for stable homelabs.

### Removed
- **`notify-upgrade` subcommand** (`cmd/notify-upgrade.go`). The helper
  generated a shoutrrr-URL env file from the pre-shoutrrr notification
  flags — a migration tool for an upstream cut-over that happened years
  ago and nobody invokes any more. The legacy `--notification-email-*` /
  `--notification-slack-*` / `--notification-gotify-*` / MSTeams flags
  remain supported via the shim in `pkg/notifications`, so existing
  deployments keep working. If you were scripting `docker run openserbia/watchtower
  notify-upgrade`, that invocation now exits with "unknown command"; either
  pin to `v1.10.x` or switch to writing shoutrrr URLs directly.

### Security
- `api.RequireToken` now uses `crypto/subtle.ConstantTimeCompare` instead of
  `!=` when checking the bearer token, closing a theoretical timing-oracle
  on `:8080`.

## [1.10.1] - 2026-04-18

### Fixed
- Internal: named the two literal `2`s used in the rollback-timeout computation
  so `golangci-lint`'s `mnd` rule stops flagging them. No user-visible behavior
  change; ships a clean lint run for the release pipeline.

## [1.10.0] - 2026-04-18

### Added
- **`com.centurylinklabs.watchtower.health-check-timeout`** label — per-container
  override for `--health-check-timeout`, accepts a Go duration string. Highest
  priority in the resolution chain (label → HEALTHCHECK-derived → global flag
  → 60s fallback).
- **Smart default** for health-check gating timeout when neither label nor
  global flag is set: derives
  `start_period + retries × (interval + timeout)` from the container's own
  `HEALTHCHECK` config (or the image's default). Believes the image author's
  declaration rather than one-size-fits-all.
- **`watchtower_rollbacks_total`** Prometheus counter — incremented whenever
  `--health-check-gated` reverts a container. Exposed via `/v1/metrics`. The
  shipped alert (`WatchtowerRollbackTriggered` in
  `observability/prometheus/alerts.yml`) fires on any non-zero 1h increase.
- **Rollback health gating + cooldown.** The rolled-back container is itself
  health-gated with a shorter timeout (half the effective). If both the new
  and old images fail, Watchtower logs at `error` with `rollback_failed=true`
  and leaves the container in place for manual intervention. After any
  rollback, the container is skipped on subsequent polls for 1 hour
  (`rollbackCooldown` in `internal/actions/update.go`) to prevent the
  stop → start → fail → rollback thrash loop when an image author keeps
  pushing broken versions.
- **`--health-check-gated`** + **`--health-check-timeout`** (envs:
  `WATCHTOWER_HEALTH_CHECK_GATED`, `WATCHTOWER_HEALTH_CHECK_TIMEOUT`,
  default 60s). Opt-in: after recreating a container, wait for its
  `State.Health.Status` to become `healthy`. If it reports unhealthy or
  times out, stop the replacement and rebuild the old container from the
  preserved config+image. Containers without a `HEALTHCHECK` skip the gate
  and emit a warning. Addresses the [upstream#1385](https://github.com/containrrr/watchtower/issues/1385)
  family ("update pulled, replaced, everything broke").
- **`--insecure-registry`** (env: `WATCHTOWER_INSECURE_REGISTRY`) — comma-separated
  list of registry hosts (`host` or `host:port`) for which TLS certificate
  verification is skipped. Replaces the previous hardcoded
  `InsecureSkipVerify: true` in `pkg/registry/digest`: verification is now
  strict (TLS 1.2+, system trust store) by default and the operator opts in
  per host.
- **`--registry-ca-bundle`** (env: `WATCHTOWER_REGISTRY_CA_BUNDLE`) — PEM file
  of additional trusted CA certificates. Extends the system trust store rather
  than replacing it, so public registries keep working while registries signed
  by a private CA also validate cleanly.
- **`observability/`** directory — ships a Grafana dashboard
  (`grafana/watchtower-dashboard.json`) and a set of Prometheus alerting rules
  (`prometheus/alerts.yml`) covering scheduler wedges, sustained failures,
  and silent-update gaps. First thing to wire up after enabling
  `WATCHTOWER_HTTP_API_METRICS`.

### Changed
- Registry HTTP calls now flow through a new `pkg/registry/transport` package
  that builds per-host `http.Client`s with the right TLS policy. The auth
  challenge and bearer-token exchange (previously using bare `http.Client{}`)
  now honor the same TLS tuning as the manifest HEAD request.

### Security
- Fixed a long-standing weakness where `pkg/registry/digest.GetDigest`
  unconditionally set `InsecureSkipVerify: true` for *all* registries, not
  just configured-insecure ones. Strict verification is now the default; the
  old behavior is available as an explicit per-host opt-in via
  `--insecure-registry`.

## [1.9.0] - 2026-04-18

### Added
- **`--audit-unmanaged`** flag (env: `WATCHTOWER_AUDIT_UNMANAGED`). With
  `--label-enable` active, warns once per poll for every container that carries
  no `com.centurylinklabs.watchtower.enable` label at all, so silent exclusions
  stop looking identical to intentional opt-outs.
- **Bounded exponential backoff** for registry HTTP calls (`pkg/registry/retry`).
  Wraps the oauth challenge, bearer-token exchange, and manifest HEAD with up to
  3 attempts (500 ms → 4 s + jitter) on network errors, 5xx, 429, and the
  401/403/404 flakes observed on registry oauth endpoints under load.
- **In-memory bearer-token cache** (`pkg/registry/auth`). Cuts registry
  authentication traffic dramatically: a poll across N containers on the same
  registry+repository scope now issues one token exchange instead of N. Keyed
  by auth URL + credential hash, respects the registry's `expires_in` (default
  60 s per the Docker token spec) minus a 10 s skew, and is concurrency-safe.
  Also reduces exposure to the oauth-endpoint flakes the retry wrapper handles.

### Fixed
- **Containers stuck un-updatable after their source image is garbage-collected.**
  When the image a container was created from has been removed locally
  (typically by a prior `--cleanup` run after the tag moved to a newer digest),
  `Container.GetContainer` now falls back to inspecting the container's image
  *reference* (e.g. `myrepo/app:latest`) instead of returning `imageInfo: nil`.
  This lets `VerifyConfiguration` pass and the update flow proceed on the
  next poll. Fixes [upstream#1217](https://github.com/containrrr/watchtower/issues/1217)
  (nil-pointer panic in `Container.ImageID()`) and
  [upstream#1413](https://github.com/containrrr/watchtower/issues/1413)
  ("Unable to update container: Error: No such image" loop).
- **`--cleanup` no longer deletes the freshly-pulled replacement image** after
  the fallback path above kicks in. Cleanup now targets `containerInfo.Image`
  (the ID Docker recorded at container creation) via the new
  `Container.SourceImageID()`, not whatever `imageInfo` currently holds.
  `RemoveImageByID` also treats `NotFound` as success so already-GC'd old
  images stop logging spurious errors. Fixes
  [upstream#966](https://github.com/containrrr/watchtower/issues/966)
  (`conflict: unable to delete <id> - image is being used by running container`).
- **Compose-deploy races** (`docker compose up` between two polls) no longer
  abort the entire scan. `ListContainers` skips containers that vanish between
  list and inspect, and `StopContainer` tolerates `NotFound` on kill.
- **Pull-failure log level raised** from `info` to `warn` in
  `actions.Update`, so operators running `WATCHTOWER_NOTIFICATIONS_LEVEL=error`
  are actually notified of stuck containers instead of the failure being
  silently swallowed.

### Changed
- `Container` interface gained `SourceImageID()` — returns the raw image ID
  Docker recorded against the container at creation time, stable across
  imageInfo fallbacks. Existing `ImageID()` / `SafeImageID()` semantics are
  unchanged.

[Unreleased]: https://github.com/openserbia/watchtower/compare/v1.11.1...HEAD
[1.11.1]: https://github.com/openserbia/watchtower/compare/v1.11.0...v1.11.1
[1.11.0]: https://github.com/openserbia/watchtower/compare/v1.10.1...v1.11.0
[1.10.1]: https://github.com/openserbia/watchtower/compare/v1.10.0...v1.10.1
[1.10.0]: https://github.com/openserbia/watchtower/compare/v1.9.0...v1.10.0
[1.9.0]: https://github.com/openserbia/watchtower/compare/v1.8.5...v1.9.0
