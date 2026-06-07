# Governance

This document describes how the `openserbia/watchtower` project is run: who is
responsible for what, how decisions get made, and how those roles change over
time. It is deliberately lightweight — this is a focused maintenance fork of the
archived [`containrrr/watchtower`](https://github.com/containrrr/watchtower), not
a large foundation project — but the roles and processes below are real and are
the ones we actually follow.

## Roles and responsibilities

### Maintainers

Maintainers have commit/merge access to the repository and are collectively
responsible for the health of the project. Responsibilities:

- Reviewing and merging pull requests.
- Triaging issues and responding to bug reports.
- Cutting and signing releases (see [`goreleaser.yml`](./goreleaser.yml) and the
  release workflows under [`.github/workflows/`](./.github/workflows/)).
- Handling security reports and coordinating disclosure per
  [`SECURITY.md`](./SECURITY.md).
- Keeping CI green (lint, test, build, CodeQL, Scorecard) and dependencies
  current (Dependabot, Trivy, Snyk).
- Setting direction and curating the roadmap (see
  [Why this fork](https://openserbia.github.io/watchtower/why-fork/)).

**Current maintainers:**

| Maintainer | GitHub | Areas |
|---|---|---|
| Lead maintainer | [@OCharnyshevich](https://github.com/OCharnyshevich) | All of the above |

> **Bus factor, stated honestly.** The project currently has a single
> maintainer, so its bus factor is 1. This is a known limitation and we are
> actively open to adding co-maintainers (see *Becoming a maintainer* below).
> Until then, continuity is provided by everything being public and
> reproducible: the full history is on GitHub, the toolchain is pinned via
> Devbox, releases are reproducible, and this document plus
> [`CLAUDE.md`](./CLAUDE.md) capture the operational knowledge needed to take
> over.

### Contributors

Anyone who opens an issue or pull request is a contributor. Contributors are
expected to follow the [code of conduct](./code_of_conduct.md) and the
[contribution guidelines](./CONTRIBUTING.md). No formal agreement (CLA/DCO) is
required at this time; if that changes it will be documented in
`CONTRIBUTING.md`.

### Users

Users interact with the project through released images/binaries, the
documentation site, and the issue tracker. User-reported bugs and feature
requests are a primary input to the roadmap.

## Decision-making

Day-to-day decisions are made by **lazy consensus**:

1. Proposals are made as issues or pull requests.
2. A change may be merged once it passes CI and a maintainer approves it. For
   substantial or potentially contentious changes, maintainers leave the
   proposal open for comment for a reasonable period before merging.
3. If maintainers disagree and cannot reach consensus, the lead maintainer makes
   the final call.

Security-sensitive decisions follow the process in [`SECURITY.md`](./SECURITY.md)
and are handled privately until a fix ships.

Because the project keeps deliberate compatibility with upstream
`containrrr/watchtower` (CLI flags, `com.centurylinklabs.watchtower.*` labels,
HTTP API, notification backends), any change that would break that compatibility
is treated as a major decision and called out explicitly in the
[`CHANGELOG.md`](./CHANGELOG.md).

## Becoming a maintainer

Contributors who have a sustained track record of high-quality contributions —
well-scoped PRs, good judgement in review/discussion, and care for the project's
compatibility and security posture — may be invited to become maintainers by an
existing maintainer. There is no fixed quota; we would welcome the help.

A new maintainer is added by an existing maintainer granting repository access
and adding them to the table above (via a pull request).

## Stepping down and removing maintainers

A maintainer may step down at any time by opening a pull request that removes
them from the maintainers table. An inactive or unreachable maintainer may be
moved to emeritus by the remaining maintainer(s). The intent is always to keep
at least one active maintainer able to perform every responsibility listed above.

## Changing this document

Governance changes are made the same way as any other change: open a pull
request against this file. Material changes are noted in the
[`CHANGELOG.md`](./CHANGELOG.md).
