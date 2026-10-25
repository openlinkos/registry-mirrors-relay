#!/usr/bin/env bash
# Offline test: the clients sit in a Docker network with no internet access (--internal) and can reach only
# the relay. A successful pull shows that no step was diverted to a direct connection to the upstream
# (the relay handles authentication and redirects on the server side). The pulled digests are compared with
# the ones fetched directly from the upstream.
#
# Clients covered: Docker with classic storage and with containerd storage, skopeo, crane, ctr.
# Setups covered: entrances with one domain per upstream (*.relay.test, network aliases); on the single-domain
# entrance, the root path (registry-mirrors in daemon.json), the path prefix and ns (ctr + hosts.toml). It also
# checks the admin console (open to every address) and the metrics (direct connections only), which share the
# pull port; with IMAGE, also the generated first password and the admin command against the running service.
#
# Usage: scripts/isolated-test.sh <linux binary of the relay>
#        IMAGE=<image> scripts/isolated-test.sh     # test the released image itself: uses the image's entrypoint, default configuration path and HEALTHCHECK
# Requires docker (able to run --privileged containers) and jq. Pulls a few test images; afterwards removes all
# the containers, networks, volumes and newly pulled images.
# Note: this uses up the Docker Hub anonymous quota of this machine's egress IP.
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

ok() { printf '  pass  %s\n' "$1"; }
bad() { printf '  FAIL  %s\n' "$1"; fail=1; }

work="$(mktemp -d)"
before="$work/images-before"
docker images --format '{{.Repository}}:{{.Tag}}@{{.ID}}' | sort >"$before"

cleanup() {
  docker rm -fv "$RELAY" "$RELAY-w" "$P-classic" "$P-ctrd" >/dev/null 2>&1 || true
  docker volume rm "$P-data" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  docker images --format '{{.Repository}}:{{.Tag}}@{{.ID}}' | sort | comm -13 "$before" - | cut -d@ -f1 |
    xargs -r docker rmi >/dev/null 2>&1 || true
  docker image prune -f >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

for img in "$SK" "$CR" "$DIND"; do docker pull -q "$img" >/dev/null; done
[ -z "$IMAGE" ] && docker pull -q "$BASE" >/dev/null

echo "Starting the relay (read-only root file system, all capabilities dropped, non-root)"
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

# Two Dockers: classic storage and containerd storage. Both use the single-domain entrance as registry-mirrors,
# and every entrance is allowed over plain HTTP
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

echo "One domain per upstream: both Dockers, skopeo (full download), crane"
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
    [ "$got" = "$want" ] && ok "$d $ref" || bad "$d $ref: '$got', upstream '$want'"
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

echo "Single-domain entrance: path prefix"
for up in docker.io/library/alpine:3.22 gcr.io/distroless/static:nonroot ghcr.io/containerd/busybox:1.36 registry.k8s.io/pause:3.10 quay.io/prometheus/busybox:latest; do
  want="$(direct "$up")"
  got="$(dpull classic "$RELAY:5000/$up")"
  [ "$got" = "$want" ] && ok "Prefix $RELAY:5000/$up" || bad "Prefix $RELAY:5000/$up: '$got', upstream '$want'"
done

echo "Single-domain entrance: root path (registry-mirrors in daemon.json)"
want="$(direct docker.io/library/busybox:1.37)"
for d in classic ctrd; do
  got="$(dpull "$d" busybox:1.37)"
  [ "$got" = "$want" ] && ok "$d docker pull busybox:1.37" || bad "$d docker pull busybox:1.37: '$got', upstream '$want'"
done

echo "Single-domain entrance: containerd ns (ctr + hosts.toml)"
docker exec "$P-ctrd" sh -c "mkdir -p /etc/containerd/certs.d/ghcr.io && printf 'server = \"https://ghcr.io\"\n[host.\"http://$RELAY:5000\"]\n  capabilities = [\"pull\", \"resolve\"]\n' > /etc/containerd/certs.d/ghcr.io/hosts.toml"
want="$(direct ghcr.io/containerd/busybox:1.36)"
if docker exec "$P-ctrd" ctr --address /var/run/docker/containerd/containerd.sock -n relaytest images pull --local --hosts-dir /etc/containerd/certs.d ghcr.io/containerd/busybox:1.36 >/dev/null 2>&1; then
  got="$(docker exec "$P-ctrd" ctr --address /var/run/docker/containerd/containerd.sock -n relaytest images ls | awk '/ghcr.io\/containerd\/busybox:1.36/ {print $3}')"
  [ "$got" = "$want" ] && ok "ctr ns=ghcr.io $got" || bad "ctr ns=ghcr.io: '$got', upstream '$want'"
else
  bad "ctr pull through ns failed"
fi

echo "Access log"
sleep 2
log="$work/logs/access.log"
total="$(wc -l <"$log")"
redirects="$(jq -s '[.[] | select(.status >= 300 and .status < 400)] | length' "$log")"
errors="$(jq -s '[.[] | select(.status >= 500)] | length' "$log")"
ns="$(jq -s '[.[] | select(.request.uri | contains("ns=ghcr.io"))] | length' "$log")"
hops="$(jq -r '[.redirect1, .redirect2, .redirect3] | map(select(. != null)) | join(" > ")' "$log" | grep -v '^$' | sed -E 's/[0-9a-f]{20,}/X/g' | sort | uniq -c | sort -rn)"
[ "$redirects" = 0 ] && ok "$total requests, no redirect handed to the client" || bad "$redirects redirects were handed to the client"
[ "$errors" = 0 ] && ok "No 5xx" || bad "$errors 5xx responses"
[ "$ns" -gt 0 ] && ok "Received $ns requests with ns=ghcr.io" || bad "Received no requests with ns"
echo "  Redirects followed on the server side:"
printf '%s\n' "$hops" | sed 's/^/    /'

echo "Admin console and metrics (sharing 5000 with pulls)"
# The console is public by default: a direct connection and one with X-Forwarded-For (as if through a
# reverse proxy) can both open the sign-in page. Metrics have no sign-in and are served only to direct connections:
# with X-Forwarded-For the answer is 404.
rip="$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" "$RELAY")"
mg="$(docker run --rm --network "$NET" --entrypoint sh "$CR" -c "
  wget -q -O /dev/null http://$rip:5000/admin/login && echo direct=200 || echo direct=fail
  wget -q -O /dev/null --header 'X-Forwarded-For: 198.51.100.7' http://$rip:5000/admin/login && echo proxied=200 || echo proxied=fail
  wget -q -O - http://$rip:5000/metrics | grep -q '^relay_' && echo dmetrics=200 || echo dmetrics=fail
  wget -q -O /dev/null --header 'X-Forwarded-For: 198.51.100.7' http://$rip:5000/metrics && echo pmetrics=200 || echo pmetrics=fail" 2>/dev/null)"
case "$mg" in *direct=200*) ok "Direct connection opens the console" ;; *) bad "Direct connection cannot open the console: $mg" ;; esac
case "$mg" in *proxied=200*) ok "Proxied request (with X-Forwarded-For) also opens the console sign-in page" ;; *) bad "Proxied request cannot open the console: $mg" ;; esac
case "$mg" in *dmetrics=200*) ok "Direct connection can scrape the metrics" ;; *) bad "Cannot scrape the metrics: $mg" ;; esac
case "$mg" in *pmetrics=fail*) ok "Metrics request with X-Forwarded-For gets 404" ;; *) bad "Proxied metrics request was not rejected: $mg" ;; esac

if [ -n "$IMAGE" ]; then
  echo "Image"
  # HEALTHCHECK runs probe without arguments: it finds the port through the configuration path in the image
  for _ in $(seq 1 40); do
    st="$(docker inspect -f '{{.State.Health.Status}}' "$RELAY")"
    [ "$st" = starting ] || break
    sleep 1
  done
  if [ "$st" = healthy ]; then ok "HEALTHCHECK: $st"; else bad "HEALTHCHECK: $st"; fi
  uid="$(docker exec "$RELAY" "$RBIN" version >/dev/null && docker inspect -f '{{.Config.User}}' "$RELAY")"
  if [ "$uid" = 65532:65532 ]; then ok "Runs as nonroot ($uid)"; else bad "Runs as user: $uid"; fi

  echo "First administrator and the admin command (the image's own configuration, writable data)"
  docker run -d --name "$RELAY-w" --read-only --cap-drop ALL --security-opt no-new-privileges \
    -v "$P-data:/var/lib/registry-mirrors-relay" "$IMAGE" >/dev/null
  for _ in $(seq 1 30); do docker exec "$RELAY-w" "$RBIN" probe 2>/dev/null && break; sleep 1; done
  if docker logs "$RELAY-w" 2>&1 | grep -q 'generated password'; then ok "First start logs a generated password"; else bad "No generated password in the log"; fi
  pw="$(docker exec "$RELAY-w" registry-mirrors-relay admin password --generate 2>/dev/null || true)"
  if [ "${#pw}" -ge 20 ] && docker exec "$RELAY-w" registry-mirrors-relay admin list 2>/dev/null | grep -q '^admin  admin  active'; then
    ok "admin password works against the running service through docker exec"
  else
    bad "admin password through docker exec failed"
  fi
  docker rm -fv "$RELAY-w" >/dev/null && docker volume rm "$P-data" >/dev/null
fi

if [ "$fail" = 0 ]; then echo "All checks passed"; else echo "Some checks failed"; fi
exit "$fail"
