#!/usr/bin/env bash
# 从源码构建镜像：先在本机为各个平台编译二进制，放进构建上下文的 <平台>/ 目录，再用仓库里的 Dockerfile 打包。
# 和发布流水线（GoReleaser）用的是同一个 Dockerfile。需要 go 和 docker（buildx）。
#
# 用法：scripts/build-image.sh [镜像名，默认 registry-mirrors-relay:dev] [平台，默认本机的 linux 平台]
#   scripts/build-image.sh registry-mirrors-relay:dev
#   PUSH=1 scripts/build-image.sh registry.example.com/relay:dev linux/amd64,linux/arm64   # 多平台要推送到仓库
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
echo "已构建 $IMAGE（$PLATFORMS）"
