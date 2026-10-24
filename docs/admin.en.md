# Admin console

[简体中文](admin.md) | English

The admin console and the [admin API](api.en.md) share the pull port (8080) by default, under `/admin/`; with `listen.admin` they move to that separate port. They answer only direct connections from loopback and private addresses, see [Remote access](#remote-access). Every page is rendered on the server, nothing is loaded from third parties, and everything works without JavaScript. The styles come from the OpenLinkOS design system [`@openlinkos/ui`](https://www.npmjs.com/package/@openlinkos/ui), embedded in the binary. Chinese and English, light and dark.

## Pages

| Group | Page | Content |
| --- | --- | --- |
| | Overview | Requests per minute, traffic, layers downloading, 5xx share, manifest cache hits, rate-limited requests, Docker Hub quota left (refreshed every 15 seconds); 24-hour charts of traffic and requests; service status: overall state, each upstream's hourly availability over 24 hours and this site's request success rate (as on the usage page); top images and clients |
| Service | Upstreams | Results of the `/v2/` probe every minute (not counted as pulls) and availability over 24 hours, circuit breaker, pull quota; redirect hosts outside the allowlist, with one-click allow; check now |
| | Entrances and usage | The configured entrances, each one's usage page setting (public, sign-in, off) and whether the usage page shows the service status; Docker `daemon.json`, containerd `hosts.toml`, k3s `registries.yaml` and Podman `registries.conf` generated for your host name |
| Access | Client accounts | Accounts for entrances that require sign-in: create (with an optional random password), disable, reset the password, per-account limits, last use |
| | Rules | "Can this image be pulled?"; temporary bans; IP lists; image rules with refusal counts |
| | Rate limits | Limits in effect, current concurrency, recently limited clients (one-click ban), accounts with their own limits |
| Records | Requests | The last 5,000 requests, filtered by client, image, upstream and status |
| | Abuse | The same client downloading the same layer in full again and again; top clients by traffic (1 hour, 24 hours, 7 days) |
| | Audit log | Every change through the console or the API, and every sign-in and sign-out |
| System | Settings | Site name, operator and links on the usage page (overriding `site` in the configuration); log settings |
| | Admins and tokens | Administrators (admin and viewer roles), API tokens, current sessions |
| | System | Version, memory, goroutines, configuration file, last reload, operational data; the management entrance (shared or separate port, allowed addresses, metrics, pprof); reload the configuration; the configuration in effect (secrets hidden) |

Accounts, rules and tokens from the configuration file are shown read-only. With read-only operational data (`admin.store.driver: none`), every change goes into the configuration file.

## The first administrator

There is no default password, and there is no setup page where whoever opens the site first chooses the password: on a freshly started service such a page hands the console to the first visitor. Without administrators, the sign-in page only explains how to create one. Three ways:

- **A password file (recommended, fits containers)**: write the password into a file only the service can read and point `RELAY_ADMIN_PASSWORD_FILE` at it (user name `admin`, or `RELAY_ADMIN_USERNAME`). Docker Compose `secrets`, Kubernetes Secrets and systemd `LoadCredential` all work this way; see [Deployment](deploy.en.md). There is deliberately no variable that takes the password itself, since it would show up in `docker inspect`, the process environment and orchestration files.

  ```bash
  openssl rand -base64 18 > admin-password   # a random password
  chmod 600 admin-password
  ```

- **The configuration file**: set `admin.initial_admin`, again preferably with `file`; a password written inline (`value`) triggers a warning.

  ```yaml
  admin:
    initial_admin:
      username: admin
      password: {file: /run/secrets/relay_admin_password}   # 12 to 72 bytes, not the user name
  ```

- **The command line**: stop the service and run `registry-mirrors-relay admin create -name admin`, then type the password twice (it can also come from standard input; it never appears in the arguments). Only one process can open the operational database, so with the service running the command fails after 2 seconds.

The first two apply only while the operational database has no administrators, and **the administrator created this way must change the password at the first sign-in**: until then only My account is open and the API refuses the session; the new password must differ from the old one. A password left in the configuration, an environment variable, a password file or a shell history therefore works once at most. Afterwards the service log reminds you to delete `initial_admin` and the password file. Likewise, an account created or reset by another administrator must change its password at the first sign-in; passwords you set yourself or through the command line need no change.

With read-only operational data (`admin.store.driver: none`, several Kubernetes replicas) the console cannot change passwords, so `initial_admin` is the only administrator and its password stays valid; change it in the configuration. Use a long random password and reach the console only through `kubectl port-forward`.

If you forget your password, another administrator can reset it under Admins and tokens. If you are the only one, stop the service and run `registry-mirrors-relay admin password -name admin`. `admin list` lists the administrators.

## Remote access

The console answers only direct connections from loopback and private addresses (`admin.allow`, `loopback` and `private_ranges` by default); requests through a CDN or reverse proxy get 404, as if the path didn't exist. The rule, and where it holds, are in [Deployment](deploy.en.md#admin-console-admin-api-and-metrics). Ways to reach it:

- **Kubernetes**: `kubectl port-forward svc/registry-mirrors-relay 8080:80`, then open `http://127.0.0.1:8080/admin/`. With the chart's `admin.separatePort`, use `svc/registry-mirrors-relay-admin 9090`.
- **SSH forwarding**: `ssh -L 8080:127.0.0.1:8080 server`, then open `http://127.0.0.1:8080/admin/` locally.
- **Docker**: the image's own configuration puts the console on a separate 9090; publish it locally only with `-p 127.0.0.1:9090:9090` and open `http://127.0.0.1:9090/admin/`.
- **LAN**: open it by IP address, for example `http://192.168.1.10:8080/admin/`.
- **Through an https reverse proxy**: set `admin.public_url`, `client_ip.trusted_proxies` and `admin.proxied_allow` (allowed client addresses), see [Deployment](deploy.en.md#admin-console-admin-api-and-metrics).

In the browser the Host must be `public_url`'s host, `localhost` or an IP address; other host names get 421 (against DNS rebinding).

## Security

The console denies by default and checks at every layer:

- **Content Security Policy**: `default-src 'none'; script-src 'self'; style-src 'self'; …; require-trusted-types-for 'script'; trusted-types 'none'`. Pages have no inline scripts, inline styles or event attributes; the one script writes text only, never markup. Also `X-Frame-Options: DENY`, `nosniff`, `Referrer-Policy: same-origin`, `Cross-Origin-Opener-Policy` and more.
- **Sessions**: a 32-byte random ID, of which the process keeps only the SHA-256; sessions end after 30 minutes without activity or 12 hours after sign-in, and a restart signs everyone out. With an https `public_url` the cookie is `__Host-relay-admin` (Secure, HttpOnly, SameSite=Lax); otherwise `relay-admin` (under `/admin` only, HttpOnly, SameSite=Strict).
- **Who can connect**: only direct connections from loopback and private addresses, see [Remote access](#remote-access); everyone else sees 404.
- **Cross-site requests**: every POST needs an `Origin` equal to `public_url` or the localhost or IP address in use, `Sec-Fetch-Site: cross-site` is refused, and the session's CSRF token is required; success answers with a 303 redirect.
- **Sign-in limits**: passwords are checked one at a time (bcrypt); at most 10 failures per client and 100 overall in 10 minutes, after which even the right password is refused (429) until the window passes. Failures and blocks are audited once per window.
- **Sensitive actions**: changes to administrators, API tokens and sessions, and changing your own password, need your password within the last 10 minutes; otherwise you confirm it first. Changing an administrator's password, role or status ends all their sessions.
- **Limited capabilities**: the console never touches docker.sock, runs no commands, and cannot send requests to arbitrary addresses (it only probes configured upstreams). All writes go through a fixed set of 26 POST routes, pinned one by one by a test.

The JSON API also accepts the console session: reads as they are, writes with `X-CSRF-Token` and the same origin checks.
