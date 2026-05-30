By default, watchtower will watch all containers. However, sometimes only some containers should be updated.

There are two options:

-   **Fully exclude**: You can choose to exclude containers entirely from being watched by watchtower.
-   **Monitor only**: In this mode, watchtower checks for container updates, sends notifications and invokes the [pre-check/post-check hooks](https://openserbia.github.io/watchtower/lifecycle-hooks/) on the containers but does **not** perform the update.

## Full Exclude 

If you need to exclude some containers, set the _com.centurylinklabs.watchtower.enable_ label to `false`.  For clarity this should be set **on the container(s)** you wish to be ignored, this is not set on watchtower.

=== "dockerfile"

    ```docker
    LABEL com.centurylinklabs.watchtower.enable="false"
    ```
=== "docker run"

    ```bash
    docker run -d --label=com.centurylinklabs.watchtower.enable=false someimage
    ```

=== "docker-compose"

    ``` yaml
    version: "3"
    services:
      someimage:
        container_name: someimage
        labels:
          - "com.centurylinklabs.watchtower.enable=false"
    ```

If instead you want to [only include containers with the enable label](https://openserbia.github.io/watchtower/arguments/#filter_by_enable_label), pass the `--label-enable` flag or the `WATCHTOWER_LABEL_ENABLE` environment variable on startup for watchtower and set the _com.centurylinklabs.watchtower.enable_ label with a value of `true` on the containers you want to watch.

=== "dockerfile"

    ```docker
    LABEL com.centurylinklabs.watchtower.enable="true"
    ```
=== "docker run"

    ```bash
    docker run -d --label=com.centurylinklabs.watchtower.enable=true someimage
    ```

=== "docker-compose"

    ``` yaml
    version: "3"
    services:
      someimage:
        container_name: someimage
        labels:
          - "com.centurylinklabs.watchtower.enable=true"
    ```

If you wish to create a monitoring scope, you will need to [run multiple instances and set a scope for each of them](https://openserbia.github.io/watchtower/running-multiple-instances).

Watchtower filters running containers by testing them against each configured criteria. A container is monitored if all criteria are met. For example:

-   If a container's name is on the monitoring name list (not empty `--name` argument) but it is not enabled (_centurylinklabs.watchtower.enable=false_), it won't be monitored;
-   If a container's name is not on the monitoring name list (not empty `--name` argument), even if it is enabled (_centurylinklabs.watchtower.enable=true_ and `--label-enable` flag is set), it won't be monitored;

## Monitor Only

Individual containers can be marked to only be monitored (without being updated).

To do so, set the *com.centurylinklabs.watchtower.monitor-only* label to `true` on that container.

```docker
LABEL com.centurylinklabs.watchtower.monitor-only="true"
```

Or, it can be specified as part of the `docker run` command line:

```bash
docker run -d --label=com.centurylinklabs.watchtower.monitor-only=true someimage
```

When the label is specified on a container, watchtower treats that container exactly as if [`WATCHTOWER_MONITOR_ONLY`](https://openserbia.github.io/watchtower/arguments/#without_updating_containers) was set, but the effect is limited to the individual container. 

## Update strategy

The global [`--update-strategy`](https://openserbia.github.io/watchtower/arguments/#update_strategy) flag
(`recreate` (default) / `rolling-restart` / `blue-green`) can be overridden per container with the
`com.centurylinklabs.watchtower.update-strategy` label. This lets a stateless web service run
`blue-green` while a database in the same fleet stays on the safe `recreate` path, all under one
Watchtower instance.

```docker
LABEL com.centurylinklabs.watchtower.update-strategy="blue-green"
```

The per-container drain window for blue-green is set with the
`com.centurylinklabs.watchtower.blue-green.drain` label (a Go duration such as `10s`, `30s`, `2m`;
overrides the global [`--blue-green-drain`](https://openserbia.github.io/watchtower/arguments/#blue_green_drain_window);
`0` disables the drain window).

### Blue-green (zero-downtime) deploys {#update-strategy-blue-green}

With `update-strategy=blue-green`, Watchtower brings the new ("green") container up *alongside* the
running ("blue") one, waits for green to report `healthy`, lets the drain window elapse so the reverse
proxy registers green and in-flight requests on blue finish, then stops blue and renames green to
blue's name. It is the only true zero-downtime strategy, but it has hard requirements.

!!! warning "Blue-green prerequisites — opt in per container"
    Blue-green only works for a container that:

    - Sits behind a **dynamic, label-based reverse proxy** (e.g. Traefik) on the Docker network. It
      **must not publish host ports** — two copies cannot bind the same host port, so a container with
      published ports falls back to `recreate` with a warning. Route through the proxy on the Docker
      network instead.
    - Uses **explicit** `traefik.http.routers.<name>` / `traefik.http.services.<name>` labels rather
      than Traefik's name-derived `defaultRule`. The proxy must treat blue and green as one service
      with two backends; explicit labels are what survive the temporary-name → canonical-name rename
      transparently. With a name-derived rule the temporary green name would create a different router
      and the rename would flip routing.
    - Defines a **`HEALTHCHECK`** so green's readiness can actually be verified. Without one, Watchtower
      can only rely on the drain window and logs a warning.
    - Is **stateless / idempotent**. During the drain window both copies receive traffic, so blue-green
      is **unsafe for databases, queues, and other stateful services** — keep those on `recreate`.

A failed green health check removes green, leaves blue serving, and records a rollback (the same
`watchtower_rollbacks_total` metric and post-rollback cooldown as `--health-check-gated`).

#### Worked example: Compose + Traefik

```yaml
services:
  traefik:
    image: traefik:v3
    command:
      - --providers.docker=true
      - --providers.docker.exposedbydefault=false
      - --entrypoints.web.address=:80
    ports:
      - "80:80"                       # only the proxy publishes host ports
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro

  web:
    image: myorg/web:latest
    # No `ports:` here — Traefik routes to it over the Docker network.
    healthcheck:                      # required for a real readiness gate
      test: ["CMD", "wget", "-qO-", "http://localhost:8080/healthz"]
      interval: 10s
      timeout: 3s
      retries: 3
      start_period: 5s
    labels:
      com.centurylinklabs.watchtower.enable: "true"
      com.centurylinklabs.watchtower.update-strategy: "blue-green"
      com.centurylinklabs.watchtower.blue-green.drain: "15s"   # optional per-container override
      traefik.enable: "true"
      # Explicit router + service labels (NOT a name-derived defaultRule) so the
      # temporary green container and the renamed survivor stay on the same route:
      traefik.http.routers.web.rule: "Host(`web.example.com`)"
      traefik.http.routers.web.service: "web"
      traefik.http.services.web.loadbalancer.server.port: "8080"

  watchtower:
    image: openserbia/watchtower:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    command:
      - --interval=60
      - --label-enable
      - --cleanup
      # Optional: set the fleet-wide default; the per-container label above still wins.
      - --update-strategy=recreate
      - --blue-green-drain=10s
```

When `myorg/web:latest` changes, Watchtower starts a second `web` container (temporary unique name,
same labels) on the new image. Traefik sees two backends for service `web` and load-balances across
both. Once green is healthy and the 15s drain elapses, Watchtower stops the old container and renames
green to `web` — no failed requests through Traefik during the cutover.
