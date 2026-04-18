Watchtower can connect to a Docker daemon whose API is protected by TLS —
typically a remote host exposing `dockerd` on `tcp://<host>:2376`. Three
things need to line up:

1. `DOCKER_HOST` set to the TLS endpoint (`tcp://<host>:2376`).
2. A directory on disk containing `ca.pem`, `cert.pem`, and `key.pem`
   signed by the daemon's CA.
3. `--tlsverify` passed to Watchtower (or `DOCKER_TLS_VERIFY=1` in the
   environment), pointed at that directory via `DOCKER_CERT_PATH`.

Mount the cert directory into the container at `/etc/ssl/docker` and
tell Watchtower's Docker client to use it:

```bash
docker run -d \
  --name watchtower \
  -e DOCKER_HOST=tcp://remote-host:2376 \
  -e DOCKER_CERT_PATH=/etc/ssl/docker \
  -e DOCKER_TLS_VERIFY=1 \
  -v /path/to/certs:/etc/ssl/docker:ro \
  openserbia/watchtower --tlsverify
```

Or, as a Compose service:

```yaml
services:
  watchtower:
    image: openserbia/watchtower
    command: --tlsverify
    environment:
      DOCKER_HOST: tcp://remote-host:2376
      DOCKER_CERT_PATH: /etc/ssl/docker
      DOCKER_TLS_VERIFY: "1"
    volumes:
      - /path/to/certs:/etc/ssl/docker:ro
```

## Where do the certs come from?

- **Hand-rolled:** Follow the [Docker daemon TLS
  guide](https://docs.docker.com/engine/security/protect-access/) to
  generate a CA and server/client cert pair. This is the long-term
  supported path.
- **Legacy `docker-machine`:** If you still have a `docker-machine`
  provisioned host lying around, `docker-machine env <host>` prints the
  matching `DOCKER_HOST` / `DOCKER_CERT_PATH` / `DOCKER_TLS_VERIFY`
  values and points at the cert directory it generated. `docker-machine`
  itself was archived by Docker in 2023, so don't bootstrap new hosts
  with it — but existing installations keep working.

## Registry TLS is separate

These options only govern the Docker daemon connection. Watchtower's
outbound calls to **container registries** use their own TLS knobs —
see [`--insecure-registry`](arguments.md#insecure_registries) and
[`--registry-ca-bundle`](arguments.md#registry_ca_bundle) in the
arguments reference.
