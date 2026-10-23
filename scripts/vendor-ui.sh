#!/usr/bin/env bash
# 把 @openlinkos/ui 的样式、字体和图标放进 internal/admin/assets/vendor/，管理后台用 go:embed 打进程序。
# 只在升级 UI 版本时运行，结果提交进仓库：Go 构建不需要 Node。
# 用法：scripts/vendor-ui.sh [版本]。需要 curl、tar、openssl。
set -euo pipefail

VERSION="${1:-0.2.1}"
DEST="${DEST:-internal/admin/assets/vendor}"
META="https://registry.npmjs.org/@openlinkos/ui/$VERSION"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

meta="$(curl -fsSL "$META")"
# 版本元数据里 dist.tarball、dist.integrity 各只出现一次
field() { grep -o "\"$1\":\"[^\"]*\"" <<<"$meta" | head -n 1 | cut -d'"' -f4; }
tarball="$(field tarball)"
integrity="$(field integrity)"
[ "${integrity%%-*}" = sha512 ] || { echo "不认识的 integrity：$integrity" >&2; exit 1; }

curl -fsSL "$tarball" -o "$work/ui.tgz"
got="sha512-$(openssl dgst -sha512 -binary "$work/ui.tgz" | openssl base64 -A)"
[ "$got" = "$integrity" ] || { echo "校验失败：期望 $integrity，实际 $got" >&2; exit 1; }

tar -xzf "$work/ui.tgz" -C "$work"
rm -rf "$DEST"
mkdir -p "$DEST/fonts"
cp "$work/package/dist/ui.css" "$work/package/dist/icons.json" "$DEST/"
cp "$work/package/dist/fonts/"*.woff2 "$DEST/fonts/"
cp "$work/package/LICENSE" "$DEST/LICENSE-openlinkos-ui.txt"
for f in "$work"/package/dist/LICENSE-* "$work"/package/dist/fonts/LICENSE-*; do
  [ -e "$f" ] && cp "$f" "$DEST/"
done
printf '%s\n' "@openlinkos/ui $VERSION" "$integrity" > "$DEST/VERSION"
echo "已更新 $DEST 到 @openlinkos/ui $VERSION"
