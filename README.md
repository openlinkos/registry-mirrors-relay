<p>
  <img alt="LinkOS Mirrors" src="https://openlinkos.com/brand/linkos-mirrors/github/readme.svg" height="56">
</p>

# LinkOS Mirrors · Relay

简体中文 | [English](README.en.md)

[LinkOS Mirrors](https://github.com/openlinkos/registry-mirrors) 是由 [OpenLinkOS](https://openlinkos.com) 公益运营的免费容器镜像加速服务。本仓库是它自己开发的零磁盘版：一个 Go 程序，只做转发，**镜像数据永远不写盘**，内置管理后台，可以用 docker run、Docker Compose、Kubernetes 或 systemd 部署。

## 三个版本怎么选

| | 本仓库（Relay） | [registry-mirrors-caddy](https://github.com/openlinkos/registry-mirrors-caddy) | [registry-mirrors](https://github.com/openlinkos/registry-mirrors) |
| --- | --- | --- | --- |
| 做法 | 自己开发的 Go 程序 | 一份 Caddy 配置 | Caddy + zot |
| 磁盘 | 不存镜像 | 不存镜像 | 缓存镜像 |
| 管理后台 | 有 | 无 | 无 |
| 适合 | 自建、团队、需要账号和规则 | 想要最少的东西 | 用户多、要省上游流量和额度 |

## 快速开始

```bash
openssl rand -base64 18 > admin-password      # 第一个管理员的口令，第一次登录后必须改
chmod 400 admin-password && sudo chown 65532 admin-password

docker run -d --name registry-mirrors-relay --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  -p 8080:8080 -p 127.0.0.1:9090:9090 \
  -v relay-data:/var/lib/registry-mirrors-relay \
  -v "$PWD/admin-password:/run/secrets/admin_password:ro" \
  -e RELAY_ADMIN_PASSWORD_FILE=/run/secrets/admin_password \
  ghcr.io/openlinkos/registry-mirrors-relay:v0.2.0
```

- 拉取：`docker pull 服务器:8080/library/alpine`（明文时要把 `服务器:8080` 加进 Docker 的 `insecure-registries`；正式使用请放在 HTTPS 之后，见[部署](docs/deploy.md)）。
- 管理后台：`http://127.0.0.1:9090/admin/`，用户名 `admin`，口令是 `admin-password` 里的内容。远程用 `ssh -L 9090:127.0.0.1:9090 服务器`。镜像把后台放在单独的 9090，只映射到本机；不用容器时后台默认和拉取共用 8080，只接受本机、内网的直连。
- 「入口与用法」页会按你的域名生成 Docker、containerd、k3s、Podman 的配置。

Docker Compose（CDN 之后、直接用 ACME、Caddy 在前三种）、Kubernetes（kustomize 或 Helm chart，带多副本、单副本可写、Gateway API、监控、网络策略）和 systemd 的写法见 **[docs/deploy.md](docs/deploy.md)**。

## 支持的用法

一个域名就能覆盖下面所有标准写法，每个上游一个子域名的用法也支持：

| 客户端写法 | 例子 |
| --- | --- |
| Docker 的 `registry-mirrors`（只对 Docker Hub 生效） | `"registry-mirrors": ["https://mirror.example.com"]` |
| containerd 的 `hosts.toml`（k8s、k3s、nerdctl） | `[host."https://mirror.example.com"]`，按 `?ns=` 区分上游 |
| 路径前缀（Podman、CRI-O 的 `registries.conf`，或直接改写镜像地址） | `docker pull mirror.example.com/ghcr.io/owner/image` |
| 每个上游一个子域名 | `docker pull ghcr.example.com/owner/image` |

内置 Docker Hub、gcr.io、ghcr.io、registry.k8s.io、quay.io，可以在配置里增加别的上游。

## 功能

- **拉取入口**：按域名或单域名（路径前缀、`ns`、根路径）的入口；HTTPS 可以用证书文件（自动重新加载）或 ACME 自动申请。
- **省额度、省资源**：manifest 在内存里缓存（有上限），按 tag 拉取时先用不计次数的 HEAD 确认；镜像层流式转发，每个流一块 16 KiB 缓冲。
- **用法页和服务状态**：打开域名就能看到加速地址、地址转换和各客户端的配置，以及各上游的连通性、近 24 小时的可用率和本站的请求成功率。需要登录的入口默认只有拉取要登录，用法页也可以设成要登录或关掉。见[部署](docs/deploy.md#用法页和服务状态)。
- **访问控制**：需要登录的入口（bcrypt 账号）、IP 黑白名单、临时封禁、镜像黑白名单（403 或 451）、按账号或 IP 的限流。
- **管理后台和 API**：概览、服务状态（各上游近 24 小时的可用率）、Docker Hub 剩余额度、客户端配置生成、账号、规则、限流、请求、滥用、审计、管理员与令牌，中英文、深浅色。默认和拉取共用一个端口，也可以拆到单独的端口。见 [docs/admin.md](docs/admin.md)、[docs/api.md](docs/api.md)。
- **运维**：Prometheus 指标（默认关）、Caddy 格式的访问日志、配置热重载（SIGHUP、文件变化、后台按钮）、优雅退出、按容器内存上限自动设置 GOMEMLIMIT。

## 设计要点

- **不是开放代理**：上游只能从配置里选，跟随重定向只去白名单内的主机，拨号时拒绝内网地址。
- **协议正确**：digest 和上游一致；透传 Range，支持断点续传；HEAD 不改成 GET，不多算 Docker Hub 次数；上游的错误码如实返回。
- **不泄露信息**：不把客户端 IP、Cookie、User-Agent 转给上游，不把源站出口 IP 和上游的额度头转给客户端；客户端 IP 只信任配置的代理，从右往左取。
- **管理后台默认安全**：只接受本机、内网不经代理的直连，经 CDN、反向代理来的请求一律 404；没有默认口令，第一个管理员的口令第一次登录后必须改；严格 CSP、CSRF、登录限速，敏感操作要求重新输入口令。
- **省资源**：流式转发，空闲约 19 MiB 内存；没有总写超时，大层可以慢慢下。压测结果见 [docs/benchmark.md](docs/benchmark.md)。

## 文档

- [部署](docs/deploy.md)：docker run、Compose、Kubernetes、systemd、从源码构建、校验发布物
- [管理后台](docs/admin.md)：页面、第一个管理员、远程访问、安全措施
- [管理 API](docs/api.md)：接口和指标
- [压测](docs/benchmark.md)
- 配置：[`config.example.yaml`](config.example.yaml)。`registry-mirrors-relay check -config config.yaml -print` 校验配置并打印生效的值（密钥隐去）。

## 发布物与校验

每个版本在 [Releases](https://github.com/openlinkos/registry-mirrors-relay/releases) 提供 linux/amd64、arm64、arm/v7 的二进制和 SBOM，镜像是 `ghcr.io/openlinkos/registry-mirrors-relay`，Helm chart 是 `oci://ghcr.io/openlinkos/charts/registry-mirrors-relay`。`SHA256SUMS`、镜像和 chart 都用 Sigstore 的 cosign 无密钥签名，并带 GitHub 的构建来源证明，校验方法见[部署](docs/deploy.md#校验发布物)。

## 路线

- [x] M0 工程骨架：配置、命令行、CI
- [x] M1 代理核心：按域名、端口和单域名（前缀、`ns`、根路径）的入口，令牌、重定向、访问日志、用法页
- [x] M2 manifest 缓存、熔断、性能压测
- [x] M3 访问控制与运营数据：账号、IP 和镜像规则、限流、指标、热重载、管理 API
- [x] M4 管理后台
- [x] M5 部署与发布：HTTPS（证书文件、ACME）、镜像、Compose、Kubernetes、systemd、签名的 v0.1.0
- [x] M6 Helm chart：两种模式（多副本只读、单副本可写），随 v0.1.1 发布到 OCI 仓库并签名
- [x] v0.2.0：拉取、管理后台、管理 API、指标共用一个端口（可以拆开），管理入口只接受直连；后台显示服务状态
- [ ] v0.3.0：在后台改入口、上游、限流、缓存等配置

## 开发

需要 Go（版本见 `go.mod`）。

```bash
go test -race ./...
golangci-lint run ./...
scripts/build-image.sh registry-mirrors-relay:dev   # 构建镜像
scripts/kind-e2e.sh                                 # 在 kind 里端到端测试（需要 docker、kind、kubectl；INSTALL=helm 时用 chart 装）
scripts/chart-check.sh                              # chart 的检查（需要 helm、kubeconform、yq、helm-docs）
```

`scripts/isolated-test.sh` 在不能上外网的 Docker 网络里用各种客户端测试拉取，`scripts/smoke.sh` 对照上游检查 digest。

## 许可

代码以 [Apache License 2.0](LICENSE) 发布。LinkOS 与 OpenLinkOS 的名称和标志不在该许可范围内，使用规范见 [openlinkos.com/zh/brand](https://openlinkos.com/zh/brand)。
