<p style="text-align: center; margin-left: 1.6rem;">
  <img alt="Logotype depicting a lighthouse" src="./images/logo-450px.png" width="450" />
</p>
<h1 align="center">
  Watchtower
</h1>

<p align="center">
  A container-based solution for automating Docker container base image updates.
  <br/><br/>
  <a href="https://github.com/openserbia/watchtower/actions/workflows/pull-request.yml">
    <img alt="CI" src="https://github.com/openserbia/watchtower/actions/workflows/pull-request.yml/badge.svg" />
  </a>
  <a href="https://codecov.io/gh/openserbia/watchtower">
    <img alt="Codecov" src="https://codecov.io/gh/openserbia/watchtower/branch/main/graph/badge.svg" />
  </a>
  <a href="https://pkg.go.dev/github.com/openserbia/watchtower">
    <img alt="Go Reference" src="https://pkg.go.dev/badge/github.com/openserbia/watchtower.svg" />
  </a>
  <a href="https://goreportcard.com/report/github.com/openserbia/watchtower">
    <img alt="Go Report Card" src="https://goreportcard.com/badge/github.com/openserbia/watchtower" />
  </a>
  <a href="https://github.com/openserbia/watchtower/releases">
    <img alt="Latest release" src="https://img.shields.io/github/v/release/openserbia/watchtower?sort=semver" />
  </a>
  <a href="https://www.apache.org/licenses/LICENSE-2.0">
    <img alt="Apache-2.0 License" src="https://img.shields.io/github/license/openserbia/watchtower.svg" />
  </a>
  <a href="https://hub.docker.com/r/openserbia/watchtower">
    <img alt="Pulls from DockerHub" src="https://img.shields.io/docker/pulls/openserbia/watchtower.svg" />
  </a>
</p>

## Quick start

Watchtower watches the Docker daemon it's pointed at, polls the registries of the running containers' images, and — when a digest changes — gracefully stops each stale container and recreates it with the same config (volumes, networks, env, command).

Pull from either registry; the images are identical:

```bash
docker pull openserbia/watchtower          # Docker Hub
docker pull ghcr.io/openserbia/watchtower  # GitHub Container Registry
```

### Run it

=== "docker run"

    ```bash
    docker run --detach \
        --name watchtower \
        --restart unless-stopped \
        --volume /var/run/docker.sock:/var/run/docker.sock \
        openserbia/watchtower \
        --interval 60 --cleanup
    ```

=== "docker-compose"

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

That's enough to start: Watchtower polls every 60 s and deletes old images after a successful update.

### Scoping what it updates

By default, every container on the host is a candidate. To opt in only a chosen set, pass `--label-enable` and label the target containers:

```bash
docker run -d --label com.centurylinklabs.watchtower.enable=true my-app:latest
```

Omit the label to keep a container untouched (databases, stateful services, anything with a manual release process).

### What's next

- **[Arguments](arguments.md)** — every flag and `WATCHTOWER_*` env var
- **[Container selection](container-selection.md)** — include/exclude by label, scope, or name
- **[Notifications](notifications.md)** — Shoutrrr, email, Slack, Teams, Gotify
- **[Private registries](private-registries.md)** — credentials via `config.json`
- **[HTTP API mode](http-api-mode.md)** — trigger updates and scrape Prometheus metrics
- **[Lifecycle hooks](lifecycle-hooks.md)** — pre/post scripts inside the target container
