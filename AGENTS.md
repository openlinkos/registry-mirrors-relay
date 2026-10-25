# AGENTS.md

Instructions for AI coding agents working in this repository. `CLAUDE.md` is a symlink to this file; edit `AGENTS.md` only.

## Project

registry-mirrors-relay is a container registry mirror that only relays requests and never stores image data: entrances by host name or path prefix, upstream tokens and redirects handled on the server, a manifest cache, access control, rate limits, a usage page, an admin console and a JSON API. Go, one binary, with the console's templates and assets embedded.

## Commands

```bash
go test ./...                  # unit tests; CI also runs -race, Windows, and kind end-to-end tests
golangci-lint run ./...        # also with GOOS=linux: some files are Unix-only
scripts/chart-check.sh         # Helm chart: lint, kubeconform, fail cases, helm-docs, kustomize parity
scripts/isolated-test.sh BIN   # pulls through the relay from a network with no route to the upstreams (needs Docker)
scripts/smoke.sh               # pulls every upstream through a running relay
scripts/vendor-ui.sh VERSION   # vendor @openlinkos/ui into internal/admin/assets/vendor
```

## Rules

- **Use English** for code, comments, logs, CLI output, scripts and commit messages. British spelling, as in the docs.
- **Messages people read go through `internal/i18n`**: write the English format string with `i18n.Errorf`, `i18n.Coded` or `i18n.M`, and add the Chinese to `internal/i18n/zh.go`, keyed by that string. `TestCatalogue` fails on a missing or unused translation, and `TestNoChinese` on Chinese anywhere outside the catalogues and `docs/zh/`. Console text uses the keyed catalogues `internal/admin/messages_en.go` (the source) and `messages_zh.go`; `TestMessagesComplete` checks they match. Errors sent to registry clients, logs and CLI output stay English.
- **API errors** are `{"error", "code"}`; codes are stable. Ops errors take their code and message from `internal/ops/messages.go`.
- **Docs**: English in `README.md` and `docs/`, Chinese in `README.zh-CN.md` and `docs/zh/`, section for section. Change both together.
- **The console**: server-rendered, strict CSP (no inline scripts, styles or event attributes), works without JavaScript. Every write is a POST route pinned by `TestWriteRoutesPinned`; sensitive ones require re-authentication. Styles come from the design system `@openlinkos/ui` and use its tokens only.
- **Security first**: management paths are guarded by `internal/mgmt`; metrics and pprof answer direct connections only. Do not weaken a check to make a test pass.
- **Public repository**: name machines by location and network, never by internal host names or real client addresses.
- **Ask before adding dependencies**; keep `go.mod` tidy.

## Git

- Conventional Commits in English: `<type>(<scope>): <summary>`, types `feat | fix | docs | refactor | test | perf | build | ci | chore`.
- Run the tests and the linter before committing. Do not add attribution trailers such as `Co-Authored-By`.
- Releases: tag `vX.Y.Z-rc.N` first (the release workflow builds, signs and verifies), test it, then tag `vX.Y.Z`. Bump `charts/registry-mirrors-relay/Chart.yaml` and the version references in the docs together.
