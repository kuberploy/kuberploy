#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_initialize
kp_namespace="$(kp_run_namespace)"
kp_namespace_json="$(kp_kubectl get namespace "${kp_namespace}" \
  --ignore-not-found -o json)"

if [[ -z "${kp_namespace_json}" ]]; then
  printf 'No run namespace exists; cleanup is complete.\n'
  exit 0
fi

kp_assert_owned_namespace_json "${kp_namespace_json}"
kp_kubectl delete namespace "${kp_namespace}" \
  --wait=true --timeout=180s >/dev/null

kp_remaining="$(kp_kubectl get namespace "${kp_namespace}" \
  --ignore-not-found -o name)"
[[ -z "${kp_remaining}" ]] || \
  kp_die "run namespace still exists after cleanup"

printf 'Owned run namespace deleted.\n'
