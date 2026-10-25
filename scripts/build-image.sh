#!/usr/bin/env bash
# Builds the image from source: compiles the binary for each platform on this machine, puts it in the
# build context's <platform>/ directory, then packages it with the repository's Dockerfile.
# It uses the same Dockerfile as the release pipeline (GoReleaser). Requires go and docker (buildx).
#
# Usage: scripts/build-image.sh [image name, default registry-mirrors-relay:dev]
#                               [platforms, default this machine's linux platform]
#   scripts/build-image.sh registry-mirrors-relay:dev
#   PUSH=1 scripts/build-image.sh registry.example.com/relay:dev linux/amd64,linux/arm64   # multi-platform builds must be pushed to a registry
set -euo pipefail

IMAGE="${1:-registry-mirrors-relay:dev}"
PLATFORMS="${2:-linux/$(go env GOARCH)}"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"

ctx="$(mktemp -d)"
trap 'rm -rf "$ctx"' EXIT

IFS=, read -ra list <<<"$PLATFORMS"
for p in "${list[@]}"; do
  IFS=/ read -r os arch variant <<<"$p"
  mkdir -p "$ctx/$p"
  GOOS="$os" GOARCH="$arch" GOARM="${variant#v}" CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w -X github.com/openlinkos/registry-mirrors-relay/internal/version.Version=$VERSION" \
    -o "$ctx/$p/registry-mirrors-relay" ./cmd/registry-mirrors-relay
done
mkdir -p "$ctx/deploy/docker"
cp Dockerfile "$ctx/"
cp deploy/docker/config.yaml "$ctx/deploy/docker/"

if [ "${PUSH:-}" = 1 ]; then
  docker buildx build --platform "$PLATFORMS" -t "$IMAGE" --push "$ctx"
else
  docker buildx build --platform "$PLATFORMS" -t "$IMAGE" --load "$ctx"
fi
echo "Built $IMAGE ($PLATFORMS)"
