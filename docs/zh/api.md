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

API 和后台一样，默认对所有地址开放（`admin.public: true`），靠令牌或会话保护；对外请放在 HTTPS 之后。`admin.public: false` 时只回应本机、内网不经代理的直连，Host 只接受 `admin.public_url` 的主机、`localhost` 和 IP 地址（其他返回 421），见[部署](deploy.md#管理后台管理-api-和指标)。

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
| `GET /config?view=effective\|file` | 生效的配置（YAML，密钥只显示来源）；`view=file` 是只按配置文件的，不含后台的修改 |
| `GET /config/overrides` | 后台对配置的修改：`key`、`value`（YAML）、`state`（`active` 生效；`drift` 生效，但配置文件之后改过这些字段；`suspended` 和配置文件冲突，没有生效；`ignored` 因为 `admin.config_edit: false` 没有生效）、`error`、修改人和时间；`editable` 表示现在能不能改 |
| `PUT /config/overrides/{key}`，请求体是这一段的 JSON | 改配置，立即生效。`key` 是 `limits`、`cache`、`proxy`、`site`、`upstreams/<名字>`（对象，逐字段覆盖）或 `entrances`（数组，整个替换），能改的字段见[管理后台](admin.md#在后台改配置)。只存和配置文件不同的字段，全都一样时去掉这一处（返回的 `override` 为 null）；`warnings` 是这次修改带来的新提醒。写错了，或者和配置文件合起来通不过校验，返回 422，什么都不改 |
| `DELETE /config/overrides/{key}` | 去掉一处修改，改回配置文件的值 |
| `GET /config/export?format=overrides\|merged` | 把生效中的修改导出成配置文件的写法（`yaml`）：`overrides`（默认）只有改过的部分；`merged` 合进配置文件、保留注释，直接写在配置里的密钥隐去 |
| `POST /reload` | 重新读取配置文件；也可以发 SIGHUP，或者等文件变化后自动重载（每 10 秒检查一次） |

规则、账号和配置的修改立即生效，不用重载。配置文件里的规则和账号在 API 里只读，要改请改配置文件。

```bash
# 把每个客户端每秒的 manifest 请求改成 5，全局处理中的请求改成 2000
curl -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"manifest_rate": 5, "max_inflight": 2000}' http://127.0.0.1:8080/admin/api/v1/config/overrides/limits
# 改回配置文件的值
curl -X DELETE -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/admin/api/v1/config/overrides/limits
```

## 指标

`metrics.enabled: true` 时，管理入口（默认是拉取端口，写了 `listen.admin` 就是那个端口）的 `/metrics` 是 Prometheus 文本格式。指标没有登录，所以只接受本机、内网不经代理的直连（`admin.allow`）：

- `relay_requests_total{upstream,kind,code,cache}`、`relay_response_bytes_total`、`relay_request_duration_seconds`；
- `relay_manifest_cache_events_total{result}`、`relay_manifest_cache_bytes`；
- `relay_inflight_requests`、`relay_blob_streams`、`relay_rate_limited_total`、`relay_upstream_breaker_open{upstream}`、`relay_accesslog_dropped_total`；
- `relay_config_overrides{key,state}`：后台对配置的每一处修改，`state` 同 `GET /config/overrides`；
- `relay_upstream_probe_up{upstream}`、`relay_upstream_probe_seconds{upstream}`（每分钟探测一次 `/v2/`，`proxy.health_interval` 为 0 时不探测）、`relay_upstream_quota_remaining{upstream}`；
- `go_goroutines`、`go_memstats_heap_inuse_bytes`、`process_resident_memory_bytes`、`relay_build_info`。

再写 `metrics.pprof: true` 时，同一个入口上还有 `/debug/pprof/`。两个开关都能热重载。
