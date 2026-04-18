# CLAUDE.md

Orientation for Claude Code (and other coding agents) working in this repository.

## Orientation in one paragraph

Watchtower is a single-binary Go service (`main.go` → `cmd.Execute()`) that polls the Docker API, compares running container image digests against the registry, and recreates stale containers with the same config. This repo is the **actively maintained fork** of the abandoned `containrrr/watchtower` — the module path is `github.com/openserbia/watchtower`, the published images are `openserbia/watchtower` and `ghcr.io/openserbia/watchtower`. **Never reintroduce `containrrr/watchtower` as an import path.** CLI flags, labels (`com.centurylinklabs.watchtower.*`), HTTP API endpoints, and notification backends are deliberately kept compatible with upstream so users can swap the image and move on.

## Toolchain

Go 1.26, golangci-lint v2, gofumpt, gci, and go-task are pinned via **Devbox** (`devbox.json`). Always run commands inside Devbox so tool versions match CI:

```bash
devbox shell                    # enter the environment
# or
devbox run -- <command>         # one-shot
```

All Go code uses vendored deps — pass `-mod vendor` (the `task` targets already do). Running `go` directly outside Devbox picks up whatever Go is on your PATH and is **not** how CI builds.

## Common commands

`Taskfile.yml` at the repo root is the single entry point:

```bash
devbox run -- task              # list targets
devbox run -- task deps         # go mod download + tidy + vendor
devbox run -- task fmt          # gci + gofumpt
devbox run -- task lint         # golangci-lint (auto-runs fmt first)
devbox run -- task test         # go test -mod vendor with coverage and -race
devbox run -- task build        # ./build/watchtower with version ldflags
devbox run -- task tplprev      # WASM template-preview bundle
devbox run -- task docker:build # dev-self-contained Docker image

# Focus a single Ginkgo spec (suites use Describe/It)
devbox run -- go test -mod vendor ./internal/actions -v -ginkgo.focus="the update action"

# Full local stack (watchtower + prometheus + grafana + demo containers)
docker compose up --build
```

## Architecture map

Entry points:

- `main.go` → `cmd.Execute()` (Cobra).
- `cmd/root.go` wires runtime state: flag parsing in `PreRun`, constructs the Docker `container.Client` and the `Notifier`, then `Run` either executes once (`--run-once`) or schedules updates via `robfig/cron`. A buffered channel (`updateLock`, capacity 1) serializes runs between the scheduler and the HTTP API — only one update is ever in flight.
- `cmd/notify-upgrade.go` is a secondary subcommand added in `Execute()`.

Package boundaries worth knowing before editing:

| Package | Role |
|---|---|
| `internal/flags` | **Single source of truth** for every CLI flag and env var. `SetDefaults`, `RegisterDockerFlags`, `RegisterSystemFlags`, `RegisterNotificationFlags`, `ProcessFlagAliases`, `EnvConfig`, `GetSecretsFromFiles` are all called from `cmd/root.go`. Add new flags here, not inline in `cmd/`. |
| `internal/actions` | High-level orchestration. `actions.Update` is the main loop: list → stale check → stop/start with dependency ordering via `pkg/sorter`, with optional pre/post-check and pre/post-update hooks from `pkg/lifecycle`. `CheckForSanity` and `CheckForMultipleWatchtowerInstances` run before scheduling. |
| `pkg/container` | Docker client abstraction. `Client` is an interface (`client.go`); `container.go` wraps `types.ContainerJSON` and encapsulates metadata/label parsing (see `metadata.go`). Mocks live under `pkg/container/mocks` and `internal/actions/mocks`. |
| `pkg/registry` (+ `registry/digest`, `registry/auth`, `registry/manifest`) | Authenticates to registries and resolves image digests to decide staleness. Called from `Client.IsContainerStale`. |
| `pkg/filters` | Composes filter predicates from positional args, `--disable-containers`, `--label-enable`, `--scope`, and per-run image lists (`FilterByImage`) used by the HTTP update endpoint. |
| `pkg/session` | Per-run state (`Progress`, `Report`, `ContainerStatus`) handed to the notifier and metrics. |
| `pkg/metrics` + `pkg/api/metrics` | Prometheus `/v1/metrics` endpoint. |
| `pkg/api/update` | Manual-trigger `/v1/update` endpoint. Both APIs are mounted on the shared `pkg/api` server, gated by `--http-api-token`. |
| `pkg/notifications` | shoutrrr-backed notifier plus legacy email/slack/msteams/gotify shims; batches startup/update messages and exposes `AddLogHook` so `logrus` errors become notifications. Templates in `pkg/notifications/templates`; the WASM preview tool in `tplprev/` reuses them. |
| `pkg/lifecycle` | User-defined lifecycle commands inside containers via labels (`com.centurylinklabs.watchtower.lifecycle.pre-update`, etc.). |
| `internal/meta` | `Version` is injected at build time via `-ldflags` in the `task build` target. |

Tests use **Ginkgo v1 + Gomega** (the library pin in `go.mod`; the `ginkgo` CLI installed by Devbox is v2, which is fine — only the library version matters for the specs). Each package has a `*_suite_test.go` bootstrap; spec files use `Describe`/`Context`/`It`. Prefer editing existing suites over introducing plain `testing.T` tests.

## Linter configuration

`.golangci.yml` enables golangci-lint v2 with the full opinionated default set plus formatters (gofumpt, gci). Exclusions that are intentional, not oversights:

- **`**/mocks/`** — hand-written Docker API fixtures; treated as generated.
- **`pkg/notifications/preview/` and `tplprev/`** — WASM/preview shims.
- **`_test.go`** — skips `mnd`, `prealloc`, `goconst`, `dupl`; revive's `dot-imports` and `unused-parameter` are allowed because Ginkgo/Gomega tests rely on both.
- **`pkg/session/container_status.go`** — `errname` disabled because `ContainerStatus` is a session status record exposed via the `ContainerReport.Error()` interface, not an error type.

**Fix findings rather than suppress them.** If a finding doesn't fit the codebase, prefer editing `.golangci.yml` with a reasoned exclusion over `//nolint` comments.

## CI expectations

`.github/workflows/pull-request.yml` runs three jobs via Devbox:

1. **Lint** — `devbox run -- task lint` (must pass with 0 findings).
2. **Test** — `devbox run -- task test` on `ubuntu-latest`, uploads coverage to Codecov.
3. **Build** — `devbox run -- task build`.

The release pipeline (`release.yml`, `goreleaser.yml`) still uses the legacy goreleaser v0.155 config for multi-arch Docker images (`linux/amd64`, `linux/arm64/v8`, `linux/arm/v6`, `linux/386`); it's untouched by the Devbox migration and only runs on release tags. Images are pushed to both Docker Hub (`openserbia/watchtower`) and GHCR (`ghcr.io/openserbia/watchtower`).

## MCP Tools: code-review-graph

**This project ships a knowledge graph. Prefer the `code-review-graph` MCP tools over Grep/Glob/Read when exploring the codebase** — the graph is faster, cheaper in tokens, and gives you structural context (callers, dependents, test coverage) that file scanning cannot.

### When to reach for the graph first

- **Exploring code** → `semantic_search_nodes` or `query_graph` instead of Grep.
- **Understanding impact** → `get_impact_radius` instead of manually tracing imports.
- **Code review** → `detect_changes` + `get_review_context` instead of reading entire files.
- **Finding relationships** → `query_graph` with `callers_of` / `callees_of` / `imports_of` / `tests_for`.
- **Architecture questions** → `get_architecture_overview` + `list_communities`.

Fall back to Grep/Glob/Read **only** when the graph doesn't cover what you need.

### Key tools

| Tool | Use when |
|------|----------|
| `detect_changes` | Reviewing code changes — gives risk-scored analysis |
| `get_review_context` | Need source snippets for review — token-efficient |
| `get_impact_radius` | Understanding blast radius of a change |
| `get_affected_flows` | Finding which execution paths are impacted |
| `query_graph` | Tracing callers, callees, imports, tests, dependencies |
| `semantic_search_nodes` | Finding functions/classes by name or keyword |
| `get_architecture_overview` | Understanding high-level codebase structure |
| `refactor_tool` | Planning renames, finding dead code |

### Workflow

1. The graph auto-updates on file changes (via hooks).
2. Use `detect_changes` for code review.
3. Use `get_affected_flows` to understand impact.
4. Use `query_graph pattern="tests_for"` to check coverage before editing.
