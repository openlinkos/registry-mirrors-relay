#!/usr/bin/env bash
# Tools for CI and releases; versions and checksums are all pinned here (GitHub's ubuntu runner, linux/amd64):
#   kubectl  kubectl
#   kind     kind
#   helm     Helm 4 (installed as helm4) and Helm 3 (installed as helm: chart-testing only supports Helm 3)
#   chart    kubeconform, yq, helm-docs (go install; Go's checksum database guarantees the contents)
# Usage: scripts/ci-tools.sh kubectl helm chart
set -euo pipefail

get() { # URL sha256 file-name-to-save-as
  curl -fsSLo "$3" "$1"
  echo "$2  $3" | sha256sum -c - >/dev/null
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
for t in "$@"; do
  case "$t" in
    kubectl)
      get https://dl.k8s.io/release/v1.37.0/bin/linux/amd64/kubectl 6129359f4e1f3848a5572ccb0b26cf28b8ca08cef38c95a765b2f64a2c961a2f kubectl
      sudo install -m 0755 kubectl /usr/local/bin/
      ;;
    kind)
      get https://github.com/kubernetes-sigs/kind/releases/download/v0.33.0/kind-linux-amd64 aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d kind
      sudo install -m 0755 kind /usr/local/bin/
      ;;
    helm)
      get https://get.helm.sh/helm-v4.3.1-linux-amd64.tar.gz 0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b helm4.tar.gz
      get https://get.helm.sh/helm-v3.22.1-linux-amd64.tar.gz 896cf63684a42a723d61c980369d6e588330927deb13676ca0d85006dd6da0ea helm3.tar.gz
      mkdir h4 h3
      tar -xzf helm4.tar.gz -C h4 --strip-components=1 linux-amd64/helm
      tar -xzf helm3.tar.gz -C h3 --strip-components=1 linux-amd64/helm
      sudo install -m 0755 h4/helm /usr/local/bin/helm4
      sudo install -m 0755 h3/helm /usr/local/bin/helm
      ;;
    chart)
      go install github.com/yannh/kubeconform/cmd/kubeconform@v0.8.0
      go install github.com/mikefarah/yq/v4@v4.54.1
      go install github.com/norwoodj/helm-docs/cmd/helm-docs@v1.14.2
      ;;
    *)
      echo "Unknown tool: $t" >&2
      exit 2
      ;;
  esac
done
