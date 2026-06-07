# Releasing

Releases are cut by a maintainer pushing a signed, annotated git tag. The
[Release (Production)](./.github/workflows/release.yml) workflow triggers on tags
matching `v[0-9]+.[0-9]+.[0-9]+`, then lints, tests, and runs GoReleaser to build
and publish the multi-arch images and binary archives.

## Signed release tags

Release tags **must be cryptographically signed** so their authenticity can be
verified independently of the GitHub account that pushed them. The repository is
configured with `tag.gpgsign = true`, so an annotated tag is signed
automatically:

```bash
git tag -a v1.18.0 -m "v1.18.0"   # signed automatically via tag.gpgsign
# (equivalent explicit form: git tag -s v1.18.0 -m "v1.18.0")
git push origin v1.18.0
```

Verify a tag's signature with:

```bash
git tag -v v1.18.0
```

This is independent of, and complementary to, the cosign signatures GoReleaser
applies to the release **artifacts** (checksums, images) — see
[`goreleaser.yml`](./goreleaser.yml) and the README's *Verifying a release*.

### One-time signing setup

Tag signing reuses the same key as commit signing. If you don't yet have one
configured:

```bash
git config user.signingkey <your-key-id>   # GPG key id, or SSH key with gpg.format=ssh
git config tag.gpgsign true                 # already set in this repo's local config
```

GitHub will show the tag as **Verified** once your public key is added to your
GitHub account.

## Release steps

1. Ensure `main` is green (lint, test, build, CodeQL).
2. Update [`CHANGELOG.md`](./CHANGELOG.md): move the `[Unreleased]` entries under
   a new `[vX.Y.Z]` heading with the date.
3. Sweep `docs/` (arguments, metrics, http-api-mode, why-fork) for anything that
   drifted since the previous tag.
4. Create and push the **signed** annotated tag (above).
5. Watch the release workflow publish images to Docker Hub + GHCR and attach the
   signed artifacts, SLSA provenance, and SBOM.
