Watchtower exposes a small HTTP control-plane on port `8080` with three
optional endpoints. Each is opt-in via its own flag, and all share the
same listener and bearer-token scheme.

| Endpoint     | Flag                 | Env                             | Default auth                    | Purpose |
| ------------ | -------------------- | ------------------------------- | ------------------------------- | ------- |
| `/v1/update` | `--http-api-update`  | `WATCHTOWER_HTTP_API_UPDATE`    | Bearer token (always required)  | Trigger a scan and update cycle on demand. |
| `/v1/metrics`| `--http-api-metrics` | `WATCHTOWER_HTTP_API_METRICS`   | Bearer token (optional opt-out) | Prometheus exposition format for every watchtower metric. |
| `/v1/audit`  | `--http-api-audit`   | `WATCHTOWER_HTTP_API_AUDIT`     | Bearer token                    | JSON watch-status report (`managed` / `excluded` / `unmanaged` / `infrastructure`). |

Set `--http-api-token` (env `WATCHTOWER_HTTP_API_TOKEN`) and bind port
`8080` to enable any of them. The token check uses
`crypto/subtle.ConstantTimeCompare`, so timing-based probes aren't
useful.

The only exception: if `/v1/metrics` is the **only** endpoint enabled
and `--http-api-metrics-no-auth` is set, `--http-api-token` is not
required to start the daemon.

## `/v1/update`

Triggers a scan + update cycle — the same work the scheduler would do
at its next tick. The request blocks until the update completes (or is
skipped because another update is already running). Responses:

- `200` — update completed or was skipped because another was in flight.
- `401` — missing or wrong bearer token.
- The handler does not stream per-container progress; scrape
  `watchtower_scans_total` or tail the container's logs for in-flight
  detail.

```yaml
services:
  watchtower:
    image: openserbia/watchtower
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    command:
      - --http-api-update
      - --debug
    environment:
      WATCHTOWER_HTTP_API_TOKEN: mytoken
    ports:
      - "8080:8080"
```

By default, enabling this flag **disables** periodic polling (`--interval`
/ `--schedule`). Pass `--http-api-periodic-polls` to keep the scheduler
running alongside the HTTP trigger.

Trigger a full scan:

```bash
curl -H "Authorization: Bearer mytoken" http://localhost:8080/v1/update
```

Scope the update to specific images by passing an `image=` query
parameter — accepts a comma-separated list:

```bash
curl -H "Authorization: Bearer mytoken" \
  "http://localhost:8080/v1/update?image=foo/bar,foo/baz"
```

## `/v1/metrics`

Serves the Prometheus exposition format. Pair with
`--http-api-token` for bearer-token auth, or enable
`--http-api-metrics-no-auth` (env `WATCHTOWER_HTTP_API_METRICS_NO_AUTH`)
to drop the token gate — conventional for Prometheus scraping on
trusted networks where a localhost bind or firewall in front of `:8080`
provides the real access boundary. `/v1/update` remains
mandatorily token-gated regardless.

With both `--http-api-metrics --http-api-metrics-no-auth` (and no
`--http-api-update`), `--http-api-token` becomes optional and the daemon
starts without it.

```yaml
services:
  watchtower:
    image: openserbia/watchtower
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    command:
      - --http-api-metrics
      - --http-api-metrics-no-auth
      - --http-api-periodic-polls
    ports:
      - "127.0.0.1:8080:8080"
```

See [Metrics](metrics.md) for the full metric catalog, scrape config,
and the shipped Grafana dashboard.

## `/v1/audit`

Returns a JSON watch-status report for every container the Docker daemon
reports — the pull-model alternative to the log-based
[`--audit-unmanaged`](arguments.md#audit_unmanaged_containers) warning.
Useful for post-deploy verification scripts, dashboards, and ad-hoc
`curl | jq` during incident response.

Each container is classified as:

- `managed` — `com.centurylinklabs.watchtower.enable=true`.
- `excluded` — `com.centurylinklabs.watchtower.enable=false` (intentional opt-out).
- `unmanaged` — no `enable` label at all. Silently skipped under `--label-enable`.
- `infrastructure` — Docker-managed scaffolding (`moby/buildkit*`, `docker/desktop-*`, `com.docker.buildx.*` / `com.docker.desktop.*` labels). Tracked separately so transient builder containers don't inflate the unmanaged count.

```yaml
services:
  watchtower:
    image: openserbia/watchtower
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    command:
      - --http-api-audit
      - --label-enable
    environment:
      WATCHTOWER_HTTP_API_TOKEN: mytoken
    ports:
      - "8080:8080"
```

```bash
curl -s -H "Authorization: Bearer mytoken" http://localhost:8080/v1/audit | jq
```

```json
{
  "generated_at": "2026-04-18T12:00:00Z",
  "summary": {
    "managed": 5,
    "excluded": 2,
    "unmanaged": 3,
    "infrastructure": 1,
    "total": 11
  },
  "containers": [
    {"name": "/api",    "image": "myorg/api:latest",    "status": "managed"},
    {"name": "/db",     "image": "postgres:15",         "status": "excluded"},
    {"name": "/chromium","image": "browserless/chromium:latest", "status": "unmanaged"}
  ]
}
```

## Security notes

- All three endpoints share port `8080`. Bind to `127.0.0.1` or put a
  reverse proxy in front if any of them is opened on a network other
  than the one watchtower's scraper lives on.
- `/v1/update` is always token-gated regardless of other flags.
- `/v1/metrics` and `/v1/audit` disclose different data surfaces:
  metrics are cluster-level counters (low sensitivity); audit reports
  container names and image references (reveals stack topology). Keep
  the audit endpoint token-gated on networks where topology leakage
  matters.
- Non-2xx responses are counted in `watchtower_api_requests_total{status}`
  — a burst of `401` on `/v1/update` usually means credential stuffing.
  No alert for this ships by default (homelab context — `/v1/update`
  sees negligible traffic so a threshold alert would be trivially
  noise-tunable per-deployment). Operators who want one can add:
  `sum(increase(watchtower_api_requests_total{endpoint="/v1/update", status="401"}[10m])) > 5`.
