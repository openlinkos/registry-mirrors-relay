#!/usr/bin/env bash
# 断网测试：客户端放进不能上外网的 Docker 网络（--internal），只能连到 relay。拉取成功就说明没有哪一步
# 被引去直连上游（认证、重定向都由 relay 在服务端处理）。拉到的 digest 和直接从上游取的比对。
#
# 覆盖的客户端：经典存储和 containerd 存储的 Docker、skopeo、crane、ctr。
# 覆盖的写法：每个上游一个域名的入口（*.relay.test，网络别名）；单域名入口的根路径（daemon.json 的 registry-mirrors）、
# 路径前缀、ns（ctr + hosts.toml）。另外检查和拉取共用端口的管理后台、指标只接受不经代理的直连。
#
# 用法：scripts/isolated-test.sh <relay 的 linux 二进制>
#       IMAGE=<镜像> scripts/isolated-test.sh     # 测试发布的镜像本身：用镜像的入口、默认配置路径和 HEALTHCHECK
# 需要 docker（能跑 --privileged 容器）和 jq。会拉几个测试镜像，测完删除所有容器、网络、卷和新拉的镜像。
# 注意：会消耗这台机器出口 IP 的 Docker Hub 匿名额度。
set -euo pipefail

IMAGE="${IMAGE:-}"
if [ -z "$IMAGE" ]; then
  BIN="$(realpath "$1")"
fi
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

for img in "$SK" "$CR" "$DIND"; do docker pull -q "$img" >/dev/null; done
[ -z "$IMAGE" ] && docker pull -q "$BASE" >/dev/null

echo "启动 relay（只读根文件系统、去掉全部能力、非 root）"
docker network create --internal "$NET" >/dev/null
mkdir -p "$work/logs" && chmod 777 "$work/logs"
if [ -n "$IMAGE" ]; then
  RBIN=/usr/local/bin/registry-mirrors-relay
  docker run -d --name "$RELAY" --read-only --cap-drop ALL --security-opt no-new-privileges \
    -v "$CFG:/etc/registry-mirrors-relay/config.yaml:ro" -v "$work/logs:/logs" "$IMAGE" >/dev/null
else
  RBIN=/relay
  cp "$BIN" "$work/relay" && chmod 755 "$work/relay"
  docker run -d --name "$RELAY" --read-only --cap-drop ALL --security-opt no-new-privileges --tmpfs /tmp \
    -v "$work/relay:/relay:ro" -v "$CFG:/config.yaml:ro" -v "$work/logs:/logs" \
    "$BASE" /relay serve -config /config.yaml >/dev/null
fi
HOSTS="docker.relay.test gcr.relay.test ghcr.relay.test k8s.relay.test quay.relay.test"
aliases=(--alias "$RELAY")
for h in $HOSTS; do aliases+=(--alias "$h"); done
docker network connect "${aliases[@]}" "$NET" "$RELAY"
for _ in $(seq 1 30); do
  docker exec "$RELAY" "$RBIN" probe -url http://127.0.0.1:5000/healthz 2>/dev/null && break
  sleep 1
done

# 两个 Docker：经典存储、containerd 存储。都把单域名入口配成 registry-mirrors，所有入口都允许明文
insecure="$(for h in "$RELAY" $HOSTS; do printf '"%s:5000",' "$h"; done)"
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

echo "每个上游一个域名：两种 Docker、skopeo（完整下载）、crane"
HOST_CASES=(
  "docker.relay.test library/hello-world:latest docker.io/library/hello-world:latest"
  "docker.relay.test library/alpine:3.22 docker.io/library/alpine:3.22"
  "gcr.relay.test distroless/static:nonroot gcr.io/distroless/static:nonroot"
  "ghcr.relay.test containerd/busybox:1.36 ghcr.io/containerd/busybox:1.36"
  "k8s.relay.test pause:3.10 registry.k8s.io/pause:3.10"
  "k8s.relay.test coredns/coredns:v1.12.0 registry.k8s.io/coredns/coredns:v1.12.0"
  "quay.relay.test prometheus/busybox:latest quay.io/prometheus/busybox:latest"
)
for c in "${HOST_CASES[@]}"; do
  read -r host name up <<<"$c"
  want="$(direct "$up")"
  ref="$host:5000/$name"
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

echo "管理后台和指标（和拉取共用 5000）"
# 后台默认公开：直连和带 X-Forwarded-For（像经过反向代理）都能打开登录页。指标没有登录，只给直连：
# 带 X-Forwarded-For 就是 404。
rip="$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" "$RELAY")"
mg="$(docker run --rm --network "$NET" --entrypoint sh "$CR" -c "
  wget -q -O /dev/null http://$rip:5000/admin/login && echo direct=200 || echo direct=fail
  wget -q -O /dev/null --header 'X-Forwarded-For: 198.51.100.7' http://$rip:5000/admin/login && echo proxied=200 || echo proxied=fail
  wget -q -O - http://$rip:5000/metrics | grep -q '^relay_' && echo dmetrics=200 || echo dmetrics=fail
  wget -q -O /dev/null --header 'X-Forwarded-For: 198.51.100.7' http://$rip:5000/metrics && echo pmetrics=200 || echo pmetrics=fail" 2>/dev/null)"
case "$mg" in *direct=200*) ok "直连能打开后台" ;; *) bad "直连打不开后台：$mg" ;; esac
case "$mg" in *proxied=200*) ok "经代理（带 X-Forwarded-For）也能打开后台登录页" ;; *) bad "经代理打不开后台：$mg" ;; esac
case "$mg" in *dmetrics=200*) ok "直连能抓指标" ;; *) bad "抓不到指标：$mg" ;; esac
case "$mg" in *pmetrics=fail*) ok "带 X-Forwarded-For 的指标请求是 404" ;; *) bad "经代理的指标请求没有被拒绝：$mg" ;; esac

if [ -n "$IMAGE" ]; then
  echo "镜像"
  # HEALTHCHECK 不带参数运行 probe：按镜像里的配置路径找端口
  for _ in $(seq 1 40); do
    st="$(docker inspect -f '{{.State.Health.Status}}' "$RELAY")"
    [ "$st" = starting ] || break
    sleep 1
  done
  if [ "$st" = healthy ]; then ok "HEALTHCHECK：$st"; else bad "HEALTHCHECK：$st"; fi
  uid="$(docker exec "$RELAY" "$RBIN" version >/dev/null && docker inspect -f '{{.Config.User}}' "$RELAY")"
  if [ "$uid" = 65532:65532 ]; then ok "以 nonroot（$uid）运行"; else bad "运行用户：$uid"; fi
fi

if [ "$fail" = 0 ]; then echo "全部通过"; else echo "有检查失败"; fi
exit "$fail"
