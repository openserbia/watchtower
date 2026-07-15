# Changelog

All notable changes to this fork are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Cross-references with a leading `upstream#` link to issues on
[containrrr/watchtower](https://github.com/containrrr/watchtower/issues) that
this fork has addressed (upstream went dormant after 2023 and was archived on
2025-12-17 without shipping a fix).

## [Unreleased]

### Added
- **`watchtower_stranded_init_deps` gauge — a non-resolving signal for the
  "new code on an un-migrated schema" trap.** The existing
  `watchtower_stranded_init_deps_total` counter only increments when a *stale*
  target is scanned, so an alert built on `increase(...[1h]) > 0` auto-resolves
  an hour after the last such scan whether or not the operator actually
  re-armed the service — decoupling alert resolution from remediation. The new
  gauge is recomputed every scan and reads the number of compose targets
  **currently** stranded: empty `com.docker.compose.depends_on` while the
  project still holds a `migrate`/`pg-ready` one-shot, excluding
  `no-init-deps` opt-outs. It drops to 0 on the first scan after
  `docker compose up -d --force-recreate <service>` restores the label, so an
  alert on `watchtower_stranded_init_deps > 0` fires while broken and clears
  exactly on fix. The detection predicate (`strandedInitSiblings`) is now the
  single source of truth for both the gauge and the per-target warning, and it
  skips one-shot init containers themselves so two sibling one-shots
  (migrate ↔ pg-ready) surfaced by `WATCHTOWER_INCLUDE_STOPPED` no longer count
  each other. The gauge is detection-only; the companion auto-recovery that
  re-runs the stranded siblings ships alongside it (see Fixed).

### Fixed
- **`--rerun-init-deps` now recovers a stranded target instead of only warning
  about it.** When a compose target's `com.docker.compose.depends_on` is empty
  but its project still holds one-shot `migrate`/`pg-ready` siblings — the state
  a `docker compose up --no-deps <svc>` in-place recreate leaves behind (it
  stamps an EMPTY `depends_on` because `--no-deps` tells Compose to disregard the
  dependency graph), then carried verbatim across every later blue-green cutover
  — the update path previously skipped the init rerun and let the new image start
  against the old schema, logging only a warning. It now re-runs the detected
  one-shot siblings before recreating the target, gated identically to declared
  init deps (a failing sibling caches the digest as rejected and leaves the old
  container serving). Siblings are ordered by each one's own init-dep count so a
  leaf (`pg-ready`) runs before a `migrate` that gates on it. The
  `watchtower_stranded_init_deps` gauge still fires so the operator re-arms the
  label with `docker compose up -d --force-recreate <service>` to restore the
  declared contract and exact ordering. Also corrects the prior misattribution
  of the empty label to blue-green cutovers: green inherits blue's labels
  verbatim (`GetCreateConfig` copies them), so it only ever propagates an
  already-empty value — the emptying happens at the `--no-deps` recreate.

## [1.18.3] - 2026-06-25

### Changed
- **GitHub release notes are now sourced from `CHANGELOG.md`.** The Release
  (Production) workflow extracts the tagged version's CHANGELOG section,
  appends a linked commit list (and a full-diff link) for the commits since
  the previous tag, and passes the result to GoReleaser via `--release-notes`
  — replacing the previous auto-generated raw commit-SHA list with curated
  prose *plus* the precise commit refs. The notes file is written under
  `RUNNER_TEMP` so it doesn't re-trip GoReleaser's git-state check, and the
  step fails fast if the tag has no matching CHANGELOG section. ([3721b57](https://github.com/openserbia/watchtower/commit/3721b57))

## [1.18.2] - 2026-06-25

### Fixed
- **Release pipeline: GoReleaser no longer aborts on a dirty `devbox.lock`.**
  The Build job runs `devbox run -- goreleaser …`, and `devbox run`
  re-resolves the `"latest"`-pinned tools and rewrites `devbox.lock`, so
  GoReleaser's git-state guard saw a dirty tree and aborted before building
  anything — the **v1.18.0 and v1.18.1 releases failed to publish any
  images for this reason**. The Build step now restores `devbox.lock` inside
  the devbox shell before invoking GoReleaser. The v1.18.0 and v1.18.1
  changes (the metrics/label/dependency work and the HTTP API Slowloris
  hardening) ship unchanged in this release. ([a604e8b](https://github.com/openserbia/watchtower/commit/a604e8b))

## [1.18.1] - 2026-06-25

### Security
- **The HTTP API server now sets explicit read/idle timeouts.** The API
  listener previously used a bare `http.ListenAndServe` with no timeouts, so a
  slow or idle client could hold connections open and exhaust the accept loop /
  file descriptors (Slowloris). It now uses an explicit `http.Server` with
  `ReadHeaderTimeout` (5s, the key mitigation), `ReadTimeout` (15s) and
  `IdleTimeout` (60s). `WriteTimeout` is intentionally left unset because
  `/v1/update` is synchronous — it pulls images and recreates containers inside
  the handler before responding, which legitimately takes minutes on a large
  fleet, so a write deadline would truncate that response. ([df27c83](https://github.com/openserbia/watchtower/commit/df27c83))

## [1.18.0] - 2026-06-25

### Added
- **`watchtower_promotion_aborts_total` metric — blue-green cutovers that bring
  up a healthy replacement but cannot retire the old container are now counted
  separately from rollbacks.** Previously a failed stop of the old ("blue")
  container during a cutover incremented `watchtower_rollbacks_total`,
  conflating it with a genuine health-gated rollback even though the outcomes
  are opposite: a rollback means the new image was rejected and the old one
  restored, whereas a promotion abort means the new image IS live (served by the
  healthy "green" container on a temporary name) and the old container merely
  lingers. The two now have distinct counters so alerting can describe each
  accurately. Reconcile a promotion abort with
  `docker compose up -d --force-recreate <service>`. ([4d80f6e](https://github.com/openserbia/watchtower/commit/4d80f6e))
- **`watchtower_stranded_init_deps_total` metric and a `WARN` when
  `--rerun-init-deps` finds a stranded init dependency.** A blue-green cutover
  recreates the "green" container by inheriting the old container's labels and
  never re-derives Compose metadata, so once `com.docker.compose.depends_on`
  goes empty (a drop that originates in an earlier cutover) it stays empty
  across every subsequent cutover. `ComposeInitDependencies()` then returns
  nothing and the rerun silently skips the target — the new image runs against
  an un-migrated schema with no log and no rejection ("new code, old schema").
  Watchtower now detects the signature — a stale, Compose-managed target with
  no declared init deps whose project still holds a one-shot init sibling (a
  `migrate`/`pg-ready` container with restart policy `no`) — logs a `WARN`
  naming the offending siblings, and increments
  `watchtower_stranded_init_deps_total`. Detection only; update behaviour is
  unchanged. Recovery is `docker compose up -d --force-recreate <service>`,
  which rewrites the label from the Compose file. ([1ba4a27](https://github.com/openserbia/watchtower/commit/1ba4a27))
- **`com.centurylinklabs.watchtower.no-init-deps=true` opt-out label for the
  stranded-init-deps warning.** `WatchtowerStrandedInitDeps`
  (`watchtower_stranded_init_deps_total` + the WARN) flags a stale,
  compose-managed target that declares no `service_completed_successfully`
  deps while its project still holds one-shot init siblings (migrate/pg-ready)
  — the signature of a `com.docker.compose.depends_on` label dropped by a
  blue-green cutover. But a frontend that shares a Compose project with init
  one-shots owned by a sibling API tier (e.g. a web tier whose only
  `depends_on` is the API) legitimately has no init deps of its own, so its
  empty `depends_on` is a false positive, not a dropped label. Setting
  `com.centurylinklabs.watchtower.no-init-deps=true` on such a service affirms
  "no init deps by design" and suppresses the warning/metric **for that
  service only** — every sibling that genuinely depends on the one-shots keeps
  the detector, so a real dropped-label stranding is still surfaced. ([6f3803c](https://github.com/openserbia/watchtower/commit/6f3803c))

### Changed
- **Dependency refresh.** Routine bump of Go module dependencies (direct and
  indirect) via `go get -u ./...`, then tidy + re-vendor. Direct: `moby/moby/api`
  v1.54.2 → v1.55.0 and `moby/moby/client` v0.4.1 → v0.5.0 (the Docker v29 SDK;
  still pre-stable, but the minor bump needed no changes in `pkg/container`),
  `docker/cli` v29.5.3 → v29.6.0, `nicholas-fedor/shoutrrr` v0.16.0 → v0.16.1,
  `onsi/ginkgo/v2` v2.29.0 → v2.32.0, `onsi/gomega` v1.41.0 → v1.42.1,
  `golang.org/x/text` v0.37.0 → v0.38.0. Notable indirect:
  `docker/docker-credential-helpers` v0.9.7 → v0.9.8, `felixge/httpsnoop`
  v1.0.4 → v1.1.0, `prometheus/common` v0.68.1 → v0.69.0, and the
  `golang.org/x/{net,sys,mod,sync,term,tools}` set. Build, tests (`-race`), and
  lint all pass; no source changes required. ([cf2f8a6](https://github.com/openserbia/watchtower/commit/cf2f8a6))

### Fixed
- **`StopContainer` now retries `ContainerKill` on transient connection
  errors, so a proxy keep-alive race no longer strands a blue-green cutover.**
  The kill is a single non-idempotent POST, so Go's `net/http` transport does
  not transparently retry it when it reuses a pooled keep-alive connection the
  server has already closed — the call surfaces a bare `EOF` while concurrent
  GET inspects (which are replayable) succeed. Against a docker-socket-proxy
  (HAProxy) whose `timeout http-keep-alive` is shorter than the SDK's idle
  timeout this race is routine, and it aborted an otherwise-complete stop — most
  visibly a blue-green cutover that already had a healthy green, which left the
  old container running and green stuck on its temporary name. The kill now runs
  under the same bounded backoff as the list-retry path (3 attempts, transient
  errors only; `NotFound`/permission/invalid-argument bail immediately),
  incrementing `watchtower_docker_api_retries_total{operation="kill"}` per
  retry. Re-sending the signal is safe — a terminating container ignores the
  duplicate, an already-gone one returns `NotFound`. ([4d80f6e](https://github.com/openserbia/watchtower/commit/4d80f6e))
- **`--rerun-init-deps` now resolves init dependencies against the full
  daemon container list instead of the scan-filtered one.** With
  `WATCHTOWER_LABEL_ENABLE`, init one-shots that operators deliberately
  label `com.centurylinklabs.watchtower.enable=false` (so an exited
  `migrate`/`pg-ready` sibling is neither an update candidate nor
  unmanaged-container noise) were invisible to the dep lookup: every new
  digest of their parent was rejected with `init dep not found`
  (`exit_code:-1`) and cached, silently stopping auto-updates for that
  service until a manual `docker compose up`. The same applied to deps
  hidden by scope/name filters, and — before `WATCHTOWER_INCLUDE_STOPPED` —
  to every Exited(0) init container. Dep *discovery* now uses a dedicated
  `ListAllContainers` lookup (every container state, no filter, independent
  of `WATCHTOWER_INCLUDE_STOPPED`); which containers get *updated* is still
  governed solely by the configured scan filters. Targets whose init dep
  genuinely does not exist on the daemon are still rejected as before. ([c708dcd](https://github.com/openserbia/watchtower/commit/c708dcd))

### Security
- **Release signatures are now published with a `.sigstore.json` extension
  instead of `.bundle`.** The keyless cosign signature over `checksums.txt` is
  unchanged in content (it was always a Sigstore bundle), but the asset is now
  named `watchtower_<version>_checksums.txt.sigstore.json` so OpenSSF
  Scorecard's Signed-Releases probe recognises it — that probe only credits the
  `.asc`/`.minisig`/`.sig`/`.sign`/`.sigstore`/`.sigstore.json` suffixes, so the
  prior `.bundle` asset was scored as unsigned despite being a valid signature.
  Verification is otherwise identical (`cosign verify-blob --bundle <file>
  …`); the README example is updated to the new filename. **Action required**
  only if you pinned the literal `.bundle` asset name in a verification script. ([7a9d029](https://github.com/openserbia/watchtower/commit/7a9d029))
- **Supply-chain hardening across the CI/release workflows** (no runtime
  behaviour change). Every GitHub Actions workflow now declares a least-
  privilege top-level `permissions:` block with per-job escalation; all action
  references are pinned by commit SHA (kept current by Dependabot); the
  cross-compiling builder base image is pinned to the `golang:1.26-alpine`
  multi-arch index digest; and a native Go fuzz target was added for the
  attacker-influenceable `com.docker.compose.depends_on` label parser. These
  close the OpenSSF Scorecard Token-Permissions, Pinned-Dependencies, and
  Fuzzing findings. ([7a9d029](https://github.com/openserbia/watchtower/commit/7a9d029))
- **Contributions now require a Developer Certificate of Origin (DCO)
  sign-off.** Every commit in a pull request must carry a `Signed-off-by`
  trailer (`git commit -s`); a new [DCO workflow](.github/workflows/dco.yml)
  enforces it. See CONTRIBUTING.md and <https://developercertificate.org/>. ([5b49b97](https://github.com/openserbia/watchtower/commit/5b49b97))
- **Release tags are now GPG-signed.** Tag signing is enabled
  (`tag.gpgsign`), so annotated release tags are signed and verifiable with
  `git tag -v`, complementing the existing cosign artifact signatures. The
  release process is documented in [RELEASING.md](./RELEASING.md). Governance
  and the project's security assurance case are documented in
  [GOVERNANCE.md](./GOVERNANCE.md) and [ASSURANCE-CASE.md](./ASSURANCE-CASE.md). ([5b49b97](https://github.com/openserbia/watchtower/commit/5b49b97))

### Docs
- **Fixed seven dead intra-doc anchor links in the published site.** The `toc`
  extension uses `separator: "_"`, so heading slugs join words with `_` but keep
  literal hyphens — cross-references that guessed the separator wrong (e.g.
  `arguments.md#http_api_host` → `#http_api_listen_address`,
  `#health_check_gated_updates` → `#health-check_gated_updates`,
  `#watch_status_audit_endpoint` → `#watch-status_audit_endpoint`,
  `notifications.md#microsoft-teams` → `#microsoft_teams`) landed on nothing.
  Enabled the `attr_list` Markdown extension so the intended
  `{#update-strategy-blue-green}` custom anchor on the blue-green deploys
  heading resolves (it was previously rendered as literal heading text). A
  strict `mkdocs build` now reports zero missing anchors. ([42b688f](https://github.com/openserbia/watchtower/commit/42b688f))

## [1.17.0] - 2026-06-07

### ⚠ Breaking Changes
- **MS Teams notifications now require a Power Automate workflow webhook URL.**
  Bumping the vendored shoutrrr to v0.16 replaced its MS Teams service: the
  legacy Office 365 *connector* parser (`teams.ConfigFromWebhookURL`, which
  accepted `https://outlook.office.com/webhook/…`) is gone, and the new service
  accepts only Power Automate workflow URLs (`*.logic.azure.com/…/workflows/…`
  or `*.environment.api.powerplatform.com/powerautomate/…`) — tracking
  Microsoft's retirement of O365 connectors. The `--notification-msteams-hook` /
  `WATCHTOWER_NOTIFICATION_MSTEAMS_HOOK_URL` flag is unchanged, but a stale
  connector URL now fails fast at startup with a validation error instead of
  silently failing at send time. **Action required:** MS Teams users must mint a
  Power Automate workflow URL — see the
  [notification docs](https://openserbia.github.io/watchtower/notifications/#microsoft-teams). ([d5d0b0e](https://github.com/openserbia/watchtower/commit/d5d0b0e))

### Fixed
- **Self-update can no longer strand the live watchtower under an
  unrecoverable random name.** The self-update rename-and-respawn renamed the
  running self to an opaque 32-character `util.RandName()` before recreating the
  replacement. That name embeds nothing, so once it crept in — a non-Compose
  `docker run --name watchtower` (no `com.docker.compose.service` label to
  recover from), a respawn that failed *after* the rename, or a `--no-restart`
  cycle that renamed without ever respawning — the operator-chosen name existed
  nowhere recoverable, and the existing safety nets either dead-ended (the
  Compose-label rescue) or cemented the random name (the post-create check
  compared the new name against an *already-random* "expected"). Watchtower
  could end up running indefinitely as e.g. `pmGEucoAmWufDGCRjdiooekxKbtHMNkU`
  with no `watchtower` container at all. The outgoing self is now renamed to a
  **structured** temporary name that embeds the canonical name —
  `<canonical>-wt-self-XXXXXXXX`, the deliberate twin of the blue-green
  `<canonical>-wt-bluegreen-XXXXXXXX` pattern — so the real name is always
  recoverable from the daemon-side container name alone, with zero dependence on
  Compose labels or `os.Hostname()`-to-short-ID matching. Recovery happens three
  ways: the next poll re-derives the canonical name from the temp name's capture
  group; a new `CleanupOrphanSelf` startup sweep (mirroring
  `CleanupOrphanBlueGreen`, run for every strategy before the `--run-once`
  exit and before `CheckForMultipleWatchtowerInstances`) promotes a stranded
  self back to its canonical name or removes it when the canonical self already
  exists; and a failed respawn now renames the live self straight back to its
  canonical name instead of leaving it stranded. ([95c68c5](https://github.com/openserbia/watchtower/commit/95c68c5))
- **`--no-restart` no longer renames the live self at all.** The destructive
  rename was gated only on "this is self", independent of the respawn gate, so a
  stale self under `--no-restart` was renamed to a temp name with no replacement
  ever created. The rename is now skipped entirely under `--no-restart`,
  mirroring the existing blue-green `NoRestart` short-circuit — the live self
  keeps its canonical name and image-only monitoring is unaffected. ([95c68c5](https://github.com/openserbia/watchtower/commit/95c68c5))

### Changed
- **Self-update name recovery is now structural rather than heuristic.** The
  Compose-service-only "looks-random, derive from label" rescue and the
  post-create `verifySelfContainerName` re-rename (both defeated by the cases
  above) are removed in favor of the embedded-canonical temp name and the
  `CleanupOrphanSelf` sweep. `CheckForMultipleWatchtowerInstances` now prefers a
  canonically-named survivor over a newer transient (`-wt-self-`/random) one, so
  a just-promoted self — which keeps its older `CreatedAt` across the rename —
  is not reaped by the keep-newest rule. The `-wt-self-` suffix joins
  `-wt-bluegreen-` as a reserved container-name pattern operators should avoid. ([95c68c5](https://github.com/openserbia/watchtower/commit/95c68c5))
- **Migrated the Docker Engine Go SDK from the deprecated
  `github.com/docker/docker` module to the split `github.com/moby/moby/api` +
  `github.com/moby/moby/client` modules (the Docker v29 SDK).** Upstream froze
  `github.com/docker/docker` as of Docker v29 — future engine-SDK fixes, including
  CVE patches, land only on the new modules — and its `+incompatible`
  pseudo-versioning never sat well with Go module tooling. The entire Docker client
  layer was rewritten onto v29's uniform `(ctx, options) (Result, error)` API. This
  is **internal only**: CLI flags, container labels, the HTTP API, and notification
  backends are unchanged, and existing deployments upgrade in place. A side benefit
  is that the opportunistic API-version-negotiation workaround is gone — the v29
  client already defaults to API 1.54 and negotiates down to the daemon — and
  `--preflight` was re-validated live against a Docker 29.x daemon with every
  capability probe reporting available. ([0a55d9e](https://github.com/openserbia/watchtower/commit/0a55d9e))
- Routine dependency refresh shipped alongside the above: `docker/cli`
  v29.5.2 → v29.5.3, plus indirect bumps `google/pprof` and `prometheus/common`
  v0.68.0 → v0.68.1. ([d5d0b0e](https://github.com/openserbia/watchtower/commit/d5d0b0e))

## [1.15.1] - 2026-06-06

### Fixed
- **A Docker daemon outage mid-scan no longer crashes watchtower with a nil
  pointer panic.** When the daemon (or a socket proxy in front of it) becomes
  unreachable during a poll — e.g. it is being upgraded and returns `503
  Service Unavailable` — `actions.Update` bails early and returns a `nil`
  report along with the error. The scheduled, HTTP `/v1/update`, and
  event-triggered callers logged that error but then fed the `nil` report
  straight into `metrics.NewMetric`, which dereferenced it
  (`report.Scanned()`) and panicked with a `SIGSEGV`. Because the scan runs
  inside a `robfig/cron` goroutine with no recovery, the panic took down the
  whole process — a transient daemon blip killed watchtower until its restart
  policy brought it back. `NewMetric` now treats a `nil` report as an empty
  scan, so a failed update cycle records zero counts and the daemon keeps
  polling. Present in upstream too (same unguarded `NewMetric(result)` call). ([dec6dde](https://github.com/openserbia/watchtower/commit/dec6dde))
- **A panic during any update cycle no longer takes down the whole daemon.**
  Defense in depth for the class of bug above: the scheduled (cron) callback,
  the `--update-on-start` scan, and the Docker-event watcher each run in a
  goroutine with no recovery of its own, so an unrecovered panic anywhere in an
  update crashed the process. Every trigger path funnels through
  `runUpdatesWithNotifications`, which now recovers at that single chokepoint —
  logging the full stack to the local log, sending operators a concise (not
  64 KB) notification, and returning an empty metric so the next poll retries
  cleanly. Chosen over `cron.Recover` because it also covers the non-cron
  trigger paths and keeps the goroutine dump out of the notification payload. ([dec6dde](https://github.com/openserbia/watchtower/commit/dec6dde))

## [1.15.0] - 2026-05-31

### Fixed
- **Self-update recovers from a `"/watchtower" name is already in use`
  conflict on recreate.** When a stale watchtower container from a previous
  interrupted self-update cycle still holds the canonical name, the recreate's
  `ContainerCreate` fails with a name conflict and — because the old self that
  would clean it up is already gone — the conflict re-fires every poll, wedging
  the self-update until the next process restart (when
  `CheckForMultipleWatchtowerInstances` finally reconciles it). `StartContainer`
  now detects this case mid-recreate: on a `Conflict` from `ContainerCreate`
  for a watchtower self-update, it inspects whoever holds the name and, *only if
  that holder is a different watchtower-labeled container*, force-removes it and
  retries the create once. The recovery is deliberately narrow — an operator's
  container or a Compose recreate that races the name is never touched (it
  surfaces as the original conflict to retry next poll), and the container being
  recreated from is never removed. Complements the existing post-create orphan
  cleanup and the startup duplicate-instance reconciliation. ([17e7343](https://github.com/openserbia/watchtower/commit/17e7343))
- **Pull-stream errors are no longer silently swallowed.** Docker delivers pull
  *progress* and pull *failures* over the same newline-delimited JSON stream
  that `ImagePull` returns: a 401/404 on the manifest, a layer that 500s
  mid-download, or a partial-content abort all arrive as a `JSONMessage`
  carrying an `Error`/`errorDetail` field, not as the immediate error from the
  `ImagePull` call. `PullImage` previously drained that stream with
  `io.ReadAll` purely to keep the daemon from aborting the pull, then returned
  `nil` regardless of content — so a mid-stream failure was reported as a
  successful pull, and the stale container was either recreated against a
  half-pulled image or, with no new digest, looked "up to date" forever. The
  stream is now drained through `jsonmessage.DisplayJSONMessagesStream`, which
  both completes the pull and surfaces a `*jsonmessage.JSONError` when the
  stream reported one; the error is routed through `classifyPullError` so the
  existing `cerrdefs` classification and the bare-name local-build safeguard
  still apply. ([31becce](https://github.com/openserbia/watchtower/commit/31becce))
- **Locally-built bare-name images whose registry returns 401 are now correctly
  treated as local.** On the containerd image store, `docker build -t app:latest .`
  produces an image with a `docker.io/library/app` RepoDigest, so a pull
  attempt hits Docker Hub for a repository that does not exist. Hub answers a
  non-existent `docker.io/library/<bare-name>` repo with a **401 unauthorized**
  (`pull access denied` / `insufficient_scope` / "requires `docker login`"),
  *not* a 404 — but `pullFailureLooksLocal` only recognized the 404 not-found
  case, so the 401 fell through as a real auth failure and the locally-built
  container churned a `pull access denied` error on every poll. The
  local-build heuristic now also matches the unauthorized signal (via both the
  `cerrdefs` classification and a case-insensitive substring check, since the
  in-stream `JSONError` carries no `cerrdefs` class) for references that lack a
  registry hostname. Hostname-qualified references (`ghcr.io/foo/bar`,
  `registry:5000/x`) never reach the broadened branch, so typos and genuinely
  broken private-registry credentials still fail loudly instead of being masked. ([31becce](https://github.com/openserbia/watchtower/commit/31becce))
- **Self-update no longer leaks a half-created container or storms
  notifications on repeated failure.** Two distinct failure modes are
  addressed: (1) When a recreate failed *after* `ContainerCreate` had already
  produced the new container — a network attach error or a `start` failure —
  the orphaned container was left behind. On a Watchtower self-update that
  orphan occupies the `/watchtower` name and wedges every subsequent poll with
  a name conflict on `ContainerCreate`. `StartContainer` now force-removes the
  container it just created when a later step fails (best-effort, strictly
  scoped to that one ID; a `NotFound` means the daemon already tore it down).
  (2) A wedged self-update re-enters the failed-start branch every poll
  (default 60s) with the same error, and each `Error` becomes a notification
  via the logrus hook — turning a single broken self-update into a per-poll
  notification storm. The self-update path now dedups identical
  `(container, error-signature)` start failures: the first occurrence (and any
  genuinely different failure) notifies, identical repeats inside a one-hour
  cooldown drop to `Debug`. The cache is in-memory and resets on restart, so a
  `docker restart watchtower` surfaces the next failure loudly. Non-self
  failures are unaffected and always log at `Error`. ([31becce](https://github.com/openserbia/watchtower/commit/31becce))
- **`--update-strategy=blue-green` hardening — three cutover edge cases closed.**
  (1) When green came up healthy but the old "blue" container *refused to stop*,
  the cutover aborted without arming any cooldown, so the next poll re-ran the
  whole cutover — starting yet another green and accumulating orphaned
  `-wt-bluegreen-*` containers. The blue-stop-failure path now arms the same
  rollback cooldown the failed-health path uses, so the poll loop backs the
  container off for an hour instead of thrashing. (2) A green container could be
  stranded under its temporary `<name>-wt-bluegreen-XXXXXXXX` name by a failed
  blue stop or a Watchtower crash between stopping blue and renaming green. A new
  startup sweep, `CleanupOrphanBlueGreen` (gated on the blue-green strategy and
  run before any poll, so it never races a live cutover), reconciles them:
  it removes the orphan when the canonical container still exists (blue keeps
  serving), or promotes it by renaming to the canonical name when blue is gone
  (the green *is* the live service). (3) `params.NoRestart` (`--no-restart`) is
  now honored by the blue-green path with an explicit guard, matching the
  long-standing gate in the recreate path — the main loop already filters these
  out, so it is a defensive consistency fix. ([261c189](https://github.com/openserbia/watchtower/commit/261c189))
- **Recreate no longer carries a stale `User` forward when the source image
  was garbage-collected (e.g. a distroless base-image switch).** Docker
  materializes an image's `USER` directive into the container's `Config.User`,
  and `GetCreateConfig` strips it as an image default by diffing `config.User`
  against the image's `User`. But when the image the container was created from
  has been GC'd off disk — routine for locally-built tags that get rebuilt and
  the old digest pruned — `GetContainer` falls back to inspecting the configured
  image *reference*, which now resolves to the freshly-pulled **target** image.
  The diff then runs against the target, not the source, so a `USER` inherited
  from the old image no longer matches and is mistaken for a runtime override
  and preserved. When the new image changes `USER` (e.g. a switch to a distroless
  base, `USER app` → a numeric nonroot UID) the carried-forward user is absent
  from the new image's passwd and `ContainerCreate` hard-fails with
  `unable to find user app: no matching entries in passwd file`, leaving the
  service down (observed migrating a service to distroless, 2026-05-25).
  Fix: `GetContainer` flags the fallback state (`imageInfoIsFallback`), and
  `GetCreateConfig` clears a mismatched `User` in that state so the target
  image's own `USER` applies. Mirrors the existing `clearHostnameOnRecreate`
  pattern — a narrowly-scoped recreate-time adjustment, not a change to the
  global image-default diff. `Entrypoint`/`Cmd`/`WorkingDir`/`Healthcheck` share
  the same fallback-baseline flaw but only silently preserve stale values; only
  `User` hard-fails `ContainerCreate`, so it is the one addressed here. ([ecabfb4](https://github.com/openserbia/watchtower/commit/ecabfb4))

### Added
- **`--preflight` flag (env `WATCHTOWER_PREFLIGHT`, default `false`) — a Docker
  API capability check at startup.** Before scheduling any polls, Watchtower
  probes each Docker endpoint it needs and aborts with an actionable error if a
  required one is blocked (e.g. filtered out by a socket proxy) or unreachable,
  instead of failing mid-update after the old container is already gone. Every
  probe is side-effect-free — a request against a deliberately bogus target, so
  nothing is created, started, or removed — and the three outcomes are
  distinguished: a permitting daemon answers with a logical error
  (not-found / bad-request / conflict) → **present**, a socket proxy answers
  403 → **blocked**, an unreachable daemon yields a transport error →
  **unreachable**. The required set is derived from the active flags so the
  probe never demands more than the run will use: image pull is skipped under
  `--no-pull`, the whole recreate write set under `--monitor-only`, image
  removal only with `--cleanup`, the init-wait only with `--rerun-init-deps`,
  and the exec capability only when a watched container actually declares a
  lifecycle label. The error names both the Docker endpoint and the
  socket-proxy variable for the first failing capability, so the fix is a
  one-shot allow-list edit. The event stream (`--watch-docker-events`) is
  treated as an optional accelerator: a missing `/events` only warns and falls
  back to scheduled polling. Opt-in; off by default. See
  [Required capabilities](required-capabilities.md) for the full catalog and a
  ready-to-paste socket-proxy environment block. ([31becce](https://github.com/openserbia/watchtower/commit/31becce))
- **`--update-strategy` flag (env `WATCHTOWER_UPDATE_STRATEGY`) with a new
  blue-green zero-downtime strategy.** Replaces the single global
  `--rolling-restart` boolean with an extensible enum — `recreate` (default;
  stop then recreate, byte-for-byte the historical behavior),
  `rolling-restart` (one container at a time), or `blue-green` (start the new
  container alongside the old, wait until it reports healthy, drain, then retire
  the old). Default stays `recreate`, so a drop-in fork upgrade changes nothing
  for existing users. ([89ce8b1](https://github.com/openserbia/watchtower/commit/89ce8b1))
  - **Blue-green deploys** bring up a "green" container from the stale "blue"
    container's config under a temporary unique name so both run side by side,
    wait for green's Docker `HEALTHCHECK` to report `healthy` (reusing
    `--health-check-timeout` and its per-container label), let a drain window
    elapse so a dynamic label-based reverse proxy (e.g. Traefik) registers green
    and in-flight requests on blue finish, then stop blue and rename green to
    blue's canonical name. A failed health check removes green and leaves blue
    serving, recording the rollback (`watchtower_rollbacks_total`) and the
    post-rollback cooldown. Intended for stateless services behind a dynamic
    reverse proxy with explicit (not name-derived) router/service labels and **no
    published host ports** (two copies can't bind the same port — such
    containers fall back to `recreate` with a warning). Opt in per container; do
    not use it for stateful services (databases, queues).
  - **`--blue-green-drain`** (env `WATCHTOWER_BLUE_GREEN_DRAIN`, default `10s`) —
    how long to keep blue and green running together after green reports healthy,
    before blue is removed. Per-container override via the
    `com.centurylinklabs.watchtower.blue-green.drain` label (`0` disables the
    drain window).
  - **`com.centurylinklabs.watchtower.update-strategy`** label — per-container
    override for the global strategy (`recreate` / `rolling-restart` /
    `blue-green`), so stateless web services get blue-green while databases stay
    on the safe recreate path under the same fleet-wide default.
  - **`--rolling-restart` is now a deprecated alias** for
    `--update-strategy=rolling-restart`. Setting it still works (it maps to the
    rolling-restart strategy and logs a deprecation warning); setting both it and
    a conflicting `--update-strategy` is a startup error. An unknown
    `--update-strategy` value also fails fast at startup.
- **`:latest-dev` images are now multi-arch, matching the goreleaser release
  set.** The dev pipeline previously published `linux/amd64` only: the
  `Dockerfile.self-contained` builder ran natively with no `GOARCH`, and the
  publish step set no `platforms`. It now cross-compiles — builder pinned to
  `$BUILDPLATFORM`, `GOOS`/`GOARCH`/`GOARM` taken from buildx's
  `TARGETOS`/`TARGETARCH`/`TARGETVARIANT`, CGO disabled so no QEMU is needed —
  and publishes a manifest list for
  `linux/amd64,linux/386,linux/arm/v6,linux/arm/v7,linux/arm64,linux/riscv64`,
  identical to `dockers_v2` in `goreleaser.yml`. `:latest-dev` now runs anywhere
  a tagged release does. The in-Dockerfile `go test` was dropped: a cross-arch
  binary can't execute on the build host, and the workflow's dedicated `test`
  job already gates publish. ([d6775d0](https://github.com/openserbia/watchtower/commit/d6775d0))

### Changed
- **Container-update failure alerts now name the image and the reason in the
  message itself.** Notifications render only the log message (not its structured
  fields), so a failed update previously surfaced as a bare
  `Failed to start container` with no hint at which image broke or why — the
  image and the underlying error were visible only in JSON logs. The failed
  start/stop/rename alerts, the `--health-check-gated` rollback alert, and the
  blue-green cutover failures now interpolate the container, image, and error
  into the text, e.g.
  `Failed to start container web (image nginx:latest): Error response from daemon: driver failed programming external connectivity`.
  The structured `image` field and `WithError` are retained for log processors. ([608311c](https://github.com/openserbia/watchtower/commit/608311c), [664a6a4](https://github.com/openserbia/watchtower/commit/664a6a4))
- **Dependency refresh.** Routine bump of Go module dependencies (direct and
  indirect) via `go get -u ./...`, then tidy + re-vendor. Direct: `shoutrrr`
  v0.14.3 → v0.15.1 (the notification backend; a minor bump, no API changes
  needed in `pkg/notifications`), `docker/cli` v29.4.2 → v29.5.2,
  `onsi/ginkgo/v2` v2.28.3 → v2.29.0, `onsi/gomega` v1.40.0 → v1.41.0,
  `golang.org/x/text` v0.36.0 → v0.37.0. Notable indirect:
  `go.opentelemetry.io/otel` (+ `metric`/`trace`/`otelhttp`) v1.43.0 → v1.44.0,
  `prometheus/common` v0.67.5 → v0.68.0, `docker/docker-credential-helpers`
  v0.9.6 → v0.9.7, `mattn/go-colorable` v0.1.14 → v0.1.15, and the
  `golang.org/x/{net,sys,mod,term,tools}` set. Build, tests (`-race`), and lint
  all pass; no source changes required. ([aaf4021](https://github.com/openserbia/watchtower/commit/aaf4021))
- **Dependency footprint reduction: dropped `spf13/viper` and
  `stretchr/testify`** (−10 modules from `go.mod`). `viper` was used only in
  `internal/flags` as an environment-variable reader (no config files, no pflag
  binding) — it is replaced by a small stdlib layer in `internal/flags/env.go`
  (`os.LookupEnv` + `strconv`, with a `parseDuration` that preserves the prior
  `cast.ToDuration` semantics and an `AllEnvKeys` that replaces `viper.AllKeys`
  for the docs-completeness test). Removing it also drops its satellites
  `go-viper/mapstructure/v2`, `spf13/cast`, `spf13/afero`,
  `sagikazarmark/locafero`, `subosito/gotenv`, `pelletier/go-toml/v2`, and
  `fsnotify/fsnotify`. `testify` (and `objx`) are gone now that the few
  `assert`/`require` test files and the `FilterableContainer` mock use Gomega
  and a hand-written stub, matching the project's Ginkgo/Gomega convention. ([89ce8b1](https://github.com/openserbia/watchtower/commit/89ce8b1))
- **Runtime image rebased from `scratch` onto digest-pinned
  `gcr.io/distroless/static-debian13`.** The published images now use Google's
  distroless static base (pinned by its multi-arch manifest-list digest for
  reproducibility and Scorecard) instead of `scratch` with a hand-rolled Alpine
  cert stage. distroless bundles `ca-certificates` and `tzdata`, so the manual
  cert/tzdata copying is gone; the binary still runs as root (the default,
  non-`:nonroot` tag) because Watchtower needs uid 0 for Docker socket access.
  **`linux/386` and `linux/arm/v6` are dropped from the published container
  image** — distroless is not built for those two platforms — but the release
  still produces binary `tar.gz` archives for them, so non-image consumers on
  32-bit x86 / ARMv6 are unaffected. The exec-form `HEALTHCHECK` runs the
  binary directly and needs no shell. ([17e7343](https://github.com/openserbia/watchtower/commit/17e7343))
- **Releases are now cryptographically signed and carry build provenance.**
  GoReleaser signs both the published container images and `checksums.txt` with
  **keyless cosign** (GitHub Actions OIDC → Sigstore, no long-lived keys), and the
  multi-arch image index ships **SLSA build provenance** and a **CycloneDX SBOM**
  (buildx `--provenance` / `--sbom`).
  Verify with `cosign verify` / `cosign verify-blob` and `docker buildx imagetools
  inspect` — see the README "Verifying a release" section. Previously releases
  offered only SHA256 checksums (integrity, not authenticity). The self-contained
  build images also take the version via a `VERSION` build-arg instead of an
  in-container `git describe`, so the dev image builds with no `git`/toolchain
  packages. ([17e7343](https://github.com/openserbia/watchtower/commit/17e7343))

## [1.14.3] - 2026-05-23

### Fixed
- **Self-update recovers the canonical container name when the cached
  Name looks like a previous-cycle random rename target.** The
  v1.14.2 safety net at `restartStaleContainer` end (rename the new
  container back when its name diverged from the cached "original")
  could only catch the case where the cached original was canonical.
  Once one historical self-update produced a random-named container
  (e.g. via the pre-cc3e07c orphan-multiplication path), every
  subsequent self-update faithfully copied that random name forward:
  StartContainer received `c.Name()` = `/random_X`, the safety net
  compared the new container's name to `/random_X`, found them equal,
  and concluded no rename was needed. The canonical name stayed lost
  through the whole chain — observed all afternoon on AX41 across
  ~7 successive self-updates today.
  Fix: a new `util.IsRandName(s)` reports whether `s` has the exact
  shape `util.RandName()` produces (32 chars of `[a-zA-Z]`). When the
  cached Name matches *and* the container carries a
  `com.docker.compose.service` label, `restartStaleContainer`'s
  self-update branch substitutes `/<service>` as the canonical name
  via the new `Container.SetCreateName(s)` override.
  `StartContainer` reads through `c.CreateName()` (override-aware
  wrapper around `c.Name()`), so the new container is created with
  the canonical name from the start — no rescue rename needed and
  no transient random-name window for deploy-notifiers to alert on.
  The existing safety net remains as a backstop for unusual edge
  cases that don't fit the IsRandName signature. ([80034d8](https://github.com/openserbia/watchtower/commit/80034d8))

## [1.14.2] - 2026-05-23

### Fixed
- **Self-update clears Hostname on recreate so `DetectSelfContainerID` stays
  accurate across self-update chains.** The v1.14.0
  `DetectSelfContainerID` mechanism matches `os.Hostname()` against
  watchtower-labeled containers' short IDs to identify "self" vs orphans.
  That lookup is correct only on the *initial* container creation, where
  docker sets `Hostname` to the new short ID by default. On every
  subsequent self-update, `ContainerCreate` carries the previous
  container's `Hostname` through `c.GetCreateConfig()`, freezing it at the
  founding container's short ID forever. The new container's
  `os.Hostname()` then no longer matches any live container's short ID,
  `DetectSelfContainerID` returns `""`, and both `isRunningSelf` and the
  startup-time `CheckForMultipleWatchtowerInstances` degrade to the
  legacy label-only `IsWatchtower` check. On a host with a transient
  orphan window, the cleanup can pick the wrong survivor and SIGTERM the
  canonical `watchtower` instead of the orphan — observed on AX41 today
  during the v1.14.1 → safety-net hop.
  Fix: `restartStaleContainer`'s self-update branch now calls
  `container.SetClearHostnameOnRecreate(true)` before `RenameContainer`.
  `GetCreateConfig` honors the flag by emitting an empty `Hostname`, so
  docker assigns the new container's own short ID as hostname. The next
  self-update then sees `os.Hostname() == own short ID` and self-detection
  remains accurate — for every link in the chain, not just the first. ([cc3e07c](https://github.com/openserbia/watchtower/commit/cc3e07c))
- **Belt-and-suspenders backstop: self-update verifies the new container's
  name and renames it back when it diverged.** Even with the
  Hostname-clear root-cause fix above, the rename-and-respawn pattern can
  still leave a divergent name in edge cases (e.g. a transient orphan that
  is treated as "self" before the new container is fully up).
  `restartStaleContainer` now captures `originalName` before the rename,
  and on success inspects the freshly-created self container and renames
  it back when its name diverges. The check is best-effort: rename
  failures log a warning but never abort the update, since the service
  half of the contract (right image, healthy) is already satisfied by the
  time the name is checked. ([487eaef](https://github.com/openserbia/watchtower/commit/487eaef))

## [1.14.1] - 2026-05-23

### Fixed
- **Release pipeline.** v1.14.0's tag-driven release workflow failed at
  the Lint step because golangci-lint 2.12.2's `mnd` (magic number) and
  `unparam` rules flagged three items in the new `--rerun-init-deps`
  code: two magic literals in `parseComposeDependsOn` (now named
  constants `composeDepMaxFields` / `composeDepMinFieldsForCondition`)
  and an always-"openserbia" `project` parameter in the rerun test
  fixture (now a package-level `composeProjectName` constant).
  Behavior is identical to v1.14.0 as specified; only the build
  pipeline differs. No GitHub release or Docker image ever published
  for the v1.14.0 tag. ([0a7970a](https://github.com/openserbia/watchtower/commit/0a7970a))

### Docs
- **README badges.** Added Release-workflow status, OpenSSF Scorecard,
  and Snyk vulnerability badges; stubbed an OpenSSF Best Practices
  badge as an HTML comment pending self-attestation registration on
  bestpractices.dev. ([a68e840](https://github.com/openserbia/watchtower/commit/a68e840))

## [1.14.0] - 2026-05-23

### Added
- **`--rerun-init-deps` honors Compose `service_completed_successfully` on
  every update.** Compose's `depends_on: { condition: service_completed_successfully }`
  is evaluated only by `docker compose up`, never by Watchtower's
  container-level update loop. Stacks that moved bootstrap logic out of an
  `entrypoint.sh` wrapper and into a sibling init container (the canonical
  example: a `migrate` service that runs `goose up` against the new image)
  silently regressed to "new code, old schema" on every Watchtower-driven
  restart. The new opt-in flag closes that gap by re-executing each
  `service_completed_successfully` init sibling against the resolved new
  digest *before* the target container is recreated. The old target keeps
  serving traffic the entire time the init runs — if the init container
  exits non-zero, the new digest is cached in an in-process rejected-digest
  map and the target stays on its old image until the registry serves a
  different digest (operator pushed a fix). The failed init container is
  left in `Exited(N)` state for `docker logs` inspection. Same-image init
  containers (migrate using the same image tag as the target — common
  pattern when goose is bundled into the app image) inherit the target's
  freshly-resolved digest so both run against identical bits; different-image
  init containers (e.g. `pg-ready: postgres:18`) keep their own pinning.
  Independent of `--compose-depends-on`; both can be enabled together.
  Migrations must be backwards-compatible with the previous image — the old
  container keeps serving while the init runs, so the schema in between
  must be readable by both versions. New package `internal/initrerun`,
  new client method `RerunInitContainer` (unconditional start that bypasses
  the `IsRunning()` gate so `Exited(0)` init containers actually run again),
  new parser `Container.ComposeInitDependencies()` (preserves the
  `service_completed_successfully` filter that `ComposeDependencies()`
  intentionally strips for the sorter). ([8452c54](https://github.com/openserbia/watchtower/commit/8452c54))

### Fixed
- **Self-update no longer multiplies orphan watchtower containers.** The
  rename-and-respawn pattern in `restartStaleContainer` was gated by
  `IsWatchtower()` — a label-only check that returns true for *every*
  watchtower-labeled container in the scan, not just the running self.
  When a previous self-update left a renamed orphan around (e.g. its
  startup `CheckForMultipleWatchtowerInstances` was outraced or the
  orphan's `restart: unless-stopped` policy fought the cleanup), the next
  scan treated the orphan as another "self," renamed it to a fresh random
  string, and created a *new* container under the orphan's prior random
  name. After a few cycles the operator's container was a chain of
  random-named replacements with the original `watchtower` name lost
  entirely, and the deploy-notifier (or any monitor watching `start`
  events with the watchtower label) fired for each random name.
  `cmd/root.go` now detects the watchtower process's own container ID
  once at startup by matching `os.Hostname()` against
  watchtower-labeled containers' short IDs, and threads it through
  `UpdateParams.SelfContainerID`. `stopStaleContainer` and
  `restartStaleContainer` use `cont.ID() == params.SelfContainerID` for
  the self check; orphans take the normal stop+remove path and skip the
  recreate (resurrecting an orphan under its random name was the original
  bug). The legacy `IsWatchtower()` check remains as a fallback for
  watchtower running outside a container or with `--hostname` overridden
  off the short-ID default. ([e1aedb6](https://github.com/openserbia/watchtower/commit/e1aedb6))
- **Self-update notification noise filtered.** Same root cause — orphan
  rename-respawn — was emitting a `Found new image` line on every poll
  for every container the registry had a newer digest for, *even when an
  image cooldown was holding the update*. With `WATCHTOWER_IMAGE_COOLDOWN`
  set to 72h on a single container, that produced ~4320 identical log
  lines before the cooldown elapsed, all paired with `Skipping update:
  image cooldown window has not elapsed`. Both messages, plus the
  per-poll `Skipping update: container is on post-rollback cooldown`
  during the 1h post-rollback window, are now `Debug` so the default
  INFO log captures only state transitions: the `Stopping` /
  `Creating` / `Removing image` lines and the per-scan `Session done`
  summary already report what actually happened. Operators who need
  visibility into "registry has a newer digest but cooldown is holding"
  can flip `WATCHTOWER_DEBUG=true`. ([e1aedb6](https://github.com/openserbia/watchtower/commit/e1aedb6), [ad677f8](https://github.com/openserbia/watchtower/commit/ad677f8))

### Changed
- **HTTP API startup banner downgraded from INFO to DEBUG.** Three
  startup lines moved off INFO: `Watchtower HTTP API listening on …`
  (in `pkg/api/api.go`), `The HTTP API is enabled at …` (in
  `cmd/root.go`'s `writeStartupMessage`), and `Serving /v1/metrics
  without token auth — …` (only emitted when
  `--http-api-metrics-no-auth` is set). The first two duplicated each
  other and were just metadata; the third is a security-relevant choice
  but reflects the operator's own configuration, not a runtime
  condition that needs an alert. With these gone, default-INFO logs
  show only Watchtower's actual scan/update activity instead of being
  front-loaded with config echo. Visible again under `WATCHTOWER_DEBUG`. ([e1aedb6](https://github.com/openserbia/watchtower/commit/e1aedb6))

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
  required. ([e855f79](https://github.com/openserbia/watchtower/commit/e855f79))
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
  now structured the same way. ([0a28559](https://github.com/openserbia/watchtower/commit/0a28559), [9ce1891](https://github.com/openserbia/watchtower/commit/9ce1891))

### Added
- **`--disable-memory-swappiness` for Podman / cgroupv2 hosts.** Podman with
  crun on cgroupv2 rejects the implicit `MemorySwappiness=0` Docker writes
  when the field is unset, but the inspected `HostConfig` still carries that
  `0`, so a Watchtower recreate that copies the inspected config back through
  `ContainerCreate` failed with `swappiness must be in the range [0, 100]`.
  When this flag (or `WATCHTOWER_DISABLE_MEMORY_SWAPPINESS=true`) is set,
  `MemorySwappiness` is dropped from the recreate request — Podman accepts
  the absent field. No-op on Docker hosts (opt-in). Borrowed from
  [beatkind/watchtower](https://github.com/beatkind/watchtower/commit/70426e1838cf78416faaddae4c71dc420b82199e). ([cd8e77f](https://github.com/openserbia/watchtower/commit/cd8e77f))
- **Typed pull-error sentinels for unauthorized and not-found.** `PullImage`
  now wraps daemon errors as `ErrPullImageUnauthorized` (HTTP 401) and
  `ErrPullImageNotFound` (HTTP 404), logging auth failures at warn (so a
  rotated credential doesn't sit silent at debug) and missing manifests
  at debug (so a transient registry blip doesn't escalate to warn). The
  underlying `cerrdefs` error stays in the chain via `fmt.Errorf("%w: %w")`,
  so the existing local-build safeguard in `pullFailureLooksLocal` keeps
  firing on bare-name references — no behaviour change for the silent-skip
  path. Borrowed from
  [nicholas-fedor/watchtower#1477](https://github.com/nicholas-fedor/watchtower/pull/1477). ([67b8a9c](https://github.com/openserbia/watchtower/commit/67b8a9c))

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
  cut healthy multi-GB pulls off mid-stream). ([cd8e77f](https://github.com/openserbia/watchtower/commit/cd8e77f))
- **Startup log reflects the actual HTTP API listen address.** When
  `--http-api-update` was enabled, the startup banner always said
  `The HTTP API is enabled at :8080.` even if the operator had bound to
  `127.0.0.1:9090` via `--http-api-host`. The log now reads from the same
  flag the API server uses, so the banner matches reality. ([9ce1891](https://github.com/openserbia/watchtower/commit/9ce1891))
- **Misconfigured port bindings no longer abort a recreate.** A compose
  file with `ports: ["8080:"]` (or any entry that resolves to an empty port
  number) used to fall through to `ContainerCreate` and surface as Docker's
  opaque `invalid port range: value is empty`, leaving operators chasing a
  failure whose root cause was upstream of watchtower. `VerifyConfiguration`
  now strips empty / `"/proto"`-only entries from `HostConfig.PortBindings`
  and `Config.ExposedPorts` before the create call, logging a warn that
  identifies the offending key and the affected container. Borrowed from
  [nicholas-fedor/watchtower#1478](https://github.com/nicholas-fedor/watchtower/pull/1478). ([67b8a9c](https://github.com/openserbia/watchtower/commit/67b8a9c))
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
  new build doesn't get re-created on rollback either. ([5c0617c](https://github.com/openserbia/watchtower/commit/5c0617c))
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
  containers self-heal on the next poll. ([5c0617c](https://github.com/openserbia/watchtower/commit/5c0617c))
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
  shape and per-image still-in-use guard are unchanged. ([178bd7a](https://github.com/openserbia/watchtower/commit/178bd7a))
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
  are still respected. ([46addc5](https://github.com/openserbia/watchtower/commit/46addc5))
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
  surface loudly instead of being silently masked. ([46addc5](https://github.com/openserbia/watchtower/commit/46addc5))
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
  qualified refs, auth errors, 5xx) still increment as before. ([b771007](https://github.com/openserbia/watchtower/commit/b771007))
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
  removal). Real daemon errors (connection, 5xx, auth) still count. ([536e843](https://github.com/openserbia/watchtower/commit/536e843))

### Docs
- **`docs/arguments.md` corrected to reflect actual `--api-version`
  default.** The page advertised `"1.24"`; the real default has been
  the empty string (i.e. negotiate) for as long as `SetDefaults` has
  existed. Expanded the section with a migration note for operators
  who have `DOCKER_API_VERSION=1.44` left in their compose or env
  file from an older deployment — removing it is the recommended
  step on Docker 25+ to unlock the Identity-based local-build
  detection. ([46addc5](https://github.com/openserbia/watchtower/commit/46addc5))

## [1.12.2] - 2026-04-20

### Added
- **`--version` flag on the root command.** `watchtower --version` now
  prints the compile-time version (the same `internal/meta.Version`
  value used in the HTTP `User-Agent`) without having to start the
  daemon or `docker inspect` the image. Makes support triage and image
  tag verification a one-liner. ([50e9d3c](https://github.com/openserbia/watchtower/commit/50e9d3c))
- **New Prometheus counter `watchtower_docker_api_retries_total{operation}`.**
  Pairs with the new `ListContainers` retry (see below) and follows the
  shape of `watchtower_registry_retries_total`. One increment per retry
  attempt, not per failed call. A sustained non-zero value points at a
  flaky daemon — daemon restarts during polls, socket-proxy blips, or
  engine-API 5xx under load. ([99a4325](https://github.com/openserbia/watchtower/commit/99a4325))

### Changed
- **`WATCHTOWER_NOTIFICATIONS_LEVEL` is now honored in report mode.**
  Before, the default report template fired a per-poll summary
  regardless of level — a strict `NOTIFICATIONS_LEVEL=warn` still got
  paged on routine successful updates. Now a report-mode notification
  is suppressed when everything in the batch is below the configured
  threshold: no level-appropriate log entries AND no failed or
  error-marked skipped containers. At info/debug/trace thresholds
  behavior is unchanged (verbose mode fires for any report). Inspired
  by [nicholas-fedor/watchtower#1290](https://github.com/nicholas-fedor/watchtower/pull/1290). ([99a4325](https://github.com/openserbia/watchtower/commit/99a4325))
- **Pinned-image skip demoted from `warn` to `debug`.** Containers
  whose tag is a `sha256:...` digest can never be updated (there's no
  moving target), so the per-poll `Unable to update container ...
  Proceeding to next.` was noise operators couldn't act on. The typed
  `container.ErrPinnedImage` sentinel is now used at the call site in
  `actions.Update` to demote the log line without altering the skip
  semantics. Existing error message is preserved verbatim so
  downstream log parsers and notification templates keep matching. ([99a4325](https://github.com/openserbia/watchtower/commit/99a4325))
- **Pre-update lifecycle command failures logged at `warn`, not `error`.**
  User-defined `com.centurylinklabs.watchtower.lifecycle.pre-update`
  scripts are user code; a failure is the script's problem, not
  watchtower's orchestration. Strict `NOTIFICATIONS_LEVEL=error`
  feeds no longer fire for user-script flakes. The container is still
  skipped and still recorded in the session report. ([99a4325](https://github.com/openserbia/watchtower/commit/99a4325))

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
  the SDK's `ImageInspectWithRawResponse` option — no SDK bump needed. ([50e9d3c](https://github.com/openserbia/watchtower/commit/50e9d3c))
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
  [nicholas-fedor/watchtower#1459](https://github.com/nicholas-fedor/watchtower/pull/1459). ([99a4325](https://github.com/openserbia/watchtower/commit/99a4325))
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
  [nicholas-fedor/watchtower#1522](https://github.com/nicholas-fedor/watchtower/pull/1522). ([99a4325](https://github.com/openserbia/watchtower/commit/99a4325))
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
  [nicholas-fedor/watchtower#1428](https://github.com/nicholas-fedor/watchtower/pull/1428). ([99a4325](https://github.com/openserbia/watchtower/commit/99a4325))

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
  the next poll. ([2496562](https://github.com/openserbia/watchtower/commit/2496562))

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
  [nicholas-fedor/watchtower#1514](https://github.com/nicholas-fedor/watchtower/pull/1514). ([4c68ee7](https://github.com/openserbia/watchtower/commit/4c68ee7))
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
  [nicholas-fedor/watchtower#1481](https://github.com/nicholas-fedor/watchtower/pull/1481). ([398a67e](https://github.com/openserbia/watchtower/commit/398a67e))
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
  [nicholas-fedor/watchtower#669](https://github.com/nicholas-fedor/watchtower/pull/669). ([e78e0bf](https://github.com/openserbia/watchtower/commit/e78e0bf))

### Changed
- **`/v1/metrics` no-auth startup log demoted from `warn` to `info`.**
  The message is informational (operator opted in to public scraping) —
  not a warning about anything going wrong. Stops muddying
  `NOTIFICATIONS_LEVEL=warn` feeds. ([398a67e](https://github.com/openserbia/watchtower/commit/398a67e))

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
  conditions. ([4c16bf2](https://github.com/openserbia/watchtower/commit/4c16bf2))
- New `Container.ComposeProject()`, `ComposeService()`,
  `ComposeDependencies()` methods on the `types.Container` interface
  for programmatic use. ([4c16bf2](https://github.com/openserbia/watchtower/commit/4c16bf2))
- **`--image-cooldown`** flag (env: `WATCHTOWER_IMAGE_COOLDOWN`) +
  per-container label `com.centurylinklabs.watchtower.image-cooldown` —
  supply-chain gate that defers applying a new image digest until it has
  been stable for the configured duration. If the registry serves a
  different digest during the window (author re-pushed), the clock
  resets. Directly addresses the long-standing "broken `:latest` push
  reaches prod in one poll interval" rough edge. Default `0` keeps
  existing behavior unchanged. ([4c16bf2](https://github.com/openserbia/watchtower/commit/4c16bf2))
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
  in front. Inspired by [nicholas-fedor/watchtower#697](https://github.com/nicholas-fedor/watchtower/pull/697). ([4c16bf2](https://github.com/openserbia/watchtower/commit/4c16bf2))
- **Auto-detect container stop timeout.** If a container was started
  with its own `StopTimeout` (via `docker run --stop-timeout` or
  Compose's `stop_grace_period`), Watchtower honors that value instead
  of the global `--stop-timeout`. Matches Docker's own precedence of
  per-container over daemon default. New `Container.StopTimeout()`
  method on the `types.Container` interface. Inspired by
  [nicholas-fedor/watchtower#1182](https://github.com/nicholas-fedor/watchtower/pull/1182). ([4c16bf2](https://github.com/openserbia/watchtower/commit/4c16bf2))
- **`--update-on-start`** flag (env: `WATCHTOWER_UPDATE_ON_START`) — run
  one scan immediately at startup in addition to the scheduled cadence,
  so operators can verify a fresh deployment without waiting for the
  first poll interval. Skipped if the HTTP API already holds the update
  lock at boot. Inspired by [nicholas-fedor/watchtower#672](https://github.com/nicholas-fedor/watchtower/pull/672). ([4c16bf2](https://github.com/openserbia/watchtower/commit/4c16bf2))
- **Structured JSON response from `GET /v1/update`**. The endpoint now
  returns `{"status": "completed", "scanned": N, "updated": N, "failed": N}`
  on success instead of an empty 200 body — automation can tell how the
  scan went without scraping logs. Inspired by
  [nicholas-fedor/watchtower#673](https://github.com/nicholas-fedor/watchtower/pull/673). ([4c16bf2](https://github.com/openserbia/watchtower/commit/4c16bf2))

### Changed
- **`/v1/update` returns HTTP 429** (with a `{"status":"skipped","reason":...}`
  body) when another update is already running, instead of silently
  succeeding with 200. Lets clients retry with backoff. Targeted updates
  (`?image=<name>`) still block on the lock rather than 429-ing, because a
  caller explicitly asking for an image usually wants it eventually.
  Inspired by [nicholas-fedor/watchtower#1304](https://github.com/nicholas-fedor/watchtower/pull/1304). ([4c16bf2](https://github.com/openserbia/watchtower/commit/4c16bf2))

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
  overdue. ([0a49fa6](https://github.com/openserbia/watchtower/commit/0a49fa6))

## [1.11.2] - 2026-04-18

### Fixed
- **`docs/introduction.md`** — the `centurylink/wetty-cli` example image
  was an upstream-era artefact. Replaced with a coherent `nginx:latest`
  walkthrough (container name, image, and port mapping now match). ([51a2559](https://github.com/openserbia/watchtower/commit/51a2559))
- **`docs/notifications.md`** — the shoutrrr reference pointed at the
  upstream `containrrr/shoutrrr` project and its `v0.8` docs, but the
  fork actually vendors `nicholas-fedor/shoutrrr v0.14.3`. Updated the
  link and added a one-line note on fork lineage + URL compatibility. ([51a2559](https://github.com/openserbia/watchtower/commit/51a2559))
- **`docs/secure-connections.md`** — rewritten around the supported
  `DOCKER_HOST` + `DOCKER_CERT_PATH` + `DOCKER_TLS_VERIFY` path with a
  Compose example. `docker-machine` demoted to a "works if you still
  have the certs it generated" footnote (Docker archived the tool in
  2023). Added a pointer differentiating daemon TLS from registry TLS. ([51a2559](https://github.com/openserbia/watchtower/commit/51a2559))
- **`docs/http-api-mode.md`** — removed a reference to a
  `WatchtowerAPIUnauthorizedBurst` alert that was dropped during the
  production-tuning pass but still advertised as "shipped"; replaced
  with the PromQL snippet for operators who want to compose their own.
  Added an **Env** column to the endpoint table, tightened the
  `/v1/update` response-shape prose, quoted Compose port mappings. ([7fac931](https://github.com/openserbia/watchtower/commit/7fac931))
- **`docs/why-fork.md`** — expanded *What changed* from a single
  toolchain table into four grouped tables (Project health, Update
  behavior, Security, Observability) so the comparison isn't just
  "we updated Go". Added rows for the shoutrrr swap, registry TLS
  default change, constant-time token compare, `/v1/audit`, metric
  count, and the shipped observability bundle. ([7fac931](https://github.com/openserbia/watchtower/commit/7fac931))

### Changed
- **CI workflows** — set `FORCE_JAVASCRIPT_ACTIONS_TO_NODE24=true`
  across every GitHub Actions workflow so JS-based actions run on
  Node 24 uniformly (instead of inheriting whatever default the
  specific action ships with). ([e51c282](https://github.com/openserbia/watchtower/commit/e51c282))

## [1.11.1] - 2026-04-18

### Added
- **`infrastructure` audit bucket** — containers matching Docker-managed
  scaffolding (image prefixes `moby/buildkit*` / `docker/desktop-*`, label
  prefixes `com.docker.buildx.*` / `com.docker.desktop.*`) are now
  classified as `infrastructure` instead of `unmanaged`. Silences the
  recurring audit warning every `docker buildx build` caused by the
  ephemeral `buildx_buildkit_*` container. Exposed via: ([eed614c](https://github.com/openserbia/watchtower/commit/eed614c))
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
  post-deploy verification. Token-gated like `/v1/update`. ([a1943ea](https://github.com/openserbia/watchtower/commit/a1943ea))
- **Three new Prometheus gauges** — `watchtower_containers_managed`,
  `watchtower_containers_excluded`, `watchtower_containers_unmanaged` —
  published every poll regardless of whether any audit flag is set, so the
  Grafana dashboard shows the watch-status breakdown at a glance. Dashboard
  (`observability/grafana/watchtower-dashboard.json`) adds a donut, a
  stat-with-threshold for unmanaged, and a stacked history panel. Alerts
  add `WatchtowerUnmanagedContainersPresent` (info, >1 h). ([a1943ea](https://github.com/openserbia/watchtower/commit/a1943ea))
- **`watchtower_poll_interval_seconds`** gauge — configured scan cadence
  derived from the active schedule at startup. Replaces the hardcoded 2 h
  window in the `WatchtowerScansStopped` alert with
  `(time() - last_scan) > 2 × poll_interval`, so long-cadence deployments
  (e.g. `@every 12h`) no longer false-alarm. ([34ee213](https://github.com/openserbia/watchtower/commit/34ee213))

### Changed
- **Observability artifacts** (`observability/`) aligned with production
  tuning. Alerts trimmed to the six that have proven actionable
  (`WatchtowerRollbackTriggered`, `WatchtowerScansStopped`,
  `WatchtowerFailuresSustained`, `WatchtowerUnmanagedContainersPresent`,
  `WatchtowerRegistryErrorsSustained`,
  `WatchtowerDockerAPIErrorsSustained`); noise-heavy candidates
  (`WatchtowerAllScansSkipped`, `WatchtowerScansWithoutUpdates`,
  `WatchtowerAPIUnauthorizedBurst`) dropped. Descriptions tightened to
  single-line operational prose with `humanizeDuration` templating. ([7dbc891](https://github.com/openserbia/watchtower/commit/7dbc891))
- Dashboard gains a collapsed "Logs (requires Loki)" row with two panels
  (warn/error log rate + logs explorer, both querying
  `{container="watchtower"}`). Uses a new `DS_LOKI` datasource variable
  so operators without Loki can pick "Do not save" at import time and the
  rest of the dashboard keeps working. ([7dbc891](https://github.com/openserbia/watchtower/commit/7dbc891))
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
  and `WatchtowerDockerAPIErrorsSustained`. ([6d55ab8](https://github.com/openserbia/watchtower/commit/6d55ab8))
- **`--http-api-metrics-no-auth`** flag (env:
  `WATCHTOWER_HTTP_API_METRICS_NO_AUTH`). Exposes `/v1/metrics` without
  bearer-token auth, matching Prometheus convention for trusted-network
  scraping. `/v1/update` remains token-gated unconditionally. When only the
  (public) metrics endpoint is enabled, `--http-api-token` is no longer
  required to start the daemon. ([a1943ea](https://github.com/openserbia/watchtower/commit/a1943ea))

- `--audit-unmanaged` is no longer spammy. The audit warns about each
  unlabeled container the first time it appears (startup baseline) and then
  stays silent on subsequent polls unless the set changes — a new unlabeled
  container shows up, or a previously-unlabeled one gets labeled or removed.
  Same signal, orders of magnitude less log noise for stable homelabs. ([a1943ea](https://github.com/openserbia/watchtower/commit/a1943ea))

### Removed
- **`notify-upgrade` subcommand** (`cmd/notify-upgrade.go`). The helper
  generated a shoutrrr-URL env file from the pre-shoutrrr notification
  flags — a migration tool for an upstream cut-over that happened years
  ago and nobody invokes any more. The legacy `--notification-email-*` /
  `--notification-slack-*` / `--notification-gotify-*` / MSTeams flags
  remain supported via the shim in `pkg/notifications`, so existing
  deployments keep working. If you were scripting `docker run openserbia/watchtower
  notify-upgrade`, that invocation now exits with "unknown command"; either
  pin to `v1.10.x` or switch to writing shoutrrr URLs directly. ([a1943ea](https://github.com/openserbia/watchtower/commit/a1943ea))

### Security
- `api.RequireToken` now uses `crypto/subtle.ConstantTimeCompare` instead of
  `!=` when checking the bearer token, closing a theoretical timing-oracle
  on `:8080`. ([a1943ea](https://github.com/openserbia/watchtower/commit/a1943ea))

## [1.10.1] - 2026-04-18

### Fixed
- Internal: named the two literal `2`s used in the rollback-timeout computation
  so `golangci-lint`'s `mnd` rule stops flagging them. No user-visible behavior
  change; ships a clean lint run for the release pipeline. ([38e2be2](https://github.com/openserbia/watchtower/commit/38e2be2))

## [1.10.0] - 2026-04-18

### Added
- **`com.centurylinklabs.watchtower.health-check-timeout`** label — per-container
  override for `--health-check-timeout`, accepts a Go duration string. Highest
  priority in the resolution chain (label → HEALTHCHECK-derived → global flag
  → 60s fallback). ([855052c](https://github.com/openserbia/watchtower/commit/855052c))
- **Smart default** for health-check gating timeout when neither label nor
  global flag is set: derives
  `start_period + retries × (interval + timeout)` from the container's own
  `HEALTHCHECK` config (or the image's default). Believes the image author's
  declaration rather than one-size-fits-all. ([855052c](https://github.com/openserbia/watchtower/commit/855052c))
- **`watchtower_rollbacks_total`** Prometheus counter — incremented whenever
  `--health-check-gated` reverts a container. Exposed via `/v1/metrics`. The
  shipped alert (`WatchtowerRollbackTriggered` in
  `observability/prometheus/alerts.yml`) fires on any non-zero 1h increase. ([855052c](https://github.com/openserbia/watchtower/commit/855052c))
- **Rollback health gating + cooldown.** The rolled-back container is itself
  health-gated with a shorter timeout (half the effective). If both the new
  and old images fail, Watchtower logs at `error` with `rollback_failed=true`
  and leaves the container in place for manual intervention. After any
  rollback, the container is skipped on subsequent polls for 1 hour
  (`rollbackCooldown` in `internal/actions/update.go`) to prevent the
  stop → start → fail → rollback thrash loop when an image author keeps
  pushing broken versions. ([855052c](https://github.com/openserbia/watchtower/commit/855052c))
- **`--health-check-gated`** + **`--health-check-timeout`** (envs:
  `WATCHTOWER_HEALTH_CHECK_GATED`, `WATCHTOWER_HEALTH_CHECK_TIMEOUT`,
  default 60s). Opt-in: after recreating a container, wait for its
  `State.Health.Status` to become `healthy`. If it reports unhealthy or
  times out, stop the replacement and rebuild the old container from the
  preserved config+image. Containers without a `HEALTHCHECK` skip the gate
  and emit a warning. Addresses the [upstream#1385](https://github.com/containrrr/watchtower/issues/1385)
  family ("update pulled, replaced, everything broke"). ([855052c](https://github.com/openserbia/watchtower/commit/855052c))
- **`--insecure-registry`** (env: `WATCHTOWER_INSECURE_REGISTRY`) — comma-separated
  list of registry hosts (`host` or `host:port`) for which TLS certificate
  verification is skipped. Replaces the previous hardcoded
  `InsecureSkipVerify: true` in `pkg/registry/digest`: verification is now
  strict (TLS 1.2+, system trust store) by default and the operator opts in
  per host. ([855052c](https://github.com/openserbia/watchtower/commit/855052c))
- **`--registry-ca-bundle`** (env: `WATCHTOWER_REGISTRY_CA_BUNDLE`) — PEM file
  of additional trusted CA certificates. Extends the system trust store rather
  than replacing it, so public registries keep working while registries signed
  by a private CA also validate cleanly. ([855052c](https://github.com/openserbia/watchtower/commit/855052c))
- **`observability/`** directory — ships a Grafana dashboard
  (`grafana/watchtower-dashboard.json`) and a set of Prometheus alerting rules
  (`prometheus/alerts.yml`) covering scheduler wedges, sustained failures,
  and silent-update gaps. First thing to wire up after enabling
  `WATCHTOWER_HTTP_API_METRICS`. ([855052c](https://github.com/openserbia/watchtower/commit/855052c))

### Changed
- Registry HTTP calls now flow through a new `pkg/registry/transport` package
  that builds per-host `http.Client`s with the right TLS policy. The auth
  challenge and bearer-token exchange (previously using bare `http.Client{}`)
  now honor the same TLS tuning as the manifest HEAD request. ([855052c](https://github.com/openserbia/watchtower/commit/855052c))

### Security
- Fixed a long-standing weakness where `pkg/registry/digest.GetDigest`
  unconditionally set `InsecureSkipVerify: true` for *all* registries, not
  just configured-insecure ones. Strict verification is now the default; the
  old behavior is available as an explicit per-host opt-in via
  `--insecure-registry`. ([855052c](https://github.com/openserbia/watchtower/commit/855052c))

## [1.9.0] - 2026-04-18

### Added
- **`--audit-unmanaged`** flag (env: `WATCHTOWER_AUDIT_UNMANAGED`). With
  `--label-enable` active, warns once per poll for every container that carries
  no `com.centurylinklabs.watchtower.enable` label at all, so silent exclusions
  stop looking identical to intentional opt-outs. ([9fa9e44](https://github.com/openserbia/watchtower/commit/9fa9e44))
- **Bounded exponential backoff** for registry HTTP calls (`pkg/registry/retry`).
  Wraps the oauth challenge, bearer-token exchange, and manifest HEAD with up to
  3 attempts (500 ms → 4 s + jitter) on network errors, 5xx, 429, and the
  401/403/404 flakes observed on registry oauth endpoints under load. ([9fa9e44](https://github.com/openserbia/watchtower/commit/9fa9e44))
- **In-memory bearer-token cache** (`pkg/registry/auth`). Cuts registry
  authentication traffic dramatically: a poll across N containers on the same
  registry+repository scope now issues one token exchange instead of N. Keyed
  by auth URL + credential hash, respects the registry's `expires_in` (default
  60 s per the Docker token spec) minus a 10 s skew, and is concurrency-safe.
  Also reduces exposure to the oauth-endpoint flakes the retry wrapper handles. ([a1c6c2e](https://github.com/openserbia/watchtower/commit/a1c6c2e))

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
  ("Unable to update container: Error: No such image" loop). ([c12200c](https://github.com/openserbia/watchtower/commit/c12200c))
- **`--cleanup` no longer deletes the freshly-pulled replacement image** after
  the fallback path above kicks in. Cleanup now targets `containerInfo.Image`
  (the ID Docker recorded at container creation) via the new
  `Container.SourceImageID()`, not whatever `imageInfo` currently holds.
  `RemoveImageByID` also treats `NotFound` as success so already-GC'd old
  images stop logging spurious errors. Fixes
  [upstream#966](https://github.com/containrrr/watchtower/issues/966)
  (`conflict: unable to delete <id> - image is being used by running container`). ([9fa9e44](https://github.com/openserbia/watchtower/commit/9fa9e44))
- **Compose-deploy races** (`docker compose up` between two polls) no longer
  abort the entire scan. `ListContainers` skips containers that vanish between
  list and inspect, and `StopContainer` tolerates `NotFound` on kill. ([9fa9e44](https://github.com/openserbia/watchtower/commit/9fa9e44))
- **Pull-failure log level raised** from `info` to `warn` in
  `actions.Update`, so operators running `WATCHTOWER_NOTIFICATIONS_LEVEL=error`
  are actually notified of stuck containers instead of the failure being
  silently swallowed. ([9fa9e44](https://github.com/openserbia/watchtower/commit/9fa9e44))

### Changed
- `Container` interface gained `SourceImageID()` — returns the raw image ID
  Docker recorded against the container at creation time, stable across
  imageInfo fallbacks. Existing `ImageID()` / `SafeImageID()` semantics are
  unchanged. ([9fa9e44](https://github.com/openserbia/watchtower/commit/9fa9e44))

[Unreleased]: https://github.com/openserbia/watchtower/compare/v1.18.3...HEAD
[1.18.3]: https://github.com/openserbia/watchtower/compare/v1.18.2...v1.18.3
[1.18.2]: https://github.com/openserbia/watchtower/compare/v1.17.0...v1.18.2
[1.17.0]: https://github.com/openserbia/watchtower/compare/v1.15.1...v1.17.0
[1.15.1]: https://github.com/openserbia/watchtower/compare/v1.15.0...v1.15.1
[1.15.0]: https://github.com/openserbia/watchtower/compare/v1.14.3...v1.15.0
[1.14.3]: https://github.com/openserbia/watchtower/compare/v1.14.2...v1.14.3
[1.14.2]: https://github.com/openserbia/watchtower/compare/v1.14.1...v1.14.2
[1.14.1]: https://github.com/openserbia/watchtower/compare/v1.14.0...v1.14.1
[1.14.0]: https://github.com/openserbia/watchtower/compare/v1.13.0...v1.14.0
[1.13.0]: https://github.com/openserbia/watchtower/compare/v1.12.2...v1.13.0
[1.12.2]: https://github.com/openserbia/watchtower/compare/v1.12.1...v1.12.2
[1.12.1]: https://github.com/openserbia/watchtower/compare/v1.12.0...v1.12.1
[1.12.0]: https://github.com/openserbia/watchtower/compare/v1.11.2...v1.12.0
[1.11.2]: https://github.com/openserbia/watchtower/compare/v1.11.1...v1.11.2
[1.11.1]: https://github.com/openserbia/watchtower/compare/v1.11.0...v1.11.1
[1.11.0]: https://github.com/openserbia/watchtower/compare/v1.10.1...v1.11.0
[1.10.1]: https://github.com/openserbia/watchtower/compare/v1.10.0...v1.10.1
[1.10.0]: https://github.com/openserbia/watchtower/compare/v1.9.0...v1.10.0
[1.9.0]: https://github.com/openserbia/watchtower/compare/v1.8.5...v1.9.0
