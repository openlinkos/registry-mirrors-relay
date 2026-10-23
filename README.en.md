<p>
  <img alt="LinkOS Mirrors" src="https://openlinkos.com/brand/linkos-mirrors/github/readme.svg" height="56">
</p>

# LinkOS Mirrors · Relay

[简体中文](README.md) | English

[LinkOS Mirrors](https://github.com/openlinkos/registry-mirrors) is a free container registry mirror run by [OpenLinkOS](https://openlinkos.com) as a public service. This repository is its own zero-disk edition: a single Go program that only relays requests and **never writes image data to disk**. It has a built-in admin console and runs with docker run, Docker Compose or Kubernetes.

> **Work in progress**: there is no usable release yet; see the roadmap below. For a zero-disk mirror today, use [registry-mirrors-caddy](https://github.com/openlinkos/registry-mirrors-caddy).

## Which edition

| | This repository (Relay) | [registry-mirrors-caddy](https://github.com/openlinkos/registry-mirrors-caddy) | [registry-mirrors](https://github.com/openlinkos/registry-mirrors) |
| --- | --- | --- | --- |
| How | Our own Go program | A Caddy configuration | Caddy + zot |
| Disk | No image data | No image data | Caches images |
| Admin console | Yes | No | No |
| Good for | Self-hosting, teams, accounts and rules | The fewest moving parts | Many users, saving upstream traffic and quota |

## Supported setups

One domain covers every standard setup below; one subdomain per upstream works too:

| Client setup | Example |
| --- | --- |
| Docker `registry-mirrors` (Docker Hub only) | `"registry-mirrors": ["https://mirror.example.com"]` |
| containerd `hosts.toml` (Kubernetes, k3s, nerdctl) | `[host."https://mirror.example.com"]`, upstream chosen by `?ns=` |
| Path prefix (Podman and CRI-O `registries.conf`, or rewritten image names) | `docker pull mirror.example.com/ghcr.io/owner/image` |
| One subdomain per upstream | `docker pull ghcr.example.com/owner/image` |

Docker Hub, gcr.io, ghcr.io, registry.k8s.io and quay.io are built in; more upstreams can be added in the configuration.

## Design

- **Not an open proxy**: upstreams come only from the configuration, redirects are followed only to allowlisted hosts, and private addresses are refused at dial time.
- **Protocol-correct**: digests match the upstream; Range is passed through so downloads resume; HEAD stays HEAD, so it does not count against Docker Hub limits; upstream errors are returned as they are.
- **No leaks**: client IPs, cookies and User-Agent are not sent upstream, and the server's egress IP is not sent to clients; client IPs are taken from the right of X-Forwarded-For, trusting only configured proxies.
- **Light**: streaming with a small buffer per stream; only manifests are cached in memory, with a size cap; no overall write timeout, so large layers can download slowly.

## Roadmap

- [x] M0 Project skeleton: configuration, CLI, CI
- [x] M1 Proxy core: per-host, per-port and single-domain (prefix, `ns`, root path) entrances, tokens, redirects, access log, usage page
- [x] M2 Manifest cache, circuit breaker, benchmarks (results in [docs/benchmark.md](docs/benchmark.md), Chinese)
- [x] M3 Access control and operational data: accounts, IP and image rules, rate limits, metrics, hot reload, [admin API](docs/api.md) (Chinese)
- [x] M4 [Admin console](docs/admin.md) (Chinese): overview, upstreams, client configuration snippets, accounts, rules, rate limits, requests, abuse, audit log, settings, admins and tokens; Chinese and English, light and dark
- [ ] M5 Packaging and release: image, Compose, Kubernetes, signed v0.1.0
- [ ] M6 Helm chart

## Configuration

See [`config.example.yaml`](config.example.yaml). `registry-mirrors-relay check -config config.yaml -print` validates a configuration and prints the effective values with secrets hidden.

## Development

```bash
go test -race ./...
golangci-lint run ./...
```

## License

The code is released under the [Apache License 2.0](LICENSE). The LinkOS and OpenLinkOS names and marks are not covered by that license; see [openlinkos.com/brand](https://openlinkos.com/brand).
