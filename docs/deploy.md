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
| 8080 | 拉取入口（明文）；用法页；`/healthz`、`/readyz`。默认也是管理后台（`/admin/`）、管理 API 和指标（`/metrics`，默认关）的入口 | 是，或放在 CDN、反向代理之后。管理路径只接受本机、内网的直连，经代理来的是 404，见[管理后台、管理 API 和指标](#管理后台管理-api-和指标) |
| 8443（自己配） | 拉取入口（HTTPS，`tls.mode: files` 或 `acme`） | 是 |
| 9090（`listen.admin`，可选） | 写了就把管理后台、管理 API 和指标挪到这个端口，拉取端口上不再有 | **否**，只映射到本机。Docker 镜像和 Compose 的 plain、acme 例子默认这样 |

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
  ghcr.io/openlinkos/registry-mirrors-relay:v0.2.0
```

- 镜像自带一份配置：拉取入口 8080，管理入口 9090（后台、管理 API、指标），一个匹配任意域名的单域名入口。要改就把自己的配置挂到 `/etc/registry-mirrors-relay/config.yaml`（写法见 [`config.example.yaml`](../config.example.yaml)），并且保留 `listen.admin: ":9090"`：Docker 发布端口时，IPv6 客户端和 rootless Docker 的来源地址会变成容器网关，和拉取共用 8080 就分不出谁在本机。
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
| `behind-caddy/` | Caddy 在前面负责 HTTPS。relay 的 8080 只映射到宿主机的 127.0.0.1，后台和拉取共用这个端口：远程用 `ssh -L 8080:127.0.0.1:8080` 打开 `http://127.0.0.1:8080/admin/`，经 Caddy 访问后台是 404 |

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
| `base/` | Deployment（2 个副本）、Service（80 → 8080，管理后台和指标也在这个端口）、PDB。运营数据只读（`admin.store.driver: none`）：账号、规则写在 `config.yaml` 里 |
| `components/admin-password/` | 用 Secret `registry-mirrors-relay-admin` 里的口令创建第一个管理员 |
| `overlays/single-writable/` | 单副本 StatefulSet + PVC，运营数据可写，后台能建账号、加规则、封禁 |
| `overlays/gateway-api/` | 一个 HTTPRoute 挂到已有的 Gateway 上，由 Gateway 负责 HTTPS |
| `overlays/monitoring/` | 打开指标，加上 Prometheus Operator 的 ServiceMonitor（抓 8080 的 `/metrics`）和一个 Grafana 面板 |
| `overlays/networkpolicy/` | 入站只放行 8080，出站只放行 DNS 和公网的 443 |

Pod 以非 root 运行，根文件系统只读，去掉全部能力，seccomp 用 RuntimeDefault。退出时先等 5 秒（`preStop: sleep`，需要 Kubernetes 1.30+）让 Service 摘掉流量，再等进行中的下载最多 30 秒。内存上限是 512 MiB，relay 按它自动设置 `GOMEMLIMIT`。

在自己的仓库里建一个 kustomization，引用这些目录：

```yaml
# kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: registry-mirror
resources:
  - https://github.com/openlinkos/registry-mirrors-relay//deploy/kubernetes/overlays/single-writable?ref=v0.2.0
components:
  - https://github.com/openlinkos/registry-mirrors-relay//deploy/kubernetes/components/admin-password?ref=v0.2.0
```

```bash
kubectl create namespace registry-mirror
openssl rand -base64 18 > admin-password
kubectl -n registry-mirror create secret generic registry-mirrors-relay-admin --from-file=password=admin-password
rm admin-password   # 第一次登录后会要求改口令；只读模式（base）下它一直有效，请保管好
kubectl apply -k .
kubectl -n registry-mirror port-forward svc/registry-mirrors-relay 8080:80   # 打开 http://127.0.0.1:8080/admin/
```

多副本（base）时运营数据只读，配置文件里的初始管理员就是唯一的管理员，不能在后台改口令，请用足够长的随机口令，后台只通过 `port-forward` 访问。会话只在 Pod 的进程里，`port-forward` 一直连着同一个 Pod，所以不受多副本影响。

### 让节点从 relay 拉镜像

节点上的 containerd（k8s、k3s、RKE2 都是）按 `/etc/containerd/certs.d/<上游>/hosts.toml` 选镜像源。relay 要有一个节点能直接访问的地址，例如 LoadBalancer、NodePort，或者经 Gateway 的域名。用 NodePort 或 LoadBalancer 时请写 `externalTrafficPolicy: Local`：否则外部客户端的来源会变成节点地址，看起来像内网直连，能打开后台的登录页。每个节点上：

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
helm install registry-mirrors-relay oci://ghcr.io/openlinkos/charts/registry-mirrors-relay --version 0.2.0 \
  -n registry-mirror -f values.yaml
helm test registry-mirrors-relay -n registry-mirror
kubectl -n registry-mirror port-forward svc/registry-mirrors-relay 8080:80   # 打开 http://127.0.0.1:8080/admin/
```

- **chart 管的配置**：`data_dir`、`listen`、`admin.store` 和 `metrics.enabled` 由 chart 设置，`config` 里写 `data_dir`、`listen`、`admin.store` 就渲染失败。`tls` 也不支持，HTTPS 交给 Ingress 或 Gateway。
- **密钥**：values 里没有填口令的地方。管理员口令只能来自 `admin.existingSecret`。上游令牌和账号文件在 `config` 里写成 `env:` 或 `file:`，再用 `extraEnv`、`extraVolumes` 从 Secret 引入；写成 `value:` 会渲染失败。
- **配置变更**：默认（`configRollout: always`）配置一变就滚动重启。改成 `restartOnly` 后，只有要重启才能生效的改动（`listen`、`data_dir`、`admin.store`、`log.access`、`log.level`、`timeouts` 的 `read_header`/`idle`/`shutdown_grace`、`proxy.buffer_size`、`limits.tracked_clients`）才重启，其余的由 relay 自己在一分钟内热重载。
- **管理后台和指标**：默认和拉取共用 8080，`port-forward` 到主 Service 就能打开后台；`metrics.enabled` 或 `metrics.serviceMonitor.enabled` 打开指标，ServiceMonitor 抓的也是 8080。`admin.separatePort: true` 把它们挪到单独的 9090 和 `<名字>-admin` Service。拉取入口的 Service 是 NodePort 或 LoadBalancer 时，要么写 `service.externalTrafficPolicy: Local`，要么拆端口，否则 chart 拒绝渲染。
- **可选组件**（默认都关）：`ingress`、`httpRoute`（Gateway API）、`metrics.serviceMonitor`、`metrics.dashboard`（Grafana 面板）、`networkPolicy`。
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

管理后台和拉取共用 8080：在这台机器上打开 `http://127.0.0.1:8080/admin/`，远程用 `ssh -L 8080:127.0.0.1:8080 服务器`；局域网里也可以直接用 IP 打开。

## 管理后台、管理 API 和指标

管理后台（`/admin/`）、管理 API（`/admin/api/v1/`）、指标（`/metrics`）和 pprof（`/debug/pprof/`）合称管理入口。默认和拉取入口共用端口（三合一），写了 `listen.admin` 就只在那个端口上。指标默认关，`metrics.enabled: true` 打开，`metrics.pprof` 再加上 pprof。

**谁能打开**：只回应两种请求，其余一律当作这些路径不存在（404，和任何不存在的路径一样）：

- **直连**：请求里没有任何代理头（`X-Forwarded-*`、`Forwarded`、`X-Real-IP`、`Via`、CDN 自己的客户端地址头等，HTTP/1.0 也算），并且 TCP 对端在 `admin.allow` 里（默认 `loopback` 和 `private_ranges`）。`kubectl port-forward`、SSH 隧道、局域网、集群里的 Prometheus 都是这种。
- **有意经代理**：写了 `admin.public_url`、`client_ip.trusted_proxies` 和 `admin.proxied_allow`，请求的 Host 是 `public_url` 的域名，对端是受信任的代理，代理传来的客户端地址在 `proxied_allow` 里。默认不允许。

经 CDN、Ingress、Gateway、Caddy、nginx 来的请求都带代理头，所以进不来。判断直连只看 TCP 对端，不看 `X-Forwarded-For`：绕过 CDN 直连源站的人可以伪造它。

这个判断在哪些环境可靠：

| 环境 | 可靠吗 | 说明 |
| --- | --- | --- |
| systemd、直接跑二进制 | 是 | 对端就是真实客户端 |
| Kubernetes：ClusterIP + Ingress/Gateway | 是 | 外部请求都经过 Ingress 控制器，带代理头 |
| Kubernetes：NodePort、LoadBalancer，`externalTrafficPolicy: Local` | 是 | 保留客户端地址 |
| Kubernetes：NodePort、LoadBalancer，`externalTrafficPolicy: Cluster` | **否** | 来源被换成节点地址，看起来像内网。请改 `Local` 或拆端口；chart 会拒绝渲染 |
| Docker 发布端口（`-p 8080:8080`） | **否**（IPv6、rootless） | IPv6 客户端和 rootless Docker 的来源会变成容器网关。镜像自带的配置因此默认拆端口 |
| 四层负载均衡、TCP 隧道（不带 PROXY 协议） | **否** | 对端是负载均衡自己的内网地址 |

不可靠的环境里，外部客户端能看到后台的登录页，只剩口令和登录限速挡着。这时请写 `listen.admin` 拆出单独的端口，只映射到本机或内网。

**经 https 反向代理访问后台**（例如办公网固定出口）：

```yaml
client_ip:
  trusted_proxies: [10.0.0.5]          # 反向代理的地址
admin:
  public_url: https://admin.example.com
  proxied_allow: [198.51.100.0/24]     # 允许的客户端（取自代理传来的 X-Forwarded-For）
```

代理要传 `Host` 和 `X-Forwarded-For`，并且要覆盖掉客户端自己带的 `X-Forwarded-For`，否则客户端可以伪造地址。nginx 的写法：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $remote_addr;   # 覆盖，不追加
}
```

**Prometheus**：抓 `http://<Pod 或主机>:8080/metrics`（拆端口时是 9090）。Prometheus 要能直连，它的地址要在 `admin.allow` 里；Pod 网段不在私有地址段里时（例如 `100.64.0.0/10`）要加上。

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

cosign verify ghcr.io/openlinkos/registry-mirrors-relay:v0.2.0 \
  --certificate-identity-regexp '^https://github.com/openlinkos/registry-mirrors-relay/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/openlinkos/registry-mirrors-relay:v0.2.0 -R openlinkos/registry-mirrors-relay

cosign verify ghcr.io/openlinkos/charts/registry-mirrors-relay:0.2.0 \
  --certificate-identity-regexp '^https://github.com/openlinkos/registry-mirrors-relay/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/openlinkos/charts/registry-mirrors-relay:0.2.0 -R openlinkos/registry-mirrors-relay
```

## 用法页和服务状态

浏览器打开任一入口的域名，看到的是用法页：各上游的加速地址、地址转换、Docker、containerd、k3s、Podman 的配置，以及**服务状态**：

- 各上游现在能不能连通、延迟多少，熔断的上游会标出来。relay 每分钟（`proxy.health_interval`）请求一次各上游的 `/v2/`，不计入拉取额度。
- 近 24 小时每小时一格：上游的可用率，以及本站的请求成功率（非 5xx 的比例，请求少于 20 个的小时不算）。
- 版本和已运行时长。数据只在内存里，重启后从头统计。

数据来自 `/status.json`，页面每分钟刷新一次。这个接口谁都能访问，所以只有公开也无妨的内容：失败原因只给分类（超时、DNS、连接、TLS、HTTP 状态码），不给错误原文、地址、上游额度和请求量。不想显示时写 `site.show_status: false`。

## 从 v0.1 升级

v0.2.0 有几处不兼容的改动，先用新镜像跑一遍 `registry-mirrors-relay check -config <配置文件>`：

- **`listen.metrics` 去掉了**：指标和管理后台在同一个入口，写 `metrics.enabled: true`。Prometheus 改抓拉取端口（或 `listen.admin`）的 `/metrics`。写了非空的 `listen.metrics` 会报错并说明改法；空字符串只提醒。
- **按端口区分的入口（`entrances[].listen`）去掉了**：改用 `hosts` 加 `upstream`（每个上游一个域名），或者单域名入口的 `/v2/<上游>/` 前缀。
- **`listen.admin` 默认不再是 `127.0.0.1:9090`**：管理后台默认和拉取共用端口，只接受本机、内网的直连。想保持原样就写 `listen.admin: "127.0.0.1:9090"`。Docker 镜像自带的配置仍然是 `:9090`。
- **经反向代理访问后台**的，要加 `admin.proxied_allow`（见上一节），否则是 404。
- **Helm**：去掉了 `adminService`、`metrics.service`、`containerPorts.metrics`；改用 `admin.separatePort`、`admin.service.port`、`metrics.enabled`。NodePort、LoadBalancer 要加 `externalTrafficPolicy: Local` 或 `admin.separatePort: true`。
- **kustomize**：base 去掉了 `-admin` Service 和指标端口；后台用 `port-forward svc/registry-mirrors-relay 8080:80` 打开，monitoring overlay 会打开指标。

## 需要登录的入口和 Docker 的 registry-mirrors

这类入口的用法页默认谁都能看，只有拉取要登录，页面会提示先 `docker login`。要连用法页也只给登录的人看，入口写 `page: login`；不要用法页写 `page: off`（只剩拉取，打开域名是 404）。

入口配了 `auth: basic` 时，Docker 的 `daemon.json` 里的 `registry-mirrors` 用不了：Docker 访问镜像源时不会带 `docker login` 存下的账号。这类入口请用路径前缀（`docker login mirror.example.com` 后 `docker pull mirror.example.com/docker.io/library/nginx`），或者 containerd 的 `hosts.toml` 加上请求头：

```toml
[host."https://mirror.example.com"]
  capabilities = ["pull", "resolve"]
  [host."https://mirror.example.com".header]
    Authorization = ["Basic <base64(用户名:口令)>"]
```
