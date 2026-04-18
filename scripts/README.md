# scripts/

Ad-hoc developer and manual end-to-end testing scripts. **None of these are
wired into CI** — CI uses `devbox run -- task lint/test/build` driven by
`Taskfile.yml`. These scripts stay here because they exercise code paths the
Ginkgo suites don't cover well (real Docker daemon, real registry, real
network-mode containers) and because reproducing those setups by hand is
tedious.

Run them from the repo root unless noted otherwise; most rely on a Docker
daemon and will pull upstream images.

## End-to-end tests (manual)

| Script | What it exercises |
|---|---|
| [`dependency-test.sh`](./dependency-test.sh) | `com.centurylinklabs.watchtower.depends-on` and legacy `--link` shutdown ordering. Takes `depends-on`/`linked` or a custom label arg, then extra flags passed through to `watchtower`. |
| [`lifecycle-tests.sh`](./lifecycle-tests.sh) | Pre- and post-update lifecycle hook execution, including linked containers. Pass the `watchtower` binary path as arg 1 or it defaults to `../watchtower`. |
| [`contnet-tests.sh`](./contnet-tests.sh) | Container networking mode (`--net=container:<id>`) with a Gluetun VPN supplier. Requires `VPN_SERVICE_PROVIDER`, `OPENVPN_USER`, and `OPENVPN_PASSWORD` env vars to be set. |

Each script assumes a locally-built `watchtower` binary (run `devbox run --
task build` first) and cleans up its own containers on exit.

## Dev utilities

| Script | What it does |
|---|---|
| [`docker-util.sh`](./docker-util.sh) | Shared helper library — not meant to be run directly. Sourced by the scripts above and by `du-cli.sh`. Provides `start-registry`, `create-dummy-image`, `latest-image-rev`, port-inspection helpers, and a `CONTAINER_PREFIX` convention (`du-*`) so cleanup is easy. |
| [`du-cli.sh`](./du-cli.sh) | Thin CLI over `docker-util.sh` for interactive work: `du-cli.sh registry start`, `du-cli.sh image rev myrepo/myimage`, etc. Useful when you want to spin up a local registry + a couple of dummy images to point watchtower at. |
