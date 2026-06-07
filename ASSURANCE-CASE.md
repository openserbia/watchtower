# Security assurance case

This document is Watchtower's **assurance case**: a structured argument for why
the project's security requirements are met. It describes the threat model,
identifies the trust boundaries, argues that secure design principles have been
applied, and argues that common implementation weaknesses have been countered.

It complements [`SECURITY.md`](./SECURITY.md) (reporting process and scope) and
[`docs/secure-connections.md`](./docs/secure-connections.md) (operator-facing TLS
guidance). For an architectural tour, see [`CLAUDE.md`](./CLAUDE.md).

## 1. What Watchtower is, and why it is sensitive

Watchtower is a single-binary Go service that polls the Docker API, compares the
image digests of running containers against their registries, and recreates
containers whose images have changed. To do this it requires **access to the
Docker daemon socket**, which on most hosts is root-equivalent. That privilege
is the central fact of this assurance case: a compromise of Watchtower, or of an
input it trusts, can translate into control of the host's containers.

### Assets to protect

- **The host / Docker daemon**, reachable through the mounted socket.
- **Integrity of running containers** — only genuinely newer, registry-backed
  images should replace a running container.
- **Registry credentials** supplied to Watchtower.
- **The HTTP API token**, when the optional API is enabled.
- **Released artifacts** (images, binaries, checksums) and their provenance.

## 2. Trust boundaries

| # | Boundary | Trust | Primary controls |
|---|----------|-------|------------------|
| B1 | Docker daemon socket | Powerful capability; treated as the crown jewel | Operator-granted; scope/label filters (`--scope`, `--label-enable`, `--disable-containers`) limit which containers are acted upon |
| B2 | Container registries (network) | Semi-trusted external | Strict TLS by default (min TLS 1.2), certificate verification on, image **digest** comparison before any recreate, bounded exponential backoff |
| B3 | Optional HTTP API (network) | Untrusted callers | Disabled unless configured; gated by `--http-api-token`; constant-time token comparison |
| B4 | Container labels & image metadata | Attacker-influenceable input | Defensive, total parsers; fuzzed (e.g. the `com.docker.compose.depends_on` parser); unknown shapes dropped, never fatal |
| B5 | Notification backends (outbound) | Operator-chosen sinks | shoutrrr URLs validated; failures isolated from the update loop |
| B6 | Supply chain (build → publish → run) | Must be verifiable end-to-end | Signed releases, provenance, SBOM, pinned dependencies (see §5) |

## 3. Threat model

Representative threats, by boundary:

- **B2 — Man-in-the-middle / malicious registry response.** An attacker on the
  network path tries to feed a forged manifest or downgrade TLS. *Countered by*
  strict TLS (min 1.2) with certificate verification enabled by default, and by
  resolving and comparing **digests** rather than trusting mutable tags. The
  legacy upstream blanket `InsecureSkipVerify` was removed; insecurity is now an
  explicit, per-registry opt-in (`--insecure-registry`, `--registry-ca-bundle`).
- **B3 — Unauthorized API use.** An attacker tries to trigger updates or read
  state via the HTTP API. *Countered by* the API being off by default, requiring
  a bearer token, and comparing that token in constant time
  (`crypto/subtle`, `pkg/api/api.go`) to avoid timing side channels.
- **B4 — Hostile container metadata.** A crafted or corrupt label tries to crash
  the scan loop or smuggle behaviour. *Countered by* defensive parsing that is
  total by design and by native Go fuzzing of the untrusted-input parsers
  (`pkg/container/metadata_fuzz_test.go`).
- **B2/B6 — Resource exhaustion / flapping.** A flaky or hostile registry tries
  to wedge Watchtower. *Countered by* bounded exponential backoff and an
  in-memory bearer-token cache.
- **B1 — Over-broad action.** A misconfiguration causes Watchtower to touch
  containers it should not. *Countered by* opt-in scoping and label filters, and
  by `--health-check-gated` updates that wait for the replacement to report
  healthy and roll back to the previous image on failure.
- **B6 — Supply-chain tampering.** An attacker tries to ship a malicious build.
  *Countered by* the controls in §5.

Out of scope: the security of the Docker daemon itself, the host OS, and the
registries' own infrastructure; vulnerabilities in transitively vendored
dependencies that are not reachable from Watchtower's code paths (tracked via
Dependabot rather than treated as Watchtower vulnerabilities).

## 4. Argument: secure design principles are applied

- **Least privilege.** Watchtower acts only on the containers selected by the
  operator's scope/label filters; the HTTP API is off unless explicitly enabled.
- **Fail-safe defaults.** TLS verification is on by default; the API is closed by
  default; health-gated updates roll back on failure rather than leaving a broken
  replacement running.
- **Defense in depth.** Network (TLS + digest checks), authentication
  (constant-time token), input handling (defensive parsing + fuzzing), and supply
  chain (signing + provenance + scanning) are layered independently.
- **Complete mediation.** Staleness is decided by resolving the registry digest
  and comparing it before any container is recreated — tags alone are never
  trusted.
- **Economy of mechanism.** A single static binary on a distroless base image,
  no shell, minimal attack surface.
- **Secure delivery.** Releases are reproducible and cryptographically signed
  (see §5).

## 5. Argument: common implementation weaknesses are countered

| Weakness class | Mitigation in this project |
|---|---|
| Improper input handling | Defensive, total parsers for labels/metadata; native fuzzing of the compose `depends_on` parser; flag validation centralized in `internal/flags` |
| Transport security / MITM | Strict TLS by default, `MinVersion: tls.VersionTLS12` (`pkg/registry/transport/transport.go`); insecurity is explicit opt-in only |
| Authentication timing attacks | `subtle.ConstantTimeCompare` for the HTTP API token (`pkg/api/api.go`) |
| Hardcoded/leaked secrets | No secrets in the repo; runtime secrets via `*-file` flags (`GetSecretsFromFiles`); CI secrets via GitHub Actions secrets; CodeQL + Snyk scan the tree |
| Vulnerable dependencies | Dependabot (Go modules, Actions, Docker), Trivy CRITICAL image scan in CI, Snyk; dependencies vendored and pinned |
| Supply-chain tampering | Keyless cosign signatures over checksums and images, SLSA build provenance, CycloneDX SBOM; GitHub Actions pinned by commit SHA; builder base image pinned by digest |
| Denial of service / flapping | Bounded exponential backoff on registry errors; bearer-token cache |
| Unsafe memory behaviour | Go is memory-safe; tests run under the race detector |

## 6. How these claims are verified

- **Static analysis:** golangci-lint v2 (full ruleset, CI gate at 0 findings),
  CodeQL (per-PR and weekly), and Snyk Code.
- **Dynamic analysis:** the Go race detector on every test run, native Go
  fuzzing, and Trivy CVE scanning of published images.
- **Tests:** Ginkgo/Gomega suites run on every pull request and push.
- **Supply chain:** OpenSSF Scorecard runs weekly; releases are signed and carry
  provenance and an SBOM that anyone can verify (see the README's
  "Verifying a release").

## 7. Residual risks and assumptions

- Watchtower is only as trustworthy as the Docker socket it is given and the host
  it runs on; operators are responsible for restricting that access.
- Operators who enable `--insecure-registry` knowingly weaken boundary B2 for the
  named registry.
- With a single maintainer today (see [`GOVERNANCE.md`](./GOVERNANCE.md)), review
  depth depends on automated gates more than on second-person human review;
  expanding the maintainer team is the intended mitigation.

## 8. Keeping this current

This assurance case is reviewed when security-relevant architecture changes
land, and at minimum alongside each release that touches a trust boundary.
Changes are noted in the [`CHANGELOG.md`](./CHANGELOG.md).
