# Admin API

[简体中文](zh/api.md) | English

The JSON API lives under `/admin/api/v1/` on the management entrance: the pull port by default, or `listen.admin` when set. The web console is described in [admin.md](admin.md); both share one implementation of validation and writes, so everything the console can do, the API can do. Errors carry a stable code and a message in English or Chinese, see [Errors](#errors).

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

Like the console, the API is open to every address by default (`admin.public: true`), protected by tokens or sessions; put it behind HTTPS when it faces the internet. With `admin.public: false` it answers only direct connections from loopback and private addresses, and the Host must be the host of `admin.public_url`, `localhost` or an IP address (anything else gets 421), see [Deployment](deploy.md#admin-console-admin-api-and-metrics).

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
| `PUT /config/overrides/{key}` with the section as JSON | Change the configuration, in effect at once. `key` is `limits`, `cache`, `proxy`, `site`, `upstreams/<name>` (an object, field by field) or `entrances` (an array, replaced whole); the fields that can change are listed under [Admin console](admin.md#changing-the-configuration-in-the-console). Only fields that differ from the file are stored, and if none do the change is removed (`override` is null); `warnings` are new warnings caused by this change. Invalid input, or a result that fails validation with the file, returns 422 and changes nothing |
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

## Errors

Every error is JSON with a stable `code` for programs and an `error` for people:

```json
{"error": "Operational data is read-only (admin.store.driver: none). Change the configuration file instead.", "code": "store.readonly"}
```

`error` is in English, or in Chinese when the request asks for it with `Accept-Language: zh` (or carries the console's language cookie `relay-lang`). Match on `code`, never on `error`. The status code is given where it is fixed; the others depend on the operation (usually 400 for invalid input, 404 for something that does not exist, 409 for a conflict, 422 for a configuration that fails validation).

| Code | Status | Meaning |
| --- | --- | --- |
| `auth.required` | 401 | A valid API token is required. |
| `auth.too_many` | 429 | Too many failed attempts. Try again later. |
| `auth.viewer` | 403 | A viewer cannot make changes. |
| `auth.cross_site` | 403 | Cross-site requests are not accepted. |
| `auth.must_change` | 403 | Change your password in the console first. |
| `auth.origin` | 403 | The request comes from the wrong origin. |
| `auth.csrf` | 403 | X-CSRF-Token is missing or wrong. |
| `host.misdirected` | 421 | Use admin.public_url, localhost or an IP address to reach the console. |
| `request.media_type` | 415 | The request body must be application/json. |
| `request.json` | 400 | The request body is not valid JSON. |
| `password.length` | | Passwords must be 12 to 72 bytes long. |
| `user.password_or_hash` | | Give either a password or a hash, not both. |
| `user.hash` | | Only bcrypt hashes are accepted. |
| `user.name` | | Names may contain letters, digits, dots, underscores and hyphens, start with a letter or digit, and be at most 64 characters long. |
| `user.in_file` | | This user is already in the users_file of the configuration. |
| `user.password_required` | | A password is required. |
| `user.exists` | | This user already exists. |
| `user.not_found` | | No such user. Users from users_file are changed in that file. |
| `limit.manifest_rate` | | Manifest requests per second must be between 0 and 1000. |
| `limit.blob_streams` | | Concurrent layer downloads must be between 0 and 10000. |
| `note.length` | | Notes are at most 500 bytes long. |
| `cidr.invalid` | | Write an IP address or a CIDR range, such as 198.51.100.7 or 198.51.100.0/24. |
| `cidr.all` | | A rule cannot cover every address. |
| `kind.invalid` | | The kind must be allow or deny. |
| `rule.exists` | | This rule already exists. |
| `rule.not_found` | | No such rule. Rules from the configuration file are changed there. |
| `ban.duration` | | Write the duration like 30m, 2h or 7d, from 1 minute to 365 days. |
| `ban.reason` | | The reason is at most 500 bytes long. |
| `ban.not_found` | | No such ban. |
| `image.pattern` | | Invalid pattern. Start with the upstream, such as ghcr.io/owner/**; * matches within one segment and ** matches any number of segments. |
| `image.upstream` | | The first segment must be a configured upstream (or *), such as docker.io/library/nginx or ghcr.io/owner/**. |
| `image.status` | | The status must be 403 or 451. |
| `image.text` | | The message is at most 300 bytes and the note at most 500 bytes long. |
| `upstream.not_found` | | No such upstream. |
| `host.invalid` | | Write one full host name, such as cdn99.quay.io. Wildcards belong in the configuration file. |
| `host.exists` | | This host is already allowed. |
| `host.not_found` | | This host is not in the list. Hosts from the configuration file are changed there. |
| `role.invalid` | | The role must be admin or viewer. |
| `token.in_config` | | The configuration file already has a token with this name. |
| `token.exists` | | This token already exists. |
| `token.not_found` | | No such token. Tokens from the configuration file are changed there. |
| `password.same` | | The new password must differ from the current one. |
| `password.username` | | The password must not be the username. |
| `admin.exists` | | This administrator already exists. |
| `admin.self` | | You cannot disable yourself or lower your own role. |
| `admin.delete_self` | | You cannot delete yourself. |
| `admin.last` | | At least one enabled administrator with the admin role must remain. |
| `admin.not_found` | | No such administrator. |
| `site.name` | | The site name is required and at most 80 bytes long; the operator is at most 120 bytes long. |
| `site.link_key` | | Unknown link …. |
| `site.link` | | The … link must be a full http(s) address. |
| `store.readonly` | | Operational data is read-only (admin.store.driver: none). Change the configuration file instead. |
| `reload.failed` | | Reloading the configuration failed |
| `config.invalid` | | The configuration was not saved |
| `config.edit_off` | | The configuration file sets admin.config_edit: false, so the configuration cannot be changed here, only reset. |
| `config.not_found` | | … has not been changed here. |
| `config.view` | | The view must be effective or file. |
| `config.format` | | The format must be overrides or merged. |
| `config.upstreamName` | | The upstream name must be written as in image names, such as registry.example.com (lowercase, with at least one dot). |
| `config.upstreamExists` | | The upstream … already exists; change it from the list. |
| `internal` | 500 | Anything else; the message is the error itself, in English |

## Metrics

With `metrics.enabled: true`, `/metrics` on the management entrance (the pull port by default, or `listen.admin`) uses the Prometheus text format. Metrics have no sign-in, so they answer only direct connections from loopback and private addresses (`admin.allow`):

- `relay_requests_total{upstream,kind,code,cache}`, `relay_response_bytes_total`, `relay_request_duration_seconds`;
- `relay_manifest_cache_events_total{result}`, `relay_manifest_cache_bytes`;
- `relay_inflight_requests`, `relay_blob_streams`, `relay_rate_limited_total`, `relay_upstream_breaker_open{upstream}`, `relay_accesslog_dropped_total`;
- `relay_config_overrides{key,state}`: each change made to the configuration in the console, with `state` as in `GET /config/overrides`;
- `relay_upstream_probe_up{upstream}`, `relay_upstream_probe_seconds{upstream}` (a `/v2/` probe every minute; none when `proxy.health_interval` is 0), `relay_upstream_quota_remaining{upstream}`;
- `go_goroutines`, `go_memstats_heap_inuse_bytes`, `process_resident_memory_bytes`, `relay_build_info`.

Adding `metrics.pprof: true` serves `/debug/pprof/` on the same entrance. Both switches hot-reload.
