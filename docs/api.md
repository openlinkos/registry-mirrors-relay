# 管理 API

简体中文 | [English](api.en.md)

JSON API 在管理入口的 `/admin/api/v1/` 下：默认和拉取入口共用端口，写了 `listen.admin` 就在那个端口；只接受本机、内网不经代理的直连（见[部署](deploy.md#管理后台管理-api-和指标)）。网页后台见 [admin.md](admin.md)。两边的校验和写入是同一套代码，网页能做的事 API 都能做。

## 认证

请求头带 `Authorization: Bearer <令牌>`。令牌有两个来源：

- 配置文件的 `admin.api_tokens`，适合自动化和 k8s Secret：

  ```yaml
  admin:
    api_tokens:
      - {name: ops, role: admin, token: {env: RELAY_ADMIN_TOKEN}}   # 至少 32 个字符，例如 openssl rand -hex 32
      - {name: grafana, role: viewer, token: {file: /run/secrets/relay_viewer}}
  ```

- `POST /tokens` 创建，存进运营数据库；明文只在创建时返回一次，数据库里只存 SHA-256。

也可以用网页后台的会话（Cookie）：读操作直接用，写操作要带 `X-CSRF-Token` 并且 `Origin` 正确，否则返回 403。必须先改口令的管理员，改之前会话不能用于 API。

`viewer` 只能读，写操作要 `admin`。所有写操作都记审计。认证失败按 IP 限次数（`limits.auth_failures_per_min`），超过返回 429。

Host 只接受 `admin.public_url` 的主机、`localhost` 和 IP 地址，其他返回 421。远程管理请用 SSH 转发或 `kubectl port-forward`，或者按[部署](deploy.md#管理后台管理-api-和指标)写 `public_url` 和 `proxied_allow`，放在可信的反向代理之后。

运营数据的存储方式是 `none`（只读）时，写操作返回 409。

## 接口

| 方法和路径 | 说明 |
| --- | --- |
| `GET /status` | 版本、启动时间、最近一次重载、manifest 缓存、限流计数、各上游熔断状态和探测结果、内存 |
| `GET /status/report` | 服务状态，和用法页的 `/status.json` 一样：总体状态、各上游近 24 小时每小时的可用率、本站的请求成功率。`site.show_status: false` 时也能看 |
| `GET /stats/series?minutes=60` | 每分钟的请求数、字节数、4xx、5xx、限流次数、缓存命中（最多 1440 分钟） |
| `GET /stats/summary?hours=24&n=20` | 一段时间的汇总、热门镜像、热门客户端、同一客户端反复完整下载同一层 |
| `GET /requests?limit=100&client_ip=&upstream=&image=&status=&q=` | 最近的请求（内存里最多 5000 条）；`status` 可以写 4 或 5 表示 4xx、5xx |
| `GET /upstreams` | 各上游的重定向白名单、熔断状态、最近一次探测、拉取额度（Docker Hub 的 `Ratelimit-*`，不转给客户端）、白名单外交给客户端的跳转主机 |
| `POST /upstreams/{name}/check` | 立即探测一次上游的 `/v2/`（不计拉取次数） |
| `POST /upstreams/{name}/redirect-hosts` `{"host": "cdn99.quay.io"}` | 把一个主机加进白名单（按字面匹配，不支持通配） |
| `DELETE /upstreams/{name}/redirect-hosts/{host}` | 去掉后台加的主机 |
| `GET /users` | 客户端账号（不含哈希），`source` 是 config（users_file）或 store |
| `POST /users` `{"name", "password" 或 "hash", "manifest_rate", "blob_streams", "note"}` | 创建账号；口令 12–72 字节，服务端用 bcrypt 存；`hash` 只接受 bcrypt |
| `PATCH /users/{name}` | 改口令、停用（`"disabled": true`）、单独的限流、备注 |
| `DELETE /users/{name}` | 删除账号 |
| `GET /rules` | IP 规则、生效中的封禁、镜像规则（含来源） |
| `POST /rules/ip` `{"cidr", "kind": "allow"\|"deny", "note"}`；`DELETE /rules/ip?cidr=&kind=` | IP 白名单、黑名单 |
| `POST /bans` `{"cidr", "duration": "2h", "reason"}`；`DELETE /bans?cidr=` | 临时封禁，最长 365 天 |
| `POST /rules/image` `{"pattern", "kind", "status": 403\|451, "message", "note"}`；`DELETE /rules/image?pattern=&kind=` | 镜像规则，写全上游：`ghcr.io/owner/**`、`*/library/nginx` |
| `POST /rules/image/test` `{"image": "nginx:1.27"}` | 这个镜像能不能拉（没写上游时按 Docker Hub） |
| `GET /tokens`；`POST /tokens` `{"name", "role"}`；`DELETE /tokens/{name}` | API 令牌 |
| `GET /admins`；`POST /admins` `{"name", "password", "role"}`；`PATCH /admins/{name}` `{"password", "role", "disabled"}`；`DELETE /admins/{name}` | 后台管理员；至少要留一个能用的 admin 角色，不能停用或删除自己；改动会结束那个管理员的会话。新建或重设别人的口令后，对方第一次登录必须先改口令（`must_change`） |
| `GET /site`；`PUT /site` `{"name", "operator", "links"}`；`DELETE /site` | 用法页的站点信息；设置后整体覆盖配置文件的 `site`，删除后改回配置文件的 |
| `GET /audit?limit=100` | 审计记录，最新的在前 |
| `GET /config` | 生效的配置（YAML，密钥只显示来源） |
| `POST /reload` | 重新读取配置文件；也可以发 SIGHUP，或者等文件变化后自动重载（每 10 秒检查一次） |

规则和账号的修改立即生效，不用重载。配置文件里的规则和账号在 API 里只读，要改请改配置文件。

## 指标

`metrics.enabled: true` 时，管理入口（默认是拉取端口，写了 `listen.admin` 就是那个端口）的 `/metrics` 是 Prometheus 文本格式，和后台一样只接受本机、内网不经代理的直连：

- `relay_requests_total{upstream,kind,code,cache}`、`relay_response_bytes_total`、`relay_request_duration_seconds`；
- `relay_manifest_cache_events_total{result}`、`relay_manifest_cache_bytes`；
- `relay_inflight_requests`、`relay_blob_streams`、`relay_rate_limited_total`、`relay_upstream_breaker_open{upstream}`、`relay_accesslog_dropped_total`；
- `relay_upstream_probe_up{upstream}`、`relay_upstream_probe_seconds{upstream}`（每分钟探测一次 `/v2/`，`proxy.health_interval` 为 0 时不探测）、`relay_upstream_quota_remaining{upstream}`；
- `go_goroutines`、`go_memstats_heap_inuse_bytes`、`process_resident_memory_bytes`、`relay_build_info`。

再写 `metrics.pprof: true` 时，同一个入口上还有 `/debug/pprof/`。两个开关都能热重载。
