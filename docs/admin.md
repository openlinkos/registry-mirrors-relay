# 管理后台

简体中文 | [English](admin.en.md)

管理后台和[管理 API](api.md) 在同一个监听上（`listen.admin`，默认 `127.0.0.1:9090`），打开 `http://127.0.0.1:9090/admin/`。页面全部在服务端渲染，不加载任何第三方资源，没有脚本也能用；样式来自 OpenLinkOS 的设计系统 [`@openlinkos/ui`](https://www.npmjs.com/package/@openlinkos/ui)，打包在程序里。支持中英文和深浅色。

## 页面

| 分组 | 页面 | 内容 |
| --- | --- | --- |
| | 概览 | 每分钟请求、流量、正在下载的层、5xx 比例、manifest 缓存命中、被限流的请求、Docker Hub 剩余额度（每 15 秒刷新）；24 小时的流量和请求曲线；各上游状态；热门镜像和客户端 |
| 服务 | 上游 | 每分钟一次的 `/v2/` 探测结果（不计拉取次数）、熔断状态、拉取额度；白名单外的重定向主机，可以一键加进白名单；立即探测 |
| | 入口与用法 | 配置里的入口；按域名生成 Docker `daemon.json`、containerd `hosts.toml`、k3s `registries.yaml`、Podman `registries.conf` 的配置 |
| 访问 | 客户端账号 | 需要登录的入口用的账号：新建（可以生成随机口令）、停用、重设口令、单独限流、最后使用时间 |
| | 规则 | 镜像能不能拉的测试；临时封禁；IP 名单；镜像规则和拒绝次数 |
| | 限流 | 生效的限流值、当前的并发、最近被限流的客户端（可以一键封禁）、单独限流的账号 |
| 记录 | 请求 | 最近 5,000 个请求，按客户端、镜像、上游、状态筛选 |
| | 滥用 | 同一个客户端反复完整下载同一层、流量最大的客户端（1 小时、24 小时、7 天） |
| | 审计 | 后台和 API 的每一次修改、登录、退出 |
| 系统 | 设置 | 用法页上的站点名称、运营者和链接（覆盖配置文件的 `site`）；日志设置 |
| | 管理员与令牌 | 后台管理员（管理员、只读两种角色）、API 令牌、当前的登录会话 |
| | 系统 | 版本、内存、协程、配置文件、上次重载、运营数据、pprof；重新加载配置；生效的配置（密钥隐去） |

配置文件里的账号、规则和令牌在后台只能查看。运营数据是只读的（`admin.store.driver: none`）时，所有修改都要改配置文件。

## 第一个管理员

没有默认口令，也没有"第一个打开网页的人设口令"的初始化页面：那样的页面在服务刚启动、还没人设口令时，谁先访问谁就拿到管理员。还没有管理员时，登录页只说明怎么创建。三种办法：

- **口令文件（推荐，适合容器）**：把口令写进一个只有服务能读的文件，用环境变量 `RELAY_ADMIN_PASSWORD_FILE` 指向它（用户名默认 `admin`，可用 `RELAY_ADMIN_USERNAME` 改）。Docker Compose 的 `secrets`、k8s 的 Secret、systemd 的 `LoadCredential` 都是这种形式，见[部署](deploy.md)。不提供直接写口令的环境变量：那样口令会出现在 `docker inspect`、进程环境和编排文件里。

  ```bash
  openssl rand -base64 18 > admin-password   # 生成一个随机口令
  chmod 600 admin-password
  ```

- **配置文件**：写 `admin.initial_admin`，口令同样建议用 `file`；直接写在配置里（`value`）会收到提醒。

  ```yaml
  admin:
    initial_admin:
      username: admin
      password: {file: /run/secrets/relay_admin_password}   # 12 到 72 个字节，不能和用户名相同
  ```

- **命令行**：停止服务，运行 `registry-mirrors-relay admin create -name admin`，按提示输入两次口令（也可以从标准输入读，口令不出现在命令行参数里）。运营数据库同时只能被一个进程打开，服务在运行时命令会在 2 秒后报错。

前两种办法只在运营数据库里还没有管理员时生效，并且**按它创建的管理员第一次登录后必须先改口令**：改之前只能打开「我的账号」，API 也不认这个会话；新口令不能和原来的一样。这样留在配置、环境变量、口令文件、终端历史里的口令只能用一次。改完后服务的日志会提醒把 `initial_admin` 和对应的口令文件删掉。同样，另一个管理员新建的账号、重设过口令的账号，第一次登录后也必须先改口令；自己改的、命令行设的不需要。

运营数据只读（`admin.store.driver: none`，k8s 多副本）时，后台不能改口令，`initial_admin` 就是唯一的管理员，口令一直有效，只能在配置里换。这时请用足够长的随机口令，并且后台只通过 `kubectl port-forward` 访问。

忘记口令时，其他管理员可以在「管理员与令牌」里重设；只有一个管理员时，停止服务后运行 `registry-mirrors-relay admin password -name admin`。`admin list` 列出所有管理员。

## 远程访问

后台默认只监听本机。几种访问方式：

- **SSH 转发**：`ssh -L 9090:127.0.0.1:9090 服务器`，然后在本机打开 `http://127.0.0.1:9090/admin/`。
- **Kubernetes**：`kubectl port-forward svc/registry-mirrors-relay-admin 9090`（见[部署](deploy.md#kubernetes)）。
- **Docker**：`-p 127.0.0.1:9090:9090`。镜像自带的配置已经让后台在容器里监听 `:9090`；挂载自己的配置时也要这样写。
- **反向代理**：放在 https 的反向代理之后，并配置 `admin.public_url: https://admin.example.com`。代理的地址要写进 `client_ip.trusted_proxies`，登录限速才能按真实的客户端计数。

没有配置 `public_url` 时，后台只接受用 `127.0.0.1`、`localhost`、`[::1]` 访问（其他 Host 返回 421，防 DNS 重绑定）。

## 安全

照 [Origin](https://github.com/openlinkos/origin) 的后台实现：

- **内容安全策略**：`default-src 'none'; script-src 'self'; style-src 'self'; …; require-trusted-types-for 'script'; trusted-types 'none'`。页面没有内联脚本、内联样式和事件属性；唯一的脚本只写文本，不写标记。另有 `X-Frame-Options: DENY`、`nosniff`、`Referrer-Policy: same-origin`、`Cross-Origin-Opener-Policy` 等。
- **会话**：32 字节随机 ID，进程里只存它的 SHA-256；30 分钟没有操作或登录满 12 小时后失效；重启后都要重新登录。`public_url` 是 https 时 Cookie 叫 `__Host-relay-admin`（Secure、HttpOnly、SameSite=Lax），否则叫 `relay-admin`（只在 `/admin` 下，HttpOnly、SameSite=Strict）。
- **跨站请求**：每个 POST 都要 `Origin` 和 `public_url` 或回环地址一致，拒绝 `Sec-Fetch-Site: cross-site`，并且要带会话的 CSRF 令牌；成功后 303 跳转。
- **登录限速**：口令一个一个地校验（bcrypt），每个客户端 10 分钟内最多失败 10 次，全局最多 100 次，超过后口令正确也拒绝（429），直到窗口过去。失败和封锁在每个窗口里只记一次审计。
- **敏感操作**：管理员、API 令牌、登录会话的修改和改自己的口令，要求 10 分钟内输入过口令，否则先确认身份。改了管理员的口令、角色或停用状态，那个管理员的会话全部结束。
- **能力限制**：后台不碰 docker.sock，不执行命令，不能向任意地址发请求（只能探测配置里的上游）。全部写操作是固定的 26 个 POST 路由，测试把它们逐个固定下来。

JSON API 也认后台的会话：读操作直接用，写操作要带 `X-CSRF-Token` 并通过同样的来源检查。
