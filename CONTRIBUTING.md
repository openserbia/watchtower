# Contributing to Watchtower

Thanks for wanting to help keep this fork alive. The bar is pragmatism, not perfection — small, focused PRs are far more welcome than grand rewrites.

## Prerequisites

- **[Devbox](https://www.jetify.com/devbox)** — the single tool you need. It pins Go, golangci-lint, gofumpt, gci, go-task, ginkgo, docker, and goreleaser at the exact versions CI uses. No global Go install required; no `GO111MODULE` juggling.
- **Docker** — for integration-style runs via `docker compose up --build`.

```bash
curl -fsSL https://get.jetify.com/devbox | bash
git clone git@github.com:<yourfork>/watchtower.git
cd watchtower
devbox shell     # enters the pinned toolchain
```

## Day-to-day

Everything runs through `go-task` inside Devbox — CI uses the same targets, so green locally means green in CI (barring runner flakes):

```bash
devbox run -- task              # list targets
devbox run -- task deps         # go mod download + tidy + vendor
devbox run -- task fmt          # gci + gofumpt
devbox run -- task lint         # golangci-lint (auto-runs fmt first) — must be 0 findings
devbox run -- task test         # full Ginkgo v2 suite with coverage
devbox run -- task build        # ./build/watchtower
```

Focus a single Ginkgo spec:

```bash
devbox run -- go test -mod vendor ./internal/actions -v -ginkgo.focus="the update action"
```

Spin up the full stack locally (Watchtower + Prometheus + Grafana + a few demo containers):

```bash
docker compose up --build
```

## Pull request checklist

- [ ] `devbox run -- task lint` passes with **0** findings. Fix findings rather than suppress them; if an exclusion is justified, edit `.golangci.yml` with a reason, not `//nolint`.
- [ ] `devbox run -- task test` passes. Tests use Ginkgo v2 + Gomega — prefer extending the existing `Describe`/`It` suites over adding plain `testing.T` tests.
- [ ] `devbox run -- task build` succeeds.
- [ ] You're editing flags in `internal/flags/` (the single source of truth) — not inline in `cmd/`.
- [ ] Commit message is a short imperative ("Fix X when Y" / "Add Z support for W"). No noise like "update code". The full PR description is where the *why* belongs.
- [ ] You haven't reintroduced `github.com/containrrr/watchtower` as a Go import path — the module path is `github.com/openserbia/watchtower`.

## Architecture orientation

For a deeper tour of the codebase — package boundaries, the Cobra/cron scheduling wiring, how `actions.Update` orchestrates a run, how the Docker client is mocked — see [`CLAUDE.md`](./CLAUDE.md). It's written for AI coding assistants but reads well for humans navigating the project for the first time.

## Security issues

Do **not** file public issues for security bugs — use [GitHub Security Advisories](https://github.com/openserbia/watchtower/security/advisories/new) instead. See [SECURITY.md](./SECURITY.md) for the full policy.

## Code of conduct

By participating, you agree to follow the [code of conduct](./code_of_conduct.md).
