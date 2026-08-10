#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_assert_orbstack_context
kp_require_tools helm jq kubectl

readonly KP_REQUIRED_HELM="v4.2.3"
kp_helm_version="$(helm version --template '{{.Version}}')"
[[ "${kp_helm_version}" == "${KP_REQUIRED_HELM}" ]] || \
  kp_die "Helm ${KP_REQUIRED_HELM} is locked; found ${kp_helm_version}"

kp_server_version="$(kubectl version -o json | jq -r '.serverVersion.gitVersion')"
[[ "${kp_server_version}" =~ ^v1\.(34|35|36)([.+-]|$) ]] || \
  kp_die "Kubernetes ${kp_server_version} is outside the locked 1.34-1.36 support window"

for kp_api in \
  "apps/v1" \
  "networking.k8s.io/v1" \
  "admissionregistration.k8s.io/v1"; do
  kubectl api-versions | grep -Fx "${kp_api}" >/dev/null || \
    kp_die "required API is unavailable: ${kp_api}"
done

kubectl get storageclass -o custom-columns='NAME:.metadata.name,DEFAULT:.metadata.annotations.storageclass\.kubernetes\.io/is-default-class,PROVISIONER:.provisioner'
kubectl get nodes -o custom-columns='NAME:.metadata.name,VERSION:.status.nodeInfo.kubeletVersion,ARCH:.status.nodeInfo.architecture,RUNTIME:.status.nodeInfo.containerRuntimeVersion'

printf 'OrbStack preflight passed (context=%s, kubernetes=%s, helm=%s).\n' \
  "${KP_EXPECTED_CONTEXT}" "${kp_server_version}" "${kp_helm_version}"
