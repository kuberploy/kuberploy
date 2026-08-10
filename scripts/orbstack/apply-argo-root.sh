#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_assert_orbstack_context
kp_require_tools kubectl sed
kp_run_id="${1:?usage: apply-argo-root.sh <run-id>}"
kp_namespace="$(kp_require_run_namespace "${kp_run_id}")"
kp_root="$(kp_repo_root)"
kp_git_url="git://kuberploy-git.${kp_namespace}.svc.cluster.local/kuberploy-environments.git"
kp_stage="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-root-${kp_run_id}.XXXXXX")"

kp_remove_stage() {
  [[ -n "${kp_stage:-}" && "${kp_stage}" == *"/kuberploy-root-${kp_run_id}."* ]] && rm -rf -- "${kp_stage}"
}
trap kp_remove_stage EXIT

for kp_manifest in appproject root-application; do
  sed \
    -e "s|@@RUN_ID@@|${kp_run_id}|g" \
    -e "s|@@RUN_NAMESPACE@@|${kp_namespace}|g" \
    -e "s|@@GIT_URL@@|${kp_git_url}|g" \
    "${kp_root}/deploy/orbstack/argo-bootstrap/${kp_manifest}.yaml.tmpl" \
    > "${kp_stage}/${kp_manifest}.yaml"
done

kubectl apply -f "${kp_stage}/appproject.yaml"
kubectl apply -f "${kp_stage}/root-application.yaml"
printf 'Applied the one bootstrap root Application for run %s; Argo owns subsequent workload writes.\n' "${kp_run_id}"
