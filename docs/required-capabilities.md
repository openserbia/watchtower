# Required Docker capabilities

Watchtower talks to the Docker daemon over the socket (or a `DOCKER_HOST` endpoint). When you put a
proxy such as [`tecnativa/docker-socket-proxy`](https://github.com/Tecnativa/docker-socket-proxy) in
front of the socket to narrow the attack surface, you have to allow the exact set of API endpoints
Watchtower uses — and a too-tight allow-list otherwise only reveals itself mid-update, *after* the old
container has already been stopped and removed and the recreate hits a blocked endpoint.

The table below is the full catalog of Docker API operations Watchtower can issue, the
socket-proxy environment variable that gates each one, **when** it is required (some are conditional on a
flag or a label), and **why**. It is the same catalog the [`--preflight`](arguments.md#preflight_docker_capability_check)
check probes at startup — enable `--preflight` to have Watchtower verify this list against your daemon (or
proxy) before it schedules any polls, and abort with an error naming the blocked endpoint *and* its proxy
variable.

!!! tip "Let preflight check it for you"
    Run with `--preflight` (env `WATCHTOWER_PREFLIGHT=true`) to probe every required endpoint at startup.
    Each probe is side-effect-free (a request against a bogus target — nothing is created, started, or
    removed), and a blocked or unreachable required endpoint aborts before the first update instead of
    failing halfway through one.

## How the proxy variables map

`tecnativa/docker-socket-proxy` gates the daemon API with two kinds of switch:

- **Per-resource variables** — `CONTAINERS`, `IMAGES`, `NETWORKS`, `EVENTS`, `EXEC`, `PING` — allow the
  endpoints for that resource group.
- **Per-method variables** — most importantly `POST` — allow the *mutating* HTTP methods. The proxy
  defaults to read-only (`GET`/`HEAD`), so any create / start / stop / remove / tag / connect call needs
  **both** the resource variable *and* `POST=1`. That is why the write rows below read e.g. `CONTAINERS`
  **+** `POST`.

`DELETE`-method endpoints (`DELETE /containers/{id}`, `DELETE /images/{name}`) are also gated by `POST` in
the proxy's default configuration — the proxy treats `POST` as the umbrella "allow mutating methods" switch.

## Capability catalog

### Always-on reads

Every poll reaches the daemon, lists containers, and inspects each container and its image before deciding
anything. These are always required.

| Capability | Docker endpoint | Socket-proxy variable | When required | Why |
| --- | --- | --- | --- | --- |
| Ping | `GET /_ping` | `PING` | Always | Confirms the daemon socket is reachable and negotiates the API version on startup. |
| Container list | `GET /containers/json` | `CONTAINERS` | Always | Lists running containers each poll to decide which ones Watchtower manages. |
| Container inspect | `GET /containers/{id}/json` | `CONTAINERS` | Always | Inspects each container to read its image, config, labels, and network attachments. |
| Image inspect | `GET /images/{name}/json` | `IMAGES` | Always | Resolves the local image ID behind a container so staleness can be compared against the registry digest. |

### Staleness detection

| Capability | Docker endpoint | Socket-proxy variable | When required | Why |
| --- | --- | --- | --- | --- |
| Image pull | `POST /images/create` | `IMAGES` + `POST` | Unless `--no-pull` | Pulls the candidate image during staleness detection. Not needed with `--no-pull` (or `WATCHTOWER_NO_PULL`). |

### The recreate write set

These are the mutating operations a recreate performs: stop (kill + remove), recreate (create + start),
re-tag to the resolved digest, re-attach networks, and rename for the self-update dance. They are skipped
**wholesale** under `--monitor-only` (Watchtower then only detects staleness and notifies).

| Capability | Docker endpoint | Socket-proxy variable | When required | Why |
| --- | --- | --- | --- | --- |
| Container kill | `POST /containers/{id}/kill` | `CONTAINERS` + `POST` | Unless `--monitor-only` | Sends the stop signal to a stale container before it is removed and recreated. |
| Container remove | `DELETE /containers/{id}` | `CONTAINERS` + `POST` | Unless `--monitor-only` | Removes the old container after it has stopped so the replacement can take its name. |
| Container create | `POST /containers/create` | `CONTAINERS` + `POST` | Unless `--monitor-only` | Recreates the container from the new image, carrying its previous config forward. |
| Container start | `POST /containers/{id}/start` | `CONTAINERS` + `POST` | Unless `--monitor-only` | Starts the freshly recreated container. |
| Image tag | `POST /images/{name}/tag` | `IMAGES` + `POST` | Unless `--monitor-only` | Re-binds the original tag to the resolved digest just before recreate, so a CI retag between scan and create cannot strand the container on a missing image. |
| Network connect | `POST /networks/{id}/connect` | `NETWORKS` + `POST` | Unless `--monitor-only` | Re-attaches the recreated container to each of its original networks with the original aliases. |
| Network disconnect | `POST /networks/{id}/disconnect` | `NETWORKS` + `POST` | Unless `--monitor-only` | Detaches the single network `ContainerCreate` auto-attached so the full original network set can be restored cleanly. |
| Container rename | `POST /containers/{id}/rename` | `CONTAINERS` + `POST` | Unless `--monitor-only` | Renames Watchtower's own container during a self-update so the replacement can claim the canonical name. |

### Conditional writes

| Capability | Docker endpoint | Socket-proxy variable | When required | Why |
| --- | --- | --- | --- | --- |
| Container exec | `POST /containers/{id}/exec` | `EXEC` + `POST` | With `--enable-lifecycle-hooks` **and** a watched container that declares a `com.centurylinklabs.watchtower.lifecycle.*` label | Runs user-defined [lifecycle hook](lifecycle-hooks.md) commands inside containers. |
| Image remove | `DELETE /images/{name}` | `IMAGES` + `POST` | With `--cleanup` | Deletes the superseded image after a successful update. Not needed without `--cleanup` (or `WATCHTOWER_CLEANUP`). |
| Container wait | `POST /containers/{id}/wait` | `CONTAINERS` + `POST` | With `--rerun-init-deps` | Blocks until a re-run Compose init container exits. Not needed without `--rerun-init-deps` (or `WATCHTOWER_RERUN_INIT_DEPS`). |

### Optional accelerator

| Capability | Docker endpoint | Socket-proxy variable | When required | Why |
| --- | --- | --- | --- | --- |
| Events | `GET /events` | `EVENTS` | Optional, with `--watch-docker-events` | Subscribes to the engine event stream to trigger targeted scans on local image rebuilds. Optional accelerator for `--watch-docker-events`; Watchtower degrades to scheduled polling without it, and `--preflight` only *warns* when it is missing. |

## Socket-proxy environment block

A ready-to-paste `tecnativa/docker-socket-proxy` service that grants exactly what Watchtower needs. `POST=1`
enables the mutating methods (create / start / kill / remove / tag / connect / rename); the per-resource
variables scope which endpoints are exposed. Leave `EXEC=0` unless you use [lifecycle hooks](lifecycle-hooks.md).

```yaml
services:
  docker-proxy:
    image: tecnativa/docker-socket-proxy
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    environment:
      # Read endpoints (always required)
      - PING=1          # GET /_ping
      - CONTAINERS=1    # GET /containers/json, GET /containers/{id}/json
      - IMAGES=1        # GET /images/{name}/json, POST /images/create (+POST)
      - NETWORKS=1      # POST /networks/{id}/connect|disconnect (+POST)
      - EVENTS=1        # GET /events (optional; only needed for --watch-docker-events)
      # Mutating methods (required for the recreate write set; drop under --monitor-only)
      - POST=1          # create / start / kill / remove / tag / connect / disconnect / rename
      # Lifecycle hooks (only if a watched container uses com.centurylinklabs.watchtower.lifecycle.*)
      - EXEC=0          # set to 1 with --enable-lifecycle-hooks
    restart: unless-stopped

  watchtower:
    image: openserbia/watchtower
    environment:
      - DOCKER_HOST=tcp://docker-proxy:2375
      - WATCHTOWER_PREFLIGHT=true   # verify the allow-list at startup
    depends_on:
      - docker-proxy
    restart: unless-stopped
```

!!! note "Trim for `--monitor-only`"
    Under `--monitor-only` Watchtower never mutates anything, so you can drop `POST`, `NETWORKS`, and `EXEC`
    entirely and keep only the read variables (`PING`, `CONTAINERS`, `IMAGES`, and optionally `EVENTS`).

Watchtower does not mount `/var/run/docker.sock` itself in this setup — it reaches the daemon through the
proxy via `DOCKER_HOST`, the same mechanism described under [Remote hosts](remote-hosts.md). Combine with the
TLS options in [Secure connections](secure-connections.md) when the proxy is reached over the network rather
than a shared Docker network.
