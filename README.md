<p>
  <img alt="LinkOS Mirrors" src="https://openlinkos.com/brand/linkos-mirrors/github/readme.svg" height="56">
</p>

# LinkOS Mirrors · Relay

[简体中文](README.zh-CN.md) | English

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
docker run -d --name relay --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  -p 8080:8080 \
  -v relay-data:/var/lib/registry-mirrors-relay \
  ghcr.io/openlinkos/registry-mirrors-relay:v0.5.0
docker logs relay 2>&1 | grep 'generated password'   # the first administrator's password, shown once
```

- Pull: `docker pull server:8080/library/alpine` (over plain HTTP, add `server:8080` to Docker's `insecure-registries`; for real use put it behind HTTPS, see [Deployment](docs/deploy.md)).
- Admin console: `http://server:8080/admin/`, user `admin` and the password from the log; you choose your own at the first sign-in. To set a password at any time: `docker exec -it relay registry-mirrors-relay admin password`. The console shares the pull port and is open to every address, protected by sign-in; put it behind HTTPS when it faces the internet, or set `admin.public: false` to keep it to the local machine and the LAN.
- Each entrance's page in the console generates Docker, containerd, k3s and Podman configuration for its host name.

Docker Compose (behind a CDN, direct with ACME, or behind Caddy), Kubernetes (kustomize or the Helm chart, with several replicas, a writable single replica, Gateway API, monitoring and network policy) and systemd are covered in **[docs/deploy.md](docs/deploy.md)**.

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

- **Pull entrances**: by host or single-domain (path prefix, `ns`, root path); HTTPS with certificate files (reloaded on change) or ACME.
- **Saves quota and resources**: manifests are cached in memory with a cap, and pulls by tag are confirmed with a HEAD, which Docker Hub does not count; layers are streamed with one 16 KiB buffer per stream.
- **Usage page and service status**: opening a host shows, in English and Chinese, the steps to configure each client with the site's hosts filled in, an address converter and the mirror addresses, plus each upstream's reachability, its availability over the last 24 hours and this site's request success rate; `site.brand` gives it the colours and marks of an OpenLinkOS or LinkOS brand. On entrances that require sign-in only pulls need it by default; the page can also require sign-in or be turned off. See [Deployment](docs/deploy.md#usage-page-and-service-status).
- **Access control**: entrances that require sign-in (bcrypt accounts), IP allow and deny lists, temporary bans, image allow and deny rules (403 or 451), rate limits per account or IP.
- **Admin console and API**: overview, service status (each upstream's availability over 24 hours), Docker Hub quota left, client configuration, accounts, rules, rate limits, requests, abuse, audit log, admins and tokens; Chinese and English, light and dark. Entrances, upstreams, rate limits, the cache and more can be changed there too: on top of the configuration file, in effect at once, and exportable back to the file. It shares the port and host name with pulls by default and can move to a port of its own. See [docs/admin.md](docs/admin.md) and [docs/api.md](docs/api.md).
- **Operations**: Prometheus metrics (off by default), Caddy-format access logs, hot reload (SIGHUP, file changes, a button in the console), graceful shutdown, GOMEMLIMIT set from the container's memory limit.

## Design

- **Not an open proxy**: upstreams come only from the configuration, redirects are followed only to allowlisted hosts, and private addresses are refused at dial time.
- **Protocol-correct**: digests match the upstream; Range is passed through so downloads resume; HEAD stays HEAD, so it does not count against Docker Hub limits; upstream errors are returned as they are.
- **No leaks**: client IPs, cookies and User-Agent are not sent upstream, and neither the server's egress IP nor the upstream's quota headers reach clients; client IPs are taken from the right of X-Forwarded-For, trusting only configured proxies.
- **An admin console protected by sign-in**: there is no default password, and the first administrator must change it at the first sign-in; bcrypt, sign-in limits, strict CSP, CSRF protection and re-authentication for sensitive actions. It is open to every address by default; with `admin.public: false` it answers only direct connections from loopback and private addresses. Metrics and pprof, which have no sign-in, always take direct connections only.
- **Light**: streaming, about 19 MiB of memory when idle; no overall write timeout, so large layers can download slowly. Benchmarks are in [docs/benchmark.md](docs/benchmark.md).

## Documentation

- [Deployment](docs/deploy.md): docker run, Compose, Kubernetes, systemd, building from source, verifying releases
- [Admin console](docs/admin.md): pages, the first administrator, opening the console, changing the configuration, security
- [Admin API](docs/api.md): endpoints and metrics
- [Benchmarks](docs/benchmark.md)
- Configuration: [`config.example.yaml`](config.example.yaml) (comments in Chinese). `registry-mirrors-relay check -config config.yaml -print` validates a configuration and prints the effective values with secrets hidden.

## Releases and verification

Each release on the [Releases](https://github.com/openlinkos/registry-mirrors-relay/releases) page has binaries for linux/amd64, arm64 and arm/v7 with SBOMs; the image is `ghcr.io/openlinkos/registry-mirrors-relay` and the Helm chart `oci://ghcr.io/openlinkos/charts/registry-mirrors-relay`. `SHA256SUMS`, the image and the chart are signed keylessly with Sigstore cosign and carry GitHub build provenance attestations; see [Deployment](docs/deploy.md#verifying-releases) for how to verify them.

## Roadmap

- [x] M0 Project skeleton: configuration, CLI, CI
- [x] M1 Proxy core: per-host, per-port and single-domain (prefix, `ns`, root path) entrances, tokens, redirects, access log, usage page
- [x] M2 Manifest cache, circuit breaker, benchmarks
- [x] M3 Access control and operational data: accounts, IP and image rules, rate limits, metrics, hot reload, admin API
- [x] M4 Admin console
- [x] M5 Packaging and release: HTTPS (certificate files, ACME), image, Compose, Kubernetes, systemd, signed v0.1.0
- [x] M6 Helm chart: two modes (read-only replicas, a writable single replica), published to an OCI registry and signed from v0.1.1
- [x] v0.2.0: pulls, the admin console, the admin API and metrics on one port (or split), management for direct connections only; service status in the console
- [x] v0.3.0: edit entrances, upstreams, limits, cache and more from the console (on top of the configuration file, exportable)
- [x] v0.4.0: the admin console is open to every address by default (protected by sign-in), on the pull host name through a CDN or reverse proxy
- [x] v0.5.0: an admin command that works while the service runs (`admin password`), a generated first password; the code is in English and messages follow the reader's language

## Development

You need Go (see `go.mod` for the version).

```bash
go test -race ./...
golangci-lint run ./...
scripts/build-image.sh registry-mirrors-relay:dev   # build an image
scripts/kind-e2e.sh                                 # end-to-end test in kind (needs docker, kind, kubectl; INSTALL=helm installs the chart)
scripts/chart-check.sh                              # chart checks (needs helm, kubeconform, yq, helm-docs)
```

`scripts/isolated-test.sh` tests pulls with many clients on a Docker network without internet access, and `scripts/smoke.sh` checks digests against the upstreams.

## License

The code is released under the [Apache License 2.0](LICENSE). The LinkOS and OpenLinkOS names and marks are not covered by that license; see [openlinkos.com/brand](https://openlinkos.com/brand).
