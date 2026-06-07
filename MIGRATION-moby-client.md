# Migration: `github.com/docker/docker` → `github.com/moby/moby/{client,api}`

> **Status: MERGED to `main` on 2026-06-06** (fast-forward to `0a55d9e`), against
> `moby/moby/client v0.4.1` + `moby/moby/api v1.54.2`. The tree builds, lints clean
> (0 issues), and passes the full `-race` suite. Live `--preflight` was validated on
> a real v29 daemon on 2026-06-07 (all 16 capabilities available, no panic — see
> below). Shipped **ahead of the original v1.0 gate** at the maintainer's direction.
> One caveat remains: the client is still **pre-stable (v0.4.1)**, so re-verify if
> it bumps to v1.0 before tagging a release.

## What this migration actually is

A **v28 → v29 Docker SDK upgrade**. One module became two, independently versioned:

- `github.com/docker/docker/client` → `github.com/moby/moby/client`
- `github.com/docker/docker/api/types/*` → `github.com/moby/moby/api/types/*`
- `github.com/docker/docker/pkg/jsonmessage` → not used any more (see ImagePull below)
- `github.com/docker/docker/api/types/versions` → `github.com/moby/moby/client/pkg/versions`
- Do **not** import `github.com/moby/moby/v2` (engine root). Do **not** let the root
  `github.com/moby/moby` (v28) module into the build list — it shadows the new
  `api`/`client` submodules and makes `api/types/*` imports *ambiguous*. Dropping the
  old `docker/docker` require is what removes it.

`github.com/docker/cli` (credential config) and `cerrdefs` are untouched — `cerrdefs`
already backs all our error classification, which insulated most of `client.go`.

## The client v0.x API is a ground-up redesign

Every method moved to a uniform `(ctx, [id,] options) (Result, error)` shape:
`ContainerCreate(ctx, ContainerCreateOptions{...})`, `ImageTag(ctx, ImageTagOptions{Source,Target})`,
`ContainerKill`/`Remove`/`Start`/`Rename` take option structs, `ContainerWait`/`Events`
return result structs wrapping their channels, `ContainerExec*` → `Exec*`. So **every
call site** in `client.go` and the `capabilities.go` probe switch was rewritten — but
the heavy data types (`container.Config`, `HostConfig`, `Summary`, `InspectResponse`,
`network.*`, `image.*`) are pure path swaps under `moby/moby/api/types/*`.

## What turned out *easier* than the plan feared

- **Version-negotiation became a deletion, not a redesign.** The new client's
  `MaxAPIVersion` is `1.54` and negotiation is on by default, so the whole
  `upgradeAPIVersionForFeatures` opportunistic-raise (and the broken
  post-construction `WithVersion(cli)` call — Phase 1's top risk) was **removed**.
  `NewClient` is now `sdkClient.New(sdkClient.FromEnv)`.
- **`ImagePull` simplified.** The `jsonmessage.DisplayJSONMessagesStream` drain
  collapsed to `response.Wait(ctx)`, which drains the stream *and* returns in-stream
  errors mapped back through their HTTP status — so `cerrdefs.IsUnauthorized/IsNotFound`
  now work directly on pull-stream errors (better than the old substring-only path).
- **Identity raw-inspect survived** — `ImageInspectWithRawResponse` still exists, so
  the containerd-snapshotter provenance feature is unchanged.

## Ripples the spike surfaced (not in the original plan)

- **`nat.Port` (string) → `network.Port` (a validated struct)** for `ExposedPorts`
  (`network.PortSet`) and `PortBindings` (`network.PortMap`). `go-connections/nat`
  is no longer used for these; build port keys with `network.MustParsePort("80/tcp")`.
- **Typed enums:** `container.Summary.State` is `ContainerState`; `Health.Status` is
  `HealthStatus` (so the `waitForHealthy` switch needed `Starting`/`NoHealthcheck`
  cases to satisfy the `exhaustive` linter).
- **`InspectResponse` flattened** — the embedded `ContainerJSONBase` is gone; `ID`,
  `Name`, `Image`, `State`, `HostConfig` are now direct fields (touched every mock
  that constructed one).
- **Removed/moved packages:** `api/types/backend` is gone (the exec test now builds
  `container.ExecInspectResponse`); `versions` moved under the client module.
- **`NewClientWithOpts`/`WithVersion`/`WithAPIVersionNegotiation` are deprecated** in
  favour of `New`/`WithAPIVersion`/(negotiation-by-default).

## Deliberate behaviour/test change

The two "malformed port string" tests (`""` and `"/tcp"`) **collapsed into one**: a
validated `network.Port` cannot represent those distinct malformed strings — both can
only surface as the zero `Port{}` (invalid). `VerifyConfiguration` now drops
`!port.IsValid() || port.Port() == ""` entries, preserving the original intent (a
misconfigured binding doesn't reach `ContainerCreate`).

## Resulting go.mod

```
github.com/moby/moby/api    v1.54.2   // engine-API-aligned tag; matches our pin
github.com/moby/moby/client v0.4.1    // PRE-STABLE — the v1.0 gate
github.com/docker/cli       v29.5.2+incompatible   // unchanged (credential config)
```
`vendor/` is gitignored; CI re-vendors via `task deps`.

## Verification

- `task build` ✓ · `task lint` ✓ (0 issues) · `task test` (`-race` + coverage) ✓ · `go mod verify` ✓.
- **Live `--preflight` on a real v29 daemon (Server 29.5.2 / API 1.54, min 1.40), 2026-06-07** ✓
  — full-flag, scope-isolated run-once. **All 16 capabilities reported "available," no
  panic, exit 0**, including the `container_create` probe that triggered the original
  startup SIGSEGV. Version negotiation reached 1.54 via the migrated `New(FromEnv)`.
  The 15-cap pass touched zero real containers (scope-isolated; `Session done
  Failed=0/Scanned=0`); `container_exec_create` was exercised in a second run against
  one throwaway in-scope, lifecycle-labeled, `monitor-only`+`no-pull` container (then
  removed), confirming `Updated=0`.

## Still outstanding (before release)

- **Re-verify when `moby/moby/client` reaches v1.0** — it was pre-stable (v0.4.1) at
  merge time; a v1.0 bump may carry further API changes to absorb.
- **Docs sweep before the next tag:** CHANGELOG.md, docs/why-fork.md (off
  `+incompatible` onto a semver'd, CVE-patched SDK), docs/required-capabilities.md.

## References

- moby/moby Discussion #52404 — `docker/docker` module deprecation
- moby/moby Discussion #51434 — v29.0.0-rc.3 Go SDK breaking changes
- docker/buildx #3792 — migrate to `moby/moby/client` (CVE-2026-34040 / 33997 note)
