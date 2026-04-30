# Why this fork?

`containrrr/watchtower` is the de-facto Docker auto-updater, but the [upstream repository stopped accepting changes in late 2024](https://github.com/containrrr/watchtower/discussions/2135). `openserbia/watchtower` exists to keep the project alive with a modern toolchain and fixes driven by real-world homelab usage.

## Requirements

**Docker Engine 20.10 or newer.** Watchtower auto-negotiates the Docker API version with the daemon, so older engines may work but aren't tested and are out of scope for bug reports.

## Drop-in compatible

Swap the image name and you're done. The fork deliberately preserves:

- **CLI flags** — every upstream flag and environment variable still works.
- **Labels** — `com.centurylinklabs.watchtower.*` are unchanged (enable, scope, lifecycle hooks, etc.).
- **HTTP API** — `/v1/update` and `/v1/metrics` behave identically, same token-gating.
- **Notification backends** — shoutrrr, email, Slack, MS Teams, Gotify, and the legacy shims.

No config migration. No flag rename. No label rewrite.

## What changed

### Project health

|                                   | `containrrr/watchtower`            | **`openserbia/watchtower`**                                                                   |
| --------------------------------- | ---------------------------------- | --------------------------------------------------------------------------------------------- |
| Maintenance status                | Archived / unmaintained            | **Active**                                                                                    |
| Go version                        | 1.20                               | **1.26**                                                                                      |
| Linter                            | golangci-lint v1                   | **golangci-lint v2** (gofumpt + gci)                                                          |
| Dev environment                   | Ad-hoc                             | **Devbox-pinned** (reproducible, matches CI)                                                  |
| Tests                             | `go test`                          | **`go test -race` by default** (CGO-enabled lane)                                             |
| Module path                       | `github.com/containrrr/watchtower` | `github.com/openserbia/watchtower`                                                            |
| Dependency updates                | Stale                              | Tracked via Dependabot                                                                        |
| CI                                | Travis-era workflows               | **Devbox + go-task on GitHub Actions**                                                        |
| Knowledge graph for contributors  | —                                  | [`code-review-graph`](https://github.com/openserbia/code-review-graph) MCP support wired in   |
| Shoutrrr library                  | `containrrr/shoutrrr` (paused)     | **`nicholas-fedor/shoutrrr`** v0.14 (active fork, URL-compatible)                             |

### Update behavior

|                                   | `containrrr/watchtower`              | **`openserbia/watchtower`**                                                                 |
| --------------------------------- | ------------------------------------ | ------------------------------------------------------------------------------------------- |
| Health-check gating               | —                                    | **`--health-check-gated`** with auto-rollback; per-container label override; cooldown        |
| Registry retry                    | None — single request, then bail     | **Bounded exp backoff** (3 tries, 500 ms → 4 s + jitter) on network / 5xx / 429 / oauth flake |
| Docker daemon retry               | None — single request, then abort the scan | **Bounded exp backoff** on `ListContainers` for transient daemon errors (restart, socket blip, engine 5xx) |
| Bearer-token auth                 | One exchange per image               | **In-memory cache** keyed on auth URL + credential, respects `expires_in`                   |
| Local rebuild detection           | Wait for next poll (up to `--interval`) | **`--watch-docker-events`** subscribes to the Docker event stream and fires a targeted scan within seconds of `docker build -t app:latest .` |
| Local builds on containerd image store | Noisy `pull access denied for app` on every poll (heuristic ignores containerd-snapshotter RepoDigest synthesis) | **Reads the daemon's per-image `Identity` provenance record** on API v1.53+ (client auto-upgrades past the SDK's DefaultVersion cap) with a bare-name pull-error safeguard for older daemons |
| Stuck on GC'd source image        | Container becomes un-updatable (upstream#1217) | **Fallback to image-reference inspection** — update proceeds cleanly                     |
| `--cleanup` after retag           | Deletes replacement image (upstream#966) | Targets the original image via `SourceImageID()`; `NotFound` treated as success          |
| `--cleanup` vs. shared base image | Force-removes image even when another active container references it (next restart fails with `No such image`) | **Defers removal** when any non-recreated container in the scan still references the image |
| Compose-deploy race               | Aborts the scan on container NotFound | Skipped container, scan continues                                                          |
| Mid-scan container vanish (stop)  | Untyped error aborts the iteration; restart then collides with the Compose-created replacement | **Typed `ErrContainerNotFound`** — marked Skipped in the report, no restart attempt, scan continues |

### Security

|                                   | `containrrr/watchtower`              | **`openserbia/watchtower`**                                                                 |
| --------------------------------- | ------------------------------------ | ------------------------------------------------------------------------------------------- |
| Registry TLS (default)            | `InsecureSkipVerify: true` hardcoded | **Strict TLS 1.2+, system trust store**                                                    |
| Registry TLS (opt-out)            | —                                    | **`--insecure-registry`** per host, **`--registry-ca-bundle`** for private CAs             |
| Bearer-token comparison           | `!=` (timing-sensitive)              | **`crypto/subtle.ConstantTimeCompare`**                                                    |

### Observability

|                                   | `containrrr/watchtower`              | **`openserbia/watchtower`**                                                                 |
| --------------------------------- | ------------------------------------ | ------------------------------------------------------------------------------------------- |
| HTTP API endpoints                | `/v1/update`, `/v1/metrics`          | + **`/v1/audit`** (JSON watch-status report) + **`--http-api-metrics-no-auth`** for public scrape |
| Prometheus metrics                | 5 (scan cycle only)                  | **~20** (request / registry / Docker-API / auth-cache counters; fallback & rollback counters; poll-duration histogram; watch-status gauges; infrastructure bucket) |
| Ready-to-ship dashboard           | —                                    | **Grafana JSON + Prometheus alerts** under [`observability/`](https://github.com/openserbia/watchtower/tree/main/observability) |
| Unmanaged-container visibility    | Silent skip under `--label-enable`   | **`--audit-unmanaged`** (change-detecting logs) + `/v1/audit` + `watchtower_containers_unmanaged` gauge + shipped alert |

## Images and module path

- **Docker Hub:** [`openserbia/watchtower`](https://hub.docker.com/r/openserbia/watchtower)
- **GHCR:** [`ghcr.io/openserbia/watchtower`](https://github.com/openserbia/watchtower/pkgs/container/watchtower)
- **Go module:** `github.com/openserbia/watchtower`

Multi-arch images (amd64, arm64, arm/v6, arm/v7, 386, riscv64) live under the same `:latest` / `:<version>` tag — Docker picks the right variant for your host.

## Versioning

This fork picks up the upstream version line: `v1.7.1` was upstream's last tag, so the fork starts at `v1.8.0`. Semver applies — patch bumps for fixes and dep updates, minor for behavior-preserving additions, and `v2.0.0` will signal the first intentional break of upstream compatibility (CLI flags, labels, or HTTP API).

## Upstream bugs this fork has fixed

Concrete repairs for issues left open on `containrrr/watchtower` when it was archived. Examples:

- [upstream#966](https://github.com/containrrr/watchtower/issues/966) — `--cleanup` deletes the freshly-pulled replacement image and logs `conflict: unable to delete ... image is being used by running container`.
- [upstream#1217](https://github.com/containrrr/watchtower/issues/1217) — nil-pointer panic in `Container.ImageID()` when a container's source image has been garbage-collected.
- [upstream#1413](https://github.com/containrrr/watchtower/issues/1413) — `Unable to update container: Error: No such image` loop that permanently wedges the affected container.
- **`ContainerCreate` "No such image: name:latest" race after stop+remove** — the recreate referenced the tag rather than the digest resolved by `IsContainerStale`, so a CI rebuild that briefly untagged `name:latest` between scan and recreate caused `ContainerCreate` to fail *after* the old container had already been killed and removed. The service stayed down with no automatic recovery. Fixed by threading the resolved digest onto the container (`SetTargetImageID`) and re-binding the original tag to that digest via `ImageTag` just before `ContainerCreate`, so the create call still uses the human-readable tag (and every downstream reader of `Config.Image` keeps working) but is immune to a registry tag that has moved or vanished in the gap. Belt-and-braces: `--cleanup` now defers image removal by one generation per container, so the previous image stays on disk as a manual recovery target if a recreate ever fails for a reason pinning can't cover.
- **Malformed port bindings produced an opaque "invalid port range: value is empty" failure on recreate, after the old container was already gone** — a compose file with `ports: ["8080:"]` (or any entry that resolved to an empty port number) passed through `VerifyConfiguration` untouched and only surfaced inside `ContainerCreate`, by which point the previous container had been stopped and removed. `VerifyConfiguration` now strips the malformed entries from `HostConfig.PortBindings` and `Config.ExposedPorts` and logs a warn that names the offending key and container, so the create proceeds with the remaining bindings instead of dropping the service. Borrowed from [nicholas-fedor/watchtower#1478](https://github.com/nicholas-fedor/watchtower/pull/1478).

See [CHANGELOG.md](https://github.com/openserbia/watchtower/blob/main/CHANGELOG.md) for the full list per release.

## Reliability and performance improvements

Not upstream-bug repairs — additions that harden the same feature set.

### Registry and auth (v1.9)

- **Bounded exponential backoff on registry HTTP calls.** The oauth challenge, bearer-token exchange, and manifest HEAD retry up to 3 times (500 ms → 4 s + jitter) on network errors, 5xx, 429, and the 401/403/404 flakes seen on registry oauth endpoints under load. Previously a single transient failure wedged the affected image until the next poll.
- **In-memory bearer-token cache.** A poll across N containers on the same registry+repository scope now issues one token exchange instead of N. Keyed by auth URL + credential hash, respects the registry's `expires_in` (default 60 s per the Docker token spec) minus a 10 s skew. Cuts registry traffic dramatically on larger deployments and further reduces oauth-flake exposure.

### Safer updates (v1.10)

- **`--health-check-gated` with automatic rollback.** After creating the replacement container, Watchtower waits up to `--health-check-timeout` for `State.Health.Status == healthy`. If the container never reaches healthy (or reports unhealthy), the replacement is stopped and the previous image is restarted from preserved config. The rollback is itself health-gated — if the previous image is also broken, Watchtower logs `rollback_failed=true` and leaves the container in place for manual intervention rather than tearing the service down. A per-container rollback cooldown prevents the stop/start/fail loop from thrashing when an image author keeps pushing broken versions. Per-container label override (`com.centurylinklabs.watchtower.health-check-timeout=5m`) and a smart default derived from the image's own `HEALTHCHECK` (start_period + retries × (interval + timeout)) mean the gate scales across fast-boot and slow-boot services alike.
- **`--insecure-registry` and `--registry-ca-bundle`.** Upstream unconditionally set `InsecureSkipVerify: true` for every registry digest check — a long-standing security weakness. The fork enforces strict TLS (1.2+, system trust store) by default and offers per-host opt-outs: list hosts in `--insecure-registry` to skip verification for specific registries, or load additional CAs via `--registry-ca-bundle` (extends the system roots rather than replacing them).

### Operational visibility (v1.10 – v1.11)

- **Ship-ready Grafana dashboard + Prometheus alerts.** `observability/grafana/watchtower-dashboard.json` and `observability/prometheus/alerts.yml` drop into any Prometheus + Grafana stack: scan overview, watch-status donut, reliability/security row (poll p50/p95, registry outcomes, API error breakdown, Docker API errors, bearer-cache hit rate, image-fallback count), optional Loki logs row, and three annotation tracks for rollbacks/restarts/newly-appeared unmanaged containers. Six production-tuned alerts cover scheduler wedges, sustained failures, registry errors, and Docker API errors.
- **Poll-interval-aware staleness alert.** `watchtower_poll_interval_seconds` is published at startup from the active schedule, so the `WatchtowerScansStopped` alert uses `(time() - last_scan) > 2 × poll_interval` instead of a hardcoded window. Works equally for 60-second polls and 12-hour cadences.
- **`GET /v1/audit` endpoint.** JSON report of every container classified as `managed` / `excluded` / `unmanaged` / `infrastructure`. Pull-model alternative to log-based auditing — operators can `curl | jq` during incident response or wire to a dashboard. Token-gated like `/v1/update`.
- **`--http-api-metrics-no-auth`.** Matches Prometheus convention for trusted-network scraping — `/v1/metrics` can be exposed without bearer-token plumbing while `/v1/update` stays mandatorily token-gated.
- **Change-detecting `--audit-unmanaged`.** With `--label-enable` active, warns once when a container first appears without the `enable` label and stays silent on subsequent polls unless the set changes. Orders of magnitude less log noise than upstream's would-be behavior for steady-state homelabs.
- **Infrastructure bucket.** Docker's own scaffolding containers (`moby/buildkit*`, `docker/desktop-*`, anything labelled `com.docker.buildx.*` / `com.docker.desktop.*`) are classified separately from unmanaged workloads, so ephemeral buildx builders stop inflating the audit count every `docker buildx build`.
- **~20 Prometheus metrics** covering every HTTP-facing surface: API requests by endpoint + status, registry calls by host + operation + outcome, retry counts, Docker API errors by operation, bearer-cache hits/misses, image-fallback count, last-scan timestamp, and a poll-duration histogram. See [Metrics](metrics.md) for the full list.

### Security hardening (v1.11)

- **Constant-time bearer-token comparison.** `api.RequireToken` now uses `crypto/subtle.ConstantTimeCompare` instead of `!=`, closing a theoretical timing-oracle on the `/v1/*` endpoints.
- **Strict TLS by default** (noted above under v1.10).

## Known rough edges (fork roadmap)

Contributions welcome. The list is currently empty — the original "`:latest` everywhere means a broken upstream push reaches prod in one poll interval" rough edge was substantially addressed in v1.12.0 by `--image-cooldown` (supply-chain grace window: defer applying a pulled image until its digest has been stable for N) and `--health-check-gated` (revert to previous image if the replacement isn't healthy). Together they turn the classic fast-broken-push scenario into a non-event: operators who enable both can afford the one-poll detection gap because the gate holds the update and, if one slips through, the rollback catches it.

See [CHANGELOG.md](https://github.com/openserbia/watchtower/blob/main/CHANGELOG.md) for rough edges that have already been fixed (auth-flake backoff, pull-failure log levels, label fail-open audit, compose-deploy races, GC'd source images, blanket TLS skip, unmanaged-audit spam, buildkit noise, and more).

## Migrating from upstream

```diff
 services:
   watchtower:
-    image: containrrr/watchtower:latest
+    image: openserbia/watchtower:latest
     volumes:
       - /var/run/docker.sock:/var/run/docker.sock
```

That's the whole migration for the common case. If you pin a specific version, the fork resumes the version line at `v1.8.0` and later.
