# Why this fork?

`containrrr/watchtower` is the de-facto Docker auto-updater, but
the [upstream repository stopped accepting changes in late 2024](https://github.com/containrrr/watchtower/discussions/2135).
`openserbia/watchtower` exists to keep the project alive with a modern toolchain and fixes driven by real-world homelab
usage.

## Requirements

**Docker Engine 20.10 or newer.** Watchtower auto-negotiates the Docker API version with the daemon, so older engines
may work but aren't tested and are out of scope for bug reports.

## Drop-in compatible

Swap the image name and you're done. The fork deliberately preserves:

- **CLI flags** — every upstream flag and environment variable still works.
- **Labels** — `com.centurylinklabs.watchtower.*` are unchanged (enable, scope, lifecycle hooks, etc.).
- **HTTP API** — `/v1/update` and `/v1/metrics` behave identically, same token-gating.
- **Notification backends** — shoutrrr, email, Slack, MS Teams, Gotify, and the legacy shims.

No config migration. No flag rename. No label rewrite.

## What changed

### Project health

|                                  | `containrrr/watchtower`            | **`openserbia/watchtower`**                                                                 |
|----------------------------------|------------------------------------|---------------------------------------------------------------------------------------------|
| Maintenance status               | Archived / unmaintained            | **Active**                                                                                  |
| Go version                       | 1.20                               | **1.26**                                                                                    |
| Linter                           | golangci-lint v1                   | **golangci-lint v2** (gofumpt + gci)                                                        |
| Dev environment                  | Ad-hoc                             | **Devbox-pinned** (reproducible, matches CI)                                                |
| Tests                            | `go test`                          | **`go test -race` by default** (CGO-enabled lane)                                           |
| Module path                      | `github.com/containrrr/watchtower` | `github.com/openserbia/watchtower`                                                          |
| Dependency updates               | Stale                              | Tracked via Dependabot                                                                      |
| CI                               | Travis-era workflows               | **Devbox + go-task on GitHub Actions**                                                      |
| Knowledge graph for contributors | —                                  | [`code-review-graph`](https://github.com/openserbia/code-review-graph) MCP support wired in |
| Shoutrrr library                 | `containrrr/shoutrrr` (paused)     | **`nicholas-fedor/shoutrrr`** v0.15 (active fork, URL-compatible)                           |

### Update behavior

|                                                        | `containrrr/watchtower`                                                                                          | **`openserbia/watchtower`**                                                                                                                                                                                                                                                          |
|--------------------------------------------------------|------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Update strategy                                        | Global `--rolling-restart` boolean only                                                                          | **`--update-strategy`** enum (`recreate` / `rolling-restart` / `blue-green`), per-container override via the `com.centurylinklabs.watchtower.update-strategy` label; `--rolling-restart` kept as a deprecated alias                                                                  |
| Zero-downtime deploys                                  | —                                                                                                                | **`--update-strategy=blue-green`** brings the new container up alongside the old, waits for `HEALTHCHECK` healthy, drains, then retires the old (behind a dynamic label-based reverse proxy, no host ports)                                                                          |
| Health-check gating                                    | —                                                                                                                | **`--health-check-gated`** with auto-rollback; per-container label override; cooldown                                                                                                                                                                                                |
| Registry retry                                         | None — single request, then bail                                                                                 | **Bounded exp backoff** (3 tries, 500 ms → 4 s + jitter) on network / 5xx / 429 / oauth flake                                                                                                                                                                                        |
| Docker daemon retry                                    | None — single request, then abort the scan                                                                       | **Bounded exp backoff** on `ListContainers` for transient daemon errors (restart, socket blip, engine 5xx)                                                                                                                                                                           |
| Bearer-token auth                                      | One exchange per image                                                                                           | **In-memory cache** keyed on auth URL + credential, respects `expires_in`                                                                                                                                                                                                            |
| Local rebuild detection                                | Wait for next poll (up to `--interval`)                                                                          | **`--watch-docker-events`** subscribes to the Docker event stream and fires a targeted scan within seconds of `docker build -t app:latest .`                                                                                                                                         |
| Local builds on containerd image store                 | Noisy `pull access denied for app` on every poll (heuristic ignores containerd-snapshotter RepoDigest synthesis) | **Reads the daemon's per-image `Identity` provenance record** on API v1.53+ (client auto-upgrades past the SDK's DefaultVersion cap) with a bare-name pull-error safeguard for older daemons                                                                                         |
| Stuck on GC'd source image                             | Container becomes un-updatable (upstream#1217)                                                                   | **Fallback to image-reference inspection** — update proceeds cleanly                                                                                                                                                                                                                 |
| `--cleanup` after retag                                | Deletes replacement image (upstream#966)                                                                         | Targets the original image via `SourceImageID()`; `NotFound` treated as success                                                                                                                                                                                                      |
| `--cleanup` vs. shared base image                      | Force-removes image even when another active container references it (next restart fails with `No such image`)   | **Defers removal** when any non-recreated container in the scan still references the image                                                                                                                                                                                           |
| Compose-deploy race                                    | Aborts the scan on container NotFound                                                                            | Skipped container, scan continues                                                                                                                                                                                                                                                    |
| Mid-scan container vanish (stop)                       | Untyped error aborts the iteration; restart then collides with the Compose-created replacement                   | **Typed `ErrContainerNotFound`** — marked Skipped in the report, no restart attempt, scan continues                                                                                                                                                                                  |
| Compose `service_completed_successfully` init siblings | Honored only by `docker compose up`; Watchtower-driven updates silently bypass them ("new code, old schema")     | **`--rerun-init-deps`** re-executes each init sibling against the resolved new digest *before* recreating the target; old container keeps serving while the init runs; failed digests cached in-process so a broken image isn't retried until the registry serves a different digest |

### Security

|                                 | `containrrr/watchtower`              | **`openserbia/watchtower`**                                                                                                                                         |
|---------------------------------|--------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Registry TLS (default)          | `InsecureSkipVerify: true` hardcoded | **Strict TLS 1.2+, system trust store**                                                                                                                             |
| Registry TLS (opt-out)          | —                                    | **`--insecure-registry`** per host, **`--registry-ca-bundle`** for private CAs                                                                                      |
| Bearer-token comparison         | `!=` (timing-sensitive)              | **`crypto/subtle.ConstantTimeCompare`**                                                                                                                             |
| Release signatures & provenance | Unsigned (SHA256 checksums only)     | **Keyless cosign signatures** (Sigstore OIDC, no keys) on images + `checksums.txt`, plus **SLSA build provenance** and a **CycloneDX SBOM** attached to every image |
| Runtime image base              | `scratch` + hand-copied certs        | **digest-pinned `gcr.io/distroless/static-debian13`** (maintained CA bundle + tzdata, minimal, scannable)                                                           |

### Observability

|                                | `containrrr/watchtower`            | **`openserbia/watchtower`**                                                                                                                                        |
|--------------------------------|------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| HTTP API endpoints             | `/v1/update`, `/v1/metrics`        | + **`/v1/audit`** (JSON watch-status report) + **`--http-api-metrics-no-auth`** for public scrape                                                                  |
| Prometheus metrics             | 5 (scan cycle only)                | **~20** (request / registry / Docker-API / auth-cache counters; fallback & rollback counters; poll-duration histogram; watch-status gauges; infrastructure bucket) |
| Ready-to-ship dashboard        | —                                  | **Grafana JSON + Prometheus alerts** under [`observability/`](https://github.com/openserbia/watchtower/tree/main/observability)                                    |
| Unmanaged-container visibility | Silent skip under `--label-enable` | **`--audit-unmanaged`** (change-detecting logs) + `/v1/audit` + `watchtower_containers_unmanaged` gauge + shipped alert                                            |

## Images and module path

- **Docker Hub:** [`openserbia/watchtower`](https://hub.docker.com/r/openserbia/watchtower)
- **GHCR:** [`ghcr.io/openserbia/watchtower`](https://github.com/openserbia/watchtower/pkgs/container/watchtower)
- **Go module:** `github.com/openserbia/watchtower`

Multi-arch images (amd64, arm64, arm/v6, arm/v7, 386, riscv64) live under the same `:latest` / `:<version>` tag — Docker
picks the right variant for your host.

## Versioning

This fork picks up the upstream version line: `v1.7.1` was upstream's last tag, so the fork starts at `v1.8.0`. Semver
applies — patch bumps for fixes and dep updates, minor for behavior-preserving additions, and `v2.0.0` will signal the
first intentional break of upstream compatibility (CLI flags, labels, or HTTP API).

## Upstream bugs this fork has fixed

Concrete repairs for issues left open on `containrrr/watchtower` when it was archived. Examples:

- [upstream#966](https://github.com/containrrr/watchtower/issues/966) — `--cleanup` deletes the freshly-pulled
  replacement image and logs `conflict: unable to delete ... image is being used by running container`.
- [upstream#1217](https://github.com/containrrr/watchtower/issues/1217) — nil-pointer panic in `Container.ImageID()`
  when a container's source image has been garbage-collected.
- [upstream#1413](https://github.com/containrrr/watchtower/issues/1413) —
  `Unable to update container: Error: No such image` loop that permanently wedges the affected container.
- **`ContainerCreate` "No such image: name:latest" race after stop+remove** — the recreate referenced the tag rather
  than the digest resolved by `IsContainerStale`, so a CI rebuild that briefly untagged `name:latest` between scan and
  recreate caused `ContainerCreate` to fail *after* the old container had already been killed and removed. The service
  stayed down with no automatic recovery. Fixed by threading the resolved digest onto the container (`SetTargetImageID`)
  and re-binding the original tag to that digest via `ImageTag` just before `ContainerCreate`, so the create call still
  uses the human-readable tag (and every downstream reader of `Config.Image` keeps working) but is immune to a registry
  tag that has moved or vanished in the gap. Belt-and-braces: `--cleanup` now defers image removal by one generation per
  container, so the previous image stays on disk as a manual recovery target if a recreate ever fails for a reason
  pinning can't cover.
- **Malformed port bindings produced an opaque "invalid port range: value is empty" failure on recreate, after the old
  container was already gone** — a compose file with `ports: ["8080:"]` (or any entry that resolved to an empty port
  number) passed through `VerifyConfiguration` untouched and only surfaced inside `ContainerCreate`, by which point the
  previous container had been stopped and removed. `VerifyConfiguration` now strips the malformed entries from
  `HostConfig.PortBindings` and `Config.ExposedPorts` and logs a warn that names the offending key and container, so the
  create proceeds with the remaining bindings instead of dropping the service. Borrowed
  from [nicholas-fedor/watchtower#1478](https://github.com/nicholas-fedor/watchtower/pull/1478).
- **A hung Docker daemon could wedge the scan loop forever** — every call into the daemon used `context.Background()`,
  so a socket that accepted the connection but never responded (partial engine upgrade, wedged containerd snapshotter,
  daemon under extreme load) could block a single API call indefinitely with no recovery. Each operation now runs under
  a `context.WithTimeout` sized to its category (2 min for management calls, 5 min for `ImageRemove`, 30 min for the
  staleness path that streams the pull); `StopContainer` budgets `2 × stop-timeout + 2m` so both wait phases plus the
  in-loop inspect have full headroom. Healthy daemons see no change; hung daemons surface a deadline error and the next
  poll retries cleanly. Adapted
  from [Marrrrrrrrry/watchtower](https://github.com/Marrrrrrrrry/watchtower/commit/69616e64c479af8a8472d1db5722e96bbb524225)
  with revised per-call budgets — their 60 s in `IsContainerStale` would have cut healthy multi-GB pulls off mid-stream.
- **Pull failures reported over the JSON stream were silently swallowed** — Docker delivers pull progress *and* pull
  failures over the same newline-delimited JSON stream that `ImagePull` returns, so a 401/404 on the manifest, a layer
  that 500s mid-download, or a partial-content abort arrives as a `JSONMessage` error field, not as the immediate error
  from the `ImagePull` call. `PullImage` drained that stream with `io.ReadAll` only to keep the daemon from aborting the
  pull, then returned `nil` regardless of content — a mid-stream failure was reported as a *successful* pull, and the
  stale container was recreated against a half-pulled image or (with no new digest) looked perpetually "up to date". The
  stream is now drained through `jsonmessage.DisplayJSONMessagesStream`, which completes the pull and returns the
  in-stream error, routed through the existing `classifyPullError` so the registry classification and local-build
  safeguard still apply.
- **Locally-built bare-name images churned `pull access denied` on every poll under the containerd image store** —
  `docker build -t app:latest .` on the containerd image store gives the image a `docker.io/library/app` RepoDigest, so
  the staleness pull hits Docker Hub for a repository that does not exist. Hub answers a non-existent
  `docker.io/library/<bare-name>` repo with a **401 unauthorized**, not a 404, but the local-build heuristic only
  recognized the 404 not-found case — so the 401 fell through as a real auth failure and the container logged
  `pull access denied` every poll. The heuristic now also treats the unauthorized signal as "locally built" for
  references that lack a registry hostname, while hostname-qualified references (`ghcr.io/foo/bar`, `registry:5000/x`)
  still fail loudly, so typos and broken private-registry credentials aren't masked. Complements the daemon-`Identity`
  -provenance fast path noted in the table above (the safeguard for older daemons that don't expose it).
- **A self-update that failed mid-recreate leaked an orphan and could storm notifications** — when `ContainerCreate` had
  already produced the new container but a later step failed (network attach error, `start` failure), the orphan was
  left behind; on a self-update it occupies the `/watchtower` name and wedges every subsequent poll with a name
  conflict. `StartContainer` now force-removes the container it just created when a later step fails (best-effort,
  strictly scoped to that one ID). Separately, a wedged self-update re-enters the failed-start branch every poll with
  the same error, and each `Error` became a notification via the logrus hook — a per-poll storm. The self-update path
  now dedups identical `(container, error-signature)` start failures: the first occurrence (and any genuinely different
  failure) notifies, identical repeats inside a one-hour cooldown drop to `Debug`; the cache resets on restart so a
  `docker restart watchtower` surfaces the next failure loudly.

- **A self-update could wedge on a `"/watchtower" name is already in use` conflict** — if a stale watchtower container
  from a previous interrupted self-update still held the canonical name, the recreate's `ContainerCreate` failed with a
  name conflict that re-fired every poll until the next restart (the old self that would clean it up is already gone).
  `StartContainer` now detects this mid-recreate: on a conflict during a watchtower self-update it inspects whoever
  holds the name and, only when that holder is a *different watchtower-labeled* container, force-removes it and retries
  the create once. Scoped narrowly — an operator's container or a Compose recreate that races the name is left
  untouched, and the container being recreated from is never removed.

- **A self-update could strand the live watchtower under an unrecoverable random name** — the rename-and-respawn renamed
  the running self to an opaque 32-character `util.RandName()` before recreating the replacement. That name embeds
  nothing, so once it crept in — a non-Compose `docker run --name watchtower` with no `com.docker.compose.service` label
  to recover from, a respawn that failed *after* the rename, or a `--no-restart` cycle that renamed without respawning —
  the operator-chosen name existed nowhere recoverable, and watchtower could run indefinitely as e.g.
  `pmGEucoAmWufDGCRjdiooekxKbtHMNkU` with no `watchtower` container at all. The outgoing self is now renamed to a
  *structured* temp name that embeds the canonical name, `<canonical>-wt-self-XXXXXXXX` (the twin of the blue-green
  `<canonical>-wt-bluegreen-XXXXXXXX` pattern), so the real name is always recoverable from the daemon-side container
  name with zero dependence on Compose labels or `os.Hostname()`-to-short-ID matching. The next poll re-derives the
  canonical name from the temp name, a `CleanupOrphanSelf` startup sweep promotes a stranded self (or removes it when
  the canonical self already exists), and a failed respawn renames the live self straight back. The fragile
  Compose-label rescue and the post-create re-rename — both defeated by the cases above — are removed. `--no-restart`
  now skips the self-rename entirely (mirroring the blue-green `NoRestart` short-circuit). The `-wt-self-` suffix is a
  reserved container-name pattern, like `-wt-bluegreen-`.

- **A Docker daemon outage mid-scan crashed watchtower with a nil-pointer panic** — when the daemon (or a socket proxy
  in front of it) became unreachable during a poll, e.g. it was being upgraded and answered `503 Service Unavailable`,
  `actions.Update` bailed early and returned a `nil` report alongside the error. The scheduled, HTTP `/v1/update`, and
  event-triggered callers logged the error but fed the `nil` report straight into `metrics.NewMetric`, which
  dereferenced it (`report.Scanned()`) and panicked with a `SIGSEGV`. Because the scan runs inside a `robfig/cron`
  goroutine with no recovery, the panic took down the entire process — a transient daemon blip killed watchtower until
  its restart policy brought it back. `NewMetric` now treats a `nil` report as an empty scan, so a failed update cycle
  records zero counts and the poll loop survives.

See [CHANGELOG.md](https://github.com/openserbia/watchtower/blob/main/CHANGELOG.md) for the full list per release.

## Reliability and performance improvements

Not upstream-bug repairs — additions that harden the same feature set.

### Registry and auth (v1.9)

- **Bounded exponential backoff on registry HTTP calls.** The oauth challenge, bearer-token exchange, and manifest HEAD
  retry up to 3 times (500 ms → 4 s + jitter) on network errors, 5xx, 429, and the 401/403/404 flakes seen on registry
  oauth endpoints under load. Previously a single transient failure wedged the affected image until the next poll.
- **In-memory bearer-token cache.** A poll across N containers on the same registry+repository scope now issues one
  token exchange instead of N. Keyed by auth URL + credential hash, respects the registry's `expires_in` (default 60 s
  per the Docker token spec) minus a 10 s skew. Cuts registry traffic dramatically on larger deployments and further
  reduces oauth-flake exposure.

### Safer updates (v1.10)

- **`--health-check-gated` with automatic rollback.** After creating the replacement container, Watchtower waits up to
  `--health-check-timeout` for `State.Health.Status == healthy`. If the container never reaches healthy (or reports
  unhealthy), the replacement is stopped and the previous image is restarted from preserved config. The rollback is
  itself health-gated — if the previous image is also broken, Watchtower logs `rollback_failed=true` and leaves the
  container in place for manual intervention rather than tearing the service down. A per-container rollback cooldown
  prevents the stop/start/fail loop from thrashing when an image author keeps pushing broken versions. Per-container
  label override (`com.centurylinklabs.watchtower.health-check-timeout=5m`) and a smart default derived from the image's
  own `HEALTHCHECK` (start_period + retries × (interval + timeout)) mean the gate scales across fast-boot and slow-boot
  services alike.
- **`--insecure-registry` and `--registry-ca-bundle`.** Upstream unconditionally set `InsecureSkipVerify: true` for
  every registry digest check — a long-standing security weakness. The fork enforces strict TLS (1.2+, system trust
  store) by default and offers per-host opt-outs: list hosts in `--insecure-registry` to skip verification for specific
  registries, or load additional CAs via `--registry-ca-bundle` (extends the system roots rather than replacing them).

### Operational visibility (v1.10 – v1.11)

- **Ship-ready Grafana dashboard + Prometheus alerts.** `observability/grafana/watchtower-dashboard.json` and
  `observability/prometheus/alerts.yml` drop into any Prometheus + Grafana stack: scan overview, watch-status donut,
  reliability/security row (poll p50/p95, registry outcomes, API error breakdown, Docker API errors, bearer-cache hit
  rate, image-fallback count), optional Loki logs row, and three annotation tracks for rollbacks/restarts/newly-appeared
  unmanaged containers. Six production-tuned alerts cover scheduler wedges, sustained failures, registry errors, and
  Docker API errors.
- **Poll-interval-aware staleness alert.** `watchtower_poll_interval_seconds` is published at startup from the active
  schedule, so the `WatchtowerScansStopped` alert uses `(time() - last_scan) > 2 × poll_interval` instead of a hardcoded
  window. Works equally for 60-second polls and 12-hour cadences.
- **`GET /v1/audit` endpoint.** JSON report of every container classified as `managed` / `excluded` / `unmanaged` /
  `infrastructure`. Pull-model alternative to log-based auditing — operators can `curl | jq` during incident response or
  wire to a dashboard. Token-gated like `/v1/update`.
- **`--http-api-metrics-no-auth`.** Matches Prometheus convention for trusted-network scraping — `/v1/metrics` can be
  exposed without bearer-token plumbing while `/v1/update` stays mandatorily token-gated.
- **Change-detecting `--audit-unmanaged`.** With `--label-enable` active, warns once when a container first appears
  without the `enable` label and stays silent on subsequent polls unless the set changes. Orders of magnitude less log
  noise than upstream's would-be behavior for steady-state homelabs.
- **Infrastructure bucket.** Docker's own scaffolding containers (`moby/buildkit*`, `docker/desktop-*`, anything
  labelled `com.docker.buildx.*` / `com.docker.desktop.*`) are classified separately from unmanaged workloads, so
  ephemeral buildx builders stop inflating the audit count every `docker buildx build`.
- **~20 Prometheus metrics** covering every HTTP-facing surface: API requests by endpoint + status, registry calls by
  host + operation + outcome, retry counts, Docker API errors by operation, bearer-cache hits/misses, image-fallback
  count, last-scan timestamp, and a poll-duration histogram. See [Metrics](metrics.md) for the full list.

### Security hardening (v1.11)

- **Constant-time bearer-token comparison.** `api.RequireToken` now uses `crypto/subtle.ConstantTimeCompare` instead of
  `!=`, closing a theoretical timing-oracle on the `/v1/*` endpoints.
- **Strict TLS by default** (noted above under v1.10).

### Compose-aware init reruns (v1.14)

- **`--rerun-init-deps` honors `service_completed_successfully`.** Compose's
  `depends_on: { condition: service_completed_successfully }` is evaluated only by `docker compose up`, never by
  Watchtower's container-level update loop. Stacks that moved bootstrap (goose schema migrations, seed jobs, anything
  one-shot before the long-running service) into a sibling init container therefore silently regressed to "new code, old
  schema" on every Watchtower-driven restart. The new opt-in flag closes the gap:
    - When a target with `service_completed_successfully` deps becomes stale, Watchtower re-executes each declared init
      sibling against the *new* image first; only after every init exits 0 does it stop+recreate the target. Old
      container keeps serving traffic the entire time.
    - On non-zero exit the failed init is left in `Exited(N)` for `docker logs` inspection, the new digest is cached in
      an in-process rejected-digest map, and the target keeps its old image until the registry serves a different
      digest (operator pushed a fix). The rejected cache evicts naturally on Watchtower restart, so a
      `docker restart watchtower` is the manual "retry once" lever.
    - Same-image init containers (the common `migrate` service using the same image tag as the target) inherit the
      target's freshly-resolved digest so both run against identical bits; different-image inits (e.g.
      `pg-ready: postgres:18`) keep their own pinning untouched.
    - Migrations must be backwards-compatible with the previous image — the old container keeps serving while the init
      runs, so the schema in between must be readable by both versions. Standard expand-then-contract practice; warn in
      the flag's help text.
    - Independent of `--compose-depends-on`; both can be enabled together. New package `internal/initrerun`, new client
      method `RerunInitContainer` (unconditional start that bypasses the `IsRunning()` gate so `Exited(0)` init
      containers actually run again), new parser `Container.ComposeInitDependencies()` (preserves the
      `service_completed_successfully` filter that `ComposeDependencies()` intentionally strips for the sorter).

### Zero-downtime blue-green deploys (v1.15)

- **`--update-strategy=blue-green` brings up the new container alongside the old, then cuts over.**
  Upstream's only update model is stop-then-recreate — there is always a gap between stopping the old
  container and the new one being ready, so the service is down for that window. `--rolling-restart`
  softens batch updates and `--health-check-gated` rolls back a broken image, but neither gives true
  zero-downtime for a single service. The new `--update-strategy` flag adds a **blue-green** strategy
  (alongside the existing `recreate` default and `rolling-restart`, which the deprecated
  `--rolling-restart` boolean now aliases). For a stale container marked blue-green it:
    - Starts a "green" copy from the old container's config + labels under a temporary unique name, so
      both run side by side on the new image. With explicit `traefik.http.routers.*` /
      `traefik.http.services.*` labels, the reverse proxy treats blue and green as one service with two
      backends.
    - Waits for green's Docker `HEALTHCHECK` to report `healthy` (reusing `--health-check-timeout` and
      its per-container label). No `HEALTHCHECK` ⇒ warns and relies on the drain window only.
    - Lets a drain window elapse (`--blue-green-drain`, default `10s`; per-container override
      `com.centurylinklabs.watchtower.blue-green.drain`) so the proxy registers green and in-flight
      requests on blue finish.
    - Stops blue, renames green to blue's canonical name, runs the post-update hook, and rotates the
      old image out under `--cleanup`.
    - On a failed health check: removes green, leaves blue serving, records the rollback
      (`watchtower_rollbacks_total`) and the post-rollback cooldown.
    - Opt in per container via `com.centurylinklabs.watchtower.update-strategy=blue-green`. It requires a
      dynamic label-based reverse proxy (Traefik) on the Docker network with explicit (not name-derived)
      router/service labels and **no published host ports** (two copies can't bind the same host port —
      such containers fall back to `recreate` with a warning), and is unsafe for stateful services
      (DBs, queues) because both copies receive traffic during the drain. Default stays `recreate`, so
      nothing changes for existing users. See
      [Container selection → Update strategy](container-selection.md#update-strategy-blue-green) for a
      worked Compose + Traefik example.

### Startup capability preflight (v1.15)

- **`--preflight` probes the Docker API capabilities Watchtower needs before scheduling.** Running behind
  a [socket proxy](required-capabilities.md) (e.g. `tecnativa/docker-socket-proxy`) hardens the daemon surface, but a
  too-tight allow-list otherwise only reveals itself mid-update — *after* the old container has been killed and removed
  and the recreate hits a blocked endpoint. The opt-in `--preflight` flag closes that gap: at startup it issues one
  side-effect-free probe per required endpoint (a request against a deliberately bogus target — nothing is created,
  started, or removed) and aborts with an actionable error before any poll runs. It distinguishes **present** (daemon
  answered with a logical not-found/bad-request/conflict), **blocked** (a proxy returned 403), and **unreachable** (
  transport error), and the abort message names *both* the Docker endpoint and the socket-proxy variable for the first
  failing capability, so the fix is a one-shot allow-list edit. The required set is derived from the active flags —
  image pull is skipped under `--no-pull`, the recreate write set under `--monitor-only`, image removal only with
  `--cleanup`, exec only when a watched container declares a lifecycle label — so the probe never demands more than the
  run will use. The `/events` stream is treated as an optional accelerator and only warns.
  See [Required capabilities](required-capabilities.md) for the full catalog and a ready-to-paste socket-proxy
  environment block.

### Supply-chain hardening (v1.15)

- **Published images are signed, carry build provenance and an SBOM, and run on a minimal distroless base.** The runtime
  image moved from `scratch` (with a hand-rolled Alpine cert stage) to a *
  *digest-pinned `gcr.io/distroless/static-debian13`** — a maintained base that bundles `ca-certificates` and `tzdata`,
  stays tiny (~26 MB with the binary), and is actually scannable. Every release — both tagged `vX.Y.Z` and the rolling
  `:latest-dev` — is **signed with keyless cosign** (GitHub Actions OIDC → Sigstore Fulcio/Rekor, no long-lived keys to
  manage or leak), and each multi-arch image carries a **SLSA build provenance** attestation and a **CycloneDX SBOM** (
  BuildKit `--provenance` / `--sbom`); the GoReleaser `checksums.txt` is cosign-signed too. Consumers can answer all
  three supply-chain questions — *who built it* (signature), *how* (provenance), and *what's inside* (SBOM) — and verify
  them with `cosign verify` / `cosign verify-blob` / `docker buildx imagetools inspect`;
  see [Verifying a release](https://github.com/openserbia/watchtower#verifying-a-release). Releases are additionally
  gated in CI by `govulncheck` (Go vulnerability DB), Trivy, and Snyk. The distroless base is not published for
  `linux/386` or `linux/arm/v6`, so those two platforms are dropped from the **container image** — the binary `tar.gz`
  archives still ship for them.

## Known rough edges (fork roadmap)

Contributions welcome. The list is currently empty — the original "`:latest` everywhere means a broken upstream push
reaches prod in one poll interval" rough edge was substantially addressed in v1.12.0 by `--image-cooldown` (supply-chain
grace window: defer applying a pulled image until its digest has been stable for N) and `--health-check-gated` (revert
to previous image if the replacement isn't healthy). Together they turn the classic fast-broken-push scenario into a
non-event: operators who enable both can afford the one-poll detection gap because the gate holds the update and, if one
slips through, the rollback catches it.

See [CHANGELOG.md](https://github.com/openserbia/watchtower/blob/main/CHANGELOG.md) for rough edges that have already
been fixed (auth-flake backoff, pull-failure log levels, label fail-open audit, compose-deploy races, GC'd source
images, blanket TLS skip, unmanaged-audit spam, buildkit noise, and more).

## Migrating from upstream

```diff
 services:
   watchtower:
-    image: containrrr/watchtower:latest
+    image: openserbia/watchtower:latest
     volumes:
       - /var/run/docker.sock:/var/run/docker.sock
```

That's the whole migration for the common case. If you pin a specific version, the fork resumes the version line at
`v1.8.0` and later.
