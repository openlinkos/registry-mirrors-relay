# Deployment

[简体中文](deploy.md) | English

registry-mirrors-relay is a statically linked Go program. It never writes image data to disk; its data directory holds only the operational database (accounts, rules, audit log, usually under 10 MiB) and ACME certificates. Five ways to run it:

| Way | Fits | Files |
| --- | --- | --- |
| `docker run` | A first try | The commands below |
| Docker Compose | One machine, long term | [`deploy/compose/`](../deploy/compose/) |
| Kubernetes (kustomize) | Clusters, several replicas | [`deploy/kubernetes/`](../deploy/kubernetes/) |
| Kubernetes (Helm) | Clusters managed with Helm, Flux or Argo CD | [`charts/registry-mirrors-relay/`](../charts/registry-mirrors-relay/) |
| systemd | No containers | [`deploy/systemd/`](../deploy/systemd/) |

The image is `ghcr.io/openlinkos/registry-mirrors-relay` for linux/amd64, arm64 and arm/v7, based on distroless and running as a non-root user (65532). Binaries are on the [Releases](https://github.com/openlinkos/registry-mirrors-relay/releases) page.

Ports:

| Port | Purpose | Public |
| --- | --- | --- |
| 8080 | Pull entrance (plain HTTP); usage page; `/healthz`, `/readyz`. By default also the admin console (`/admin/`), the admin API and metrics (`/metrics`, off by default) | Yes, or behind a CDN or reverse proxy. The management paths answer only direct connections from loopback and private addresses; proxied requests get 404, see [Admin console, admin API and metrics](#admin-console-admin-api-and-metrics) |
| 8443 (if configured) | Pull entrance (HTTPS, `tls.mode: files` or `acme`) | Yes |
| 9090 (`listen.admin`, optional) | When set, the admin console, admin API and metrics move here and leave the pull port | **No**: publish on the local machine only. The Docker image and the plain and acme Compose examples do this by default |

## The first administrator

There is no default password. Create the first administrator from a password file (a Docker, Compose or Kubernetes secret, or a systemd credential) that `RELAY_ADMIN_PASSWORD_FILE` points to; the user name is `admin` (or `RELAY_ADMIN_USERNAME`):

```bash
openssl rand -base64 18 > admin-password
```

This administrator must change the password at the first sign-in, after which the file is useless and can be deleted (the variable may stay; a missing file only logs a warning). Why, and the other ways, are in [admin.en.md](admin.en.md#the-first-administrator).

## docker run

```bash
openssl rand -base64 18 > admin-password
chmod 400 admin-password && sudo chown 65532 admin-password

docker run -d --name registry-mirrors-relay --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  -p 8080:8080 -p 127.0.0.1:9090:9090 \
  -v relay-data:/var/lib/registry-mirrors-relay \
  -v "$PWD/admin-password:/run/secrets/admin_password:ro" \
  -e RELAY_ADMIN_PASSWORD_FILE=/run/secrets/admin_password \
  ghcr.io/openlinkos/registry-mirrors-relay:v0.3.0
```

- The image carries a configuration: pull entrance 8080, management entrance 9090 (console, admin API, metrics), one single-domain entrance for any host name. To change it, mount your own at `/etc/registry-mirrors-relay/config.yaml` (see [`config.example.yaml`](../config.example.yaml)) and keep `listen.admin: ":9090"`: with Docker port publishing, IPv6 clients and rootless Docker arrive from the container gateway, so on a shared 8080 the relay could not tell local users from the internet.
- The password file must be readable by user 65532 inside the container, hence the `chown 65532`.
- The console is published on 127.0.0.1 only: open `http://127.0.0.1:9090/admin/`, or `ssh -L 9090:127.0.0.1:9090 server` from elsewhere.
- The image's `HEALTHCHECK` runs `registry-mirrors-relay probe`, which probes `/healthz` on the port from the configuration.

Try it:

```bash
curl -s http://127.0.0.1:8080/v2/library/alpine/manifests/latest \
  -H 'Accept: application/vnd.oci.image.index.v1+json' | head -c 200
```

## Docker Compose

[`deploy/compose/`](../deploy/compose/) has three examples, each a `compose.yaml` and a `config.yaml` (comments in Chinese):

| Directory | Setup |
| --- | --- |
| `plain/` | Behind a CDN or reverse proxy that handles HTTPS and forwards to 8080. Put their addresses in `client_ip.trusted_proxies` |
| `acme/` | Public, with certificates from ACME (Let's Encrypt) obtained by the relay itself, on the host's ports 80 and 443. The domain must already point to the machine |
| `behind-caddy/` | Caddy handles HTTPS in front. The relay's 8080 is published on the host's 127.0.0.1 only and serves the console too: from elsewhere use `ssh -L 8080:127.0.0.1:8080` and open `http://127.0.0.1:8080/admin/`; the console through Caddy is 404 |

```bash
cd deploy/compose/plain
# edit the domain, trusted_proxies and site information in config.yaml
openssl rand -base64 18 > admin-password && chmod 400 admin-password && sudo chown 65532 admin-password
docker compose up -d
docker compose logs -f relay
```

To upgrade, change the image tag in `compose.yaml` and run `docker compose up -d`. Changes to the configuration file are reloaded within 10 seconds, or at once with `docker compose kill -s HUP relay`.

## Kubernetes

[`deploy/kubernetes/`](../deploy/kubernetes/) uses kustomize:

| Directory | Content |
| --- | --- |
| `base/` | Deployment (2 replicas), Service (80 → 8080, which also serves the console and metrics), PDB. Operational data is read-only (`admin.store.driver: none`): accounts and rules live in `config.yaml` |
| `components/admin-password/` | Creates the first administrator from the Secret `registry-mirrors-relay-admin` |
| `overlays/single-writable/` | One replica as a StatefulSet with a PVC; operational data is writable, so the console can create accounts, add rules and ban |
| `overlays/gateway-api/` | An HTTPRoute attached to your existing Gateway, which handles HTTPS |
| `overlays/monitoring/` | Turns metrics on and adds a Prometheus Operator ServiceMonitor (scraping `/metrics` on 8080) and a Grafana dashboard |
| `overlays/networkpolicy/` | Ingress to 8080 only; egress to DNS and public 443 only |

Pods run as non-root with a read-only root filesystem, no capabilities and the RuntimeDefault seccomp profile. On shutdown they wait 5 seconds (`preStop: sleep`, Kubernetes 1.30+) for the Service to drop them, then up to 30 seconds for running downloads. The memory limit is 512 MiB, from which the relay sets `GOMEMLIMIT`.

Reference these directories from a kustomization in your own repository:

```yaml
# kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: registry-mirror
resources:
  - https://github.com/openlinkos/registry-mirrors-relay//deploy/kubernetes/overlays/single-writable?ref=v0.3.0
components:
  - https://github.com/openlinkos/registry-mirrors-relay//deploy/kubernetes/components/admin-password?ref=v0.3.0
```

```bash
kubectl create namespace registry-mirror
openssl rand -base64 18 > admin-password
kubectl -n registry-mirror create secret generic registry-mirrors-relay-admin --from-file=password=admin-password
rm admin-password   # a change is required at the first sign-in; in read-only mode (base) it stays valid, so keep it safe
kubectl apply -k .
kubectl -n registry-mirror port-forward svc/registry-mirrors-relay 8080:80   # open http://127.0.0.1:8080/admin/
```

With several replicas (base) the operational data is read-only, so the initial administrator from the configuration is the only one and its password cannot be changed in the console: use a long random password and reach the console only through `port-forward`. Sessions live in the pod's process; `port-forward` stays on one pod, so several replicas don't get in the way.

### Pulling through the relay on nodes

containerd on the nodes (Kubernetes, k3s and RKE2 alike) picks registry hosts from `/etc/containerd/certs.d/<upstream>/hosts.toml`. The relay needs an address the nodes can reach directly, such as a LoadBalancer, a NodePort or a host name through the Gateway. With a NodePort or LoadBalancer, set `externalTrafficPolicy: Local`: otherwise outside clients arrive with node addresses, look like direct internal connections and can reach the console's sign-in page. On each node:

```toml
# /etc/containerd/certs.d/docker.io/hosts.toml
server = "https://registry-1.docker.io"

[host."https://mirror.example.com"]
  capabilities = ["pull", "resolve"]
```

Write one for each other upstream (`ghcr.io`, `registry.k8s.io`, `quay.io`, `gcr.io`) with the upstream's own address as `server` and the relay as `host`: containerd adds `?ns=<upstream>` to its requests and the relay picks the upstream from it. If the relay fails, containerd falls back to `server`. The console's Entrances and usage page generates all of this for your host name, including k3s `registries.yaml`.

## Helm

The chart lives in [`charts/registry-mirrors-relay/`](../charts/registry-mirrors-relay/) and, from v0.1.1 on, every release publishes it to `oci://ghcr.io/openlinkos/charts/registry-mirrors-relay`. The chart version is the application version without the `v`; unless you set `image.tag`, it pulls the image of the same release by digest. It needs Helm 3.8 or later (or Helm 4) and Kubernetes 1.30 or later.

| `mode` | Workload | Operational data | kustomize equivalent |
| --- | --- | --- | --- |
| `readonly` (default) | Deployment, 2 replicas, with a PDB | Read-only; accounts and rules live in `config` | `base/` |
| `writable` | StatefulSet, 1 replica, 1 GiB PVC | Writable | `overlays/single-writable/` |

```yaml
# values.yaml
mode: writable
admin:
  existingSecret: registry-mirrors-relay-admin
# Becomes the relay's config.yaml as is; see config.example.yaml
config:
  site:
    name: Example Mirror
  client_ip:
    trusted_proxies: [private_ranges]
ingress:
  enabled: true
  className: traefik
  hosts:
    - host: mirror.example.com
  tls:
    - secretName: mirror-example-com-tls
      hosts: [mirror.example.com]
```

```bash
kubectl create namespace registry-mirror
openssl rand -base64 18 > admin-password
kubectl -n registry-mirror create secret generic registry-mirrors-relay-admin --from-file=password=admin-password
rm admin-password
helm install registry-mirrors-relay oci://ghcr.io/openlinkos/charts/registry-mirrors-relay --version 0.3.0 \
  -n registry-mirror -f values.yaml
helm test registry-mirrors-relay -n registry-mirror
kubectl -n registry-mirror port-forward svc/registry-mirrors-relay 8080:80   # then open http://127.0.0.1:8080/admin/
```

- **Settings the chart owns**: `data_dir`, `listen`, `admin.store` and `metrics.enabled`; setting `data_dir`, `listen` or `admin.store` in `config` fails the render. `tls` isn't supported either; leave HTTPS to the Ingress or Gateway.
- **Secrets**: no value takes a password. The administrator password comes only from `admin.existingSecret`. Write upstream tokens and the accounts file as `env:` or `file:` in `config` and bring them in from Secrets with `extraEnv` and `extraVolumes`; a `value:` fails the render.
- **Configuration changes**: by default (`configRollout: always`) any change rolls the pods. With `restartOnly`, only changes that need a restart (`listen`, `data_dir`, `admin.store`, `log.access`, `log.level`, the `read_header`, `idle` and `shutdown_grace` timeouts, `proxy.buffer_size`, `limits.tracked_clients`) do; the relay hot-reloads the rest within a minute.
- **Console and metrics**: they share 8080 by default, so `port-forward` to the main Service opens the console; `metrics.enabled` or `metrics.serviceMonitor.enabled` turns metrics on, and the ServiceMonitor scrapes 8080 too. `admin.separatePort: true` moves them to 9090 and a `<name>-admin` Service. With a NodePort or LoadBalancer pull Service, set `service.externalTrafficPolicy: Local` or split the port; the chart refuses to render otherwise.
- **Changing the configuration in the console**: with `mode: writable`, entrances, upstreams, rate limits and more can be changed in the console (see [Admin console](admin.en.md#changing-the-configuration-in-the-console)). Those changes live on the PVC and are invisible to `helm upgrade`, Flux and Argo CD. To let values decide everything, write `admin: {config_edit: false}` under `config`, or alert on the metric `relay_config_overrides`. In `readonly` mode the configuration cannot be changed in the console.
- **Optional parts** (all off by default): `ingress`, `httpRoute` (Gateway API), `metrics.serviceMonitor`, `metrics.dashboard` (a Grafana dashboard) and `networkPolicy`.
- Every value is listed in the chart's [README](../charts/registry-mirrors-relay/README.md). For pulling through the relay on nodes see the previous section; with `service.type: NodePort`, the `helm install` notes include a `hosts.toml` example.

## systemd

```bash
sudo install -m 0755 registry-mirrors-relay /usr/local/bin/
sudo install -d /etc/registry-mirrors-relay
sudo install -m 0644 config.yaml /etc/registry-mirrors-relay/        # start from config.example.yaml
sudo install -m 0644 deploy/systemd/registry-mirrors-relay.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now registry-mirrors-relay
```

The unit uses a dynamic user (`DynamicUser`), keeps operational data in `/var/lib/registry-mirrors-relay` (`StateDirectory`) and has a strict sandbox (`systemd-analyze security` rates it 1.5). For the first administrator, write the password into a file only root can read and uncomment the `LoadCredential=` and `Environment=RELAY_ADMIN_PASSWORD_FILE=` lines. `systemctl reload` reloads the configuration without interrupting downloads.

The console shares 8080 with pulls: open `http://127.0.0.1:8080/admin/` on the machine, use `ssh -L 8080:127.0.0.1:8080 server` from elsewhere, or open it by IP address from the LAN.

## Admin console, admin API and metrics

The admin console (`/admin/`), the admin API (`/admin/api/v1/`), metrics (`/metrics`) and pprof (`/debug/pprof/`) make up the management entrance. By default it shares the pull port; with `listen.admin` it lives on that port only. Metrics are off until `metrics.enabled: true`; `metrics.pprof` adds pprof.

**Who gets in**: only two kinds of request are answered, and to anything else these paths do not exist (404, like any unknown path):

- **Direct**: the request carries no proxy header (any `X-Forwarded-*`, `Forwarded`, `X-Real-IP`, `Via`, CDN client-address headers and the like; HTTP/1.0 counts too), and the TCP peer is in `admin.allow` (`loopback` and `private_ranges` by default). `kubectl port-forward`, SSH tunnels, the LAN and Prometheus inside the cluster all look like this.
- **Deliberately proxied**: `admin.public_url`, `client_ip.trusted_proxies` and `admin.proxied_allow` are set, the Host is `public_url`'s host, the peer is a trusted proxy, and the client address it passes on is in `proxied_allow`. Off by default.

Requests through a CDN, Ingress, Gateway, Caddy or nginx carry proxy headers and never get in. Direct access is judged by the TCP peer only, never by `X-Forwarded-For`, which anyone bypassing the CDN can forge.

Where the check holds:

| Environment | Reliable | Why |
| --- | --- | --- |
| systemd, the binary itself | Yes | The peer is the real client |
| Kubernetes: ClusterIP + Ingress/Gateway | Yes | Outside traffic goes through the Ingress controller and carries proxy headers |
| Kubernetes: NodePort or LoadBalancer with `externalTrafficPolicy: Local` | Yes | The client address is kept |
| Kubernetes: NodePort or LoadBalancer with `externalTrafficPolicy: Cluster` | **No** | The source becomes a node address and looks internal. Switch to `Local` or split the port; the chart refuses to render it |
| Docker port publishing (`-p 8080:8080`) | **No** (IPv6, rootless) | IPv6 clients and rootless Docker arrive from the container gateway. That is why the image's own configuration splits the port |
| L4 load balancers, TCP tunnels (without PROXY protocol) | **No** | The peer is the load balancer's internal address |

Where it doesn't hold, outside clients can reach the console's sign-in page and only the password and the sign-in limits stand in the way. Set `listen.admin` there to split off a port you publish locally or internally only.

**Reaching the console through an https reverse proxy** (for example from a fixed office address):

```yaml
client_ip:
  trusted_proxies: [10.0.0.5]          # the reverse proxy
admin:
  public_url: https://admin.example.com
  proxied_allow: [198.51.100.0/24]     # allowed clients, from the proxy's X-Forwarded-For
```

The proxy must pass `Host` and `X-Forwarded-For`, and must overwrite any `X-Forwarded-For` the client sent, or clients could forge their address. In nginx:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $remote_addr;   # overwrite, don't append
}
```

**Prometheus**: scrape `http://<pod or host>:8080/metrics` (9090 with a split port). Prometheus must connect directly from an address in `admin.allow`; add your pod network if it isn't in the private ranges (for example `100.64.0.0/10`).

## Building from source

You need Go (see `go.mod` for the version):

```bash
go build -trimpath -o registry-mirrors-relay ./cmd/registry-mirrors-relay
scripts/build-image.sh registry-mirrors-relay:dev          # an image from the same Dockerfile
```

## Verifying releases

Each release's `SHA256SUMS`, image and Helm chart are signed keylessly with [Sigstore](https://www.sigstore.dev/) cosign by this repository's release workflow; there are also GitHub build provenance attestations and an SBOM per archive.

```bash
cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp '^https://github.com/openlinkos/registry-mirrors-relay/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c SHA256SUMS

cosign verify ghcr.io/openlinkos/registry-mirrors-relay:v0.3.0 \
  --certificate-identity-regexp '^https://github.com/openlinkos/registry-mirrors-relay/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/openlinkos/registry-mirrors-relay:v0.3.0 -R openlinkos/registry-mirrors-relay

cosign verify ghcr.io/openlinkos/charts/registry-mirrors-relay:0.3.0 \
  --certificate-identity-regexp '^https://github.com/openlinkos/registry-mirrors-relay/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/openlinkos/charts/registry-mirrors-relay:0.3.0 -R openlinkos/registry-mirrors-relay
```

## Usage page and service status

Opening any entrance's host in a browser shows the usage page: the mirror address for each upstream, an address converter, configuration for Docker, containerd, k3s and Podman, and the **service status**:

- Whether each upstream is reachable right now and how fast it answers; upstreams behind an open circuit breaker are marked. The relay requests every upstream's `/v2/` once a minute (`proxy.health_interval`), which does not count against pull quotas.
- One bar per hour for the last 24 hours: each upstream's availability, and this site's request success rate (the share of responses that are not 5xx; hours with fewer than 20 requests are not counted).
- Version and uptime. The figures live in memory and start again after a restart.

The page reads `/status.json` and refreshes it every minute. Anyone can fetch it, so it holds only what is safe to publish: failures are reduced to a category (timeout, DNS, connection, TLS, HTTP status), never the raw error, addresses, upstream quotas or request counts. Set `site.show_status: false` to turn it off.

## Upgrading from v0.1

v0.2.0 changes a few things incompatibly. Run `registry-mirrors-relay check -config <file>` with the new image first:

- **`listen.metrics` is gone**: metrics live on the management entrance; set `metrics.enabled: true` and point Prometheus at `/metrics` on the pull port (or `listen.admin`). A non-empty `listen.metrics` is an error that says what to do; an empty one only warns.
- **Per-port entrances (`entrances[].listen`) are gone**: use `hosts` with `upstream` (one host name per upstream), or the `/v2/<upstream>/` prefix of a single-domain entrance.
- **`listen.admin` no longer defaults to `127.0.0.1:9090`**: the console shares the pull port and answers only direct connections from loopback and private addresses. Set `listen.admin: "127.0.0.1:9090"` to keep the old layout. The Docker image's own configuration still uses `:9090`.
- **Reaching the console through a reverse proxy** now needs `admin.proxied_allow` (see the previous section); otherwise it is 404.
- **Helm**: `adminService`, `metrics.service` and `containerPorts.metrics` are gone; use `admin.separatePort`, `admin.service.port` and `metrics.enabled`. A NodePort or LoadBalancer needs `externalTrafficPolicy: Local` or `admin.separatePort: true`.
- **kustomize**: the base has no `-admin` Service and no metrics port any more; open the console with `port-forward svc/registry-mirrors-relay 8080:80`, and the monitoring overlay turns metrics on.

## Entrances that require sign-in and Docker's registry-mirrors

On such an entrance the usage page is open to everyone by default and only pulls need a sign-in; the page tells visitors to `docker login` first. To show the page only to signed-in users, set `page: login` on the entrance; to have no page at all, set `page: off` (pulls only, the host answers 404).

With `auth: basic` on an entrance, Docker's `registry-mirrors` in `daemon.json` cannot be used: Docker does not send the credentials stored by `docker login` to registry mirrors. For such entrances use the path prefix (`docker login mirror.example.com`, then `docker pull mirror.example.com/docker.io/library/nginx`), or a header in containerd's `hosts.toml`:

```toml
[host."https://mirror.example.com"]
  capabilities = ["pull", "resolve"]
  [host."https://mirror.example.com".header]
    Authorization = ["Basic <base64(user:password)>"]
```
