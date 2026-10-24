# registry-mirrors-relay

A container registry mirror that relays and never stores image data, with a built-in admin console. Pulls from Docker Hub, gcr.io, ghcr.io, registry.k8s.io, quay.io and any registry you add, through one host name or one per upstream, without writing image layers to disk.

**Homepage:** <https://github.com/openlinkos/registry-mirrors-relay>

## Install

```bash
kubectl create namespace registry-mirror
# The first administrator: there is no default password
openssl rand -base64 18 > admin-password
kubectl -n registry-mirror create secret generic registry-mirrors-relay-admin --from-file=password=admin-password

helm install registry-mirrors-relay oci://ghcr.io/openlinkos/charts/registry-mirrors-relay \
  --namespace registry-mirror --version 0.1.2 \
  --set admin.existingSecret=registry-mirrors-relay-admin
```

Open the admin console with `kubectl -n registry-mirror port-forward svc/registry-mirrors-relay-admin 9090` and `http://127.0.0.1:9090/admin/`.

## Modes

- `mode: readonly` (default): a Deployment with two replicas and a PodDisruptionBudget. Operational data (accounts, IP and image rules, bans) comes only from `config`; the console shows it read-only. The first administrator from `admin.existingSecret` is the only one and its password stays valid, so keep it long and random.
- `mode: writable`: a StatefulSet with one replica and a PersistentVolumeClaim for the operational database. The console can create accounts, rules and bans. The first administrator must change the password at the first sign-in; afterwards the Secret can be deleted.

## Configuration

`config` is the relay configuration (see [config.example.yaml](https://github.com/openlinkos/registry-mirrors-relay/blob/main/config.example.yaml)) and is rendered into a ConfigMap. The chart sets `data_dir`, `listen` and `admin.store` itself, and rejects `tls`, per-port entrances and secrets written as `value`: mount secrets and reference them with `env` or `file`.

```yaml
config:
  client_ip:
    trusted_proxies: [10.42.0.0/16]   # your ingress controller's pod network
  entrances:
    - name: mirror
      hosts: [mirror.example.com]
ingress:
  enabled: true
  className: traefik
  hosts:
    - host: mirror.example.com
  tls:
    - hosts: [mirror.example.com]
      secretName: mirror-example-com-tls
```

To let containerd on the nodes pull through the relay, expose it on an address the nodes can reach (`service.type: NodePort` or `LoadBalancer`, or the Ingress host) and write `/etc/containerd/certs.d/<upstream>/hosts.toml`; see the [deployment guide](https://github.com/openlinkos/registry-mirrors-relay/blob/main/docs/deploy.en.md).

## Verify

The chart is signed keylessly by this repository's release workflow and has a build provenance attestation:

```bash
cosign verify ghcr.io/openlinkos/charts/registry-mirrors-relay:0.1.2 \
  --certificate-identity-regexp '^https://github.com/openlinkos/registry-mirrors-relay/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/openlinkos/charts/registry-mirrors-relay:0.1.2 -R openlinkos/registry-mirrors-relay
```

## Requirements

Kubernetes: `>=1.30.0-0`

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| admin.existingSecret | string | `""` | Name of an existing Secret holding the first administrator's password. There is no default password. With `writable` the administrator must change it at the first sign-in, after which the Secret can be deleted. With `readonly` this is the only administrator and the password stays valid. |
| admin.existingSecretKey | string | `"password"` | Key in that Secret. |
| admin.username | string | `"admin"` | User name of the first administrator. |
| adminService.enabled | bool | `true` | A ClusterIP Service for the admin console (`kubectl port-forward`). Never expose it publicly. |
| adminService.port | int | `9090` | Service port. |
| affinity | object | `{}` | Affinity. |
| commonLabels | object | `{}` | Labels added to every resource. |
| config | object | `{}` | The relay configuration (see config.example.yaml in the repository), rendered into a ConfigMap. The chart sets `data_dir`, `listen` and `admin.store` itself; `tls` and per-port entrances (`entrances[].listen`) are not supported here. Secrets must come from `env` or `file`, never `value`. |
| configRollout | string | `"always"` | `always`: any configuration change restarts the pods. `restartOnly`: only changes that need a restart do; the rest are hot-reloaded (within about a minute). |
| containerPorts.admin | int | `9090` | Admin console and API. |
| containerPorts.http | int | `8080` | Pull entrance. |
| containerPorts.metrics | int | `9180` | Prometheus metrics. |
| dataDir.sizeLimit | string | `"64Mi"` | sizeLimit of the emptyDir used for the data directory in `readonly` mode. |
| extraEnv | list | `[]` | Extra environment variables. |
| extraEnvFrom | list | `[]` | Extra environment sources. |
| extraVolumeMounts | list | `[]` | Extra volume mounts. Mount Secrets under /run/secrets. |
| extraVolumes | list | `[]` | Extra volumes, e.g. a Secret with an htpasswd file for `config.access.users_file`. |
| fullnameOverride | string | `""` | Overrides the full resource name. |
| httpRoute.enabled | bool | `false` | Create a Gateway API HTTPRoute for the pull entrance. |
| httpRoute.hostnames | list | `[]` | Host names. |
| httpRoute.parentRefs | list | `[]` | Gateways to attach to. |
| httpRoute.timeouts | object | `{"backendRequest":"0s","request":"0s"}` | Timeouts; `0s` disables them, since layers can take a long time to download. |
| image.digest | string | `""` | Image digest (`sha256:...`). Takes precedence over `tag`. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/openlinkos/registry-mirrors-relay"` | Image repository. |
| image.tag | string | `""` | Image tag. Empty means the chart's appVersion, pinned to the digest recorded at release. |
| imagePullSecrets | list | `[]` | Secrets for pulling the image. |
| ingress.annotations | object | `{}` | Ingress annotations. Disable request and response buffering and long timeouts in your controller. |
| ingress.className | string | `""` | IngressClass name. |
| ingress.enabled | bool | `false` | Create an Ingress for the pull entrance. |
| ingress.hosts | list | `[]` | Hosts; each `{host: mirror.example.com}` routes `/` to the relay. |
| ingress.tls | list | `[]` | TLS settings. |
| livenessProbe | object | `{"failureThreshold":6,"httpGet":{"path":"/healthz","port":"http"},"periodSeconds":10}` | Liveness probe. |
| metrics.dashboard.enabled | bool | `false` | Create a ConfigMap with a Grafana dashboard. |
| metrics.dashboard.labels | object | `{"grafana_dashboard":"1"}` | Labels for the Grafana sidecar. |
| metrics.service.enabled | bool | `true` | A ClusterIP Service for the metrics port. |
| metrics.service.port | int | `9180` | Service port. |
| metrics.serviceMonitor.enabled | bool | `false` | Create a Prometheus Operator ServiceMonitor. |
| metrics.serviceMonitor.interval | string | `"30s"` | Scrape interval. |
| metrics.serviceMonitor.labels | object | `{}` | Extra labels, e.g. for the Prometheus selector. |
| metrics.serviceMonitor.relabelings | list | `[]` | Relabelings. |
| mode | string | `"readonly"` | `readonly`: a Deployment with `replicaCount` replicas; operational data is read-only and comes from `config` only. `writable`: a StatefulSet with one replica and a PersistentVolumeClaim; the admin console can create accounts, rules and bans. |
| nameOverride | string | `""` | Overrides the chart name in resource names. |
| networkPolicy.dnsTo | list | `[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"kube-system"}}}]` | Peers serving DNS. |
| networkPolicy.egressExceptIPv4 | list | `["10.0.0.0/8","172.16.0.0/12","192.168.0.0/16","100.64.0.0/10","169.254.0.0/16","127.0.0.0/8"]` | IPv4 ranges excluded from HTTPS egress. |
| networkPolicy.egressExceptIPv6 | list | `["fc00::/7","fe80::/10","::1/128"]` | IPv6 ranges excluded from HTTPS egress. |
| networkPolicy.enabled | bool | `false` | Create a NetworkPolicy: ingress to the pull entrance and metrics, egress to DNS and public HTTPS only. |
| networkPolicy.extraEgress | list | `[]` | Extra egress rules. |
| networkPolicy.extraIngress | list | `[]` | Extra ingress rules. |
| networkPolicy.metricsFrom | list | `[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}}}]` | Peers allowed to scrape metrics. |
| nodeSelector | object | `{}` | Node selector. |
| pdb.enabled | bool | `true` | Create a PodDisruptionBudget in `readonly` mode with more than one replica. |
| pdb.minAvailable | int | `1` | minAvailable. |
| persistence.accessModes | list | `["ReadWriteOnce"]` | Access modes. |
| persistence.enabled | bool | `true` | Use a PersistentVolumeClaim in `writable` mode. Without it the operational data is lost when the pod restarts. |
| persistence.retentionPolicy | object | `{}` | StatefulSet persistentVolumeClaimRetentionPolicy. |
| persistence.size | string | `"1Gi"` | Size of the operational database volume (usually under 10 MiB used). |
| persistence.storageClass | string | `""` | StorageClass; empty uses the cluster default. |
| podAnnotations | object | `{}` | Pod annotations. |
| podLabels | object | `{}` | Pod labels. |
| podSecurityContext | object | `{"fsGroup":65532,"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context. |
| preStopSleepSeconds | int | `5` | Seconds to wait before shutting down so that Services drop the pod first. |
| priorityClassName | string | `""` | Priority class. |
| readinessProbe | object | `{"failureThreshold":2,"httpGet":{"path":"/readyz","port":"http"},"periodSeconds":5}` | Readiness probe. `/readyz` turns 503 while shutting down; it never checks the upstreams. |
| replicaCount | string | `nil` | Replicas in `readonly` mode; empty means 2. `writable` mode always runs one, and asking for more fails. |
| resources | object | `{"limits":{"memory":"512Mi"},"requests":{"cpu":"100m","memory":"64Mi"}}` | Resources. GOMEMLIMIT is set to 90% of the memory limit automatically. |
| revisionHistoryLimit | int | `5` | Revision history to keep. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}` | Container security context. |
| service.annotations | object | `{}` | Service annotations. |
| service.externalTrafficPolicy | string | `""` | externalTrafficPolicy for NodePort and LoadBalancer. |
| service.loadBalancerClass | string | `""` | loadBalancerClass for LoadBalancer. |
| service.loadBalancerSourceRanges | list | `[]` | loadBalancerSourceRanges for LoadBalancer. |
| service.nodePort | string | `nil` | Node port when `type` is NodePort or LoadBalancer. |
| service.port | int | `80` | Service port. |
| service.type | string | `"ClusterIP"` | Service type for the pull entrance. Use NodePort or LoadBalancer when node containerd should pull through it. |
| serviceAccount.annotations | object | `{}` | Annotations. |
| serviceAccount.create | bool | `true` | Create a ServiceAccount (its token is never mounted). |
| serviceAccount.name | string | `""` | Name; empty uses the full name. |
| startupProbe | object | `{}` | Startup probe. |
| terminationGracePeriodSeconds | int | `60` | Grace period: running downloads get up to 30 seconds (`timeouts.shutdown_grace`). |
| tests.enabled | bool | `true` | Install the `helm test` pod. |
| tolerations | list | `[]` | Tolerations. |
| topologySpreadConstraints | list | `[{"maxSkew":1,"topologyKey":"kubernetes.io/hostname","whenUnsatisfiable":"ScheduleAnyway"}]` | Topology spread constraints; `labelSelector` is filled in when omitted. |
