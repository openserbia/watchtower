# Security Policy

## Supported versions

Security fixes land on the latest `v1.x` line only. Images tagged `openserbia/watchtower:latest` (and the corresponding GHCR tag) track the most recent release — containers running `:latest` with Watchtower itself enabled will pick up security fixes on the next poll cycle.

| Version | Supported |
|---------|-----------|
| `v1.x` (latest tag) | yes |
| older `v1.x` tags | no (pin at your own risk) |
| upstream `containrrr/watchtower` | not our tree — see [their discussion](https://github.com/containrrr/watchtower/discussions/2135) |

## Reporting a vulnerability

**Use [GitHub Security Advisories](https://github.com/openserbia/watchtower/security/advisories/new).** This opens a private thread with the maintainers and keeps the report out of the public issue tracker until a fix is ready. GitHub will notify us on submit; we'll acknowledge within a few days and keep you updated as triage progresses.

Please don't:

- Open a public GitHub issue for a security bug.
- Email the old upstream contacts from the `containrrr/watchtower` repo — they no longer apply to this fork.

If the issue is genuinely non-critical (e.g. hardening suggestions, low-impact info leaks in logs), a regular GitHub issue is fine.

## Scope

In scope for security reports:

- The `watchtower` binary, its CLI surface, and its interaction with the Docker socket.
- The HTTP API endpoints gated by `--http-api-token`.
- Released container images (`openserbia/watchtower`, `ghcr.io/openserbia/watchtower`).
- Release artifacts (`checksums.txt` + `.tar.gz` archives).

Out of scope:

- CVEs in transitively vendored dependencies that are *not* reachable from Watchtower's code paths — these are tracked via Dependabot but don't warrant a private advisory.
- The upstream `containrrr/watchtower` tree.

## Disclosure

After a fix ships in a new release, we'll publish the advisory with credit (or anonymously, if you prefer).
