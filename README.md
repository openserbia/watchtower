<div align="center">

  <img src="https://raw.githubusercontent.com/openserbia/watchtower/main/logo.png" width="450" />

  # Watchtower — the maintained fork

  **Automatic base-image updates for Docker containers.**
  A drop-in replacement for [`containrrr/watchtower`](https://github.com/containrrr/watchtower), which is [no longer maintained upstream](https://github.com/containrrr/watchtower/discussions/2135).

  [![CI](https://github.com/openserbia/watchtower/actions/workflows/pull-request.yml/badge.svg)](https://github.com/openserbia/watchtower/actions/workflows/pull-request.yml)
  [![Release](https://github.com/openserbia/watchtower/actions/workflows/release.yml/badge.svg)](https://github.com/openserbia/watchtower/actions/workflows/release.yml)
  [![codecov](https://codecov.io/gh/openserbia/watchtower/branch/main/graph/badge.svg)](https://codecov.io/gh/openserbia/watchtower)
  [![Go Reference](https://pkg.go.dev/badge/github.com/openserbia/watchtower.svg)](https://pkg.go.dev/github.com/openserbia/watchtower)
  [![Go Report Card](https://goreportcard.com/badge/github.com/openserbia/watchtower)](https://goreportcard.com/report/github.com/openserbia/watchtower)
  [![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/openserbia/watchtower/badge)](https://scorecard.dev/viewer/?uri=github.com/openserbia/watchtower)
  [![Known Vulnerabilities](https://snyk.io/test/github/openserbia/watchtower/badge.svg)](https://snyk.io/test/github/openserbia/watchtower)
  <!-- Register the project at https://www.bestpractices.dev/ to get a numeric ID, then replace <ID> below and uncomment. -->
  <!-- [![OpenSSF Best Practices](https://www.bestpractices.dev/projects/<ID>/badge)](https://www.bestpractices.dev/projects/<ID>) -->
  [![Latest release](https://img.shields.io/github/v/release/openserbia/watchtower?sort=semver)](https://github.com/openserbia/watchtower/releases)
  [![Docker Hub](https://img.shields.io/docker/v/openserbia/watchtower?label=Docker%20Hub&logo=docker&sort=semver)](https://hub.docker.com/r/openserbia/watchtower)
  [![Docker pulls](https://img.shields.io/docker/pulls/openserbia/watchtower.svg?logo=docker)](https://hub.docker.com/r/openserbia/watchtower)
  [![GHCR](https://img.shields.io/badge/ghcr.io-openserbia%2Fwatchtower-24292f?logo=github)](https://github.com/openserbia/watchtower/pkgs/container/watchtower)
  [![Image size](https://img.shields.io/docker/image-size/openserbia/watchtower/latest?logo=docker)](https://hub.docker.com/r/openserbia/watchtower/tags)
  [![License](https://img.shields.io/github/license/openserbia/watchtower)](./LICENSE.md)

  **[Documentation](https://openserbia.github.io/watchtower/)** · **[Why this fork?](https://openserbia.github.io/watchtower/why-fork/)** · **[Changelog](./CHANGELOG.md)**

</div>

## TL;DR

- **What it is:** a small Go daemon that polls the Docker socket, checks registries for new image digests, and recreates stale containers with the same config (volumes, networks, env, command).
- **Who it's for:** homelabs, self-hosted stacks, media centers, dev environments — anywhere a running Kubernetes cluster would be overkill.
- **Who it's *not* for:** production workloads that need staged rollouts, canaries, or rollback. Use Kubernetes (or [k3s](https://k3s.io/) / [MicroK8s](https://microk8s.io/)) for that.
- **Images:** `docker.io/openserbia/watchtower` and `ghcr.io/openserbia/watchtower` (multi-arch: amd64, arm64, arm/v6, arm/v7, 386, riscv64).
- **Module path:** `github.com/openserbia/watchtower`.

## Quick start

**Requirements:** Docker Engine 20.10 or newer. Watchtower auto-negotiates the API version, so older daemons may work but aren't tested.

```bash
docker run --detach \
    --name watchtower \
    --volume /var/run/docker.sock:/var/run/docker.sock \
    openserbia/watchtower
```

That's it — Watchtower polls every 24h by default and updates any container it can see. To scope it, opt containers in with a label:

```bash
docker run --label com.centurylinklabs.watchtower.enable=true my-app:latest
docker run -v /var/run/docker.sock:/var/run/docker.sock \
    openserbia/watchtower --label-enable
```

### docker-compose

```yaml
services:
  watchtower:
    image: openserbia/watchtower:latest
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    command:
      - --interval=60
      - --cleanup
      - --label-enable
```

Full flag reference, notification setup, lifecycle hooks, HTTP API, and metrics all live on the [docs site](https://openserbia.github.io/watchtower/).

## Why this fork

`containrrr/watchtower` stopped accepting changes in late 2024. This fork keeps it alive with a modern toolchain (Go 1.26, golangci-lint v2, Devbox-pinned CI) and extends the same feature set across four axes:

- **Fixes real-world upstream bugs** left unmerged when the project was archived — [#966](https://github.com/containrrr/watchtower/issues/966) (`--cleanup` deletes the replacement image), [#1217](https://github.com/containrrr/watchtower/issues/1217) (nil-pointer panic on GC'd source image), [#1413](https://github.com/containrrr/watchtower/issues/1413) (`No such image` loop that wedges the container).
- **Safer updates** — opt-in `--health-check-gated` waits for the replacement container to report healthy and automatically rolls back to the previous image on failure, with per-container label overrides and a post-rollback cooldown that prevents thrash.
- **Flexible update strategies** — a new `--update-strategy` flag (`recreate` (default) / `rolling-restart` / `blue-green`), selectable per container via the `com.centurylinklabs.watchtower.update-strategy` label. **Blue-green** is true zero-downtime: it starts the new container alongside the old, waits for it to report healthy, drains, then retires the old — for stateless services behind a dynamic label-based reverse proxy (Traefik) with no published host ports.
- **Hardened network layer** — bounded exponential backoff on registry flakes, an in-memory bearer-token cache, strict TLS by default (removing upstream's blanket `InsecureSkipVerify`), constant-time bearer-token comparison, and opt-in `--insecure-registry` / `--registry-ca-bundle` for self-signed registries.
- **Operational visibility** — ship-ready Grafana dashboard + Prometheus alerts, a `GET /v1/audit` JSON endpoint for post-deploy verification, `--http-api-metrics-no-auth` for trusted-network scraping, and ~20 new metrics covering every HTTP-facing surface (request counts by status/endpoint/host/outcome, retry counters, Docker API errors, bearer-cache hit rate, poll-duration histogram, and more).

**Drop-in compatible** — same CLI flags, labels (`com.centurylinklabs.watchtower.*`), HTTP API, and notification backends. Swap the image name and you're done. Migration diff, full comparison table, and roadmap: **[Why this fork?](https://openserbia.github.io/watchtower/why-fork/)**.

## Verifying a release

```bash
# Binary checksums
sha256sum -c watchtower_<version>_checksums.txt --ignore-missing

# Verify the checksums file's keyless cosign signature (Sigstore)
cosign verify-blob \
    --certificate watchtower_<version>_checksums.txt.pem \
    --signature watchtower_<version>_checksums.txt.sig \
    --certificate-identity-regexp '^https://github.com/openserbia/watchtower/' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    watchtower_<version>_checksums.txt

# Verify a published image's keyless cosign signature
cosign verify ghcr.io/openserbia/watchtower:latest \
    --certificate-identity-regexp '^https://github.com/openserbia/watchtower/' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com

# Inspect the SLSA build provenance attached to the image
docker buildx imagetools inspect ghcr.io/openserbia/watchtower:latest \
    --format '{{ json .Provenance }}'

# Inspect the CycloneDX SBOM attached to the image
docker buildx imagetools inspect ghcr.io/openserbia/watchtower:latest \
    --format '{{ json .SBOM }}'
```

## Security

Report vulnerabilities via **[GitHub Security Advisories](https://github.com/openserbia/watchtower/security/advisories/new)** — this opens a private thread with the maintainers. Please do not file public issues for security bugs. See [SECURITY.md](./SECURITY.md) for scope and policy.

## Contributing

```bash
devbox shell                 # reproducible toolchain (Go 1.26, golangci-lint v2, go-task)
devbox run -- task lint      # 0 findings required
devbox run -- task test      # Ginkgo suites with -race
devbox run -- task build     # ./build/watchtower
```

See [CONTRIBUTING.md](./CONTRIBUTING.md) for PR expectations and [CLAUDE.md](./CLAUDE.md) for an architectural tour tuned for AI coding assistants (and handy for humans too).

## Security scanning

Trivy + Snyk are wired through `Taskfile.security.yml` (see `task -l security` for the full list). Quick start:
- `task security:snyk:deps` — Snyk Open Source vulnerability scan
- `task security:snyk:code` — Snyk Code SAST
- `task security:all` — every trivy + snyk scan

Shared config across CLI and the JetBrains Snyk plugin:
- [`.snyk`](.snyk) — path excludes and per-issue ignores. Both the CLI and the IDE plugin read this automatically.
- [`.idea/snyk.xml`](.idea/snyk.xml) — project-level plugin settings; sets `--severity-threshold=high` to match `SNYK_SEVERITY` in the Taskfile.

One-time IDE setup (per-user state): in **Settings → Tools → Snyk**, enable **Open Source** and **Code**. Sign in via `SNYK_TOKEN` env or the plugin's "Authenticate" button — the CLI and plugin share the same token store.

## License

Apache 2.0. Originally © containrrr authors; fork maintained under the same license. See [LICENSE.md](./LICENSE.md).
