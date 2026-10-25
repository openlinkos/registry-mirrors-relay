#!/usr/bin/env bash
# Kubernetes end-to-end test: deploys the relay in kind (two replicas, read-only operational data, administrator
# password from a Secret), points the nodes' containerd at the relay for docker.io and ghcr.io, then starts two
# Pods that pull images.
#   INSTALL=kustomize (default): the kustomize in deploy/test/kind (base plus the admin password component);
#   INSTALL=helm: installs with charts/registry-mirrors-relay and deploy/test/kind/values.yaml, also runs helm test,
#                 and finally upgrades to mode=writable, confirms that the StatefulSet is ready and that the first
#                 sign-in requires a password change, changes one setting in the console and checks that it
#                 survives a restart, then splits off a separate management port.
#
# The relay's namespace enforces restricted Pod Security; the test Pods that pull images run in a separate
# namespace. The server in hosts.toml (the fallback that connects directly to the upstream) is deliberately an
# address that does not resolve: when the relay is not working the Pods cannot pull, instead of silently
# bypassing the relay. Finally it checks that the relay's access log has these pulls and that the admin console
# accepts the password from the Secret.
#
# Requires docker, kind, kubectl, go and openssl (helm mode also needs helm, which HELM can point to).
# Uses up some of the Docker Hub anonymous quota (a few pulls) of the egress IP of the machine it runs on.
# With KEEP=1 the cluster is kept after the test.
set -euo pipefail

CLUSTER="${CLUSTER:-relay-e2e}"
INSTALL="${INSTALL:-kustomize}"
HELM="${HELM:-helm}"
NS=relay
IMAGE=registry-mirrors-relay:e2e
fail=0
ok() { printf '  pass  %s\n' "$1"; }
bad() { printf '  FAIL  %s\n' "$1"; fail=1; }

work="$(mktemp -d)"
pf=""
cleanup() {
  [ -n "$pf" ] && kill "$pf" 2>/dev/null || true
  if [ "${KEEP:-}" != 1 ]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
  rm -rf "$work"
}
trap cleanup EXIT

# session_start <port>: signs in with $work/pw through port-forward (port 80 of svc/registry-mirrors-relay),
# keeps the session in $work/jar and puts the CSRF token in csrf. session_stop ends the port-forward.
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
# api <curl arguments>: calls the management API with the session (write operations carry the CSRF token and Origin).
api() { curl -s -b "$work/jar" -H "Origin: $base" -H "X-CSRF-Token: $csrf" "$@"; }

# admin_login [Service] [port]: signs in through port-forward with the password from the Secret and prints the
# address the sign-in redirects to. The default is three-in-one: the console is on the pull entrance's Service (port 80).
admin_login() {
  kubectl -n "$NS" port-forward "svc/${1:-registry-mirrors-relay}" "19090:${2:-80}" >/dev/null 2>&1 &
  pf=$!
  for _ in $(seq 1 30); do curl -fs -o /dev/null http://127.0.0.1:19090/admin/login && break; sleep 1; done
  curl -s -o /dev/null -w '%{redirect_url}' -H 'Origin: http://127.0.0.1:19090' \
    --data-urlencode username=admin --data-urlencode "password=$(cat "$work/pw")" http://127.0.0.1:19090/admin/login
  kill "$pf" 2>/dev/null || true
  pf=""
}

echo "Building the image and creating the cluster (install method: $INSTALL)"
scripts/build-image.sh "$IMAGE" >/dev/null
kind create cluster --name "$CLUSTER" --wait 120s >/dev/null
kind load docker-image "$IMAGE" --name "$CLUSTER" >/dev/null
# CoreDNS may not be ready yet in a freshly created cluster, and helm test resolves the Service by name
kubectl -n kube-system rollout status deploy/coredns --timeout=120s >/dev/null

echo "Deploying"
kubectl create namespace "$NS" >/dev/null
kubectl label namespace "$NS" pod-security.kubernetes.io/enforce=restricted >/dev/null
kubectl create namespace pull >/dev/null
openssl rand -base64 18 >"$work/pw"
kubectl -n "$NS" create secret generic registry-mirrors-relay-admin --from-file=password="$work/pw" >/dev/null
case "$INSTALL" in
  kustomize) kubectl -n "$NS" apply -k deploy/test/kind >/dev/null ;;
  helm) "$HELM" install relay charts/registry-mirrors-relay -n "$NS" -f deploy/test/kind/values.yaml >/dev/null ;;
  *) echo "INSTALL must be kustomize or helm" >&2; exit 2 ;;
esac
if kubectl -n "$NS" rollout status deploy/registry-mirrors-relay --timeout=180s >/dev/null; then
  ok "Two replicas ready (restricted Pod Security)"
else
  bad "Not ready"
  kubectl -n "$NS" describe pods
  kubectl -n "$NS" get events
  exit 1
fi
if [ "$INSTALL" = kustomize ]; then
  echo "Validating the other deployment methods on the API server (those that need no extra CRDs)"
  kubectl create namespace dryrun >/dev/null
  for d in deploy/kubernetes/overlays/single-writable deploy/kubernetes/overlays/networkpolicy; do
    if kubectl -n dryrun apply --dry-run=server -k "$d" >/dev/null; then ok "$d"; else bad "$d"; fi
  done
fi

echo "Pointing the nodes' containerd at the relay"
for node in $(kind get nodes --name "$CLUSTER"); do
  ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$node")"
  for up in docker.io ghcr.io; do
    docker exec "$node" mkdir -p "/etc/containerd/certs.d/$up"
    printf 'server = "https://fallback.invalid"\n\n[host."http://%s:30080"]\n  capabilities = ["pull", "resolve"]\n' "$ip" |
      docker exec -i "$node" cp /dev/stdin "/etc/containerd/certs.d/$up/hosts.toml"
  done
done

echo "Pulling"
kubectl -n pull run e2e-dockerhub --image=docker.io/library/busybox:1.37 --restart=Never --command -- sh -c 'echo ok' >/dev/null
kubectl -n pull run e2e-ghcr --image=ghcr.io/containerd/busybox:1.36 --restart=Never --command -- sh -c 'echo ok' >/dev/null
for pod in e2e-dockerhub e2e-ghcr; do
  if kubectl -n pull wait --for=jsonpath='{.status.phase}'=Succeeded "pod/$pod" --timeout=240s >/dev/null; then
    ok "$pod pulled its image and ran to completion"
  else
    bad "$pod did not run to completion"
    kubectl -n pull describe pod "$pod" | tail -20
  fi
done

echo "Access log"
kubectl -n "$NS" logs -l app.kubernetes.io/name=registry-mirrors-relay --tail=-1 --prefix=false >"$work/logs" 2>/dev/null
for want in 'library/busybox/manifests/1.37' 'containerd/busybox/manifests/1.36'; do
  if grep -F "$want" "$work/logs" | grep -q '"status":200'; then ok "Relay handled $want"; else bad "$want not in the log"; fi
done
if grep -F 'ns=ghcr.io' "$work/logs" | grep -q '/blobs/'; then ok "Layers downloaded through the relay (with ns=ghcr.io)"; else bad "ghcr.io layers were not downloaded through the relay"; fi

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

echo "Admin console"
loc="$(admin_login)"
if [ "$loc" = "http://127.0.0.1:19090/admin/" ]; then ok "Signed in to the admin console through port-forward with the password from the Secret (sharing the pull port)"; else bad "Sign-in redirected to $loc"; fi
# A Pod in the cluster: opens the console over a direct connection, and with X-Forwarded-For (as if through an
# Ingress or CDN) too, since the console is public by default; metrics are off by default.
# The console only accepts a Host that is localhost, an IP or the public_url host (against DNS rebinding), so the
# Service IP is used
svc="http://$(kubectl -n "$NS" get svc registry-mirrors-relay -o jsonpath='{.spec.clusterIP}')"
kubectl -n pull run mgmt-check --image=docker.io/library/busybox:1.37 --restart=Never --command -- sh -c \
  "wget -q -O /dev/null $svc/admin/login && echo direct=200 || echo direct=fail
   wget -q -O /dev/null --header 'X-Forwarded-For: 198.51.100.7' $svc/admin/login && echo proxied=200 || echo proxied=fail
   wget -q -O /dev/null $svc/metrics && echo metrics=200 || echo metrics=fail" >/dev/null
kubectl -n pull wait --for=jsonpath='{.status.phase}'=Succeeded pod/mgmt-check --timeout=120s >/dev/null || true
out="$(kubectl -n pull logs mgmt-check 2>/dev/null)"
case "$out" in *direct=200*) ok "A Pod in the cluster opens the console over a direct connection" ;; *) bad "A Pod in the cluster cannot open the console over a direct connection: $out" ;; esac
case "$out" in *proxied=200*) ok "Console public by default: the sign-in page also opens with X-Forwarded-For" ;; *) bad "Proxied request cannot open the console: $out" ;; esac
case "$out" in *metrics=fail*) ok "Metrics off by default" ;; *) bad "Metrics should be off by default: $out" ;; esac

if [ "$INSTALL" = helm ]; then
  echo "Upgrading to mode=writable"
  if "$HELM" upgrade relay charts/registry-mirrors-relay -n "$NS" -f deploy/test/kind/values.yaml --set mode=writable --wait --timeout 180s >/dev/null &&
    kubectl -n "$NS" rollout status statefulset/registry-mirrors-relay --timeout=180s >/dev/null; then
    ok "StatefulSet ready"
  else
    bad "Upgrade failed"
    kubectl -n "$NS" get all,pvc
  fi
  # Wait for the Deployment's old Pods to exit so that port-forward does not connect to them
  for _ in $(seq 1 90); do [ -z "$(kubectl -n "$NS" get pods -l pod-template-hash -o name)" ] && break; sleep 1; done
  if [ -z "$(kubectl -n "$NS" get deploy -o name)$(kubectl -n "$NS" get pods -l pod-template-hash -o name)" ] &&
    [ "$(kubectl -n "$NS" get pvc -o jsonpath='{.items[*].status.phase}')" = Bound ]; then
    ok "Deployment deleted, PVC bound"
  else
    bad "Deployment still present or PVC not bound"
  fi
  loc="$(admin_login)"
  if [ "$loc" = "http://127.0.0.1:19090/admin/account" ]; then ok "In writable mode the first sign-in requires a password change"; else bad "Sign-in redirected to $loc"; fi

  echo "Changing the configuration in the console"
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
    ok "Changed the limits, effective immediately"
  else
    bad "Changing the limits: $code"
  fi
  session_stop
  kubectl -n "$NS" delete pod registry-mirrors-relay-0 --wait=true >/dev/null
  for _ in $(seq 1 60); do kubectl -n "$NS" get pod registry-mirrors-relay-0 >/dev/null 2>&1 && break; sleep 1; done
  kubectl -n "$NS" wait --for=condition=Ready pod/registry-mirrors-relay-0 --timeout=180s >/dev/null
  session_start 19093
  if api "$base/admin/api/v1/config/overrides" | grep -q '"state": "active"'; then ok "Change survives a restart (stored in the PVC)"; else bad "Change lost after a restart"; fi
  code="$(api -o /dev/null -w '%{http_code}' -X DELETE "$base/admin/api/v1/config/overrides/limits")"
  if [ "$code" = 204 ] && ! api "$base/admin/api/v1/config" | grep -q 'max_inflight: 1234'; then ok "After a reset the configuration file value is back"; else bad "Reset: $code"; fi
  session_stop

  echo "Setting a password from the command line while the service runs"
  if pw3="$(kubectl -n "$NS" exec registry-mirrors-relay-0 -- registry-mirrors-relay admin password --generate 2>/dev/null)" && [ -n "$pw3" ]; then
    printf '%s' "$pw3" >"$work/pw"
    ok "admin password --generate through kubectl exec"
  else
    bad "admin password through kubectl exec failed"
  fi
  loc="$(admin_login)"
  if [ "$loc" = "http://127.0.0.1:19090/admin/" ]; then ok "Signed in with the password set from the command line, no change required"; else bad "Sign-in redirected to $loc"; fi

  echo "Moving the management entrance to a separate port"
  if "$HELM" upgrade relay charts/registry-mirrors-relay -n "$NS" -f deploy/test/kind/values.yaml --set mode=writable \
    --set admin.separatePort=true --set metrics.enabled=true --wait --timeout 180s >/dev/null &&
    kubectl -n "$NS" rollout status statefulset/registry-mirrors-relay --timeout=180s >/dev/null; then
    ok "Upgraded to admin.separatePort=true"
  else
    bad "Upgrade failed"
  fi
  loc="$(admin_login registry-mirrors-relay-admin 9090)"
  if [ "$loc" = "http://127.0.0.1:19090/admin/" ]; then ok "Console on port 9090 of -admin"; else bad "-admin sign-in redirected to $loc"; fi
  kubectl -n "$NS" port-forward svc/registry-mirrors-relay 19091:80 >/dev/null 2>&1 &
  pf=$!
  for _ in $(seq 1 30); do curl -s -o /dev/null http://127.0.0.1:19091/healthz && break; sleep 1; done
  code="$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:19091/admin/login)"
  kill "$pf" 2>/dev/null || true
  pf=""
  if [ "$code" = 404 ]; then ok "No console on the pull port any more"; else bad "/admin/login on the pull port returned $code"; fi
  kubectl -n "$NS" port-forward svc/registry-mirrors-relay-admin 19092:9090 >/dev/null 2>&1 &
  pf=$!
  for _ in $(seq 1 30); do curl -s -o /dev/null http://127.0.0.1:19092/admin/login && break; sleep 1; done
  if curl -fs http://127.0.0.1:19092/metrics | grep -q '^relay_'; then ok "Metrics on port 9090 of -admin"; else bad "Cannot scrape the metrics on -admin"; fi
  kill "$pf" 2>/dev/null || true
  pf=""
fi

if [ "$fail" = 0 ]; then echo "All checks passed"; else echo "Some checks failed"; fi
exit "$fail"
