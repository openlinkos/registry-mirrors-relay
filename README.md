<p>
  <img alt="LinkOS Mirrors" src="https://openlinkos.com/brand/linkos-mirrors/github/readme.svg" height="56">
</p>

# LinkOS Mirrors · Relay

简体中文 | [English](README.en.md)

[LinkOS Mirrors](https://github.com/openlinkos/registry-mirrors) 是由 [OpenLinkOS](https://openlinkos.com) 公益运营的免费容器镜像加速服务。本仓库是它自己开发的零磁盘版：一个 Go 程序，只做转发，**镜像数据永远不写盘**，内置管理后台，可以用 docker run、Docker Compose 或 Kubernetes 部署。

> **开发中**：还没有可用的版本。进度见下面的「路线」。现在要部署零磁盘加速，请用 [registry-mirrors-caddy](https://github.com/openlinkos/registry-mirrors-caddy)。

## 三个版本怎么选

| | 本仓库（Relay） | [registry-mirrors-caddy](https://github.com/openlinkos/registry-mirrors-caddy) | [registry-mirrors](https://github.com/openlinkos/registry-mirrors) |
| --- | --- | --- | --- |
| 做法 | 自己开发的 Go 程序 | 一份 Caddy 配置 | Caddy + zot |
| 磁盘 | 不存镜像 | 不存镜像 | 缓存镜像 |
| 管理后台 | 有 | 无 | 无 |
| 适合 | 自建、团队、需要账号和规则 | 想要最少的东西 | 用户多、要省上游流量和额度 |

## 支持的用法

一个域名就能覆盖下面所有标准写法，每个上游一个子域名的用法也支持：

| 客户端写法 | 例子 |
| --- | --- |
| Docker 的 `registry-mirrors`（只对 Docker Hub 生效） | `"registry-mirrors": ["https://mirror.example.com"]` |
| containerd 的 `hosts.toml`（k8s、k3s、nerdctl） | `[host."https://mirror.example.com"]`，按 `?ns=` 区分上游 |
| 路径前缀（Podman、CRI-O 的 `registries.conf`，或直接改写镜像地址） | `docker pull mirror.example.com/ghcr.io/owner/image` |
| 每个上游一个子域名 | `docker pull ghcr.example.com/owner/image` |

内置 Docker Hub、gcr.io、ghcr.io、registry.k8s.io、quay.io，可以在配置里增加别的上游。

## 设计要点

- **不是开放代理**：上游只能从配置里选，跟随重定向只去白名单内的主机，拨号时拒绝内网地址。
- **协议正确**：digest 和上游一致；透传 Range，支持断点续传；HEAD 不改成 GET，不多算 Docker Hub 次数；上游的错误码如实返回。
- **不泄露信息**：不把客户端 IP、Cookie、User-Agent 转给上游，不把源站出口 IP 转给客户端；客户端 IP 只信任配置的代理，从右往左取。
- **省资源**：流式转发，每个流一块小缓冲；内存里只缓存 manifest，有上限；没有总写超时，大层可以慢慢下。

## 路线

- [x] M0 工程骨架：配置、命令行、CI
- [ ] M1 代理核心：按域名和端口的入口、令牌、重定向、访问日志、用法页
- [ ] M2 单域名入口（根路径、`ns`、路径前缀）、manifest 缓存、性能压测
- [ ] M3 访问控制与运营数据：账号、IP 和镜像规则、限流、指标、热重载
- [ ] M4 管理后台
- [ ] M5 部署与发布：镜像、Compose、Kubernetes、签名的 v0.1.0
- [ ] M6 Helm chart

## 配置

见 [`config.example.yaml`](config.example.yaml)。运行 `registry-mirrors-relay check -config config.yaml -print` 可以校验配置并查看生效的值（密钥隐去）。

## 开发

```bash
go test -race ./...
golangci-lint run ./...
```

## 许可

代码以 [Apache License 2.0](LICENSE) 发布。LinkOS 与 OpenLinkOS 的名称和标志不在该许可范围内，使用规范见 [openlinkos.com/zh/brand](https://openlinkos.com/zh/brand)。
