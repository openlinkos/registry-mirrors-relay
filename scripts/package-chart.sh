#!/usr/bin/env bash
# 打包 Helm chart（release.yml 发布时用，chart-check.sh 也用它检查）：
# 版本号取发布标签去掉 v，appVersion 就是标签；镜像 digest 写进 Chart.yaml 的注解 openlinkos.org/image-digest，
# 用户没指定 image.tag、image.digest 时就按 digest 拉取，拉到的一定是同一次发布签名的镜像。
# 用法：scripts/package-chart.sh <标签，如 v0.1.1> <镜像 digest> <输出目录>
# 需要 helm 和 yq（v4），路径可用 HELM、YQ 覆盖。
set -euo pipefail

tag="$1" digest="$2" out="$3"
HELM="${HELM:-helm}"
YQ="${YQ:-yq}"
case "$tag" in v[0-9]*) ;; *) echo "标签要以 v 开头：$tag" >&2; exit 2 ;; esac
case "$digest" in sha256:*) ;; *) echo "digest 要以 sha256: 开头：$digest" >&2; exit 2 ;; esac

src="$(mktemp -d)"
trap 'rm -rf "$src"' EXIT
cp -R charts/registry-mirrors-relay "$src/"
DIGEST="$digest" "$YQ" -i '.annotations["openlinkos.org/image-digest"] = strenv(DIGEST)' "$src/registry-mirrors-relay/Chart.yaml"
mkdir -p "$out"
"$HELM" package "$src/registry-mirrors-relay" --version "${tag#v}" --app-version "$tag" -d "$out"
