#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_assert_orbstack_context
kp_require_tools curl helm kubectl sed
kp_run_id="${1:?usage: platform-up.sh <run-id>}"
kp_namespace="$(kp_require_run_namespace "${kp_run_id}")"
kp_root="$(kp_repo_root)"
kp_bootstrap_token="kp_bootstrap_${kp_run_id}_test_only"
kp_values="$(mktemp "${TMPDIR:-/tmp}/kuberploy-platform-${kp_run_id}.XXXXXX.yaml")"

kp_remove_values() {
  [[ -n "${kp_values:-}" && "${kp_values}" == *"/kuberploy-platform-${kp_run_id}."*.yaml ]] && rm -f -- "${kp_values}"
}
trap kp_remove_values EXIT

for kp_prerequisite in local-deps local-git traefik argocd; do
  kp_prerequisite_release="$(kp_release_name "${kp_prerequisite}" "${kp_run_id}")"
  helm status "${kp_prerequisite_release}" -n "${kp_namespace}" >/dev/null 2>&1 || \
    kp_die "required release is missing: ${kp_prerequisite_release}"
done

"$(dirname "${BASH_SOURCE[0]}")/build-local-platform-images.sh" "${kp_run_id}"

kubectl create secret generic kuberploy-bootstrap -n "${kp_namespace}" \
  --from-literal="token=${kp_bootstrap_token}" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl label secret kuberploy-bootstrap -n "${kp_namespace}" --overwrite \
  "kuberploy.io/test-run=${kp_run_id}"

sed \
  -e "s|@@RUN_ID@@|${kp_run_id}|g" \
  -e "s|@@RUN_NAMESPACE@@|${kp_namespace}|g" \
  "${kp_root}/test/e2e/fixtures/platform-live-values.yaml.tmpl" > "${kp_values}"

"$(dirname "${BASH_SOURCE[0]}")/install-platform.sh" "${kp_run_id}" "${kp_values}"

kubectl rollout status deployment/"$(kp_release_name kuberploy "${kp_run_id}")"-api \
  -n "${kp_namespace}" --timeout=5m
kubectl rollout status deployment/"$(kp_release_name kuberploy "${kp_run_id}")"-worker \
  -n "${kp_namespace}" --timeout=5m
kubectl rollout status deployment/"$(kp_release_name kuberploy "${kp_run_id}")"-web \
  -n "${kp_namespace}" --timeout=5m

curl --fail --show-error --silent --max-time 15 \
  http://kuberploy.k8s.orb.local/v1/meta >/dev/null

printf 'Kuberploy control plane is live at http://kuberploy.k8s.orb.local\n'
printf 'Disposable local bootstrap token: %s\n' "${kp_bootstrap_token}"
