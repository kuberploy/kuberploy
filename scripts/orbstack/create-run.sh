#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_assert_orbstack_context
kp_require_tools kubectl
kp_run_id="${1:?usage: create-run.sh <run-id>}"
kp_namespace="$(kp_run_namespace "${kp_run_id}")"

kubectl create namespace "${kp_namespace}" --dry-run=client -o yaml | kubectl apply -f -
kubectl label namespace "${kp_namespace}" --overwrite \
  "kuberploy.io/test-run=${kp_run_id}" \
  "app.kubernetes.io/managed-by=kuberploy-e2e" \
  "pod-security.kubernetes.io/enforce=restricted" \
  "pod-security.kubernetes.io/audit=restricted" \
  "pod-security.kubernetes.io/warn=restricted"

printf 'Created or verified run namespace %s.\n' "${kp_namespace}"
