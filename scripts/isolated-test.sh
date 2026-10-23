#!/usr/bin/env bash
# 断网测试：客户端放进不能上外网的 Docker 网络（--internal），只能连到 relay。拉取成功就说明没有哪一步
# 被引去直连上游（认证、重定向都由 relay 在服务端处理）。拉到的 digest 和直接从上游取的比对。
#
# 覆盖的客户端：经典存储和 containerd 存储的 Docker、skopeo、crane、ctr。
# 覆盖的写法：每个上游一个端口的入口；单域名入口的根路径（daemon.json 的 registry-mirrors）、路径前缀、ns（ctr + hosts.toml）。
#
# 用法：scripts/isolated-test.sh <relay 的 linux 二进制>
# 需要 docker（能跑 --privileged 容器）和 jq。会拉几个测试镜像，测完删除所有容器、网络、卷和新拉的镜像。
# 注意：会消耗这台机器出口 IP 的 Docker Hub 匿名额度。
set -euo pipefail

BIN="$(realpath "$1")"
CFG="$(realpath "$(dirname "$0")/../deploy/test/config.yaml")"
P=relaytest
NET="$P-int"
RELAY="$P-relay"
SK=quay.io/skopeo/stable:v1.20.0
CR=gcr.io/go-containerregistry/crane:debug
DIND=docker:29-dind
BASE=gcr.io/distroless/static-debian12:nonroot
fail=0

ok() { printf '  通过  %s\n' "$1"; }
bad() { printf '  失败  %s\n' "$1"; fail=1; }

work="$(mktemp -d)"
before="$work/images-before"
docker images --format '{{.Repository}}:{{.Tag}}@{{.ID}}' | sort >"$before"

cleanup() {
  docker rm -fv "$RELAY" "$P-classic" "$P-ctrd" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  docker images --format '{{.Repository}}:{{.Tag}}@{{.ID}}' | sort | comm -13 "$before" - | cut -d@ -f1 |
    xargs -r docker rmi >/dev/null 2>&1 || true
  docker image prune -f >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

for img in "$SK" "$CR" "$DIND" "$BASE"; do docker pull -q "$img" >/dev/null; done

echo "启动 relay（只读根文件系统、去掉全部能力、非 root）"
docker network create --internal "$NET" >/dev/null
mkdir -p "$work/logs" && chmod 777 "$work/logs"
cp "$BIN" "$work/relay" && chmod 755 "$work/relay"
docker run -d --name "$RELAY" --read-only --cap-drop ALL --security-opt no-new-privileges --tmpfs /tmp \
  -v "$work/relay:/relay:ro" -v "$CFG:/config.yaml:ro" -v "$work/logs:/logs" \
  "$BASE" /relay serve -config /config.yaml >/dev/null
docker network connect --alias "$RELAY" "$NET" "$RELAY"
for _ in $(seq 1 30); do
  docker exec "$RELAY" /relay probe -url http://127.0.0.1:5000/healthz 2>/dev/null && break
  sleep 1
done

# 两个 Docker：经典存储、containerd 存储。都把单域名入口配成 registry-mirrors，五个端口都允许明文
insecure="$(for p in 5000 5001 5002 5003 5004 5005; do printf '"%s:%s",' "$RELAY" "$p"; done)"
mkdir -p "$work/classic" "$work/ctrd"
printf '{"features":{"containerd-snapshotter":false},"registry-mirrors":["http://%s:5000"],"insecure-registries":[%s]}\n' "$RELAY" "${insecure%,}" >"$work/classic/daemon.json"
printf '{"registry-mirrors":["http://%s:5000"],"insecure-registries":[%s]}\n' "$RELAY" "${insecure%,}" >"$work/ctrd/daemon.json"
for d in classic ctrd; do
  docker run -d --privileged --name "$P-$d" --network "$NET" -v "$work/$d/daemon.json:/etc/docker/daemon.json:ro" "$DIND" >/dev/null
done
for d in classic ctrd; do
  for _ in $(seq 1 30); do docker exec "$P-$d" docker info >/dev/null 2>&1 && break; sleep 1; done
done

direct() { echo "sha256:$(docker run --rm "$SK" inspect --raw "docker://$1" 2>/dev/null | sha256sum | cut -d' ' -f1)"; }
dpull() { docker exec "$P-$1" docker pull "$2" 2>&1 | sed -n 's/^Digest: //p'; }

echo "每个上游一个端口：两种 Docker、skopeo（完整下载）、crane"
PORT_CASES=(
  "5001 library/hello-world:latest docker.io/library/hello-world:latest"
  "5001 library/alpine:3.22 docker.io/library/alpine:3.22"
  "5002 distroless/static:nonroot gcr.io/distroless/static:nonroot"
  "5003 containerd/busybox:1.36 ghcr.io/containerd/busybox:1.36"
  "5004 pause:3.10 registry.k8s.io/pause:3.10"
  "5004 coredns/coredns:v1.12.0 registry.k8s.io/coredns/coredns:v1.12.0"
  "5005 prometheus/busybox:latest quay.io/prometheus/busybox:latest"
)
for c in "${PORT_CASES[@]}"; do
  read -r port name up <<<"$c"
  want="$(direct "$up")"
  ref="$RELAY:$port/$name"
  for d in classic ctrd; do
    got="$(dpull "$d" "$ref")"
    [ "$got" = "$want" ] && ok "$d $ref" || bad "$d $ref：'$got'，上游 '$want'"
  done
  if docker run --rm --network "$NET" --tmpfs /out:size=2g "$SK" copy -q --src-tls-verify=false "docker://$ref" dir:/out/x >/dev/null 2>&1; then
    ok "skopeo $ref"
  else
    bad "skopeo $ref"
  fi
  if docker run --rm --network "$NET" --entrypoint sh "$CR" -c "crane pull --insecure $ref /tmp/x.tar" >/dev/null 2>&1; then
    ok "crane $ref"
  else
    bad "crane $ref"
  fi
done

echo "单域名入口：路径前缀"
for up in docker.io/library/alpine:3.22 gcr.io/distroless/static:nonroot ghcr.io/containerd/busybox:1.36 registry.k8s.io/pause:3.10 quay.io/prometheus/busybox:latest; do
  want="$(direct "$up")"
  got="$(dpull classic "$RELAY:5000/$up")"
  [ "$got" = "$want" ] && ok "前缀 $RELAY:5000/$up" || bad "前缀 $RELAY:5000/$up：'$got'，上游 '$want'"
done

echo "单域名入口：根路径（daemon.json 的 registry-mirrors）"
want="$(direct docker.io/library/busybox:1.37)"
for d in classic ctrd; do
  got="$(dpull "$d" busybox:1.37)"
  [ "$got" = "$want" ] && ok "$d docker pull busybox:1.37" || bad "$d docker pull busybox:1.37：'$got'，上游 '$want'"
done

echo "单域名入口：containerd 的 ns（ctr + hosts.toml）"
docker exec "$P-ctrd" sh -c "mkdir -p /etc/containerd/certs.d/ghcr.io && printf 'server = \"https://ghcr.io\"\n[host.\"http://$RELAY:5000\"]\n  capabilities = [\"pull\", \"resolve\"]\n' > /etc/containerd/certs.d/ghcr.io/hosts.toml"
want="$(direct ghcr.io/containerd/busybox:1.36)"
if docker exec "$P-ctrd" ctr --address /var/run/docker/containerd/containerd.sock -n relaytest images pull --local --hosts-dir /etc/containerd/certs.d ghcr.io/containerd/busybox:1.36 >/dev/null 2>&1; then
  got="$(docker exec "$P-ctrd" ctr --address /var/run/docker/containerd/containerd.sock -n relaytest images ls | awk '/ghcr.io\/containerd\/busybox:1.36/ {print $3}')"
  [ "$got" = "$want" ] && ok "ctr ns=ghcr.io $got" || bad "ctr ns=ghcr.io：'$got'，上游 '$want'"
else
  bad "ctr 经 ns 拉取失败"
fi

echo "访问日志"
sleep 2
log="$work/logs/access.log"
total="$(wc -l <"$log")"
redirects="$(jq -s '[.[] | select(.status >= 300 and .status < 400)] | length' "$log")"
errors="$(jq -s '[.[] | select(.status >= 500)] | length' "$log")"
ns="$(jq -s '[.[] | select(.request.uri | contains("ns=ghcr.io"))] | length' "$log")"
hops="$(jq -r '[.redirect1, .redirect2, .redirect3] | map(select(. != null)) | join(" > ")' "$log" | grep -v '^$' | sed -E 's/[0-9a-f]{20,}/X/g' | sort | uniq -c | sort -rn)"
[ "$redirects" = 0 ] && ok "$total 条请求，没有把重定向交给客户端" || bad "有 $redirects 个重定向交给了客户端"
[ "$errors" = 0 ] && ok "没有 5xx" || bad "有 $errors 个 5xx"
[ "$ns" -gt 0 ] && ok "收到 $ns 个带 ns=ghcr.io 的请求" || bad "没有收到带 ns 的请求"
echo "  服务端跟随的跳转："
printf '%s\n' "$hops" | sed 's/^/    /'

if [ "$fail" = 0 ]; then echo "全部通过"; else echo "有检查失败"; fi
exit "$fail"
