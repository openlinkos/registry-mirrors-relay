# 部署

简体中文 | [English](deploy.en.md)

registry-mirrors-relay 是一个静态链接的 Go 程序，镜像数据不写盘，只在数据目录里存运营数据库（账号、规则、审计，通常不到 10 MiB）和 ACME 证书。五种部署方式：

| 方式 | 适合 | 文件 |
| --- | --- | --- |
| `docker run` | 先试一试 | 下面的命令 |
| Docker Compose | 一台机器长期运行 | [`deploy/compose/`](../deploy/compose/) |
| Kubernetes（kustomize） | 集群、多副本 | [`deploy/kubernetes/`](../deploy/kubernetes/) |
| Kubernetes（Helm） | 集群，用 Helm 或 Flux、Argo CD 管理 | [`charts/registry-mirrors-relay/`](../charts/registry-mirrors-relay/) |
| systemd | 不用容器 | [`deploy/systemd/`](../deploy/systemd/) |

镜像是 `ghcr.io/openlinkos/registry-mirrors-relay`，支持 linux/amd64、arm64、arm/v7，基于 distroless，以非 root 用户（65532）运行。二进制在 [Releases](https://github.com/openlinkos/registry-mirrors-relay/releases)。

监听的端口：

| 端口 | 用途 | 对外 |
| --- | --- | --- |
| 8080 | 拉取入口（明文）；用法页；`/healthz`、`/readyz` | 是，或放在 CDN、反向代理之后 |
| 8443（自己配） | 拉取入口（HTTPS，`tls.mode: files` 或 `acme`） | 是 |
| 9090 | 管理后台和管理 API | **否**，只映射到本机，远程用 SSH 转发或 `kubectl port-forward` |
| 9180 | Prometheus 指标（可选 pprof） | 只给监控 |

## 第一个管理员

没有默认口令。部署时用一个口令文件（Docker、Compose、k8s 的 secret，systemd 的 credential）创建第一个管理员，环境变量 `RELAY_ADMIN_PASSWORD_FILE` 指向它，用户名默认 `admin`（`RELAY_ADMIN_USERNAME` 可改）：

```bash
openssl rand -base64 18 > admin-password
```

这个管理员第一次登录后必须改口令，之后口令文件就没用了，可以删掉（环境变量留着也没关系，文件不存在时只在日志里提醒）。原因和其他办法见 [admin.md](admin.md#第一个管理员)。

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
  ghcr.io/openlinkos/registry-mirrors-relay:v0.1.2
```

- 镜像自带一份配置：拉取入口 8080，管理后台 9090，指标 9180，一个匹配任意域名的单域名入口。要改就把自己的配置挂到 `/etc/registry-mirrors-relay/config.yaml`（写法见 [`config.example.yaml`](../config.example.yaml)）；容器里 `listen.admin`、`listen.metrics` 要写成 `:9090`、`:9180`。
- 口令文件要让容器里的 65532 用户能读，所以要 `chown 65532`。
- 管理后台只映射到宿主机的 127.0.0.1：打开 `http://127.0.0.1:9090/admin/`，远程用 `ssh -L 9090:127.0.0.1:9090 服务器`。
- 镜像的 `HEALTHCHECK` 运行 `registry-mirrors-relay probe`，它按配置里的端口探测 `/healthz`。

试一下：

```bash
curl -s http://127.0.0.1:8080/v2/library/alpine/manifests/latest \
  -H 'Accept: application/vnd.oci.image.index.v1+json' | head -c 200
```

## Docker Compose

[`deploy/compose/`](../deploy/compose/) 下有三个例子，每个目录里一个 `compose.yaml` 和一个 `config.yaml`：

| 目录 | 做法 |
| --- | --- |
| `plain/` | 放在 CDN 或反向代理之后，它们负责 HTTPS，回源到 8080。`client_ip.trusted_proxies` 写它们的回源地址段 |
| `acme/` | 直接对外，relay 自己用 ACME（Let's Encrypt）申请证书，监听宿主机的 80 和 443。域名要先解析到这台机器 |
| `behind-caddy/` | Caddy 在前面负责 HTTPS，relay 只在 Compose 的内部网络里 |

```bash
cd deploy/compose/plain
# 改 config.yaml 里的域名、trusted_proxies、站点信息
openssl rand -base64 18 > admin-password && chmod 400 admin-password && sudo chown 65532 admin-password
docker compose up -d
docker compose logs -f relay
```

更新：改 `compose.yaml` 里的镜像标签，`docker compose up -d`。配置文件改了会在 10 秒内自动重新加载，也可以 `docker compose kill -s HUP relay`。

## Kubernetes

[`deploy/kubernetes/`](../deploy/kubernetes/) 是 kustomize 的写法：

| 目录 | 内容 |
| --- | --- |
| `base/` | Deployment（2 个副本）、Service（80 → 8080，指标 9180）、管理后台单独的 Service（9090，不要对外暴露）、PDB。运营数据只读（`admin.store.driver: none`）：账号、规则写在 `config.yaml` 里 |
| `components/admin-password/` | 用 Secret `registry-mirrors-relay-admin` 里的口令创建第一个管理员 |
| `overlays/single-writable/` | 单副本 StatefulSet + PVC，运营数据可写，后台能建账号、加规则、封禁 |
| `overlays/gateway-api/` | 一个 HTTPRoute 挂到已有的 Gateway 上，由 Gateway 负责 HTTPS |
| `overlays/monitoring/` | Prometheus Operator 的 ServiceMonitor 和一个 Grafana 面板 |
| `overlays/networkpolicy/` | 入站只放行 8080 和监控的 9180，出站只放行 DNS 和公网的 443 |

Pod 以非 root 运行，根文件系统只读，去掉全部能力，seccomp 用 RuntimeDefault。退出时先等 5 秒（`preStop: sleep`，需要 Kubernetes 1.30+）让 Service 摘掉流量，再等进行中的下载最多 30 秒。内存上限是 512 MiB，relay 按它自动设置 `GOMEMLIMIT`。

在自己的仓库里建一个 kustomization，引用这些目录：

```yaml
# kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: registry-mirror
resources:
  - https://github.com/openlinkos/registry-mirrors-relay//deploy/kubernetes/overlays/single-writable?ref=v0.1.2
components:
  - https://github.com/openlinkos/registry-mirrors-relay//deploy/kubernetes/components/admin-password?ref=v0.1.2
```

```bash
kubectl create namespace registry-mirror
openssl rand -base64 18 > admin-password
kubectl -n registry-mirror create secret generic registry-mirrors-relay-admin --from-file=password=admin-password
rm admin-password   # 第一次登录后会要求改口令；只读模式（base）下它一直有效，请保管好
kubectl apply -k .
kubectl -n registry-mirror port-forward svc/registry-mirrors-relay-admin 9090   # 打开 http://127.0.0.1:9090/admin/
```

多副本（base）时运营数据只读，配置文件里的初始管理员就是唯一的管理员，不能在后台改口令，请用足够长的随机口令，后台只通过 `port-forward` 访问。会话只在 Pod 的进程里，`port-forward` 一直连着同一个 Pod，所以不受多副本影响。

### 让节点从 relay 拉镜像

节点上的 containerd（k8s、k3s、RKE2 都是）按 `/etc/containerd/certs.d/<上游>/hosts.toml` 选镜像源。relay 要有一个节点能直接访问的地址，例如 LoadBalancer、NodePort，或者经 Gateway 的域名。每个节点上：

```toml
# /etc/containerd/certs.d/docker.io/hosts.toml
server = "https://registry-1.docker.io"

[host."https://mirror.example.com"]
  capabilities = ["pull", "resolve"]
```

其他上游（`ghcr.io`、`registry.k8s.io`、`quay.io`、`gcr.io`）各写一个，`server` 是上游自己的地址，`host` 都指向 relay：containerd 会在请求里带上 `?ns=<上游>`，relay 据此选上游。relay 出问题时 containerd 会退回 `server` 直连。管理后台的「入口与用法」页能按你的域名生成全部配置，包括 k3s 的 `registries.yaml`。

## Helm

chart 在 [`charts/registry-mirrors-relay/`](../charts/registry-mirrors-relay/)，从 v0.1.1 起随每个版本发布到 `oci://ghcr.io/openlinkos/charts/registry-mirrors-relay`。chart 的版本号就是应用的版本号去掉 `v`；没指定 `image.tag` 时，按 digest 拉取同一次发布的镜像。需要 Helm 3.8 以上（或 Helm 4）和 Kubernetes 1.30 以上。

| `mode` | 工作负载 | 运营数据 | 对应的 kustomize |
| --- | --- | --- | --- |
| `readonly`（默认） | Deployment，2 个副本，带 PDB | 只读，账号、规则写在 `config` 里 | `base/` |
| `writable` | StatefulSet，1 个副本，PVC 1 GiB | 可写 | `overlays/single-writable/` |

```yaml
# values.yaml
mode: writable
admin:
  existingSecret: registry-mirrors-relay-admin
# 原样写进 relay 的 config.yaml，写法见 config.example.yaml
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
helm install registry-mirrors-relay oci://ghcr.io/openlinkos/charts/registry-mirrors-relay --version 0.1.2 \
  -n registry-mirror -f values.yaml
helm test registry-mirrors-relay -n registry-mirror
kubectl -n registry-mirror port-forward svc/registry-mirrors-relay-admin 9090   # 打开 http://127.0.0.1:9090/admin/
```

- **chart 管的配置**：`data_dir`、`listen` 和 `admin.store` 由 chart 按 `mode` 设置，`config` 里再写就渲染失败。`tls` 和按端口区分的入口（`entrances[].listen`）也不支持，HTTPS 交给 Ingress 或 Gateway。
- **密钥**：values 里没有填口令的地方。管理员口令只能来自 `admin.existingSecret`。上游令牌和账号文件在 `config` 里写成 `env:` 或 `file:`，再用 `extraEnv`、`extraVolumes` 从 Secret 引入；写成 `value:` 会渲染失败。
- **配置变更**：默认（`configRollout: always`）配置一变就滚动重启。改成 `restartOnly` 后，只有要重启才能生效的改动（`listen`、`data_dir`、`admin.store`、`log.access`、`limits.tracked_clients`）才重启，其余的由 relay 自己在一分钟内热重载。
- **可选组件**（默认都关）：`ingress`、`httpRoute`（Gateway API）、`metrics.serviceMonitor`、`metrics.dashboard`（Grafana 面板）、`networkPolicy`。指标在单独的 ClusterIP Service 上，拉取入口的 Service 改成 NodePort 或 LoadBalancer 时，指标不会一起暴露。
- 全部参数见 chart 的 [README](../charts/registry-mirrors-relay/README.md)。节点怎样从 relay 拉镜像见上一节；`service.type: NodePort` 时，`helm install` 的提示里有 `hosts.toml` 的例子。

## systemd

```bash
sudo install -m 0755 registry-mirrors-relay /usr/local/bin/
sudo install -d /etc/registry-mirrors-relay
sudo install -m 0644 config.yaml /etc/registry-mirrors-relay/        # 从 config.example.yaml 改
sudo install -m 0644 deploy/systemd/registry-mirrors-relay.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now registry-mirrors-relay
```

单元文件用动态用户（`DynamicUser`），运营数据在 `/var/lib/registry-mirrors-relay`（`StateDirectory`），并且开了较严的沙箱（`systemd-analyze security` 评分 1.5）。第一个管理员：把口令写进一个只有 root 能读的文件，取消单元文件里 `LoadCredential=` 和 `Environment=RELAY_ADMIN_PASSWORD_FILE=` 两行的注释。`systemctl reload` 重新加载配置，不中断进行中的下载。

## 从源码构建

需要 Go（版本见 `go.mod`）：

```bash
go build -trimpath -o registry-mirrors-relay ./cmd/registry-mirrors-relay
scripts/build-image.sh registry-mirrors-relay:dev          # 用同一个 Dockerfile 构建镜像
```

## 校验发布物

每个版本的 `SHA256SUMS`、镜像和 Helm chart 都用 [Sigstore](https://www.sigstore.dev/) 的 cosign 无密钥签名，签名人是本仓库的发布工作流；另有 GitHub 的构建来源证明和每个压缩包的 SBOM。

```bash
cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp '^https://github.com/openlinkos/registry-mirrors-relay/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c SHA256SUMS

cosign verify ghcr.io/openlinkos/registry-mirrors-relay:v0.1.2 \
  --certificate-identity-regexp '^https://github.com/openlinkos/registry-mirrors-relay/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/openlinkos/registry-mirrors-relay:v0.1.2 -R openlinkos/registry-mirrors-relay

cosign verify ghcr.io/openlinkos/charts/registry-mirrors-relay:0.1.2 \
  --certificate-identity-regexp '^https://github.com/openlinkos/registry-mirrors-relay/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/openlinkos/charts/registry-mirrors-relay:0.1.2 -R openlinkos/registry-mirrors-relay
```

## 需要登录的入口和 Docker 的 registry-mirrors

这类入口的用法页也要登录，没登录的人只会看到 401。

入口配了 `auth: basic` 时，Docker 的 `daemon.json` 里的 `registry-mirrors` 用不了：Docker 访问镜像源时不会带 `docker login` 存下的账号。这类入口请用路径前缀（`docker login mirror.example.com` 后 `docker pull mirror.example.com/docker.io/library/nginx`），或者 containerd 的 `hosts.toml` 加上请求头：

```toml
[host."https://mirror.example.com"]
  capabilities = ["pull", "resolve"]
  [host."https://mirror.example.com".header]
    Authorization = ["Basic <base64(用户名:口令)>"]
```
