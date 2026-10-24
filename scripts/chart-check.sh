#!/usr/bin/env bash
# Helm chart 的检查（CI 的 chart 任务；本地有这些工具时也能跑）：
#   1. helm lint（Helm 4，另给 HELM3 时再用 Helm 3 跑一遍）；
#   2. ci/、test-values/ 的每组 values 都渲染出来，按 k8s 1.30 和 1.37 用 kubeconform 严格校验；
#      渲染出的 config.yaml 再用 registry-mirrors-relay check 校验；
#   3. test-values/fail/ 的每一组都必须渲染失败；
#   4. 和 kustomize 的写法比对关键字段（Pod 模板、工作负载、配置、端口、PDB、网络策略、HTTPRoute、ServiceMonitor），
#      防止两套部署方式走样；
#   5. 两份 Grafana 面板一致；appVersion 和 kustomize 的镜像标签一致；按发布的做法打包后，默认按 digest 拉镜像；
#   6. helm-docs 生成的 README 和仓库里的一致。
#
# 需要：helm、kubeconform、yq（v4）、kubectl（渲染 kustomize）、helm-docs、go。工具路径可用环境变量覆盖：
#   HELM HELM3 KUBECONFORM YQ KUBECTL HELM_DOCS
set -euo pipefail

CHART=charts/registry-mirrors-relay
HELM="${HELM:-helm}"
HELM3="${HELM3:-}"
KUBECONFORM="${KUBECONFORM:-kubeconform}"
YQ="${YQ:-yq}"
KUBECTL="${KUBECTL:-kubectl}"
HELM_DOCS="${HELM_DOCS:-helm-docs}"
# CRD 的 JSON Schema（HTTPRoute、ServiceMonitor），按提交固定
CRDS="https://raw.githubusercontent.com/datreeio/CRDs-catalog/501527ab9c40c827144c8f45945b25a023e344ce/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json"

fail=0
ok() { printf '  通过  %s\n' "$1"; }
bad() { printf '  失败  %s\n' "$1"; fail=1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

echo "构建 registry-mirrors-relay（校验渲染出的配置）"
go build -o "$work/relay" ./cmd/registry-mirrors-relay

echo "helm lint"
for h in "$HELM" ${HELM3:+"$HELM3"}; do
  if "$h" lint --strict "$CHART" >"$work/lint" 2>&1; then ok "$("$h" version --short)"; else bad "$h lint"; cat "$work/lint"; fi
done

echo "渲染并校验"
for f in "$CHART"/ci/*.yaml "$CHART"/test-values/*.yaml; do
  for h in "$HELM" ${HELM3:+"$HELM3"}; do
    for kv in 1.30.0 1.37.0; do
      if ! "$h" template t "$CHART" -n relay -f "$f" --kube-version "$kv" >"$work/out.yaml" 2>"$work/err"; then
        bad "$f（$kv）：$(head -c 300 "$work/err")"
        continue
      fi
      if ! "$KUBECONFORM" -strict -summary -kubernetes-version "$kv" -schema-location default -schema-location "$CRDS" \
        "$work/out.yaml" >"$work/kc" 2>&1; then
        bad "$f（$kv）kubeconform"
        cat "$work/kc"
      fi
    done
  done
  "$YQ" 'select(.kind == "ConfigMap" and (.metadata.name | test("-config$"))) | .data["config.yaml"]' "$work/out.yaml" >"$work/config.yaml"
  # 配置里引用的环境变量用占位值，只检查写法
  if DOCKERHUB_TOKEN=placeholder "$work/relay" check -config "$work/config.yaml" >"$work/check" 2>&1; then
    ok "$f"
  else
    bad "$f 的配置：$(cat "$work/check")"
  fi
done

echo "应当渲染失败的写法"
for f in "$CHART"/test-values/fail/*.yaml; do
  if "$HELM" template t "$CHART" -f "$f" >/dev/null 2>&1; then bad "$f 不应通过"; else ok "$(basename "$f")"; fi
done

echo "和 kustomize 比对"
# 同名渲染：fullnameOverride 和 kustomize 的资源名一致
chart() { "$HELM" template r "$CHART" --set fullnameOverride=registry-mirrors-relay "$@"; }
kz() { "$KUBECTL" kustomize "$1"; }
# Pod 模板：去掉镜像、拉取策略、ConfigMap 名（kustomize 带哈希）、拓扑分布的 selector（chart 多一个 instance 标签）和注释
podspec='select(.kind == env(KIND)) | .spec.template.spec
  | del(.containers[].image) | del(.containers[].imagePullPolicy)
  | del(.volumes[] | select(.name == "config") | .configMap.name)
  | del(.topologySpreadConstraints[].labelSelector)
  | ... comments="" | ... style="" | sort_keys(..)'
workload='select(.kind == env(KIND)) | .spec | del(.template) | del(.selector) | del(.serviceName)
  | del(.volumeClaimTemplates[].metadata.labels) | ... comments="" | ... style="" | sort_keys(..)'
config='select(.kind == "ConfigMap" and (.metadata.name | test("-config"))) | .data["config.yaml"] | from_yaml | ... comments="" | ... style="" | sort_keys(..)'
ports='[select(.kind == "Service") | .spec.ports[] | {"name": .name, "port": .port, "targetPort": .targetPort}] | sort_by(.name)'
compare() { # 名字 jq表达式 kustomize渲染 chart渲染
  KIND="${5:-}" "$YQ" ea "$2" "$3" >"$work/a" && KIND="${5:-}" "$YQ" ea "$2" "$4" >"$work/b"
  if diff -u "$work/a" "$work/b" >"$work/diff"; then ok "$1"; else bad "$1"; cat "$work/diff"; fi
}
kz deploy/kubernetes/base >"$work/k-base.yaml"
chart >"$work/c-base.yaml"
compare "base：Pod 模板" "$podspec" "$work/k-base.yaml" "$work/c-base.yaml" Deployment
compare "base：Deployment" "$workload" "$work/k-base.yaml" "$work/c-base.yaml" Deployment
compare "base：配置" "$config" "$work/k-base.yaml" "$work/c-base.yaml"
compare "base：端口（chart 把指标拆成单独的 Service）" "$ports" "$work/k-base.yaml" "$work/c-base.yaml"
compare "base：PDB" 'select(.kind == "PodDisruptionBudget") | .spec.minAvailable' "$work/k-base.yaml" "$work/c-base.yaml"
kz deploy/kubernetes/overlays/single-writable >"$work/k-w.yaml"
chart --set mode=writable >"$work/c-w.yaml"
compare "single-writable：Pod 模板" "$podspec" "$work/k-w.yaml" "$work/c-w.yaml" StatefulSet
compare "single-writable：StatefulSet" "$workload" "$work/k-w.yaml" "$work/c-w.yaml" StatefulSet
compare "single-writable：配置" "$config" "$work/k-w.yaml" "$work/c-w.yaml"
kz deploy/kubernetes/overlays/networkpolicy >"$work/k-np.yaml"
chart --set networkPolicy.enabled=true >"$work/c-np.yaml"
compare "networkpolicy" 'select(.kind == "NetworkPolicy") | .spec | del(.podSelector) | ... comments="" | ... style="" | sort_keys(..)' "$work/k-np.yaml" "$work/c-np.yaml"
kz deploy/kubernetes/overlays/gateway-api >"$work/k-gw.yaml"
chart -f "$CHART"/test-values/gateway.yaml >"$work/c-gw.yaml"
compare "gateway-api：HTTPRoute 规则" 'select(.kind == "HTTPRoute") | .spec.rules | ... comments="" | ... style="" | sort_keys(..)' "$work/k-gw.yaml" "$work/c-gw.yaml"
kz deploy/kubernetes/overlays/monitoring >"$work/k-mon.yaml"
chart -f "$CHART"/test-values/monitoring.yaml >"$work/c-mon.yaml"
compare "monitoring：ServiceMonitor" 'select(.kind == "ServiceMonitor") | .spec.endpoints | ... comments="" | ... style="" | sort_keys(..)' "$work/k-mon.yaml" "$work/c-mon.yaml"
if cmp -s deploy/kubernetes/overlays/monitoring/dashboard.json "$CHART"/files/dashboard.json; then ok "Grafana 面板"; else bad "两份 dashboard.json 不一致"; fi
app="$("$YQ" '.appVersion' "$CHART"/Chart.yaml)"
tag="$("$YQ" '.images[0].newTag' deploy/kubernetes/base/kustomization.yaml)"
if [ "$app" = "$tag" ]; then ok "appVersion = kustomize 镜像标签（$app）"; else bad "appVersion $app ≠ kustomize 镜像标签 $tag"; fi

echo "发布打包（scripts/package-chart.sh）"
d0="sha256:$(printf '%064d' 0)"
if HELM="$HELM" YQ="$YQ" scripts/package-chart.sh v9.9.9-rc.1 "$d0" "$work/pkg" >/dev/null; then
  img="$("$HELM" template t "$work/pkg/registry-mirrors-relay-9.9.9-rc.1.tgz" | "$YQ" 'select(.kind == "Deployment") | .spec.template.spec.containers[0].image')"
  want="ghcr.io/openlinkos/registry-mirrors-relay:v9.9.9-rc.1@$d0"
  if [ "$img" = "$want" ]; then ok "打包后默认按 digest 拉镜像"; else bad "打包后的镜像是 $img，应为 $want"; fi
else
  bad "打包失败"
fi

echo "helm-docs"
cp "$CHART"/README.md "$work/README.before"
"$HELM_DOCS" --chart-search-root charts --template-files README.md.gotmpl >/dev/null
if cmp -s "$work/README.before" "$CHART"/README.md; then ok "README 是最新的"; else bad "README 过期：运行 helm-docs 后提交"; diff -u "$work/README.before" "$CHART"/README.md | head -40; fi

if [ "$fail" = 0 ]; then echo "全部通过"; else echo "有检查失败"; fi
exit "$fail"
