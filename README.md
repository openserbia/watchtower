<div align="center">

  <img src="https://raw.githubusercontent.com/openserbia/watchtower/main/logo.png" width="450" />

  # Watchtower — the maintained fork

  **Automatic base-image updates for Docker containers.**
  A drop-in replacement for [`containrrr/watchtower`](https://github.com/containrrr/watchtower), which is [no longer maintained upstream](https://github.com/containrrr/watchtower/discussions/2135).

  [![CI](https://github.com/openserbia/watchtower/actions/workflows/pull-request.yml/badge.svg)](https://github.com/openserbia/watchtower/actions/workflows/pull-request.yml)
  [![codecov](https://codecov.io/gh/openserbia/watchtower/branch/main/graph/badge.svg)](https://codecov.io/gh/openserbia/watchtower)
  [![Go Reference](https://pkg.go.dev/badge/github.com/openserbia/watchtower.svg)](https://pkg.go.dev/github.com/openserbia/watchtower)
  [![Go Report Card](https://goreportcard.com/badge/github.com/openserbia/watchtower)](https://goreportcard.com/report/github.com/openserbia/watchtower)
  [![Latest release](https://img.shields.io/github/v/release/openserbia/watchtower?sort=semver)](https://github.com/openserbia/watchtower/releases)
  [![Docker Hub](https://img.shields.io/docker/v/openserbia/watchtower?label=Docker%20Hub&logo=docker&sort=semver)](https://hub.docker.com/r/openserbia/watchtower)
  [![Docker pulls](https://img.shields.io/docker/pulls/openserbia/watchtower.svg?logo=docker)](https://hub.docker.com/r/openserbia/watchtower)
  [![GHCR](https://img.shields.io/badge/ghcr.io-openserbia%2Fwatchtower-24292f?logo=github)](https://github.com/openserbia/watchtower/pkgs/container/watchtower)
  [![Image size](https://img.shields.io/docker/image-size/openserbia/watchtower/latest?logo=docker)](https://hub.docker.com/r/openserbia/watchtower/tags)
  [![License](https://img.shields.io/github/license/openserbia/watchtower)](./LICENSE.md)

</div>

## TL;DR

- **What it is:** a small Go daemon that polls the Docker socket, checks registries for new image digests, and recreates stale containers with the same config (volumes, networks, env, command).
- **Who it's for:** homelabs, self-hosted stacks, media centers, dev environments — anywhere a running Kubernetes cluster would be overkill.
- **Who it's *not* for:** production workloads that need staged rollouts, canaries, or rollback. Use Kubernetes (or [k3s](https://k3s.io/) / [MicroK8s](https://microk8s.io/)) for that.
- **Images:** `docker.io/openserbia/watchtower` and `ghcr.io/openserbia/watchtower` (multi-arch: amd64, arm64, arm/v6, arm/v7, 386, riscv64 — see [supported architectures](#supported-architectures)).
- **Go module path:** `github.com/openserbia/watchtower`.

## Quick start

Pull from either registry — the images are identical:

```bash
# Docker Hub
docker pull openserbia/watchtower

# GitHub Container Registry (auth-free for public images)
docker pull ghcr.io/openserbia/watchtower
```

Run it:

```bash
docker run --detach \
    --name watchtower \
    --volume /var/run/docker.sock:/var/run/docker.sock \
    openserbia/watchtower
```

That's it — Watchtower will poll every 24h by default and update any container it can see. To scope it, label the containers you want managed:

```bash
docker run --label com.centurylinklabs.watchtower.enable=true my-app:latest
docker run -v /var/run/docker.sock:/var/run/docker.sock \
    openserbia/watchtower --label-enable
```

### docker-compose

Most self-hosted deployments land on compose. Minimal stanza:

```yaml
services:
  watchtower:
    image: openserbia/watchtower:latest
    container_name: watchtower
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    command:
      - --interval=60
      - --cleanup
      - --label-enable
```

Common flags you'll reach for: `--interval 60`, `--cleanup`, `--label-enable`, `--http-api-metrics`, `--notification-url`. Run `docker run --rm openserbia/watchtower --help` for the full list, or see the [docs site](https://openserbia.github.io/watchtower/) — the flag surface is still compatible with upstream.

### Supported architectures

| Platform | Typical host |
|---|---|
| `linux/amd64` | x86-64 servers, most homelabs |
| `linux/arm64` | Raspberry Pi 4/5 (64-bit), Apple-silicon dev machines |
| `linux/arm/v7` | Raspberry Pi 2/3 (32-bit), many cheap SBCs |
| `linux/arm/v6` | Raspberry Pi Zero / 1 |
| `linux/386` | legacy 32-bit x86 |
| `linux/riscv64` | SiFive boards, VisionFive, emulated dev setups |

All six live under the same `:latest` / `:<version>` tag — Docker picks the right variant for your host automatically. No need to specify the arch.

## Why this fork

`containrrr/watchtower` is the de-facto Docker auto-updater, but the upstream repository stopped accepting changes in late 2024. This fork exists to keep it alive with a modern toolchain and fixes driven by real-world homelab usage.

| | `containrrr/watchtower` | **`openserbia/watchtower`** |
|---|---|---|
| Maintenance status | Archived / unmaintained | **Active** |
| Go version | 1.20 | **1.26** |
| Linter | golangci-lint v1 | **golangci-lint v2** (gofumpt + gci) |
| Dev environment | Ad-hoc | **Devbox-pinned** (reproducible, matches CI) |
| Module path | `github.com/containrrr/watchtower` | `github.com/openserbia/watchtower` |
| Dependency updates | Stale | Tracked via Dependabot |
| CI | Travis-era workflows | **Devbox + go-task on GitHub Actions** |
| Knowledge graph for contributors | — | [`code-review-graph`](https://github.com/openserbia/code-review-graph) MCP support wired in |

Drop-in compatible: same CLI flags, same labels (`com.centurylinklabs.watchtower.*`), same HTTP API endpoints, same notification backends (shoutrrr, email, Slack, MS Teams, Gotify). Swap the image name and you're done.

## Known rough edges (fork roadmap)

This fork tracks a running deployment (Timeweb private registry, ~13 watched images, 60s poll) and collects fixes for behaviors that bite in that setup. Contributions welcome on any of these:

1. **No retry-with-backoff on registry auth flakes.** When the registry's oauth endpoint returns a transient 403/404, Watchtower logs `no available image info. Proceeding to next.` and waits for the next poll with an identical request — no exponential backoff, no per-repository circuit breaker. A flaky registry can wedge a single image for minutes while manual `docker pull` succeeds instantly.
2. **Pull failures logged at `info`, not `error`.** `WATCHTOWER_NOTIFICATIONS_LEVEL=error` silently swallows repeated failed pulls for a single container. Neither Watchtower nor a success-only event watcher notifies on a stuck-in-failure loop.
3. **No self-metrics wired up by default.** Prometheus metrics exist (`WATCHTOWER_HTTP_API_METRICS=true` + token) but nothing ships ready-to-scrape. Alerts like `scanned > 0 AND updated == 0 AND image_age > N days` would catch the failure modes above automatically.
4. **Label-based opt-in is fail-open.** A new service without `com.centurylinklabs.watchtower.enable=true` is silently ignored — no security updates, no warning. Indistinguishable from intentional exclusions (databases, stateful stuff). A "tracked by neither Watchtower nor an allowlist" audit would help.
5. **Races with manual compose deploys.** `docker compose pull` between two polls can leave Watchtower's cached container ID pointing at a ghost, producing `No such container` at `level=error`. Benign but noisy enough to drown real failures.
6. **`:latest` everywhere means a broken upstream push reaches prod in one poll interval.** Not a fork bug — a deliberate tradeoff in Watchtower's design — but worth calling out so users opt in consciously.

## Documentation

Full user docs (flags, labels, notifications, lifecycle hooks, HTTP API) are published at **https://openserbia.github.io/watchtower/**. The source lives in [`docs/`](./docs) and is built with MkDocs Material; the site is republished by the `publish-docs` workflow after each successful release.

## Verifying a release

Every GitHub Release publishes `watchtower_<version>_checksums.txt` alongside the binary archives. To verify what you downloaded:

```bash
sha256sum -c watchtower_1.8.3_checksums.txt --ignore-missing
```

Container images carry OCI labels you can inspect to tie a running image back to its source commit:

```bash
docker inspect openserbia/watchtower:latest \
    --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}'
```

## Versioning and release cadence

This fork picks up the upstream version line: `v1.7.1` was upstream's last tag, so the fork starts at `v1.8.0`. We follow semver: patch bumps for fixes and dep updates, minor bumps for behavior-preserving additions, and `v2.0.0` will signal the first intentional break of upstream compatibility (CLI flags, labels, or HTTP API). Releases are cut from `main` whenever a meaningful change accumulates — there's no fixed schedule.

## Security

Report vulnerabilities via **[GitHub Security Advisories](https://github.com/openserbia/watchtower/security/advisories/new)** — this opens a private thread with the maintainers. Please do not file public issues for security bugs. See [SECURITY.md](./SECURITY.md) for the full policy and scope. Image CVE scans run against each tagged release; current status is visible on the [Docker Hub repository page](https://hub.docker.com/r/openserbia/watchtower).

## Contributing

```bash
devbox shell                 # reproducible toolchain (Go 1.26, golangci-lint v2, go-task)
devbox run -- task lint      # 0 findings required
devbox run -- task test      # Ginkgo v2 suites
devbox run -- task build     # ./build/watchtower
```

See [CONTRIBUTING.md](./CONTRIBUTING.md) for PR expectations and [CLAUDE.md](./CLAUDE.md) for an architectural tour tuned for AI coding assistants (and handy for humans too).

## License

Apache 2.0. Originally © containrrr authors; fork maintained under the same license. See [LICENSE.md](./LICENSE.md).
