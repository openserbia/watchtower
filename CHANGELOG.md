# Changelog

All notable changes to this fork are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Cross-references with a leading `upstream#` link to issues on
[containrrr/watchtower](https://github.com/containrrr/watchtower/issues) that
this fork has addressed (upstream archived in late 2024 without shipping a fix).

## [Unreleased]

## [1.13.0] - 2026-05-06

### Changed
- **Dependency refresh.** Routine bump of Go module dependencies (direct
  and indirect) via `go get -u ./...` + `go get -u all`: `fsnotify` v1.9.0
  → v1.10.1, `onsi/gomega` v1.39.1 → v1.40.0, `onsi/ginkgo/v2` v2.28.1 →
  v2.28.3, `docker/cli` v29.4.0 → v29.4.2,
  `docker/docker-credential-helpers` v0.9.5 → v0.9.6,
  `Masterminds/semver/v3` v3.4.0 → v3.5.0, `mattn/go-isatty` v0.0.21 →
  v0.0.22, `pelletier/go-toml/v2` v2.3.0 → v2.3.1, `klauspost/compress`
  v1.18.5 → v1.18.6. Build, tests, and lint all pass; no source changes
  required.
- **Container error logs now always include the image, and rollback logs
  carry the failure reason.** Every warn/error log in
  `internal/actions/update.go` and `pkg/container/client.go` that touches a
  container or image carries structured `container=`/`image=` fields. The
  health-check rollback line in particular adds `new_digest=` (the image
  that just failed) and `old_digest=` (the rollback target), and inspects
  the failed container *before* tearing it down to attach
  `oom_killed=`/`exit_code=`/`probe_exit_code=`/`probe_output=` so an
  operator chasing a "rolling back to the previous image" notification can
  see at a glance whether the new image OOMed, exited non-zero, or just
  failed its readiness probe — and what the probe printed — without having
  to reproduce the failure locally. Bare `log.Error(err)` calls in
  `stopStaleContainer`, `restartStaleContainer`, `cleanupImages`, the
  rollback inspect/stop paths, and `PullImage`'s response-read fallback are
  now structured the same way.

### Added
- **`--disable-memory-swappiness` for Podman / cgroupv2 hosts.** Podman with
  crun on cgroupv2 rejects the implicit `MemorySwappiness=0` Docker writes
  when the field is unset, but the inspected `HostConfig` still carries that
  `0`, so a Watchtower recreate that copies the inspected config back through
  `ContainerCreate` failed with `swappiness must be in the range [0, 100]`.
  When this flag (or `WATCHTOWER_DISABLE_MEMORY_SWAPPINESS=true`) is set,
  `MemorySwappiness` is dropped from the recreate request — Podman accepts
  the absent field. No-op on Docker hosts (opt-in). Borrowed from
  [beatkind/watchtower](https://github.com/beatkind/watchtower/commit/70426e1838cf78416faaddae4c71dc420b82199e).
- **Typed pull-error sentinels for unauthorized and not-found.** `PullImage`
  now wraps daemon errors as `ErrPullImageUnauthorized` (HTTP 401) and
  `ErrPullImageNotFound` (HTTP 404), logging auth failures at warn (so a
  rotated credential doesn't sit silent at debug) and missing manifests
  at debug (so a transient registry blip doesn't escalate to warn). The
  underlying `cerrdefs` error stays in the chain via `fmt.Errorf("%w: %w")`,
  so the existing local-build safeguard in `pullFailureLooksLocal` keeps
  firing on bare-name references — no behaviour change for the silent-skip
  path. Borrowed from
  [nicholas-fedor/watchtower#1477](https://github.com/nicholas-fedor/watchtower/pull/1477).

### Fixed
- **Bounded timeouts on every Docker daemon API call.** `pkg/container/client.go`
  used `context.Background()` everywhere, so a hung daemon (socket accepts
  the connection but never responds — common during a partial engine upgrade
  or a wedged containerd snapshotter) could block a single API call forever
  and wedge the scan loop with no recovery. Each operation now runs under a
  `context.WithTimeout`: 2 minutes for the management calls (list / inspect /
  create / kill / remove / rename / network / image-tag / start), 5 minutes
  for `ImageRemove` (snapshot teardown is disk-bound on multi-layer images),
  30 minutes for `IsContainerStale` (covers a streaming pull). `StopContainer`
  budgets `2 × stop-timeout + 2m` so both wait phases plus the in-loop
  `ContainerInspect` have full headroom, and its pre-remove wait now bails
  on parent-ctx cancellation instead of wasting the next call on a dead
  context (still forgiving about inspect blips and the timeout case so a
  graceful-stop overflow falls through to force-remove as before). Healthy
  daemons are unaffected; hung daemons surface a deadline error and the
  next poll retries. Adapted from
  [Marrrrrrrrry/watchtower](https://github.com/Marrrrrrrrry/watchtower/commit/69616e64c479af8a8472d1db5722e96bbb524225)
  with revised per-call budgets (their 60 s in `IsContainerStale` would have
  cut healthy multi-GB pulls off mid-stream).
- **Startup log reflects the actual HTTP API listen address.** When
  `--http-api-update` was enabled, the startup banner always said
  `The HTTP API is enabled at :8080.` even if the operator had bound to
  `127.0.0.1:9090` via `--http-api-host`. The log now reads from the same
  flag the API server uses, so the banner matches reality.
- **Misconfigured port bindings no longer abort a recreate.** A compose
  file with `ports: ["8080:"]` (or any entry that resolves to an empty port
  number) used to fall through to `ContainerCreate` and surface as Docker's
  opaque `invalid port range: value is empty`, leaving operators chasing a
  failure whose root cause was upstream of watchtower. `VerifyConfiguration`
  now strips empty / `"/proto"`-only entries from `HostConfig.PortBindings`
  and `Config.ExposedPorts` before the create call, logging a warn that
  identifies the offending key and the affected container. Borrowed from
  [nicholas-fedor/watchtower#1478](https://github.com/nicholas-fedor/watchtower/pull/1478).
- **`ContainerCreate` no longer races the registry tag.** The recreate
  flow handed Docker a tag (`name:latest`) for the new image, so a CI
  rebuild that briefly untagged the reference between the scan and the
  recreate would surface as `Error response from daemon: No such image:
  name:latest`. Because watchtower had already stopped *and removed*
  the old container by that point, the service stayed down until an
  operator intervened — observed in production with rebuild cadences
  faster than the poll interval. `IsContainerStale` records the resolved
  digest on the container (`SetTargetImageID`); the Docker client then
  re-binds the original tag to that digest via `ImageTag` immediately
  before `ContainerCreate`, so the create resolves the image we just
  pulled rather than whatever the registry tag happens to point at by
  the time we get there. The health-gated rollback path repoints the
  override at the *old* image's digest before restoring, so a rejected
  new build doesn't get re-created on rollback either.
  - **Why not write the digest into `Config.Image` directly.** The
    initial attempt at this fix mutated `Config.Image` to the digest
    inside `GetCreateConfig`, which read clean in the recreate path
    but quietly broke every consumer that treats `Config.Image` as a
    tag: `HasNewImage` compared the digest against itself and reported
    no change, `PullImage` short-circuited with `ErrPinnedImage`,
    `FilterByImage` split on `:` and never matched the original repo
    name on event-driven scans, and registry-auth lookups keyed by
    name. Containers recreated under that flow stopped receiving any
    further updates until manually restarted. Closing the race outside
    `Config.Image` keeps every downstream reader on the well-known tag
    contract.
- **`ImageName()` falls back to `RepoTags[0]` when `Config.Image` is a
  bare digest.** Recovery path for containers recreated by the early
  digest-in-`Config.Image` version of the fix above: their stored
  reference is `sha256:...`, and without a fallback they would remain
  stuck across watchtower restarts. The fallback returns the image's
  current canonical tag, which is what `HasNewImage`, `PullImage`,
  `FilterByImage`, and registry-auth all expect, so affected
  containers self-heal on the next poll.
- **`--cleanup` defers image removal by one generation per container.**
  The just-retired image now stays on disk as the previous-generation
  rollback target until the *next* successful update of the same
  container rotates it out. If a future recreate fails for a reason
  pinning can't cover (e.g. `docker image prune` ran in the gap), the
  prior image is still resolvable and an operator can `docker run`
  manually to restore service. Disk cost: roughly one extra image per
  watched container, until the next update of that container. The
  in-memory rotation map resets on watchtower restart, which under-
  cleans by one generation for the next update of each container —
  intentional, in favor of zero on-disk state. The `--cleanup` flag
  shape and per-image still-in-use guard are unchanged.
- **Identity-based local-build detection now actually reaches the wire.**
  The v1.12.2 Identity decoder was correct, but two stacked ceilings kept
  it from ever seeing a populated field on real daemons: the Docker
  engine only returns the `Identity` record at API v1.53+, and the
  vendored SDK's `DefaultVersion` caps API-version negotiation at 1.51.
  `NewClient` now pings the daemon after negotiation and, when the
  daemon advertises v1.53 or newer and the operator has not explicitly
  pinned `DOCKER_API_VERSION`, opportunistically raises the client
  version to v1.54 (or the daemon's reported version, whichever is
  lower). This is safe because URL version components are plain path
  tokens, typed unmarshal drops unknown JSON fields, and the one place
  we consume new fields (Identity) already uses
  `ImageInspectWithRawResponse`. Explicit pins via `DOCKER_API_VERSION`
  are still respected.
- **Pull-error safeguard for older daemons.** On engines below API
  v1.53 the `Identity` signal never appears, and on the containerd
  image store the old `len(RepoDigests) == 0` heuristic can't fire
  either. Added a narrow belt-and-braces path in `IsContainerStale`:
  when a pull fails with a not-found class error AND the image
  reference has no registry hostname (e.g. `tg-antispam:latest`, which
  the daemon can only resolve against Docker Hub), treat the image as
  locally built and fall through to `HasNewImage`. Hostname-qualified
  references (`ghcr.io/foo/bar`, `registry.local:5000/app`) never hit
  this path, so typos and broken private-registry credentials still
  surface loudly instead of being silently masked.
- **`docker_api_errors_total{operation="image_pull"}` no longer
  increments for safeguard-recovered local builds.** The metric was
  being bumped inside `PullImage` before the caller had a chance to
  decide whether the error was a real daemon problem or an expected
  registry miss for a locally-built image. Moved the increment to the
  callsite in `IsContainerStale`, gated on "not recovered by the
  bare-name safeguard". A local build the daemon correctly reports as
  absent from the registry isn't a Docker API failure — counting it as
  one was keeping `WatchtowerDockerAPIErrorsSustained` lit on hosts
  full of compose-managed local builds. Real pull failures (hostname-
  qualified refs, auth errors, 5xx) still increment as before.
- **`docker_api_errors_total` no longer increments on clean logical
  answers from the daemon.** Three more call sites were emitting the
  metric for situations the scan deliberately recovers from:
  `operation="inspect"` when a container vanishes mid-scan (the
  `ListContainers` skip-on-NotFound path handles this, routine on
  Compose recreations); `operation="image_inspect"` when the source
  image was GC'd (the image-reference fallback handles it and has
  its own `watchtower_image_fallback_total` counter — also fixed a
  double-count where primary+fallback failure both incremented);
  `operation="image_remove"` when the daemon reports the image is
  still in use by another container (common on self-update old/new
  overlap and shared-base-image races outside the scan view the
  cleanupImages deferral can see — now debug-logged and returns nil
  so the caller's error-log doesn't fire either, letting the next
  poll retry naturally). Same threat-model argument as the
  `image_pull` fix above: the alert tracks daemon *health* — socket
  permission drift, overload, partial upgrade — not logical misses.
  Introduced a `recordDaemonError` helper that accepts the
  expected-error predicates per call site (`cerrdefs.IsNotFound` for
  inspects; `cerrdefs.IsNotFound` + `cerrdefs.IsConflict` for image
  removal). Real daemon errors (connection, 5xx, auth) still count.

### Docs
- **`docs/arguments.md` corrected to reflect actual `--api-version`
  default.** The page advertised `"1.24"`; the real default has been
  the empty string (i.e. negotiate) for as long as `SetDefaults` has
  existed. Expanded the section with a migration note for operators
  who have `DOCKER_API_VERSION=1.44` left in their compose or env
  file from an older deployment — removing it is the recommended
  step on Docker 25+ to unlock the Identity-based local-build
  detection.

## [1.12.2] - 2026-04-20

### Added
- **`--version` flag on the root command.** `watchtower --version` now
  prints the compile-time version (the same `internal/meta.Version`
  value used in the HTTP `User-Agent`) without having to start the
  daemon or `docker inspect` the image. Makes support triage and image
  tag verification a one-liner.
- **New Prometheus counter `watchtower_docker_api_retries_total{operation}`.**
  Pairs with the new `ListContainers` retry (see below) and follows the
  shape of `watchtower_registry_retries_total`. One increment per retry
  attempt, not per failed call. A sustained non-zero value points at a
  flaky daemon — daemon restarts during polls, socket-proxy blips, or
  engine-API 5xx under load.

### Changed
- **`WATCHTOWER_NOTIFICATIONS_LEVEL` is now honored in report mode.**
  Before, the default report template fired a per-poll summary
  regardless of level — a strict `NOTIFICATIONS_LEVEL=warn` still got
  paged on routine successful updates. Now a report-mode notification
  is suppressed when everything in the batch is below the configured
  threshold: no level-appropriate log entries AND no failed or
  error-marked skipped containers. At info/debug/trace thresholds
  behavior is unchanged (verbose mode fires for any report). Inspired
  by [nicholas-fedor/watchtower#1290](https://github.com/nicholas-fedor/watchtower/pull/1290).
- **Pinned-image skip demoted from `warn` to `debug`.** Containers
  whose tag is a `sha256:...` digest can never be updated (there's no
  moving target), so the per-poll `Unable to update container ...
  Proceeding to next.` was noise operators couldn't act on. The typed
  `container.ErrPinnedImage` sentinel is now used at the call site in
  `actions.Update` to demote the log line without altering the skip
  semantics. Existing error message is preserved verbatim so
  downstream log parsers and notification templates keep matching.
- **Pre-update lifecycle command failures logged at `warn`, not `error`.**
  User-defined `com.centurylinklabs.watchtower.lifecycle.pre-update`
  scripts are user code; a failure is the script's problem, not
  watchtower's orchestration. Strict `NOTIFICATIONS_LEVEL=error`
  feeds no longer fire for user-script flakes. The container is still
  skipped and still recorded in the session report.

### Fixed
- **Locally-built images on the containerd image store (Docker 25+).**
  The v1.12.1 local-build detection relied on `RepoDigests` being
  empty, which is only true on the classic docker-image-store path.
  With the containerd-snapshotter image store (default on recent Docker
  versions), `docker build -t app:latest .` synthesizes a
  content-addressed repo digest — e.g. `app@sha256:...` — that is
  structurally indistinguishable from a real Docker Hub pull like
  `postgres@sha256:...`. The old heuristic couldn't fire, so Watchtower
  attempted a registry pull for every local build and logged
  `pull access denied for app, repository does not exist` once per poll.
  Now `ImageIsLocal` prefers the daemon's per-image `Identity`
  provenance record (populated by the containerd image store): a
  `Build` entry with no `Pull` entry means skip the pull; a `Pull`
  entry with any repository means try the pull even if a `Build` entry
  also exists (build-then-push stays on the normal path). The old
  empty-`RepoDigests` fallback still carries the classic image store,
  so this is purely additive. Decoded from the raw inspect JSON via
  the SDK's `ImageInspectWithRawResponse` option — no SDK bump needed.
- **`ListContainers` no longer fails the scan on a transient daemon
  flake.** A daemon restart during a poll, a socket-proxy blip, or any
  transient 5xx from the Docker engine API used to fail the whole scan
  cycle and fire `WatchtowerScansStopped` noise if the next poll was
  far enough away. The call is now wrapped in bounded exponential
  backoff (3 attempts, 500 ms → 4 s + jitter) mirroring the v1.9
  registry retry. Retries on network errors and the Docker errdefs
  transient classes (`IsInternal` / `IsUnavailable`); bails immediately
  on `context.Canceled`, `context.DeadlineExceeded`, and caller-fault
  errors (`IsInvalidArgument`, `IsNotFound`, `IsPermissionDenied`) that
  won't clear with a retry. Inspired by
  [nicholas-fedor/watchtower#1459](https://github.com/nicholas-fedor/watchtower/pull/1459).
- **Containers that vanish between the stale-check and the stop no
  longer abort the scan.** Between `IsContainerStale` deciding to
  recreate a container and `StopContainer` actually firing, a Compose
  `up` can land and recreate the container under a new ID. Previously
  the subsequent stop returned an untyped daemon error and the whole
  iteration bailed. `StopContainer` now returns a typed
  `container.ErrContainerNotFound`; `actions.Update` catches it, marks
  the container as `Skipped` in the session report, and continues
  scanning the rest. The scan also no longer tries to recreate the
  vanished container, which would otherwise collide with whatever
  Compose put in its place. Inspired by
  [nicholas-fedor/watchtower#1522](https://github.com/nicholas-fedor/watchtower/pull/1522).
- **Cleanup defers images still referenced by an active container.**
  When `--cleanup` is on, watchtower would force-remove the old image
  of a just-recreated container even if a separate, still-running
  container used the same image — breaking that container's next
  restart with `No such image`. Shared base images in Compose stacks
  (N services on the same image) were the common trigger. The cleanup
  step now walks the scan view: if any container that isn't itself
  being recreated still references the image, the removal is deferred
  and logged at debug. The next scan retries once the last referrer is
  gone or recreated. Also applied to the `cleanupExcessWatchtowers`
  path (multi-watchtower cleanup) so the kept instance's image isn't
  yanked out when siblings share the image. Inspired by
  [nicholas-fedor/watchtower#1428](https://github.com/nicholas-fedor/watchtower/pull/1428).

## [1.12.1] - 2026-04-19

### Added
- **`--watch-docker-events` — subscribe to the Docker engine event stream
  for instant local-rebuild detection.** Watchtower now optionally opens
  `GET /events?filters=type=image,event=tag|load` on the Docker socket and
  fires a debounced, targeted scan as soon as a locally-built image is
  tagged or loaded. Closes the "I just ran `docker build -t app:latest .`
  and I'm still waiting for the next poll" gap by reacting in a couple of
  seconds instead of the full `--interval`. Complements the scheduler
  rather than replacing it: registry-backed images still flow through the
  poll loop, which also serves as the safety net for events missed during
  daemon restarts or network blips. Event-triggered scans share the same
  update lock as the scheduler and the HTTP API, so there's no
  possibility of concurrent updates. Bursty builds (multi-stage images
  that tag several layers) are debounced into a single scan. Opt-in,
  disabled by default. New package `internal/events`. New client method
  `Container.WatchImageEvents(ctx)` on the `container.Client` interface,
  and a new `types.ImageEvent` value type so the interface stays
  decoupled from the Docker SDK. New metrics:
  `watchtower_events_received_total{action}`,
  `watchtower_events_triggered_scans_total`,
  `watchtower_events_reconnects_total`. Natural follow-up to the
  local-build pull-skip fix below — that one made local builds work
  without `--no-pull`; this one makes them react without waiting for
  the next poll.

### Fixed
- **Locally-built images no longer trip the pull path.** Containers
  whose image was created via `docker build` or `docker load` (and
  never pushed to a registry) have an empty `RepoDigests` — Watchtower
  used to still try to pull them, hit a `No such image` error from the
  daemon, and log `Unable to update container ... Proceeding to next.`
  every poll. Now detected via a new `Container.ImageIsLocal()` check
  and handled like `--no-pull`: the pull step is skipped, but
  `HasNewImage` still picks up a rebuild (the tag's image ID changes),
  so `docker build -t app:latest .` followed by a poll still triggers
  the recreate. Makes the local-build workflow work out of the box
  without setting `--no-pull` or the per-container
  `com.centurylinklabs.watchtower.no-pull=true` label. Inspired by
  [nicholas-fedor/watchtower#1514](https://github.com/nicholas-fedor/watchtower/pull/1514).
- **Self-update is now skipped when Watchtower has published host
  ports.** The rename-and-respawn self-update pattern briefly overlaps
  the old and new containers; if the current Watchtower publishes a
  host port (e.g. `--http-api-*` on `:8080`), the new container's
  create call would fail with "address already in use". Watchtower now
  detects non-empty `HostConfig.PortBindings` on itself before the
  rename and logs a loud warning instead of wedging the update path.
  Operators with published ports must `stop + pull + recreate`
  manually. New `Container.HasPublishedPorts()` method on the
  `types.Container` interface. Inspired by
  [nicholas-fedor/watchtower#1481](https://github.com/nicholas-fedor/watchtower/pull/1481).
- **HEAD → GET fallback on manifest digest fetches.** Some registries
  (GHCR and Docker Hub under certain anonymous-pull conditions) answer
  the manifest `HEAD` endpoint with `401 Unauthorized` even with a
  valid token, while the same token on `GET` returns the manifest
  fine. Previously we'd fall all the way back to a full `docker pull`
  just to compare digests — much more expensive than a single GET.
  `pkg/registry/digest.GetDigest` now tries HEAD first (single
  attempt, best-effort) and on any failure retries with GET, which
  also computes the digest by hashing the manifest body if the
  registry omits the `Docker-Content-Digest` header. New metric
  operation labels: `digest_head` and `digest_get` on
  `watchtower_registry_requests_total`. Inspired by
  [nicholas-fedor/watchtower#669](https://github.com/nicholas-fedor/watchtower/pull/669).

### Changed
- **`/v1/metrics` no-auth startup log demoted from `warn` to `info`.**
  The message is informational (operator opted in to public scraping) —
  not a warning about anything going wrong. Stops muddying
  `NOTIFICATIONS_LEVEL=warn` feeds.

## [1.12.0] - 2026-04-18

### Added
- **`--compose-depends-on`** flag (env: `WATCHTOWER_COMPOSE_DEPENDS_ON`)
  — honor Docker Compose's `depends_on` graph when ordering
  stop/start during updates. Reads the `com.docker.compose.depends_on`
  label Compose writes on every managed container and resolves service
  names to real container names within the same Compose project. Opt-in
  (default off preserves pre-v1.12 behaviour for stacks running fine on
  the link-only model). Incompatible with `--rolling-restart` — a
  warning fires at startup if both are set. Modifiers in the label
  value (e.g. `db:service_healthy:true`) are parsed and stripped;
  Watchtower only needs the graph edge, not Compose's startup
  conditions.
- New `Container.ComposeProject()`, `ComposeService()`,
  `ComposeDependencies()` methods on the `types.Container` interface
  for programmatic use.
- **`--image-cooldown`** flag (env: `WATCHTOWER_IMAGE_COOLDOWN`) +
  per-container label `com.centurylinklabs.watchtower.image-cooldown` —
  supply-chain gate that defers applying a new image digest until it has
  been stable for the configured duration. If the registry serves a
  different digest during the window (author re-pushed), the clock
  resets. Directly addresses the long-standing "broken `:latest` push
  reaches prod in one poll interval" rough edge. Default `0` keeps
  existing behavior unchanged.
  - New Prometheus gauge `watchtower_containers_in_cooldown` tracks the
    current count of deferred updates.
  - Pairs with `--health-check-gated`: cooldown gates *when* to apply,
    health-check gates *whether* the applied container works.
  - Bypassed automatically under `--run-once` since "defer to next poll"
    is meaningless when the daemon exits after this cycle.
  - Own design — not a port of the upstream equivalent — because the
    reset-on-republish semantics and label override slot directly into
    the existing per-container configuration pattern.
- **`--http-api-host`** flag (env: `WATCHTOWER_HTTP_API_HOST`) — bind
  the HTTP API listener to a specific host:port. Defaults to `:8080`
  (all interfaces, unchanged behavior). Useful for operators who want a
  localhost-only bind (`127.0.0.1:8080`) without putting a reverse proxy
  in front. Inspired by [nicholas-fedor/watchtower#697](https://github.com/nicholas-fedor/watchtower/pull/697).
- **Auto-detect container stop timeout.** If a container was started
  with its own `StopTimeout` (via `docker run --stop-timeout` or
  Compose's `stop_grace_period`), Watchtower honors that value instead
  of the global `--stop-timeout`. Matches Docker's own precedence of
  per-container over daemon default. New `Container.StopTimeout()`
  method on the `types.Container` interface. Inspired by
  [nicholas-fedor/watchtower#1182](https://github.com/nicholas-fedor/watchtower/pull/1182).
- **`--update-on-start`** flag (env: `WATCHTOWER_UPDATE_ON_START`) — run
  one scan immediately at startup in addition to the scheduled cadence,
  so operators can verify a fresh deployment without waiting for the
  first poll interval. Skipped if the HTTP API already holds the update
  lock at boot. Inspired by [nicholas-fedor/watchtower#672](https://github.com/nicholas-fedor/watchtower/pull/672).
- **Structured JSON response from `GET /v1/update`**. The endpoint now
  returns `{"status": "completed", "scanned": N, "updated": N, "failed": N}`
  on success instead of an empty 200 body — automation can tell how the
  scan went without scraping logs. Inspired by
  [nicholas-fedor/watchtower#673](https://github.com/nicholas-fedor/watchtower/pull/673).

### Changed
- **`/v1/update` returns HTTP 429** (with a `{"status":"skipped","reason":...}`
  body) when another update is already running, instead of silently
  succeeding with 200. Lets clients retry with backoff. Targeted updates
  (`?image=<name>`) still block on the lock rather than 429-ing, because a
  caller explicitly asking for an image usually wants it eventually.
  Inspired by [nicholas-fedor/watchtower#1304](https://github.com/nicholas-fedor/watchtower/pull/1304).

### Fixed
- **`WatchtowerScansStopped` alert no longer fires before the first scan**
  completes. The alert had a latent bug: after a restart on a
  long-cadence deployment (e.g. `@every 12h`), Watchtower's
  `watchtower_last_scan_timestamp_seconds` gauge sits at `0` until the
  first scheduled scan actually runs. The expression
  `time() - 0` produced a ~56-year "staleness" readout and fired the
  alert immediately instead of at the intended 2× poll interval.
  Expression now guards on
  `watchtower_last_scan_timestamp_seconds > 0` so the alert only fires
  once a scan has completed at least once and subsequent scans are
  overdue.

## [1.11.2] - 2026-04-18

### Fixed
- **`docs/introduction.md`** — the `centurylink/wetty-cli` example image
  was an upstream-era artefact. Replaced with a coherent `nginx:latest`
  walkthrough (container name, image, and port mapping now match).
- **`docs/notifications.md`** — the shoutrrr reference pointed at the
  upstream `containrrr/shoutrrr` project and its `v0.8` docs, but the
  fork actually vendors `nicholas-fedor/shoutrrr v0.14.3`. Updated the
  link and added a one-line note on fork lineage + URL compatibility.
- **`docs/secure-connections.md`** — rewritten around the supported
  `DOCKER_HOST` + `DOCKER_CERT_PATH` + `DOCKER_TLS_VERIFY` path with a
  Compose example. `docker-machine` demoted to a "works if you still
  have the certs it generated" footnote (Docker archived the tool in
  2023). Added a pointer differentiating daemon TLS from registry TLS.
- **`docs/http-api-mode.md`** — removed a reference to a
  `WatchtowerAPIUnauthorizedBurst` alert that was dropped during the
  production-tuning pass but still advertised as "shipped"; replaced
  with the PromQL snippet for operators who want to compose their own.
  Added an **Env** column to the endpoint table, tightened the
  `/v1/update` response-shape prose, quoted Compose port mappings.
- **`docs/why-fork.md`** — expanded *What changed* from a single
  toolchain table into four grouped tables (Project health, Update
  behavior, Security, Observability) so the comparison isn't just
  "we updated Go". Added rows for the shoutrrr swap, registry TLS
  default change, constant-time token compare, `/v1/audit`, metric
  count, and the shipped observability bundle.

### Changed
- **CI workflows** — set `FORCE_JAVASCRIPT_ACTIONS_TO_NODE24=true`
  across every GitHub Actions workflow so JS-based actions run on
  Node 24 uniformly (instead of inheriting whatever default the
  specific action ships with).

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

[Unreleased]: https://github.com/openserbia/watchtower/compare/v1.12.1...HEAD
[1.12.1]: https://github.com/openserbia/watchtower/compare/v1.12.0...v1.12.1
[1.12.0]: https://github.com/openserbia/watchtower/compare/v1.11.2...v1.12.0
[1.11.2]: https://github.com/openserbia/watchtower/compare/v1.11.1...v1.11.2
[1.11.1]: https://github.com/openserbia/watchtower/compare/v1.11.0...v1.11.1
[1.11.0]: https://github.com/openserbia/watchtower/compare/v1.10.1...v1.11.0
[1.10.1]: https://github.com/openserbia/watchtower/compare/v1.10.0...v1.10.1
[1.10.0]: https://github.com/openserbia/watchtower/compare/v1.9.0...v1.10.0
[1.9.0]: https://github.com/openserbia/watchtower/compare/v1.8.5...v1.9.0
