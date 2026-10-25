#!/usr/bin/env bash
# Smoke test: checks that every upstream pulls correctly, that layers are downloaded through this service, that
# the entrances let only pulls through, that the admin console answers every address and that metrics accept
# only direct connections.
# Requires docker, curl and jq. Two ways to use it:
#   Local: the relay runs with deploy/test/config.yaml on a port of this machine (default 5000); run
#     scripts/smoke.sh [host IP, default 127.0.0.1] on this machine; change the port with SMOKE_PORT.
#     The entrances with one domain per upstream (docker.relay.test etc.) are tested with curl --resolve; docker
#     pull goes through the path prefix of the single-domain entrance, and the ns and root-path forms are checked too.
#   By domain: reaches docker, gcr, ghcr, k8s and quay.<domain> over HTTPS (usually through a CDN)
#     SMOKE_DOMAIN=example.com scripts/smoke.sh
#     The console's sign-in page should then open, and metrics should be 404 (proxied requests never get them).
# The machine running this script can connect to the upstreams directly, so it cannot catch "the client was
# diverted to a direct connection to the upstream"; that needs scripts/isolated-test.sh.
set -euo pipefail

HOST="${1:-127.0.0.1}"
[ "$HOST" = localhost ] && HOST=127.0.0.1
PORT="${SMOKE_PORT:-5000}"
fail=0

ok() { printf '  pass  %s\n' "$1"; }
bad() { printf '  FAIL  %s\n' "$1"; fail=1; }
RESOLVE=()
cu() { curl "${RESOLVE[@]}" "$@"; }
status() { cu -s -o /dev/null -w '%{http_code}' "$@"; }

# Entrance address and display name of each upstream, in this order: Docker Hub, gcr.io, ghcr.io, registry.k8s.io, quay.io
BASES=()
NAMES=()
if [ -n "${SMOKE_DOMAIN:-}" ]; then
  for sub in docker gcr ghcr k8s quay; do
    BASES+=("https://$sub.$SMOKE_DOMAIN")
    NAMES+=("$sub.$SMOKE_DOMAIN")
  done
else
  for sub in docker gcr ghcr k8s quay; do
    BASES+=("http://$sub.relay.test:$PORT")
    NAMES+=("$sub.relay.test:$PORT")
    RESOLVE+=(--resolve "$sub.relay.test:$PORT:$HOST")
  done
fi
one="$HOST:$PORT" # single-domain entrance in local mode

wait_ready() {
  for _ in $(seq 1 60); do
    [ "$(status "$1/healthz")" = 200 ] && return 0
    sleep 1
  done
  return 1
}

# Upstream index, name to pull through the mirror, name to pull directly from the upstream
CASES=(
  "0 library/hello-world:latest docker.io/library/hello-world:latest"
  "1 distroless/static:nonroot gcr.io/distroless/static:nonroot"
  "2 containerd/busybox:1.36 ghcr.io/containerd/busybox:1.36"
  "3 pause:3.10 registry.k8s.io/pause:3.10"
  "4 coreos/etcd:v3.5.0 quay.io/coreos/etcd:v3.5.0"
)

ACCEPT='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'

# Status code for the first layer of the image (linux/amd64), without following redirects: 200 or 206 means
# this service forwarded it. This service fetches tokens on the server side; the client needs no token.
blob_status() {
  local base=$1 repo=$2 ref=$3 manifest digest
  manifest="$(cu -s -H "Accept: $ACCEPT" "$base/v2/$repo/manifests/$ref")"
  digest="$(jq -r '.manifests[]? | select(.platform.os == "linux" and .platform.architecture == "amd64") | .digest' <<<"$manifest" | head -n 1)"
  if [ -n "$digest" ]; then
    manifest="$(cu -s -H "Accept: $ACCEPT" "$base/v2/$repo/manifests/$digest")"
  fi
  digest="$(jq -r '.layers[0].digest // empty' <<<"$manifest")"
  [ -n "$digest" ] || { echo "no manifest"; return; }
  cu -s -o /dev/null -r 0-1023 -w '%{http_code}' "$base/v2/$repo/blobs/$digest"
}

pull_digest() { docker pull "$1" | sed -n 's/^Digest: //p' || true; }

echo "Pulling and comparing digests"
for c in "${CASES[@]}"; do
  read -r i name upstream <<<"$c"
  if ! wait_ready "${BASES[i]}"; then bad "${NAMES[i]}: not ready"; continue; fi
  # By domain, docker pull uses each upstream's domain; locally docker cannot resolve *.relay.test, so the pull
  # goes through the prefix of the single-domain entrance
  if [ -n "${SMOKE_DOMAIN:-}" ]; then ref="${NAMES[i]}/$name"; else ref="$one/$upstream"; fi
  mirrored="$(pull_digest "$ref")"
  direct="$(pull_digest "$upstream")"
  if [ -n "$mirrored" ] && [ "$mirrored" = "$direct" ]; then
    ok "$ref $mirrored"
  else
    bad "$ref: mirror '${mirrored:-none}', upstream '${direct:-none}'"
  fi
done

echo "Layers downloaded through this service (the upstream CDN addresses are on the allow list)"
for c in "${CASES[@]}"; do
  read -r i name _ <<<"$c"
  s="$(blob_status "${BASES[i]}" "${name%:*}" "${name##*:}")"
  case "$s" in
    200 | 206) ok "${NAMES[i]} ${name%:*} returned $s" ;;
    *) bad "${NAMES[i]} ${name%:*} returned $s: the upstream may have changed its CDN addresses; check redirect1..3 in the access log and extend the allow list" ;;
  esac
done

echo "No authentication challenge, origin egress IP not disclosed"
for i in "${!BASES[@]}"; do
  hdr="$(cu -s -o /dev/null -D - "${BASES[i]}/v2/" | tr -d '\r')"
  if grep -qi '^www-authenticate' <<<"$hdr"; then
    bad "${NAMES[i]} /v2/ sent an authentication challenge"
  else
    ok "${NAMES[i]} /v2/ requires no authentication"
  fi
done
read -r _ name _ <<<"${CASES[0]}"
hdr="$(cu -s -o /dev/null -D - -H "Accept: $ACCEPT" "${BASES[0]}/v2/${name%:*}/manifests/${name##*:}" | tr -d '\r')"
if grep -qi -E '^(docker-ratelimit-source|set-cookie)' <<<"$hdr"; then
  bad "${NAMES[0]} response carries Docker-Ratelimit-Source or Set-Cookie"
else
  ok "${NAMES[0]} response carries neither Docker-Ratelimit-Source nor Set-Cookie"
fi

echo "Entrance rules"
for i in "${!BASES[@]}"; do
  base="${BASES[i]}" n="${NAMES[i]}"
  s="$(status -X POST "$base/v2/smoke/blobs/uploads/")"
  [ "$s" = 405 ] && ok "$n POST returned 405" || bad "$n POST returned $s, expected 405"
  s="$(status -X DELETE "$base/v2/smoke/manifests/latest")"
  [ "$s" = 405 ] && ok "$n DELETE returned 405" || bad "$n DELETE returned $s, expected 405"
  s="$(status "$base/v2/_catalog")"
  [ "$s" = 404 ] && ok "$n /v2/_catalog returned 404" || bad "$n /v2/_catalog returned $s, expected 404"
done

echo "Upstream isolation"
for i in 1 2 3 4; do
  s="$(status -H "Accept: $ACCEPT" "${BASES[i]}/v2/library/hello-world/manifests/latest")"
  case "$s" in
    2*) bad "${NAMES[i]} returned Docker Hub's library/hello-world ($s)" ;;
    *) ok "${NAMES[i]} request for library/hello-world returned $s" ;;
  esac
  s="$(status -H "Accept: $ACCEPT" "${BASES[i]}/v2/library/hello-world/manifests/latest?ns=docker.io")"
  [ "$s" = 404 ] && ok "${NAMES[i]} ns=docker.io returned 404" || bad "${NAMES[i]} ns=docker.io returned $s, expected 404"
done

if [ -z "${SMOKE_DOMAIN:-}" ]; then
  echo "Single-domain entrance ($one): ns and root path (the path prefix was already tested by the pulls above)"
  s="$(status -I -H "Accept: $ACCEPT" "http://$one/v2/containerd/busybox/manifests/1.36?ns=ghcr.io")"
  [ "$s" = 200 ] && ok "ns=ghcr.io returned 200" || bad "ns=ghcr.io returned $s, expected 200"
  s="$(status -I -H "Accept: $ACCEPT" "http://$one/v2/hello-world/manifests/latest")"
  [ "$s" = 200 ] && ok "Root path (Docker Hub by default, library/ added) returned 200" || bad "Root path returned $s, expected 200"
  s="$(status -I -H "Accept: $ACCEPT" "http://$one/v2/example.org/x/manifests/latest")"
  [ "$s" = 404 ] && ok "Unconfigured prefix returned 404" || bad "Unconfigured prefix returned $s, expected 404"
fi

echo "Usage page and health probe"
for i in "${!BASES[@]}"; do
  ct="$(cu -s -o /dev/null -w '%{http_code} %{content_type}' "${BASES[i]}/")"
  case "$ct" in
    "200 text/html"*) ok "${NAMES[i]} / returned the usage page" ;;
    *) bad "${NAMES[i]} / returned '$ct', expected 200 text/html" ;;
  esac
  s="$(status "${BASES[i]}/healthz")"
  [ "$s" = 200 ] && ok "${NAMES[i]} /healthz returned 200" || bad "${NAMES[i]} /healthz returned $s, expected 200"
done
s="$(status "${BASES[0]}/robots.txt")"
[ "$s" = 200 ] && ok "/robots.txt returned 200" || bad "/robots.txt returned $s, expected 200"
css="$(cu -s "${BASES[0]}/" | grep -o '/assets/ui\.[0-9a-f]*\.css' | head -n 1 || true)"
ct="$(cu -s -o /dev/null -w '%{http_code} %{content_type}' "${BASES[0]}${css:-/assets/missing.css}")"
case "$ct" in
  "200 text/css"*) ok "the usage page's stylesheet ($css) is served" ;;
  *) bad "the usage page's stylesheet ${css:-(not referenced)} returned '$ct', expected 200 text/css" ;;
esac

echo "Admin console and metrics"
if [ -n "${SMOKE_DOMAIN:-}" ]; then
  # The console is public by default; metrics through a CDN or reverse proxy are treated as if they did not exist
  for i in "${!BASES[@]}"; do
    s="$(status "${BASES[i]}/admin/login")"
    [ "$s" = 200 ] && ok "${NAMES[i]}/admin/login returned 200" || bad "${NAMES[i]}/admin/login returned $s, expected 200"
    s="$(status "${BASES[i]}/metrics")"
    [ "$s" = 404 ] && ok "${NAMES[i]}/metrics returned 404" || bad "${NAMES[i]}/metrics returned $s, expected 404 when proxied"
  done
else
  s="$(status "http://$one/admin/login")"
  [ "$s" = 200 ] && ok "Direct $one/admin/login returned 200" || bad "Direct $one/admin/login returned $s, expected 200"
  s="$(status -H 'X-Forwarded-For: 198.51.100.7' "http://$one/admin/login")"
  [ "$s" = 200 ] && ok "With X-Forwarded-For the sign-in page still opens (public console)" || bad "With X-Forwarded-For returned $s, expected 200"
  s="$(status "http://$one/metrics")"
  [ "$s" = 200 ] && ok "Direct $one/metrics returned 200 (the test configuration enables metrics)" || bad "Direct $one/metrics returned $s, expected 200"
  s="$(status -H 'X-Forwarded-For: 198.51.100.7' "http://$one/metrics")"
  [ "$s" = 404 ] && ok "Metrics with X-Forwarded-For returned 404" || bad "Metrics with X-Forwarded-For returned $s, expected 404"
fi

if [ "$fail" = 0 ]; then echo "All checks passed"; else echo "Some checks failed"; fi
exit "$fail"
