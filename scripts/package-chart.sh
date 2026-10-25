#!/usr/bin/env bash
# Packages the Helm chart (used by release.yml when publishing; chart-check.sh also uses it for checks):
# the version is the release tag without the v and appVersion is the tag itself; the image digest is written
# into the Chart.yaml annotation openlinkos.org/image-digest, so when the user sets neither image.tag nor
# image.digest the image is pulled by digest, and what gets pulled is always the image signed in the same release.
# Usage: scripts/package-chart.sh <tag, e.g. v0.1.1> <image digest> <output directory>
# Requires helm and yq (v4); their paths can be overridden with HELM and YQ.
set -euo pipefail

tag="$1" digest="$2" out="$3"
HELM="${HELM:-helm}"
YQ="${YQ:-yq}"
case "$tag" in v[0-9]*) ;; *) echo "Tag must start with v: $tag" >&2; exit 2 ;; esac
case "$digest" in sha256:*) ;; *) echo "Digest must start with sha256: $digest" >&2; exit 2 ;; esac

src="$(mktemp -d)"
trap 'rm -rf "$src"' EXIT
cp -R charts/registry-mirrors-relay "$src/"
DIGEST="$digest" "$YQ" -i '.annotations["openlinkos.org/image-digest"] = strenv(DIGEST)' "$src/registry-mirrors-relay/Chart.yaml"
mkdir -p "$out"
"$HELM" package "$src/registry-mirrors-relay" --version "${tag#v}" --app-version "$tag" -d "$out"
