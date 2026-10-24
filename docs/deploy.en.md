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
| 8080 | Pull entrance (plain HTTP); usage page; `/healthz`, `/readyz` | Yes, or behind a CDN or reverse proxy |
| 8443 (if configured) | Pull entrance (HTTPS, `tls.mode: files` or `acme`) | Yes |
| 9090 | Admin console and admin API | **No**: publish on the local machine only, and use SSH forwarding or `kubectl port-forward` |
| 9180 | Prometheus metrics (optionally pprof) | Monitoring only |

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
  ghcr.io/openlinkos/registry-mirrors-relay:v0.1.3
```

- The image carries a configuration: pull entrance 8080, console 9090, metrics 9180, one single-domain entrance for any host name. To change it, mount your own at `/etc/registry-mirrors-relay/config.yaml` (see [`config.example.yaml`](../config.example.yaml)); inside a container, `listen.admin` and `listen.metrics` must be `:9090` and `:9180`.
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
| `behind-caddy/` | Caddy handles HTTPS in front; the relay stays on the Compose network |

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
| `base/` | Deployment (2 replicas), Service (80 → 8080, metrics 9180), a separate Service for the console (9090, never expose it), PDB. Operational data is read-only (`admin.store.driver: none`): accounts and rules live in `config.yaml` |
| `components/admin-password/` | Creates the first administrator from the Secret `registry-mirrors-relay-admin` |
| `overlays/single-writable/` | One replica as a StatefulSet with a PVC; operational data is writable, so the console can create accounts, add rules and ban |
| `overlays/gateway-api/` | An HTTPRoute attached to your existing Gateway, which handles HTTPS |
| `overlays/monitoring/` | A Prometheus Operator ServiceMonitor and a Grafana dashboard |
| `overlays/networkpolicy/` | Ingress to 8080 and 9180 (from monitoring) only; egress to DNS and public 443 only |

Pods run as non-root with a read-only root filesystem, no capabilities and the RuntimeDefault seccomp profile. On shutdown they wait 5 seconds (`preStop: sleep`, Kubernetes 1.30+) for the Service to drop them, then up to 30 seconds for running downloads. The memory limit is 512 MiB, from which the relay sets `GOMEMLIMIT`.

Reference these directories from a kustomization in your own repository:

```yaml
# kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: registry-mirror
resources:
  - https://github.com/openlinkos/registry-mirrors-relay//deploy/kubernetes/overlays/single-writable?ref=v0.1.3
components:
  - https://github.com/openlinkos/registry-mirrors-relay//deploy/kubernetes/components/admin-password?ref=v0.1.3
```

```bash
kubectl create namespace registry-mirror
openssl rand -base64 18 > admin-password
kubectl -n registry-mirror create secret generic registry-mirrors-relay-admin --from-file=password=admin-password
rm admin-password   # a change is required at the first sign-in; in read-only mode (base) it stays valid, so keep it safe
kubectl apply -k .
kubectl -n registry-mirror port-forward svc/registry-mirrors-relay-admin 9090   # open http://127.0.0.1:9090/admin/
```

With several replicas (base) the operational data is read-only, so the initial administrator from the configuration is the only one and its password cannot be changed in the console: use a long random password and reach the console only through `port-forward`. Sessions live in the pod's process; `port-forward` stays on one pod, so several replicas don't get in the way.

### Pulling through the relay on nodes

containerd on the nodes (Kubernetes, k3s and RKE2 alike) picks registry hosts from `/etc/containerd/certs.d/<upstream>/hosts.toml`. The relay needs an address the nodes can reach directly, such as a LoadBalancer, a NodePort or a host name through the Gateway. On each node:

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
helm install registry-mirrors-relay oci://ghcr.io/openlinkos/charts/registry-mirrors-relay --version 0.1.3 \
  -n registry-mirror -f values.yaml
helm test registry-mirrors-relay -n registry-mirror
kubectl -n registry-mirror port-forward svc/registry-mirrors-relay-admin 9090   # then open http://127.0.0.1:9090/admin/
```

- **Settings the chart owns**: `data_dir`, `listen` and `admin.store` follow `mode`; setting them in `config` fails the render. `tls` and per-port entrances (`entrances[].listen`) aren't supported either; leave HTTPS to the Ingress or Gateway.
- **Secrets**: no value takes a password. The administrator password comes only from `admin.existingSecret`. Write upstream tokens and the accounts file as `env:` or `file:` in `config` and bring them in from Secrets with `extraEnv` and `extraVolumes`; a `value:` fails the render.
- **Configuration changes**: by default (`configRollout: always`) any change rolls the pods. With `restartOnly`, only changes that need a restart (`listen`, `data_dir`, `admin.store`, `log.access`, `limits.tracked_clients`) do; the relay hot-reloads the rest within a minute.
- **Optional parts** (all off by default): `ingress`, `httpRoute` (Gateway API), `metrics.serviceMonitor`, `metrics.dashboard` (a Grafana dashboard) and `networkPolicy`. Metrics have their own ClusterIP Service, so making the pull Service a NodePort or LoadBalancer doesn't expose them.
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

cosign verify ghcr.io/openlinkos/registry-mirrors-relay:v0.1.3 \
  --certificate-identity-regexp '^https://github.com/openlinkos/registry-mirrors-relay/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/openlinkos/registry-mirrors-relay:v0.1.3 -R openlinkos/registry-mirrors-relay

cosign verify ghcr.io/openlinkos/charts/registry-mirrors-relay:0.1.3 \
  --certificate-identity-regexp '^https://github.com/openlinkos/registry-mirrors-relay/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/openlinkos/charts/registry-mirrors-relay:0.1.3 -R openlinkos/registry-mirrors-relay
```

## Usage page and service status

Opening any entrance's host in a browser shows the usage page: the mirror address for each upstream, an address converter, configuration for Docker, containerd, k3s and Podman, and the **service status**:

- Whether each upstream is reachable right now and how fast it answers; upstreams behind an open circuit breaker are marked. The relay requests every upstream's `/v2/` once a minute (`proxy.health_interval`), which does not count against pull quotas.
- One bar per hour for the last 24 hours: each upstream's availability, and this site's request success rate (the share of responses that are not 5xx; hours with fewer than 20 requests are not counted).
- Version and uptime. The figures live in memory and start again after a restart.

The page reads `/status.json` and refreshes it every minute. Anyone can fetch it, so it holds only what is safe to publish: failures are reduced to a category (timeout, DNS, connection, TLS, HTTP status), never the raw error, addresses, upstream quotas or request counts. Set `site.show_status: false` to turn it off.

## Entrances that require sign-in and Docker's registry-mirrors

On such an entrance the usage page is open to everyone by default and only pulls need a sign-in; the page tells visitors to `docker login` first. To show the page only to signed-in users, set `page: login` on the entrance; to have no page at all, set `page: off` (pulls only, the host answers 404).

With `auth: basic` on an entrance, Docker's `registry-mirrors` in `daemon.json` cannot be used: Docker does not send the credentials stored by `docker login` to registry mirrors. For such entrances use the path prefix (`docker login mirror.example.com`, then `docker pull mirror.example.com/docker.io/library/nginx`), or a header in containerd's `hosts.toml`:

```toml
[host."https://mirror.example.com"]
  capabilities = ["pull", "resolve"]
  [host."https://mirror.example.com".header]
    Authorization = ["Basic <base64(user:password)>"]
```
