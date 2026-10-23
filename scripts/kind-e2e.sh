#!/usr/bin/env bash
# Kubernetes 端到端测试：在 kind 里部署 deploy/kubernetes/base（两个副本、只读运营数据、管理员口令组件），
# 把节点上 containerd 的 docker.io 和 ghcr.io 指向 relay，然后起两个 Pod 拉镜像。
#
# hosts.toml 里的 server（直连上游的后备）故意写成解析不了的地址：relay 不工作时 Pod 就拉不到镜像，
# 不会悄悄绕过 relay。最后检查 relay 的访问日志里有这些拉取，并且管理后台能用 Secret 里的口令登录。
#
# 需要 docker、kind、kubectl、go、openssl。会消耗运行机器出口 IP 的 Docker Hub 匿名额度（几次）。
# 用法：scripts/kind-e2e.sh；KEEP=1 时测完保留集群。
set -euo pipefail

CLUSTER="${CLUSTER:-relay-e2e}"
NS=relay
IMAGE=registry-mirrors-relay:e2e
fail=0
ok() { printf '  通过  %s\n' "$1"; }
bad() { printf '  失败  %s\n' "$1"; fail=1; }

work="$(mktemp -d)"
cleanup() {
  [ -n "${pf:-}" ] && kill "$pf" 2>/dev/null || true
  if [ "${KEEP:-}" != 1 ]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
  rm -rf "$work"
}
trap cleanup EXIT

echo "构建镜像，创建集群"
scripts/build-image.sh "$IMAGE" >/dev/null
kind create cluster --name "$CLUSTER" --wait 120s >/dev/null
kind load docker-image "$IMAGE" --name "$CLUSTER" >/dev/null

echo "部署"
kubectl create namespace "$NS" >/dev/null
openssl rand -base64 18 >"$work/pw"
kubectl -n "$NS" create secret generic registry-mirrors-relay-admin --from-file=password="$work/pw" >/dev/null
kubectl -n "$NS" apply -k deploy/test/kind >/dev/null
if kubectl -n "$NS" rollout status deploy/registry-mirrors-relay --timeout=180s >/dev/null; then
  ok "两个副本就绪"
else
  bad "没有就绪"
  kubectl -n "$NS" describe pods
  exit 1
fi

echo "其他部署方式在 API Server 上校验（不需要额外 CRD 的）"
kubectl create namespace dryrun >/dev/null
for d in deploy/kubernetes/overlays/single-writable deploy/kubernetes/overlays/networkpolicy; do
  if kubectl -n dryrun apply --dry-run=server -k "$d" >/dev/null; then ok "$d"; else bad "$d"; fi
done

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
kubectl -n "$NS" run e2e-dockerhub --image=docker.io/library/busybox:1.37 --restart=Never --command -- sh -c 'echo ok' >/dev/null
kubectl -n "$NS" run e2e-ghcr --image=ghcr.io/containerd/busybox:1.36 --restart=Never --command -- sh -c 'echo ok' >/dev/null
for pod in e2e-dockerhub e2e-ghcr; do
  if kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Succeeded "pod/$pod" --timeout=240s >/dev/null; then
    ok "$pod 拉到镜像并运行完成"
  else
    bad "$pod 没有运行完成"
    kubectl -n "$NS" describe pod "$pod" | tail -20
  fi
done

echo "访问日志"
kubectl -n "$NS" logs -l app.kubernetes.io/name=registry-mirrors-relay --tail=-1 --prefix=false >"$work/logs" 2>/dev/null
for want in 'library/busybox/manifests/1.37' 'containerd/busybox/manifests/1.36'; do
  if grep -F "$want" "$work/logs" | grep -q '"status":200'; then ok "relay 处理了 $want"; else bad "日志里没有 $want"; fi
done
if grep -F 'ns=ghcr.io' "$work/logs" | grep -q '/blobs/'; then ok "镜像层经 relay 下载（带 ns=ghcr.io）"; else bad "没有经 relay 下载 ghcr.io 的镜像层"; fi

echo "管理后台"
kubectl -n "$NS" port-forward svc/registry-mirrors-relay-admin 19090:9090 >/dev/null 2>&1 &
pf=$!
for _ in $(seq 1 30); do curl -fs -o /dev/null http://127.0.0.1:19090/admin/login && break; sleep 1; done
loc="$(curl -s -o /dev/null -w '%{redirect_url}' -H 'Origin: http://127.0.0.1:19090' \
  --data-urlencode username=admin --data-urlencode "password=$(cat "$work/pw")" http://127.0.0.1:19090/admin/login)"
if [ "$loc" = "http://127.0.0.1:19090/admin/" ]; then ok "用 Secret 里的口令登录管理后台"; else bad "登录后跳到了 $loc"; fi

if [ "$fail" = 0 ]; then echo "全部通过"; else echo "有检查失败"; fi
exit "$fail"
