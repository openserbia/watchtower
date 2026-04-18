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

`containrrr/watchtower` stopped accepting changes in late 2024. This fork keeps it alive with a modern toolchain (Go 1.26, golangci-lint v2, Devbox-pinned CI) and fixes real-world bugs that went unmerged upstream — e.g. [upstream#966](https://github.com/containrrr/watchtower/issues/966), [#1217](https://github.com/containrrr/watchtower/issues/1217), [#1413](https://github.com/containrrr/watchtower/issues/1413).

**Drop-in compatible** — same CLI flags, labels (`com.centurylinklabs.watchtower.*`), HTTP API, and notification backends. Swap the image name and you're done. Migration diff, full comparison table, and roadmap: **[Why this fork?](https://openserbia.github.io/watchtower/why-fork/)**.

## Verifying a release

```bash
# Binary checksums
sha256sum -c watchtower_<version>_checksums.txt --ignore-missing

# Image provenance
docker inspect openserbia/watchtower:latest \
    --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}'
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

## License

Apache 2.0. Originally © containrrr authors; fork maintained under the same license. See [LICENSE.md](./LICENSE.md).
