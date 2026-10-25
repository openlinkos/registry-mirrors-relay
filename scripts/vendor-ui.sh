#!/usr/bin/env bash
# Puts the design system into internal/ui/vendor/, which the program embeds with go:embed: the styles,
# fonts and icons of @openlinkos/ui, and each brand's marks (favicon and logos) from
# @openlinkos/vitepress-theme. The two packages share one version.
# Run it only when upgrading the design system and commit the result: Go builds do not need Node.
# Usage: scripts/vendor-ui.sh [version]. Requires curl, tar and openssl.
# LOCAL=<path to an openlinkos/design checkout, built with pnpm build> copies from there instead of npm,
# for trying out a version before it is published; the VERSION file then says so.
set -euo pipefail

VERSION="${1:-0.5.0}"
DEST="${DEST:-internal/ui/vendor}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# fetch <package> <dir>: downloads @openlinkos/<package>@VERSION, checks its integrity, and unpacks it
# into <dir> (as npm packs it, under package/). Prints the integrity.
fetch() {
  local meta tarball integrity got
  meta="$(curl -fsSL "https://registry.npmjs.org/@openlinkos/$1/$VERSION")"
  # dist.tarball and dist.integrity each appear only once in the version metadata
  tarball="$(grep -o '"tarball":"[^"]*"' <<<"$meta" | head -n 1 | cut -d'"' -f4)"
  integrity="$(grep -o '"integrity":"[^"]*"' <<<"$meta" | head -n 1 | cut -d'"' -f4)"
  [ "${integrity%%-*}" = sha512 ] || { echo "Unknown integrity for $1: $integrity" >&2; exit 1; }
  curl -fsSL "$tarball" -o "$work/$1.tgz"
  got="sha512-$(openssl dgst -sha512 -binary "$work/$1.tgz" | openssl base64 -A)"
  [ "$got" = "$integrity" ] || { echo "Checksum mismatch for $1: expected $integrity, got $got" >&2; exit 1; }
  mkdir -p "$2"
  tar -xzf "$work/$1.tgz" -C "$2"
  echo "$integrity"
}

if [ -n "${LOCAL:-}" ]; then
  mkdir -p "$work/ui/package" "$work/theme/package"
  cp -R "$LOCAL/packages/ui/dist" "$LOCAL/packages/ui/LICENSE" "$work/ui/package/"
  cp -R "$LOCAL/packages/vitepress-theme/dist" "$work/theme/package/"
  ui_id="local build of $(git -C "$LOCAL" describe --tags --always --dirty)"
  theme_id="$ui_id"
else
  ui_id="$(fetch ui "$work/ui")"
  theme_id="$(fetch vitepress-theme "$work/theme")"
fi

ui="$work/ui/package"
brands="$work/theme/package/dist/brand"
rm -rf "$DEST"
mkdir -p "$DEST/fonts" "$DEST/brands"
cp "$ui/dist/ui.css" "$ui/dist/icons.json" "$DEST/"
cp "$ui/dist/fonts/"*.woff2 "$DEST/fonts/"
cp "$ui/LICENSE" "$DEST/LICENSE-openlinkos-ui.txt"
for f in "$ui"/dist/LICENSE-* "$ui"/dist/fonts/LICENSE-*; do
  [ -e "$f" ] && cp "$f" "$DEST/"
done
cp "$brands/brands.json" "$DEST/brands/"
for dir in "$brands"/out/*/; do
  id="$(basename "$dir")"
  mkdir -p "$DEST/brands/$id"
  cp "$dir/web/favicon.svg" "$dir/web/logo.svg" "$dir/web/logo-dark.svg" "$DEST/brands/$id/"
done
cat >"$DEST/brands/NOTICE.txt" <<'EOF'
The OpenLinkOS and LinkOS names and marks in this directory are not covered by the MIT licence of the code.
They are for OpenLinkOS and LinkOS services; see the brand guidelines at https://openlinkos.com/brand.
EOF
printf '%s\n' "@openlinkos/ui $VERSION" "$ui_id" "@openlinkos/vitepress-theme $VERSION" "$theme_id" >"$DEST/VERSION"
echo "Updated $DEST to @openlinkos/ui and @openlinkos/vitepress-theme $VERSION"
