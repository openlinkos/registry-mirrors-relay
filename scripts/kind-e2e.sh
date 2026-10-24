#!/usr/bin/env bash
# Kubernetes 端到端测试：在 kind 里部署 relay（两个副本、只读运营数据、管理员口令来自 Secret），
# 把节点上 containerd 的 docker.io 和 ghcr.io 指向 relay，然后起两个 Pod 拉镜像。
#   INSTALL=kustomize（默认）：deploy/test/kind 的 kustomize（base 加管理员口令组件）；
#   INSTALL=helm：用 charts/registry-mirrors-relay 和 deploy/test/kind/values.yaml 安装，另外跑 helm test，
#                 最后升级成 mode=writable，确认 StatefulSet 就绪、第一次登录要求改口令，
#                 在后台改一处配置、重启后还在，再拆出单独的管理端口。
#
# relay 所在的命名空间强制 restricted Pod Security；拉镜像的测试 Pod 在单独的命名空间里。
# hosts.toml 里的 server（直连上游的后备）故意写成解析不了的地址：relay 不工作时 Pod 就拉不到镜像，
# 不会悄悄绕过 relay。最后检查 relay 的访问日志里有这些拉取，并且管理后台能用 Secret 里的口令登录。
#
# 需要 docker、kind、kubectl、go、openssl（helm 模式还要 helm，可用 HELM 指定）。
# 会消耗运行机器出口 IP 的 Docker Hub 匿名额度（几次）。KEEP=1 时测完保留集群。
set -euo pipefail

CLUSTER="${CLUSTER:-relay-e2e}"
INSTALL="${INSTALL:-kustomize}"
HELM="${HELM:-helm}"
NS=relay
IMAGE=registry-mirrors-relay:e2e
fail=0
ok() { printf '  通过  %s\n' "$1"; }
bad() { printf '  失败  %s\n' "$1"; fail=1; }

work="$(mktemp -d)"
pf=""
cleanup() {
  [ -n "$pf" ] && kill "$pf" 2>/dev/null || true
  if [ "${KEEP:-}" != 1 ]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
  rm -rf "$work"
}
trap cleanup EXIT

# session_start <端口>：经 port-forward（svc/registry-mirrors-relay 的 80 口）用 $work/pw 登录，会话存在 $work/jar，
# CSRF 令牌放进 csrf。session_stop 结束 port-forward。
session_start() {
  base="http://127.0.0.1:$1"
  kubectl -n "$NS" port-forward svc/registry-mirrors-relay "$1:80" >/dev/null 2>&1 &
  pf=$!
  for _ in $(seq 1 30); do curl -fs -o /dev/null "$base/admin/login" && break; sleep 1; done
  rm -f "$work/jar"
  curl -s -o /dev/null -c "$work/jar" -H "Origin: $base" \
    --data-urlencode username=admin --data-urlencode "password=$(cat "$work/pw")" "$base/admin/login"
  csrf="$(curl -s -b "$work/jar" "$base/admin/account" | sed -n 's/.*name="_csrf" value="\([^"]*\)".*/\1/p' | head -n 1)"
}
session_stop() { kill "$pf" 2>/dev/null || true; pf=""; }
# api <curl 参数>：用会话调管理 API（写操作带 CSRF 令牌和 Origin）。
api() { curl -s -b "$work/jar" -H "Origin: $base" -H "X-CSRF-Token: $csrf" "$@"; }

# admin_login [Service] [端口]：经 port-forward 用 Secret 里的口令登录，输出登录后跳到的地址。
# 默认是三合一：后台在拉取入口的 Service（80 口）上。
admin_login() {
  kubectl -n "$NS" port-forward "svc/${1:-registry-mirrors-relay}" "19090:${2:-80}" >/dev/null 2>&1 &
  pf=$!
  for _ in $(seq 1 30); do curl -fs -o /dev/null http://127.0.0.1:19090/admin/login && break; sleep 1; done
  curl -s -o /dev/null -w '%{redirect_url}' -H 'Origin: http://127.0.0.1:19090' \
    --data-urlencode username=admin --data-urlencode "password=$(cat "$work/pw")" http://127.0.0.1:19090/admin/login
  kill "$pf" 2>/dev/null || true
  pf=""
}

echo "构建镜像，创建集群（安装方式：$INSTALL）"
scripts/build-image.sh "$IMAGE" >/dev/null
kind create cluster --name "$CLUSTER" --wait 120s >/dev/null
kind load docker-image "$IMAGE" --name "$CLUSTER" >/dev/null
# 刚建好的集群里 CoreDNS 可能还没就绪，helm test 要按 Service 名解析
kubectl -n kube-system rollout status deploy/coredns --timeout=120s >/dev/null

echo "部署"
kubectl create namespace "$NS" >/dev/null
kubectl label namespace "$NS" pod-security.kubernetes.io/enforce=restricted >/dev/null
kubectl create namespace pull >/dev/null
openssl rand -base64 18 >"$work/pw"
kubectl -n "$NS" create secret generic registry-mirrors-relay-admin --from-file=password="$work/pw" >/dev/null
case "$INSTALL" in
  kustomize) kubectl -n "$NS" apply -k deploy/test/kind >/dev/null ;;
  helm) "$HELM" install relay charts/registry-mirrors-relay -n "$NS" -f deploy/test/kind/values.yaml >/dev/null ;;
  *) echo "INSTALL 只能是 kustomize 或 helm" >&2; exit 2 ;;
esac
if kubectl -n "$NS" rollout status deploy/registry-mirrors-relay --timeout=180s >/dev/null; then
  ok "两个副本就绪（restricted Pod Security）"
else
  bad "没有就绪"
  kubectl -n "$NS" describe pods
  kubectl -n "$NS" get events
  exit 1
fi
if [ "$INSTALL" = kustomize ]; then
  echo "其他部署方式在 API Server 上校验（不需要额外 CRD 的）"
  kubectl create namespace dryrun >/dev/null
  for d in deploy/kubernetes/overlays/single-writable deploy/kubernetes/overlays/networkpolicy; do
    if kubectl -n dryrun apply --dry-run=server -k "$d" >/dev/null; then ok "$d"; else bad "$d"; fi
  done
fi

echo "节点上的 containerd 指向 relay"
for node in $(kind get nodes --name "$CLUSTER"); do
  ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$node")"
  for up in docker.io ghcr.io; do
    docker exec "$node" mkdir -p "/etc/containerd/certs.d/$up"
    printf 'server = "https://fallback.invalid"\n\n[host."http://%s:30080"]\n  capabilities = ["pull", "resolve"]\n' "$ip" |
      docker exec -i "$node" cp /dev/stdin "/etc/containerd/certs.d/$up/hosts.toml"
  done
done

echo "拉取"
kubectl -n pull run e2e-dockerhub --image=docker.io/library/busybox:1.37 --restart=Never --command -- sh -c 'echo ok' >/dev/null
kubectl -n pull run e2e-ghcr --image=ghcr.io/containerd/busybox:1.36 --restart=Never --command -- sh -c 'echo ok' >/dev/null
for pod in e2e-dockerhub e2e-ghcr; do
  if kubectl -n pull wait --for=jsonpath='{.status.phase}'=Succeeded "pod/$pod" --timeout=240s >/dev/null; then
    ok "$pod 拉到镜像并运行完成"
  else
    bad "$pod 没有运行完成"
    kubectl -n pull describe pod "$pod" | tail -20
  fi
done

echo "访问日志"
kubectl -n "$NS" logs -l app.kubernetes.io/name=registry-mirrors-relay --tail=-1 --prefix=false >"$work/logs" 2>/dev/null
for want in 'library/busybox/manifests/1.37' 'containerd/busybox/manifests/1.36'; do
  if grep -F "$want" "$work/logs" | grep -q '"status":200'; then ok "relay 处理了 $want"; else bad "日志里没有 $want"; fi
done
if grep -F 'ns=ghcr.io' "$work/logs" | grep -q '/blobs/'; then ok "镜像层经 relay 下载（带 ns=ghcr.io）"; else bad "没有经 relay 下载 ghcr.io 的镜像层"; fi

if [ "$INSTALL" = helm ]; then
  echo "helm test"
  if "$HELM" test relay -n "$NS" --logs >"$work/test" 2>&1; then
    ok "helm test"
  else
    bad "helm test"
    cat "$work/test"
    kubectl -n "$NS" logs registry-mirrors-relay-test || true
  fi
fi

echo "管理后台"
loc="$(admin_login)"
if [ "$loc" = "http://127.0.0.1:19090/admin/" ]; then ok "经 port-forward 用 Secret 里的口令登录管理后台（和拉取共用端口）"; else bad "登录后跳到了 $loc"; fi
# 集群里的 Pod 直连：能打开后台；带 X-Forwarded-For（像经过 Ingress、CDN）就是 404；指标默认关。
# 后台只接受 localhost、IP 或 public_url 的 Host（防 DNS 重绑定），所以用 Service 的 IP 访问
svc="http://$(kubectl -n "$NS" get svc registry-mirrors-relay -o jsonpath='{.spec.clusterIP}')"
kubectl -n pull run mgmt-check --image=docker.io/library/busybox:1.37 --restart=Never --command -- sh -c \
  "wget -q -O /dev/null $svc/admin/login && echo direct=200 || echo direct=fail
   wget -q -O /dev/null --header 'X-Forwarded-For: 198.51.100.7' $svc/admin/login && echo proxied=200 || echo proxied=fail
   wget -q -O /dev/null $svc/metrics && echo metrics=200 || echo metrics=fail" >/dev/null
kubectl -n pull wait --for=jsonpath='{.status.phase}'=Succeeded pod/mgmt-check --timeout=120s >/dev/null || true
out="$(kubectl -n pull logs mgmt-check 2>/dev/null)"
case "$out" in *direct=200*) ok "集群里的 Pod 直连能打开后台" ;; *) bad "集群里的 Pod 直连打不开后台：$out" ;; esac
case "$out" in *proxied=200*) ok "后台默认公开：带 X-Forwarded-For 也能打开登录页" ;; *) bad "经代理打不开后台：$out" ;; esac
case "$out" in *metrics=fail*) ok "指标默认关" ;; *) bad "指标默认应关：$out" ;; esac

if [ "$INSTALL" = helm ]; then
  echo "升级成 mode=writable"
  if "$HELM" upgrade relay charts/registry-mirrors-relay -n "$NS" -f deploy/test/kind/values.yaml --set mode=writable --wait --timeout 180s >/dev/null &&
    kubectl -n "$NS" rollout status statefulset/registry-mirrors-relay --timeout=180s >/dev/null; then
    ok "StatefulSet 就绪"
  else
    bad "升级失败"
    kubectl -n "$NS" get all,pvc
  fi
  # 等 Deployment 的旧 Pod 退出，免得 port-forward 连到它们
  for _ in $(seq 1 90); do [ -z "$(kubectl -n "$NS" get pods -l pod-template-hash -o name)" ] && break; sleep 1; done
  if [ -z "$(kubectl -n "$NS" get deploy -o name)$(kubectl -n "$NS" get pods -l pod-template-hash -o name)" ] &&
    [ "$(kubectl -n "$NS" get pvc -o jsonpath='{.items[*].status.phase}')" = Bound ]; then
    ok "Deployment 已删除，PVC 已绑定"
  else
    bad "Deployment 还在或 PVC 没有绑定"
  fi
  loc="$(admin_login)"
  if [ "$loc" = "http://127.0.0.1:19090/admin/account" ]; then ok "可写模式下第一次登录要求改口令"; else bad "登录后跳到了 $loc"; fi

  echo "在后台改配置"
  session_start 19093
  openssl rand -base64 18 >"$work/pw2"
  curl -s -o /dev/null -b "$work/jar" -c "$work/jar" -H "Origin: $base" --data-urlencode "_csrf=$csrf" \
    --data-urlencode "password=$(cat "$work/pw2")" --data-urlencode "confirm=$(cat "$work/pw2")" "$base/admin/account/password"
  mv "$work/pw2" "$work/pw"
  session_stop
  session_start 19093
  code="$(api -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/json' -d '{"max_inflight": 1234}' \
    "$base/admin/api/v1/config/overrides/limits")"
  if [ "$code" = 200 ] && api "$base/admin/api/v1/config" | grep -q 'max_inflight: 1234'; then
    ok "改了限流，立即生效"
  else
    bad "改限流：$code"
  fi
  session_stop
  kubectl -n "$NS" delete pod registry-mirrors-relay-0 --wait=true >/dev/null
  for _ in $(seq 1 60); do kubectl -n "$NS" get pod registry-mirrors-relay-0 >/dev/null 2>&1 && break; sleep 1; done
  kubectl -n "$NS" wait --for=condition=Ready pod/registry-mirrors-relay-0 --timeout=180s >/dev/null
  session_start 19093
  if api "$base/admin/api/v1/config/overrides" | grep -q '"state": "active"'; then ok "重启后修改还在（存在 PVC 里）"; else bad "重启后修改没了"; fi
  code="$(api -o /dev/null -w '%{http_code}' -X DELETE "$base/admin/api/v1/config/overrides/limits")"
  if [ "$code" = 204 ] && ! api "$base/admin/api/v1/config" | grep -q 'max_inflight: 1234'; then ok "重置后回到配置文件的值"; else bad "重置：$code"; fi
  session_stop

  echo "管理入口拆到单独的端口"
  if "$HELM" upgrade relay charts/registry-mirrors-relay -n "$NS" -f deploy/test/kind/values.yaml --set mode=writable \
    --set admin.separatePort=true --set metrics.enabled=true --wait --timeout 180s >/dev/null &&
    kubectl -n "$NS" rollout status statefulset/registry-mirrors-relay --timeout=180s >/dev/null; then
    ok "升级成 admin.separatePort=true"
  else
    bad "升级失败"
  fi
  loc="$(admin_login registry-mirrors-relay-admin 9090)"
  if [ "$loc" = "http://127.0.0.1:19090/admin/" ]; then ok "后台在 -admin 的 9090 上"; else bad "-admin 登录后跳到了 $loc"; fi
  kubectl -n "$NS" port-forward svc/registry-mirrors-relay 19091:80 >/dev/null 2>&1 &
  pf=$!
  for _ in $(seq 1 30); do curl -s -o /dev/null http://127.0.0.1:19091/healthz && break; sleep 1; done
  code="$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:19091/admin/login)"
  kill "$pf" 2>/dev/null || true
  pf=""
  if [ "$code" = 404 ]; then ok "拉取端口上不再有后台"; else bad "拉取端口的 /admin/login 返回 $code"; fi
  kubectl -n "$NS" port-forward svc/registry-mirrors-relay-admin 19092:9090 >/dev/null 2>&1 &
  pf=$!
  for _ in $(seq 1 30); do curl -s -o /dev/null http://127.0.0.1:19092/admin/login && break; sleep 1; done
  if curl -fs http://127.0.0.1:19092/metrics | grep -q '^relay_'; then ok "指标在 -admin 的 9090 上"; else bad "-admin 上抓不到指标"; fi
  kill "$pf" 2>/dev/null || true
  pf=""
fi

if [ "$fail" = 0 ]; then echo "全部通过"; else echo "有检查失败"; fi
exit "$fail"
