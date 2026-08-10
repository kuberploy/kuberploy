#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_assert_orbstack_context
kp_require_tools helm kubectl
kp_run_id="${1:?usage: cleanup-run.sh <run-id>}"
kp_namespace="$(kp_require_run_namespace "${kp_run_id}")"

printf 'Inventory for exact cleanup target %s:\n' "${kp_namespace}"
kubectl get all,ingress,networkpolicy,pvc -n "${kp_namespace}" \
  -l "kuberploy.io/test-run=${kp_run_id}" --ignore-not-found

for kp_component in kuberploy local-deps local-git traefik argocd; do
  kp_release="$(kp_release_name "${kp_component}" "${kp_run_id}")"
  if helm status "${kp_release}" -n "${kp_namespace}" >/dev/null 2>&1; then
    helm uninstall "${kp_release}" -n "${kp_namespace}" --wait
  fi
done

kubectl delete namespace "${kp_namespace}" --wait=true
printf 'Deleted exact run namespace %s. Shared CRDs were intentionally preserved.\n' "${kp_namespace}"
