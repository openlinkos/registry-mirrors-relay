# 管理后台

简体中文 | [English](../admin.md)

管理后台和[管理 API](api.md) 默认和拉取入口共用端口（8080），在 `/admin/`；写了 `listen.admin` 就挪到那个单独的端口。默认对所有地址开放，靠登录保护，见下面的[打开后台](#打开后台)。页面全部在服务端渲染，不加载任何第三方资源，没有脚本也能用；样式来自 OpenLinkOS 的设计系统 [`@openlinkos/ui`](https://www.npmjs.com/package/@openlinkos/ui)，打包在程序里。支持中英文和深浅色：语言默认跟随浏览器（既不要中文也不要英文时用英文），可以在侧栏底部的账号菜单（或者登录页底部）里选，选过之后记在 `relay-lang` Cookie 里，用法页也用它；配色（自动、浅色、深色）在它上面切换。写了 `site.brand` 时，后台和用法页一样用这个品牌的配色和标志。

## 页面

| 分组 | 页面 | 内容 |
| --- | --- | --- |
| | 概览 | 每分钟请求、流量、正在下载的层、5xx 比例、manifest 缓存命中、被限流的请求、Docker Hub 剩余额度（每 15 秒刷新）；24 小时的流量和请求曲线；服务状态：总体状态、各上游近 24 小时每小时的可用率、本站的请求成功率（和用法页一样）；热门镜像和客户端 |
| 服务 | 上游 | 每分钟一次的 `/v2/` 探测结果（不计拉取次数）和近 24 小时的可用率、熔断状态、拉取额度；白名单外的重定向主机，可以一键加进白名单；立即探测。每个上游的设置（地址、别名、重定向白名单、令牌服务）有自己的页面，新建上游也在这里 |
| | 入口 | 客户端拉取用的域名。每个入口有自己的页面：这个入口的客户端配置（按它的域名生成 Docker `daemon.json`、containerd `hosts.toml`、k3s `registries.yaml`、Podman `registries.conf`，要求登录时还有 `docker login`）和它的设置（域名、固定上游或默认上游、要不要登录、用法页） |
| | 用法页 | 浏览器打开入口域名时看到的页面：站点名称、运营者、品牌和页脚链接（覆盖配置文件的 `site`），是否显示服务状态，以及哪些入口提供用法页 |
| 访问 | 客户端账号 | 需要登录的入口用的账号：查找、筛选，在每一行的菜单里操作；在单独的页面新建，建好后显示怎么登录（生成的口令只显示这一次）；每个账号的页面有它最近的请求、限额和备注、重设口令和删除 |
| | 规则 | 镜像能不能拉的测试；临时封禁；IP 名单；镜像规则和拒绝次数 |
| | 限流 | 当前的并发；限流值，直接在这里改；最近被限流的客户端（可以一键封禁）；单独限流的账号 |
| 记录 | 请求 | 最近 5,000 个请求，按客户端、镜像、上游、状态筛选 |
| | 滥用 | 同一个客户端反复完整下载同一层、流量最大的客户端（1 小时、24 小时、7 天） |
| | 审计 | 后台和 API 的每一次修改、登录、退出 |
| 系统 | 配置 | 后台做过的所有修改，它们的状态、在哪里改和重置；manifest 缓存和上游连接；导出成配置文件的写法；生效的配置（密钥隐去） |
| | 管理员与令牌 | 后台管理员（管理员、只读两种角色）、API 令牌、当前的登录会话 |
| | 系统 | 版本、运行时间、内存、协程、运营数据；配置文件和上次重载，可以重新加载；管理入口（共用端口还是单独端口、允许的地址、指标、pprof）；日志设置 |

自己的口令和偏好在账号菜单的「我的账号」里。配置文件里的账号、规则和令牌在后台只能查看。每项设置在用到它的页面上改，见[在后台改配置](#在后台改配置)。运营数据是只读的（`admin.store.driver: none`）时，所有修改都要改配置文件。

近 24 小时的图表、服务状态和账号的最后使用时间每五分钟和退出时随运营数据保存一次，重启、升级后还在；最近的请求列表和登录会话不保存。运营数据只读时，这些都在重启后从头统计。

## 第一个管理员

没有默认口令，也没有"第一个打开网页的人设口令"的初始化页面：那样的页面在服务刚启动、还没人设口令时，谁先访问谁就拿到管理员。

**第一次启动时**，如果运营数据库里没有管理员，配置里也没写，服务会创建 `admin`（可用 `RELAY_ADMIN_USERNAME` 改名），生成一个随机口令，在日志里写一次：

```bash
docker logs relay 2>&1 | grep 'generated password'
kubectl logs sts/registry-mirrors-relay | grep 'generated password'
journalctl -u registry-mirrors-relay | grep 'generated password'
```

用它登录后改成自己的口令；改之前只能打开「我的账号」，所以日志里的口令改完就没用了。

**命令行**随时可以设口令，服务运行时也可以：

```bash
docker exec -it relay registry-mirrors-relay admin password                            # Docker
docker compose exec relay registry-mirrors-relay admin password                        # Compose
kubectl exec -it sts/registry-mirrors-relay -- registry-mirrors-relay admin password   # Kubernetes
sudo registry-mirrors-relay admin password                                             # systemd
```

- 不写名字时，设唯一那个管理员的口令；一个都没有就创建 `admin`。有多个管理员时要写名字：`admin password alice`。
- 在终端里会让你输入两次，否则从标准输入读一行（`docker exec -i relay registry-mirrors-relay admin password < password.txt`），口令不出现在命令行参数里。`--generate` 生成一个随机口令，只打印这一次。
- 立即生效，同时启用这个账号，并让它在后台的会话全部下线。命令行设的口令，第一次登录后不需要再改。
- `admin create bob -role viewer` 新建管理员，`admin list` 列出所有管理员。
- 服务在运行时，命令经运营数据库旁边的本地管理 socket `admin.sock` 交给服务去改（见[安全](#安全)）；服务停着时，命令直接打开数据库。

**第一次启动前自己定口令**：

- **口令文件**（适合容器）：把口令写进一个只有服务能读的文件，用环境变量 `RELAY_ADMIN_PASSWORD_FILE` 指向它（用户名默认 `admin`，可用 `RELAY_ADMIN_USERNAME` 改）。Docker Compose 的 `secrets`、k8s 的 Secret、systemd 的 `LoadCredential` 都是这种形式，见[部署](deploy.md)。不提供直接写口令的环境变量：那样口令会出现在 `docker inspect`、进程环境和编排文件里。

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

这两种办法只在运营数据库里还没有管理员时生效，并且**按它创建的管理员第一次登录后必须先改口令**：改之前只能打开「我的账号」，API 也不认这个会话；新口令不能和原来的一样。这样留在配置、环境变量、口令文件、终端历史里的口令只能用一次。改完后服务的日志会提醒把 `initial_admin` 和对应的口令文件删掉。同样，另一个管理员新建的账号、重设过口令的账号，第一次登录后也必须先改口令；自己改的、命令行设的不需要。

运营数据只读（`admin.store.driver: none`，k8s 多副本）时，后台和命令行都不能改口令，`initial_admin` 就是唯一的管理员，口令一直有效，只能在配置里换，换完重启 Pod。这时请用足够长的随机口令。

忘记口令时，其他管理员可以在「管理员与令牌」里重设，也可以按上面运行 `registry-mirrors-relay admin password`。

## 打开后台

后台默认对所有地址开放（`admin.public: true`），用拉取的域名或 IP 打开 `/admin/` 就行：

- **经 CDN、反向代理、Ingress**：`https://mirror.example.com/admin/`。浏览器那边是 https 时，会话 Cookie 带 `Secure`。
- **直接访问**：`http://服务器:8080/admin/`；用 `tls.mode: acme` 时是 `https://<域名>/admin/`。
- **Kubernetes**：也可以 `kubectl port-forward svc/registry-mirrors-relay 8080:80`，然后打开 `http://127.0.0.1:8080/admin/`；chart 拆了端口（`admin.separatePort`）时用 `svc/registry-mirrors-relay-admin 9090`。多副本时经 Ingress 打开要开会话保持，见[部署](deploy.md#kubernetes)。

保护靠登录（见下面的[安全](#安全)），所以对外请放在 HTTPS 之后：没有 HTTPS、也没写反向代理时，启动日志会提醒口令会明文传输。

**只让本机和内网打开**：写 `admin.public: false`。这时后台只回应本机、内网不经代理的直连（`admin.allow`，默认 `loopback` 和 `private_ranges`），经 CDN、反向代理来的请求一律 404，像这条路径不存在一样；浏览器访问时的 Host 只能是 `public_url` 的域名、`localhost` 或 IP 地址，其他域名返回 421（防 DNS 重绑定）。远程用 `kubectl port-forward` 或 `ssh -L 8080:127.0.0.1:8080 服务器`；要经 https 反向代理放行固定的地址，写 `admin.public_url`、`client_ip.trusted_proxies` 和 `admin.proxied_allow`。规则和各种环境下是否可靠，见[部署](deploy.md#管理后台管理-api-和指标)。

## 在后台改配置

下面这些配置可以在后台改，每项在用到它的页面上改（也可以用 API 的 [`/config/overrides`](api.md#接口)）：

| 配置 | 页面 | 能改的 | 怎样合并 |
| --- | --- | --- | --- |
| 入口（`entrances`） | 「入口」，每个入口一页 | 名字、域名、固定上游或默认上游、要不要登录、用法页 | 整个列表替换 |
| 上游（`upstreams.<名字>`） | 「上游」，每个上游一页 | `registry`、`aliases`、`library_prefix`、重定向白名单、`token_realm_hosts`、`unlisted_redirect`、停用；也可以新建上游 | 逐字段覆盖 |
| 限流（`limits`） | 「限流」 | 除 `tracked_clients` 以外的字段 | 逐字段覆盖 |
| manifest 缓存（`cache`） | 「配置」 | 全部字段 | 逐字段覆盖 |
| 上游连接（`proxy`） | 「配置」 | 空闲连接、重定向次数、转发 `Ratelimit-*` 头、CDN 缓存、熔断、探测间隔 | 逐字段覆盖 |
| 用法页（`site.show_status`） | 「用法页」 | 是否显示服务状态 | |

用法页的名称、运营者、品牌和链接在后台整体保存，在重置之前替换配置文件里的 `site`。「配置」页列出后台做过的所有修改，并链接到修改它的页面。

后台的修改存在运营数据库里，不写回配置文件。解析时先把它们合进配置文件，再和配置文件一样校验，所以默认值、校验规则和报错都和改配置文件一样。只存和配置文件不同的字段；改回和配置文件一样，或者点「重置」，这一处修改就去掉了。改了立即生效，不用重载。用 ACME 时，新加的入口域名在第一次访问时申请证书（最多 100 个域名）。

**只能改配置文件的**：监听、TLS、管理入口（`admin.*`）、客户端 IP、日志、指标、超时、运营数据的存储、`access.users_file`，以及上游的账号（`credentials`）、`tls_ca_file`、`allow_private_network` 和 `proxy` 的 `buffer_size`、`user_agent`、`blob_http2`。这样后台改不了谁能进后台，新建的上游也连不了内网地址。配了账号的上游，`registry` 和 `token_realm_hosts` 也只能改配置文件，免得把账号发到别的主机。

**和配置文件的关系**：配置文件是基准。

- 保存时，这一处修改要和其他生效中的修改、配置文件合起来通过校验，否则不保存，什么都不变。例如被入口引用的上游不能删。
- 重启和重新加载配置文件时，和配置文件冲突的修改暂停生效（概览上有提醒，审计里有记录），其余照常；冲突解决后自动恢复。
- 配置文件在后台修改之后又改了同样的字段，状态显示「配置文件之后改过」，仍以后台为准；确认后可以重置或重新保存。
- `registry-mirrors-relay check` 只检查配置文件，不含后台的修改。生效的配置在「配置」页和 `GET /config`。

**GitOps**：配置放在 Git 里时，后台的修改在 Git 里看不到。建议把它当作临时调整：在「配置」页导出（只有改过的部分，或者合并后的整个配置文件），提交到 Git，生效后在后台重置。要完全以 Git 为准，写 `admin.config_edit: false`：后台的修改一律不生效，只能重置。指标 `relay_config_overrides{key,state}` 列出所有修改，可以用来报警。

修改和重置都要 10 分钟内输入过口令，并写进审计（`config.override.set`、`config.override.reset`；暂停和恢复记为 `config.override.suspended`、`config.override.resumed`）。只读角色只能查看。

## 安全

后台默认拒绝、逐层把关：

- **内容安全策略**：`default-src 'none'; script-src 'self'; style-src 'self'; …; require-trusted-types-for 'script'; trusted-types 'none'`。页面没有内联脚本、内联样式和事件属性；唯一的脚本只写文本，不写标记。另有 `X-Frame-Options: DENY`、`nosniff`、`Referrer-Policy: same-origin`、`Cross-Origin-Opener-Policy` 等。
- **会话**：32 字节随机 ID，进程里只存它的 SHA-256；30 分钟没有操作或登录满 12 小时后失效；重启后都要重新登录。`public_url` 是 https 时 Cookie 叫 `__Host-relay-admin`（Secure、HttpOnly、SameSite=Lax），否则叫 `relay-admin`（只在 `/admin` 下，HttpOnly、SameSite=Strict）。
- **谁能连进来**：默认所有地址都能打开登录页，靠下面这些措施保护；`admin.public: false` 时只有本机、内网不经代理的直连，其他请求看到的是 404，见上面的[打开后台](#打开后台)。指标和 pprof 没有登录，一直只给直连。
- **跨站请求**：每个 POST 都要 `Origin` 和 `public_url` 或当前访问的 localhost、IP 地址一致，拒绝 `Sec-Fetch-Site: cross-site`，并且要带会话的 CSRF 令牌；成功后 303 跳转。
- **登录限速**：口令一个一个地校验（bcrypt），每个客户端 10 分钟内最多失败 10 次，全局最多 100 次，超过后口令正确也拒绝（429），直到窗口过去。失败和封锁在每个窗口里只记一次审计。
- **敏感操作**：管理员、API 令牌、登录会话和配置的修改，以及改自己的口令，要求 10 分钟内输入过口令，否则先确认身份。改了管理员的口令、角色或停用状态，那个管理员的会话全部结束。
- **本地管理 socket**：运营数据库旁边的 `admin.sock`，权限 0600，所在目录只有服务的用户能进；在 Linux 和 macOS 上，服务还会拒绝以其他用户身份运行的进程（root 除外）。它只提供 `admin` 子命令需要的几件事：列出管理员、新建管理员、设口令，审计里记为 `cli:<用户>`。只在运营数据可写时打开。
- **能力限制**：后台不碰 docker.sock，不执行命令，只向配置里的上游发请求；后台新建的上游也只能连公网地址。全部写操作是固定的 30 个 POST 路由，测试把它们逐个固定下来。

JSON API 也认后台的会话：读操作直接用，写操作要带 `X-CSRF-Token` 并通过同样的来源检查。
