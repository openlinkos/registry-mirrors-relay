#!/usr/bin/env bash
# Puts the @openlinkos/ui styles, fonts and icons into internal/admin/assets/vendor/; the admin console
# embeds them into the program with go:embed.
# Run it only when upgrading the UI version and commit the result to the repository: Go builds do not need Node.
# Usage: scripts/vendor-ui.sh [version]. Requires curl, tar and openssl.
set -euo pipefail

VERSION="${1:-0.2.1}"
DEST="${DEST:-internal/admin/assets/vendor}"
META="https://registry.npmjs.org/@openlinkos/ui/$VERSION"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

meta="$(curl -fsSL "$META")"
# dist.tarball and dist.integrity each appear only once in the version metadata
field() { grep -o "\"$1\":\"[^\"]*\"" <<<"$meta" | head -n 1 | cut -d'"' -f4; }
tarball="$(field tarball)"
integrity="$(field integrity)"
[ "${integrity%%-*}" = sha512 ] || { echo "Unknown integrity: $integrity" >&2; exit 1; }

curl -fsSL "$tarball" -o "$work/ui.tgz"
got="sha512-$(openssl dgst -sha512 -binary "$work/ui.tgz" | openssl base64 -A)"
[ "$got" = "$integrity" ] || { echo "Checksum mismatch: expected $integrity, got $got" >&2; exit 1; }

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
echo "Updated $DEST to @openlinkos/ui $VERSION"
