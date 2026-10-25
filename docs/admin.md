# Admin console

[简体中文](zh/admin.md) | English

The admin console and the [admin API](api.md) share the pull port (8080) by default, under `/admin/`; with `listen.admin` they move to that separate port. They are open to every address by default and protected by sign-in, see [Opening the console](#opening-the-console). Every page is rendered on the server, nothing is loaded from third parties, and everything works without JavaScript. The styles come from the OpenLinkOS design system [`@openlinkos/ui`](https://www.npmjs.com/package/@openlinkos/ui), embedded in the binary. Chinese and English, light and dark: the language follows the browser (English when it asks for neither) until you choose one in the account menu at the foot of the sidebar (or at the foot of the sign-in page), which is kept in the `relay-lang` cookie that the usage page shares. The colour scheme (Auto, Light, Dark) is switched above it. With `site.brand`, the console takes the brand's colours and marks, like the usage page.

## Pages

| Group | Page | Content |
| --- | --- | --- |
| | Overview | Requests per minute, traffic, layers downloading, 5xx share, manifest cache hits, rate-limited requests, Docker Hub quota left (refreshed every 15 seconds); 24-hour charts of traffic and requests; service status: overall state, each upstream's hourly availability over 24 hours and this site's request success rate (as on the usage page); top images and clients |
| Service | Upstreams | Results of the `/v2/` probe every minute (not counted as pulls) and availability over 24 hours, circuit breaker, pull quota; redirect hosts outside the allowlist, with one-click allow; check now. Each upstream's settings (address, aliases, redirect allowlists, token services) have their own page, where new upstreams are added too |
| | Entrances | The domains clients pull from. Each entrance has its own page: the client configuration for it (Docker `daemon.json`, containerd `hosts.toml`, k3s `registries.yaml`, Podman `registries.conf` and `docker login` when it requires sign-in, generated for its host name) and its settings (domains, fixed or default upstream, sign-in, usage page) |
| | Usage page | What a browser sees at an entrance's domain: site name, operator, brand and footer links (overriding `site` in the configuration), whether it shows the service status, and which entrances show it |
| Access | Client accounts | Accounts for entrances that require sign-in: find, filter, and act on each from its row; create one on its own page, which then shows how to sign in (and a generated password, once); each account's page shows its latest requests, its limits and note, a password reset, and deleting it |
| | Rules | "Can this image be pulled?"; temporary bans; IP lists; image rules with refusal counts |
| | Rate limits | Current concurrency; the limits, changed in place; recently limited clients (one-click ban); accounts with their own limits |
| Records | Requests | The last 5,000 requests, filtered by client, image, upstream and status |
| | Abuse | The same client downloading the same layer in full again and again; top clients by traffic (1 hour, 24 hours, 7 days) |
| | Audit log | Every change through the console or the API, and every sign-in and sign-out |
| System | Configuration | Every change made in the console, with its state, where it is edited, and reset; the manifest cache and upstream connections; export in configuration file form; the configuration in effect (secrets hidden) |
| | Admins and tokens | Administrators (admin and viewer roles), API tokens, current sessions |
| | System | Version, uptime, memory, goroutines, operational data; configuration file and last reload, with a reload button; the management entrance (shared or separate port, allowed addresses, metrics, pprof); log settings |

Your own password and preferences are under My account, in the account menu. Accounts, rules and tokens from the configuration file are shown read-only. Each setting is changed on the page that uses it, see [Changing the configuration in the console](#changing-the-configuration-in-the-console). With read-only operational data (`admin.store.driver: none`), every change goes into the configuration file.

## The first administrator

There is no default password, and no setup page where whoever opens the site first chooses one: on a freshly started service such a page would hand the console to the first visitor.

**On the first start**, when the operational database has no administrator and the configuration names none, the service creates `admin` (or the name in `RELAY_ADMIN_USERNAME`) with a random password and writes it to the log once:

```bash
docker logs relay 2>&1 | grep 'generated password'
kubectl logs sts/registry-mirrors-relay | grep 'generated password'
journalctl -u registry-mirrors-relay | grep 'generated password'
```

Sign in with it and choose your own; until then only My account is open, so the password in the log is worthless afterwards.

**The command line** sets a password at any time, also while the service runs:

```bash
docker exec -it relay registry-mirrors-relay admin password                            # Docker
docker compose exec relay registry-mirrors-relay admin password                        # Compose
kubectl exec -it sts/registry-mirrors-relay -- registry-mirrors-relay admin password   # Kubernetes
sudo registry-mirrors-relay admin password                                             # systemd
```

- Without a name it sets the password of the only administrator, or creates `admin` when there is none; with several administrators, name one: `admin password alice`.
- It asks for the password twice on a terminal, or reads one line from standard input (`docker exec -i relay registry-mirrors-relay admin password < password.txt`); the password never appears in the arguments. `--generate` makes a random password and prints it once.
- The change applies at once, re-enables the account and signs that administrator out of the console. A password set from the command line need not be changed at the first sign-in.
- `admin create bob -role viewer` adds an administrator, and `admin list` lists them.
- While the service runs, the command reaches it through the local admin socket `admin.sock` next to the operational database (see [Security](#security)); while the service is stopped, it opens the database itself.

**Choosing the first password before the first start**:

- **A password file** (fits containers): write the password into a file only the service can read and point `RELAY_ADMIN_PASSWORD_FILE` at it (user name `admin`, or `RELAY_ADMIN_USERNAME`). Docker Compose `secrets`, Kubernetes Secrets and systemd `LoadCredential` all work this way; see [Deployment](deploy.md). There is deliberately no variable that takes the password itself, since it would show up in `docker inspect`, the process environment and orchestration files.

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

These two apply only while the operational database has no administrators, and **the administrator created this way must change the password at the first sign-in**: until then only My account is open and the API refuses the session; the new password must differ from the old one. A password left in the configuration, an environment variable, a password file or a shell history therefore works once at most. Afterwards the service log reminds you to delete `initial_admin` and the password file. Likewise, an account created or reset by another administrator must change its password at the first sign-in; passwords you set yourself or through the command line need no change.

With read-only operational data (`admin.store.driver: none`, several Kubernetes replicas) neither the console nor the command line can change passwords, so `initial_admin` is the only administrator and its password stays valid; change it in the configuration and restart the pods. Use a long random password.

If you forget your password, another administrator can reset it under Admins and tokens, or run `registry-mirrors-relay admin password` as above.

## Opening the console

The console is open to every address by default (`admin.public: true`); open `/admin/` on the pull host name or IP address:

- **Through a CDN, reverse proxy or Ingress**: `https://mirror.example.com/admin/`. When the browser side is https, the session cookie is `Secure`.
- **Directly**: `http://server:8080/admin/`, or `https://<domain>/admin/` with `tls.mode: acme`.
- **Kubernetes**: `kubectl port-forward svc/registry-mirrors-relay 8080:80` works too, then open `http://127.0.0.1:8080/admin/`; with the chart's `admin.separatePort`, use `svc/registry-mirrors-relay-admin 9090`. With several replicas behind an Ingress, turn on session affinity, see [Deployment](deploy.md#kubernetes).

Sign-in protects it (see [Security](#security) below), so put it behind HTTPS when it faces the internet: without HTTPS or a configured reverse proxy, the startup log warns that passwords travel in clear text.

**Keeping it to the local machine and the LAN**: set `admin.public: false`. The console then answers only direct connections from loopback and private addresses (`admin.allow`, `loopback` and `private_ranges` by default); requests through a CDN or reverse proxy get 404, as if the path didn't exist, and in the browser the Host must be `public_url`'s host, `localhost` or an IP address (others get 421, against DNS rebinding). Reach it with `kubectl port-forward` or `ssh -L 8080:127.0.0.1:8080 server`; to let fixed addresses in through an https reverse proxy, set `admin.public_url`, `client_ip.trusted_proxies` and `admin.proxied_allow`. The rule, and where it holds, are in [Deployment](deploy.md#admin-console-admin-api-and-metrics).

## Changing the configuration in the console

These settings can be changed in the console, each on the page that uses it (and through [`/config/overrides`](api.md#endpoints) in the API):

| Setting | Page | What can change | How it combines |
| --- | --- | --- | --- |
| Entrances (`entrances`) | Entrances, one page per entrance | Name, domains, fixed or default upstream, sign-in, usage page | The whole list is replaced |
| Upstreams (`upstreams.<name>`) | Upstreams, one page per upstream | `registry`, `aliases`, `library_prefix`, redirect allowlists, `token_realm_hosts`, `unlisted_redirect`, disabled; new upstreams can be added | Field by field |
| Rate limits (`limits`) | Rate limits | Every field except `tracked_clients` | Field by field |
| Manifest cache (`cache`) | Configuration | Every field | Field by field |
| Upstream connections (`proxy`) | Configuration | Idle connections, redirects, passing `Ratelimit-*` headers, CDN caching, the circuit breaker, the probe interval | Field by field |
| Usage page (`site.show_status`) | Usage page | Whether the service status is shown | |

The usage page's name, operator, brand and links are saved in the console as a whole and replace `site` in the configuration file until reset there. The Configuration page lists every change made in the console, with a link to where it is edited.

Changes are stored in the operational database and never written back to the configuration file. When the configuration is parsed they are merged into the file first and then validated like the file, so defaults, validation rules and errors are the same as when you edit the file. Only fields that differ from the file are stored; setting a field back to the file's value, or pressing Reset, removes the change. Changes take effect at once, without a reload. With ACME, a certificate for a new entrance domain is requested the first time it is visited (at most 100 domains).

**Only in the configuration file**: listeners, TLS, the management entrance (`admin.*`), client IPs, logs, metrics, timeouts, operational data storage, `access.users_file`, and an upstream's account (`credentials`), `tls_ca_file` and `allow_private_network`, as well as `buffer_size`, `user_agent` and `blob_http2` under `proxy`. So the console cannot change who may use it, and upstreams added here cannot reach private addresses. For an upstream with an account, `registry` and `token_realm_hosts` can only be changed in the file too, so the account cannot be sent to another host.

**The configuration file is the baseline.**

- A save must pass validation together with the other changes in effect and the file; otherwise nothing is saved and nothing changes. An upstream used by an entrance cannot be removed, for example.
- On a restart or a reload of the file, changes that conflict with it are suspended (with a warning on the overview and an audit entry) and the rest apply; they resume once the conflict is resolved.
- If the file later changes a field that was changed here, the state shows "File changed since" and the console's value still applies; reset or save again once you have checked.
- `registry-mirrors-relay check` checks only the file, without the console's changes. The configuration in effect is on the Configuration page and at `GET /config`.

**GitOps**: when the configuration lives in Git, changes made here are not visible there. Treat them as temporary: export them on the Configuration page (only the changes, or the whole merged file), commit them to Git, and reset them here once deployed. To make Git the only source, set `admin.config_edit: false`: changes made here then never apply and can only be reset. The metric `relay_config_overrides{key,state}` lists every change, for alerting.

Saving and resetting need your password within the last 10 minutes and are audited (`config.override.set`, `config.override.reset`; suspensions and resumptions are `config.override.suspended` and `config.override.resumed`). Viewers can only look.

## Security

The console denies by default and checks at every layer:

- **Content Security Policy**: `default-src 'none'; script-src 'self'; style-src 'self'; …; require-trusted-types-for 'script'; trusted-types 'none'`. Pages have no inline scripts, inline styles or event attributes; the one script writes text only, never markup. Also `X-Frame-Options: DENY`, `nosniff`, `Referrer-Policy: same-origin`, `Cross-Origin-Opener-Policy` and more.
- **Sessions**: a 32-byte random ID, of which the process keeps only the SHA-256; sessions end after 30 minutes without activity or 12 hours after sign-in, and a restart signs everyone out. With an https `public_url` the cookie is `__Host-relay-admin` (Secure, HttpOnly, SameSite=Lax); otherwise `relay-admin` (under `/admin` only, HttpOnly, SameSite=Strict).
- **Who can connect**: by default every address reaches the sign-in page, protected by the measures below; with `admin.public: false` only direct connections from loopback and private addresses do and everyone else sees 404, see [Opening the console](#opening-the-console). Metrics and pprof have no sign-in and always take direct connections only.
- **Cross-site requests**: every POST needs an `Origin` equal to `public_url` or the localhost or IP address in use, `Sec-Fetch-Site: cross-site` is refused, and the session's CSRF token is required; success answers with a 303 redirect.
- **Sign-in limits**: passwords are checked one at a time (bcrypt); at most 10 failures per client and 100 overall in 10 minutes, after which even the right password is refused (429) until the window passes. Failures and blocks are audited once per window.
- **Sensitive actions**: changes to administrators, API tokens, sessions and the configuration, and changing your own password, need your password within the last 10 minutes; otherwise you confirm it first. Changing an administrator's password, role or status ends all their sessions.
- **The local admin socket**: `admin.sock` next to the operational database, mode 0600 in a directory only the service's user can enter; on Linux and macOS the service also refuses connections from processes that run as another user (root excepted). It serves only what the `admin` subcommands need: listing administrators, creating one and setting a password, audited as `cli:<user>`. It is opened only with writable operational data.
- **Limited capabilities**: the console never touches docker.sock, runs no commands, and sends requests only to configured upstreams; upstreams added in the console can reach public addresses only. All writes go through a fixed set of 30 POST routes, pinned one by one by a test.

The JSON API also accepts the console session: reads as they are, writes with `X-CSRF-Token` and the same origin checks.
