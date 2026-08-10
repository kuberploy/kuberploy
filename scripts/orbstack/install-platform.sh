#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_assert_orbstack_context
kp_require_tools helm kubectl yq
kp_run_id="${1:?usage: install-platform.sh <run-id> <values-file>}"
kp_values_file="${2:?usage: install-platform.sh <run-id> <values-file>}"
kp_namespace="$(kp_require_run_namespace "${kp_run_id}")"
kp_root="$(kp_repo_root)"
kp_release="$(kp_release_name kuberploy "${kp_run_id}")"

[[ -f "${kp_values_file}" ]] || kp_die "values file does not exist: ${kp_values_file}"
kp_postgresql_secret="$(yq -r '.config.postgresql.secretRef.name' "${kp_values_file}")"
kp_valkey_secret="$(yq -r '.config.valkey.secretRef.name' "${kp_values_file}")"
for kp_secret in "${kp_postgresql_secret}" "${kp_valkey_secret}"; do
  kubectl get secret "${kp_secret}" -n "${kp_namespace}" >/dev/null 2>&1 || \
    kp_die "required external connection Secret is missing in ${kp_namespace}: ${kp_secret}"
done

# Reusing the same release performs an in-place control-plane upgrade. The
# chart never owns Argo Applications or tenant workloads, so this cannot prune
# an application namespace. PostgreSQL startup migrations use an advisory lock.
helm upgrade --install "${kp_release}" "${kp_root}/charts/kuberploy" \
  --namespace "${kp_namespace}" \
  --values "${kp_values_file}" \
  --set-string "global.testRun=${kp_run_id}" \
  --rollback-on-failure --cleanup-on-fail --wait --timeout 10m --history-max 10

printf 'Installed/upgraded only the Kuberploy control plane release %s. Argo-managed tenant workloads were not Helm targets.\n' "${kp_release}"
