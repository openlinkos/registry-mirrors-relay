<p>
  <img alt="LinkOS Mirrors" src="https://openlinkos.com/brand/linkos-mirrors/github/readme.svg" height="56">
</p>

# LinkOS Mirrors · Relay

[简体中文](README.md) | English

[LinkOS Mirrors](https://github.com/openlinkos/registry-mirrors) is a free container registry mirror run by [OpenLinkOS](https://openlinkos.com) as a public service. This repository is its own zero-disk edition: a single Go program that only relays requests and **never writes image data to disk**. It has a built-in admin console and runs with docker run, Docker Compose, Kubernetes or systemd.

## Which edition

| | This repository (Relay) | [registry-mirrors-caddy](https://github.com/openlinkos/registry-mirrors-caddy) | [registry-mirrors](https://github.com/openlinkos/registry-mirrors) |
| --- | --- | --- | --- |
| How | Our own Go program | A Caddy configuration | Caddy + zot |
| Disk | No image data | No image data | Caches images |
| Admin console | Yes | No | No |
| Good for | Self-hosting, teams, accounts and rules | The fewest moving parts | Many users, saving upstream traffic and quota |

## Quick start

```bash
openssl rand -base64 18 > admin-password      # the first administrator's password, to be changed at the first sign-in
chmod 400 admin-password && sudo chown 65532 admin-password

docker run -d --name registry-mirrors-relay --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  -p 8080:8080 -p 127.0.0.1:9090:9090 \
  -v relay-data:/var/lib/registry-mirrors-relay \
  -v "$PWD/admin-password:/run/secrets/admin_password:ro" \
  -e RELAY_ADMIN_PASSWORD_FILE=/run/secrets/admin_password \
  ghcr.io/openlinkos/registry-mirrors-relay:v0.1.0
```

- Pull: `docker pull server:8080/library/alpine` (over plain HTTP, add `server:8080` to Docker's `insecure-registries`; for real use put it behind HTTPS, see [Deployment](docs/deploy.en.md)).
- Admin console: `http://127.0.0.1:9090/admin/`, user `admin`, password from `admin-password`. From elsewhere use `ssh -L 9090:127.0.0.1:9090 server`.
- The Entrances and usage page generates Docker, containerd, k3s and Podman configuration for your host name.

Docker Compose (behind a CDN, direct with ACME, or behind Caddy), Kubernetes (kustomize, with several replicas, a writable single replica, Gateway API, monitoring and network policy) and systemd are covered in **[docs/deploy.en.md](docs/deploy.en.md)**.

## Supported setups

One domain covers every standard setup below; one subdomain per upstream works too:

| Client setup | Example |
| --- | --- |
| Docker `registry-mirrors` (Docker Hub only) | `"registry-mirrors": ["https://mirror.example.com"]` |
| containerd `hosts.toml` (Kubernetes, k3s, nerdctl) | `[host."https://mirror.example.com"]`, upstream chosen by `?ns=` |
| Path prefix (Podman and CRI-O `registries.conf`, or rewritten image names) | `docker pull mirror.example.com/ghcr.io/owner/image` |
| One subdomain per upstream | `docker pull ghcr.example.com/owner/image` |

Docker Hub, gcr.io, ghcr.io, registry.k8s.io and quay.io are built in; more upstreams can be added in the configuration.

## Features

- **Pull entrances**: by host, by port or single-domain (path prefix, `ns`, root path); HTTPS with certificate files (reloaded on change) or ACME.
- **Saves quota and resources**: manifests are cached in memory with a cap, and pulls by tag are confirmed with a HEAD, which Docker Hub does not count; layers are streamed with one 16 KiB buffer per stream.
- **Access control**: entrances that require sign-in (bcrypt accounts), IP allow and deny lists, temporary bans, image allow and deny rules (403 or 451), rate limits per account or IP.
- **Admin console and API**: overview, upstream status and Docker Hub quota left, client configuration, accounts, rules, rate limits, requests, abuse, audit log, admins and tokens; Chinese and English, light and dark. See [docs/admin.en.md](docs/admin.en.md) and [docs/api.en.md](docs/api.en.md).
- **Operations**: Prometheus metrics, Caddy-format access logs, hot reload (SIGHUP, file changes, a button in the console), graceful shutdown, GOMEMLIMIT set from the container's memory limit.

## Design

- **Not an open proxy**: upstreams come only from the configuration, redirects are followed only to allowlisted hosts, and private addresses are refused at dial time.
- **Protocol-correct**: digests match the upstream; Range is passed through so downloads resume; HEAD stays HEAD, so it does not count against Docker Hub limits; upstream errors are returned as they are.
- **No leaks**: client IPs, cookies and User-Agent are not sent upstream, and neither the server's egress IP nor the upstream's quota headers reach clients; client IPs are taken from the right of X-Forwarded-For, trusting only configured proxies.
- **A safe admin console by default**: it listens on the local machine only; there is no default password, and the first administrator must change the password at the first sign-in; strict CSP, CSRF protection, sign-in limits, and re-authentication for sensitive actions.
- **Light**: streaming, about 19 MiB of memory when idle; no overall write timeout, so large layers can download slowly. Benchmarks are in [docs/benchmark.md](docs/benchmark.md) (Chinese).

## Documentation

- [Deployment](docs/deploy.en.md): docker run, Compose, Kubernetes, systemd, building from source, verifying releases
- [Admin console](docs/admin.en.md): pages, the first administrator, remote access, security
- [Admin API](docs/api.en.md): endpoints and metrics
- [Benchmarks](docs/benchmark.md) (Chinese)
- Configuration: [`config.example.yaml`](config.example.yaml) (comments in Chinese). `registry-mirrors-relay check -config config.yaml -print` validates a configuration and prints the effective values with secrets hidden.

## Releases and verification

Each release on the [Releases](https://github.com/openlinkos/registry-mirrors-relay/releases) page has binaries for linux/amd64, arm64 and arm/v7 with SBOMs; the image is `ghcr.io/openlinkos/registry-mirrors-relay`. `SHA256SUMS` and the image are signed keylessly with Sigstore cosign and carry GitHub build provenance attestations; see [Deployment](docs/deploy.en.md#verifying-releases) for how to verify them.

## Roadmap

- [x] M0 Project skeleton: configuration, CLI, CI
- [x] M1 Proxy core: per-host, per-port and single-domain (prefix, `ns`, root path) entrances, tokens, redirects, access log, usage page
- [x] M2 Manifest cache, circuit breaker, benchmarks
- [x] M3 Access control and operational data: accounts, IP and image rules, rate limits, metrics, hot reload, admin API
- [x] M4 Admin console
- [x] M5 Packaging and release: HTTPS (certificate files, ACME), image, Compose, Kubernetes, systemd, signed v0.1.0
- [ ] M6 Helm chart

## Development

You need Go (see `go.mod` for the version).

```bash
go test -race ./...
golangci-lint run ./...
scripts/build-image.sh registry-mirrors-relay:dev   # build an image
scripts/kind-e2e.sh                                 # end-to-end test in kind (needs docker, kind, kubectl)
```

`scripts/isolated-test.sh` tests pulls with many clients on a Docker network without internet access, and `scripts/smoke.sh` checks digests against the upstreams.

## License

The code is released under the [Apache License 2.0](LICENSE). The LinkOS and OpenLinkOS names and marks are not covered by that license; see [openlinkos.com/brand](https://openlinkos.com/brand).
