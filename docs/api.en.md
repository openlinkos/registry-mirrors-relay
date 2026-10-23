# Admin API

[简体中文](api.md) | English

The admin listener is `listen.admin` (`127.0.0.1:9090` by default); the JSON API lives under `/admin/api/v1/`, and the web console is described in [admin.en.md](admin.en.md). Both share one implementation of validation and writes, so everything the console can do, the API can do. Error messages from the API are in Chinese.

## Authentication

Send `Authorization: Bearer <token>`. Tokens come from two places:

- `admin.api_tokens` in the configuration, for automation and Kubernetes Secrets:

  ```yaml
  admin:
    api_tokens:
      - {name: ops, role: admin, token: {env: RELAY_ADMIN_TOKEN}}   # at least 32 characters, e.g. openssl rand -hex 32
      - {name: grafana, role: viewer, token: {file: /run/secrets/relay_viewer}}
  ```

- `POST /tokens`, stored in the operational database; the token is returned once, and only its SHA-256 is kept.

The console session (cookie) works too: reads as they are, writes with `X-CSRF-Token` and the right `Origin`, otherwise 403. An administrator who must change the password cannot use the session for the API until they have.

`viewer` can only read; writes need `admin`. Every write is audited. Failed authentication is limited per IP (`limits.auth_failures_per_min`), beyond which the API returns 429.

The Host must be the host of `admin.public_url` or a loopback address (`127.0.0.1`, `localhost`, `[::1]`); anything else gets 421. Manage remotely through SSH forwarding, or set `public_url` behind a trusted reverse proxy.

With read-only operational data (`none`), writes return 409.

## Endpoints

| Method and path | Description |
| --- | --- |
| `GET /status` | Version, start time, last reload, manifest cache, rate-limit counters, circuit breakers and probe results per upstream, memory |
| `GET /stats/series?minutes=60` | Per-minute requests, bytes, 4xx, 5xx, rate-limited requests, cache hits (up to 1440 minutes) |
| `GET /stats/summary?hours=24&n=20` | Totals over a period, top images, top clients, clients downloading the same layer in full repeatedly |
| `GET /requests?limit=100&client_ip=&upstream=&image=&status=&q=` | Recent requests (up to 5000 in memory); `status` 4 or 5 means 4xx or 5xx |
| `GET /upstreams` | Redirect allowlist, circuit breaker, last probe, pull quota (Docker Hub's `Ratelimit-*`, never passed to clients), and redirect hosts handed to clients for each upstream |
| `POST /upstreams/{name}/check` | Probe the upstream's `/v2/` now (not counted as a pull) |
| `POST /upstreams/{name}/redirect-hosts` `{"host": "cdn99.quay.io"}` | Allow one host (matched literally, no wildcards) |
| `DELETE /upstreams/{name}/redirect-hosts/{host}` | Remove a host added this way |
| `GET /users` | Client accounts (without hashes); `source` is config (users_file) or store |
| `POST /users` `{"name", "password" or "hash", "manifest_rate", "blob_streams", "note"}` | Create an account; passwords are 12–72 bytes and stored with bcrypt; `hash` must be bcrypt |
| `PATCH /users/{name}` | Change the password, disable (`"disabled": true`), own limits, note |
| `DELETE /users/{name}` | Delete an account |
| `GET /rules` | IP rules, bans in effect, image rules (with their source) |
| `POST /rules/ip` `{"cidr", "kind": "allow"\|"deny", "note"}`; `DELETE /rules/ip?cidr=&kind=` | IP allow and deny lists |
| `POST /bans` `{"cidr", "duration": "2h", "reason"}`; `DELETE /bans?cidr=` | Temporary bans, up to 365 days |
| `POST /rules/image` `{"pattern", "kind", "status": 403\|451, "message", "note"}`; `DELETE /rules/image?pattern=&kind=` | Image rules, starting with the upstream: `ghcr.io/owner/**`, `*/library/nginx` |
| `POST /rules/image/test` `{"image": "nginx:1.27"}` | Can this image be pulled (Docker Hub when no upstream is given) |
| `GET /tokens`; `POST /tokens` `{"name", "role"}`; `DELETE /tokens/{name}` | API tokens |
| `GET /admins`; `POST /admins` `{"name", "password", "role"}`; `PATCH /admins/{name}` `{"password", "role", "disabled"}`; `DELETE /admins/{name}` | Console administrators; at least one enabled admin role must remain, and you cannot disable or delete yourself; changes end that administrator's sessions. After you create an administrator or reset someone else's password, they must change it at the first sign-in (`must_change`) |
| `GET /site`; `PUT /site` `{"name", "operator", "links"}`; `DELETE /site` | Site information on the usage page; once set it replaces `site` from the configuration, and deleting it switches back |
| `GET /audit?limit=100` | Audit entries, newest first |
| `GET /config` | The configuration in effect (YAML, secrets show only their source) |
| `POST /reload` | Re-read the configuration file; SIGHUP does the same, and changes to the file are picked up within 10 seconds |

Changes to rules and accounts take effect at once, without a reload. Rules and accounts from the configuration file are read-only in the API; change them in the file.

## Metrics

`/metrics` on `listen.metrics` (`127.0.0.1:9180` by default) uses the Prometheus text format:

- `relay_requests_total{upstream,kind,code,cache}`, `relay_response_bytes_total`, `relay_request_duration_seconds`;
- `relay_manifest_cache_events_total{result}`, `relay_manifest_cache_bytes`;
- `relay_inflight_requests`, `relay_blob_streams`, `relay_rate_limited_total`, `relay_upstream_breaker_open{upstream}`, `relay_accesslog_dropped_total`;
- `relay_upstream_probe_up{upstream}`, `relay_upstream_probe_seconds{upstream}` (a `/v2/` probe every minute; none when `proxy.health_interval` is 0), `relay_upstream_quota_remaining{upstream}`;
- `go_goroutines`, `go_memstats_heap_inuse_bytes`, `process_resident_memory_bytes`, `relay_build_info`.

With `metrics.pprof: true`, `/debug/pprof/` is served on the same listener.
