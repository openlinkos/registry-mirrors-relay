# 管理 API

管理后台监听在 `listen.admin`（默认 `127.0.0.1:9090`），JSON API 在 `/admin/api/v1/` 下。后台网页和自动化脚本共用这套接口。

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

`viewer` 只能读，写操作要 `admin`。所有写操作都记审计。认证失败按 IP 限次数（`limits.auth_failures_per_min`），超过返回 429。

没有配置 `admin.public_url` 时，只接受用回环地址（`127.0.0.1`、`localhost`、`[::1]`）访问，Host 不对返回 421。远程管理请用 SSH 转发，或者配置 `public_url` 并放在可信的反向代理之后。

运营数据的存储方式是 `none`（只读）时，写操作返回 409。

## 接口

| 方法和路径 | 说明 |
| --- | --- |
| `GET /status` | 版本、启动时间、最近一次重载、manifest 缓存、限流计数、各上游熔断状态 |
| `GET /stats/series?minutes=60` | 每分钟的请求数、字节数、4xx、5xx、限流次数、缓存命中（最多 1440 分钟） |
| `GET /stats/summary?hours=24&n=20` | 一段时间的汇总、热门镜像、热门客户端、同一客户端反复完整下载同一层 |
| `GET /requests?limit=100&client_ip=&upstream=&image=&status=&q=` | 最近的请求（内存里最多 5000 条）；`status` 可以写 4 或 5 表示 4xx、5xx |
| `GET /upstreams` | 各上游的重定向白名单、熔断状态、白名单外交给客户端的跳转主机 |
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
| `GET /audit?limit=100` | 审计记录，最新的在前 |
| `GET /config` | 生效的配置（YAML，密钥只显示来源） |
| `POST /reload` | 重新读取配置文件；也可以发 SIGHUP，或者等文件变化后自动重载（每 10 秒检查一次） |

规则和账号的修改立即生效，不用重载。配置文件里的规则和账号在 API 里只读，要改请改配置文件。

## 指标

`listen.metrics`（默认 `127.0.0.1:9180`）的 `/metrics` 是 Prometheus 文本格式：

- `relay_requests_total{upstream,kind,code,cache}`、`relay_response_bytes_total`、`relay_request_duration_seconds`；
- `relay_manifest_cache_events_total{result}`、`relay_manifest_cache_bytes`；
- `relay_inflight_requests`、`relay_blob_streams`、`relay_rate_limited_total`、`relay_upstream_breaker_open{upstream}`、`relay_accesslog_dropped_total`；
- `go_goroutines`、`go_memstats_heap_inuse_bytes`、`process_resident_memory_bytes`、`relay_build_info`。

`metrics.pprof: true` 时，同一个监听上还有 `/debug/pprof/`。
