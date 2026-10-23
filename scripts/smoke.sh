#!/usr/bin/env bash
# 冒烟测试：检查每个上游都能正确拉取、镜像层经由本服务下载，并且入口只放行拉取。需要 docker、curl、jq。两种用法：
#   按端口：每个上游一个端口的入口（见 deploy/test/config.yaml），在本机运行
#     scripts/smoke.sh [主机，默认 localhost]；端口可用 PORT_DOCKER 等环境变量覆盖。
#     再给 ALLINONE_PORT（单域名入口的端口），另外检查路径前缀、ns 和根路径三种写法。
#   按域名：经 HTTPS 访问 docker、gcr、ghcr、k8s、quay.<域名>
#     SMOKE_DOMAIN=example.com scripts/smoke.sh
# 运行这个脚本的机器能直连上游，所以测不出"客户端被引去直连上游"的情况；那要用 scripts/isolated-test.sh。
set -euo pipefail

HOST="${1:-localhost}"
fail=0

ok() { printf '  通过  %s\n' "$1"; }
bad() { printf '  失败  %s\n' "$1"; fail=1; }
status() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

# 每个上游的入口地址和 docker pull 用的前缀，顺序：Docker Hub、gcr.io、ghcr.io、registry.k8s.io、quay.io
BASES=()
NAMES=()
if [ -n "${SMOKE_DOMAIN:-}" ]; then
  for sub in docker gcr ghcr k8s quay; do
    BASES+=("https://$sub.$SMOKE_DOMAIN")
    NAMES+=("$sub.$SMOKE_DOMAIN")
  done
else
  for port in "${PORT_DOCKER:-5001}" "${PORT_GCR:-5002}" "${PORT_GHCR:-5003}" "${PORT_K8S:-5004}" "${PORT_QUAY:-5005}"; do
    BASES+=("http://$HOST:$port")
    NAMES+=("$HOST:$port")
  done
fi

wait_ready() {
  for _ in $(seq 1 60); do
    [ "$(status "$1/healthz")" = 200 ] && return 0
    sleep 1
  done
  return 1
}

# 上游序号、经镜像站拉取的名称、直接从上游拉取的名称
CASES=(
  "0 library/hello-world:latest docker.io/library/hello-world:latest"
  "1 distroless/static:nonroot gcr.io/distroless/static:nonroot"
  "2 containerd/busybox:1.36 ghcr.io/containerd/busybox:1.36"
  "3 pause:3.10 registry.k8s.io/pause:3.10"
  "4 coreos/etcd:v3.5.0 quay.io/coreos/etcd:v3.5.0"
)

ACCEPT='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'

# 取镜像（linux/amd64）第一个层的状态码：不跟随重定向，200 或 206 说明由本服务转发。
# 本服务在服务端取令牌，客户端不需要令牌。
blob_status() {
  local base=$1 repo=$2 ref=$3 manifest digest
  manifest="$(curl -s -H "Accept: $ACCEPT" "$base/v2/$repo/manifests/$ref")"
  digest="$(jq -r '.manifests[]? | select(.platform.os == "linux" and .platform.architecture == "amd64") | .digest' <<<"$manifest" | head -n 1)"
  if [ -n "$digest" ]; then
    manifest="$(curl -s -H "Accept: $ACCEPT" "$base/v2/$repo/manifests/$digest")"
  fi
  digest="$(jq -r '.layers[0].digest // empty' <<<"$manifest")"
  [ -n "$digest" ] || { echo "无清单"; return; }
  curl -s -o /dev/null -r 0-1023 -w '%{http_code}' "$base/v2/$repo/blobs/$digest"
}

pull_digest() { docker pull "$1" | sed -n 's/^Digest: //p' || true; }

echo "拉取并比对摘要"
for c in "${CASES[@]}"; do
  read -r i name upstream <<<"$c"
  if ! wait_ready "${BASES[i]}"; then bad "${NAMES[i]}：没有就绪"; continue; fi
  mirrored="$(pull_digest "${NAMES[i]}/$name")"
  direct="$(pull_digest "$upstream")"
  if [ -n "$mirrored" ] && [ "$mirrored" = "$direct" ]; then
    ok "${NAMES[i]} $name $mirrored"
  else
    bad "${NAMES[i]} $name：镜像站 '${mirrored:-无}'，上游 '${direct:-无}'"
  fi
done

echo "镜像层经由本服务下载（上游的 CDN 地址在白名单内）"
for c in "${CASES[@]}"; do
  read -r i name _ <<<"$c"
  s="$(blob_status "${BASES[i]}" "${name%:*}" "${name##*:}")"
  case "$s" in
    200 | 206) ok "${NAMES[i]} ${name%:*} 返回 $s" ;;
    *) bad "${NAMES[i]} ${name%:*} 返回 $s：可能是上游换了 CDN 地址，请看访问日志的 redirect1..3 并补充白名单" ;;
  esac
done

echo "不发认证质询，不透露源站出口 IP"
for i in "${!BASES[@]}"; do
  hdr="$(curl -s -o /dev/null -D - "${BASES[i]}/v2/" | tr -d '\r')"
  if grep -qi '^www-authenticate' <<<"$hdr"; then
    bad "${NAMES[i]} /v2/ 带了认证质询"
  else
    ok "${NAMES[i]} /v2/ 不要求认证"
  fi
done
read -r _ name _ <<<"${CASES[0]}"
hdr="$(curl -s -o /dev/null -D - -H "Accept: $ACCEPT" "${BASES[0]}/v2/${name%:*}/manifests/${name##*:}" | tr -d '\r')"
if grep -qi -E '^(docker-ratelimit-source|set-cookie)' <<<"$hdr"; then
  bad "${NAMES[0]} 响应带了 Docker-Ratelimit-Source 或 Set-Cookie"
else
  ok "${NAMES[0]} 响应不带 Docker-Ratelimit-Source、Set-Cookie"
fi

echo "入口规则"
for i in "${!BASES[@]}"; do
  base="${BASES[i]}" n="${NAMES[i]}"
  s="$(status -X POST "$base/v2/smoke/blobs/uploads/")"
  [ "$s" = 405 ] && ok "$n POST 返回 405" || bad "$n POST 返回 $s，应为 405"
  s="$(status -X DELETE "$base/v2/smoke/manifests/latest")"
  [ "$s" = 405 ] && ok "$n DELETE 返回 405" || bad "$n DELETE 返回 $s，应为 405"
  s="$(status "$base/v2/_catalog")"
  [ "$s" = 404 ] && ok "$n /v2/_catalog 返回 404" || bad "$n /v2/_catalog 返回 $s，应为 404"
done

echo "上游隔离"
for i in 1 2 3 4; do
  s="$(status -H "Accept: $ACCEPT" "${BASES[i]}/v2/library/hello-world/manifests/latest")"
  case "$s" in
    2*) bad "${NAMES[i]} 返回了 Docker Hub 的 library/hello-world（$s）" ;;
    *) ok "${NAMES[i]} 请求 library/hello-world 返回 $s" ;;
  esac
  s="$(status -H "Accept: $ACCEPT" "${BASES[i]}/v2/library/hello-world/manifests/latest?ns=docker.io")"
  [ "$s" = 404 ] && ok "${NAMES[i]} ns=docker.io 返回 404" || bad "${NAMES[i]} ns=docker.io 返回 $s，应为 404"
done

if [ -n "${ALLINONE_PORT:-}" ]; then
  echo "单域名入口（$HOST:$ALLINONE_PORT）"
  one="$HOST:$ALLINONE_PORT"
  wait_ready "http://$one" || bad "$one 没有就绪"
  for c in "${CASES[@]}"; do
    read -r _ _ upstream <<<"$c"
    mirrored="$(pull_digest "$one/$upstream")"
    direct="$(pull_digest "$upstream")"
    if [ -n "$mirrored" ] && [ "$mirrored" = "$direct" ]; then ok "前缀 $one/$upstream"; else bad "前缀 $one/$upstream：'${mirrored:-无}' vs '${direct:-无}'"; fi
  done
  s="$(status -I -H "Accept: $ACCEPT" "http://$one/v2/containerd/busybox/manifests/1.36?ns=ghcr.io")"
  [ "$s" = 200 ] && ok "ns=ghcr.io 返回 200" || bad "ns=ghcr.io 返回 $s，应为 200"
  s="$(status -I -H "Accept: $ACCEPT" "http://$one/v2/hello-world/manifests/latest")"
  [ "$s" = 200 ] && ok "根路径（默认 Docker Hub，补 library/）返回 200" || bad "根路径返回 $s，应为 200"
  s="$(status -I -H "Accept: $ACCEPT" "http://$one/v2/example.org/x/manifests/latest")"
  [ "$s" = 404 ] && ok "没有配置的前缀返回 404" || bad "没有配置的前缀返回 $s，应为 404"
fi

echo "用法页与探活"
for i in "${!BASES[@]}"; do
  ct="$(curl -s -o /dev/null -w '%{http_code} %{content_type}' "${BASES[i]}/")"
  case "$ct" in
    "200 text/html"*) ok "${NAMES[i]} / 返回用法页" ;;
    *) bad "${NAMES[i]} / 返回 '$ct'，应为 200 text/html" ;;
  esac
  s="$(status "${BASES[i]}/healthz")"
  [ "$s" = 200 ] && ok "${NAMES[i]} /healthz 返回 200" || bad "${NAMES[i]} /healthz 返回 $s，应为 200"
done
s="$(status "${BASES[0]}/robots.txt")"
[ "$s" = 200 ] && ok "/robots.txt 返回 200" || bad "/robots.txt 返回 $s，应为 200"

if [ "$fail" = 0 ]; then echo "全部通过"; else echo "有检查失败"; fi
exit "$fail"
