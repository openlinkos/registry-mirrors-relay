# Admin API

[简体中文](api.md) | English

The JSON API lives under `/admin/api/v1/` on the management entrance: the pull port by default, or `listen.admin` when set. It answers only direct connections from loopback and private addresses (see [Deployment](deploy.en.md#admin-console-admin-api-and-metrics)); and the web console is described in [admin.en.md](admin.en.md). Both share one implementation of validation and writes, so everything the console can do, the API can do. Error messages from the API are in Chinese.

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

The Host must be the host of `admin.public_url`, `localhost` or an IP address; anything else gets 421. Manage remotely through SSH forwarding or `kubectl port-forward`, or set `public_url` and `proxied_allow` behind a trusted reverse proxy as described in [Deployment](deploy.en.md#admin-console-admin-api-and-metrics).

With read-only operational data (`none`), writes return 409.

## Endpoints

| Method and path | Description |
| --- | --- |
| `GET /status` | Version, start time, last reload, manifest cache, rate-limit counters, circuit breakers and probe results per upstream, memory |
| `GET /status/report` | The service status, as in the usage page's `/status.json`: overall state, each upstream's hourly availability over 24 hours, this site's request success rate. Available even with `site.show_status: false` |
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
| `GET /config?view=effective\|file` | The configuration in effect (YAML, secrets show only their source); `view=file` is the configuration file alone, without the console's changes |
| `GET /config/overrides` | Changes made to the configuration in the console: `key`, `value` (YAML), `state` (`active` in effect; `drift` in effect, but the file changed these fields since; `suspended` conflicts with the file and is not in effect; `ignored` not in effect because of `admin.config_edit: false`), `error`, who changed it and when; `editable` says whether changes can be made now |
| `PUT /config/overrides/{key}` with the section as JSON | Change the configuration, in effect at once. `key` is `limits`, `cache`, `proxy`, `site`, `upstreams/<name>` (an object, field by field) or `entrances` (an array, replaced whole); the fields that can change are listed under [Admin console](admin.en.md#changing-the-configuration-in-the-console). Only fields that differ from the file are stored, and if none do the change is removed (`override` is null); `warnings` are new warnings caused by this change. Invalid input, or a result that fails validation with the file, returns 422 and changes nothing |
| `DELETE /config/overrides/{key}` | Remove a change and go back to the file |
| `GET /config/export?format=overrides\|merged` | Export the changes in effect in configuration file form (`yaml`): `overrides` (the default) has only the changes; `merged` merges them into the file, keeping comments and hiding secrets written inline |
| `POST /reload` | Re-read the configuration file; SIGHUP does the same, and changes to the file are picked up within 10 seconds |

Changes to rules, accounts and the configuration take effect at once, without a reload. Rules and accounts from the configuration file are read-only in the API; change them in the file.

```bash
# Allow 5 manifest requests per second per client and 2000 requests in flight overall
curl -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"manifest_rate": 5, "max_inflight": 2000}' http://127.0.0.1:8080/admin/api/v1/config/overrides/limits
# Go back to the configuration file
curl -X DELETE -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/admin/api/v1/config/overrides/limits
```

## Metrics

With `metrics.enabled: true`, `/metrics` on the management entrance (the pull port by default, or `listen.admin`) uses the Prometheus text format; like the console, it answers only direct connections from loopback and private addresses:

- `relay_requests_total{upstream,kind,code,cache}`, `relay_response_bytes_total`, `relay_request_duration_seconds`;
- `relay_manifest_cache_events_total{result}`, `relay_manifest_cache_bytes`;
- `relay_inflight_requests`, `relay_blob_streams`, `relay_rate_limited_total`, `relay_upstream_breaker_open{upstream}`, `relay_accesslog_dropped_total`;
- `relay_config_overrides{key,state}`: each change made to the configuration in the console, with `state` as in `GET /config/overrides`;
- `relay_upstream_probe_up{upstream}`, `relay_upstream_probe_seconds{upstream}` (a `/v2/` probe every minute; none when `proxy.health_interval` is 0), `relay_upstream_quota_remaining{upstream}`;
- `go_goroutines`, `go_memstats_heap_inuse_bytes`, `process_resident_memory_bytes`, `relay_build_info`.

Adding `metrics.pprof: true` serves `/debug/pprof/` on the same entrance. Both switches hot-reload.
