#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-kube-harness.XXXXXX")"

kp_remove_tmp() {
  if [[ -n "${kp_tmp:-}" && "${kp_tmp}" == *"/kuberploy-kube-harness."* ]]; then
    rm -rf -- "${kp_tmp}"
  fi
}
trap kp_remove_tmp EXIT

kp_fixture_kubeconfig="${kp_tmp}/fixture-kubeconfig"
printf 'fixture only\n' >"${kp_fixture_kubeconfig}"
chmod 600 "${kp_fixture_kubeconfig}"

export KP_EXPECTED_KUBECONFIG="${kp_fixture_kubeconfig}"
export KP_EXPECTED_CONTEXT="fixture-context"
export KP_EXPECTED_SERVER="https://api.example.invalid:6443"

cat >"${kp_tmp}/kubectl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "${1:-}" == "--kubeconfig" && "${2:-}" == "${KP_EXPECTED_KUBECONFIG}" ]]
[[ "${3:-}" == "--context" && "${4:-}" == "${KP_EXPECTED_CONTEXT}" ]]
shift 4
if [[ "${1:-}" == "config" && "${2:-}" == "get-contexts" ]]; then
  printf '%s\n' "${KP_EXPECTED_CONTEXT}"
elif [[ "${1:-}" == "config" && "${2:-}" == "view" ]]; then
  printf '%s' "${KP_EXPECTED_SERVER}"
else
  printf 'fixture-ok\n'
fi
EOF

cat >"${kp_tmp}/helm" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "${1:-}" == "--kubeconfig" && "${2:-}" == "${KP_EXPECTED_KUBECONFIG}" ]]
[[ "${3:-}" == "--kube-context" && "${4:-}" == "${KP_EXPECTED_CONTEXT}" ]]
printf 'fixture-ok\n'
EOF
chmod 755 "${kp_tmp}/kubectl" "${kp_tmp}/helm"
export PATH="${kp_tmp}:${PATH}"

export KUBECONFIG="${kp_fixture_kubeconfig}"
export KUBERPLOY_TEST_CONTEXT="${KP_EXPECTED_CONTEXT}"
export KUBERPLOY_TEST_SERVER="${KP_EXPECTED_SERVER}"
export KUBERPLOY_E2E_RUN_ID="fixture1"

source "${kp_root}/scripts/kubernetes/lib.sh"

kp_expect_failure() {
  local kp_name="${1:?test name required}"
  shift
  if ("$@") >/dev/null 2>&1; then
    printf 'expected failure: %s\n' "${kp_name}" >&2
    exit 1
  fi
}

kp_validate_inputs
kp_assert_cluster_identity
[[ "$(kp_run_namespace)" == "kuberploy-e2e-fixture1" ]]
[[ "$(kp_kubectl get pods)" == "fixture-ok" ]]
[[ "$(kp_helm version)" == "fixture-ok" ]]

kp_expect_failure relative-kubeconfig \
  env KUBECONFIG=relative bash -c \
  'source "$1"; kp_validate_inputs' _ "${kp_root}/scripts/kubernetes/lib.sh"
kp_expect_failure kubeconfig-list \
  env KUBECONFIG="${kp_fixture_kubeconfig}:${kp_fixture_kubeconfig}" bash -c \
  'source "$1"; kp_validate_inputs' _ "${kp_root}/scripts/kubernetes/lib.sh"
kp_expect_failure missing-context \
  env -u KUBERPLOY_TEST_CONTEXT bash -c \
  'source "$1"; kp_validate_inputs' _ "${kp_root}/scripts/kubernetes/lib.sh"
kp_expect_failure missing-server \
  env -u KUBERPLOY_TEST_SERVER bash -c \
  'source "$1"; kp_validate_inputs' _ "${kp_root}/scripts/kubernetes/lib.sh"
kp_expect_failure non-https-server \
  env KUBERPLOY_TEST_SERVER=http://api.example.invalid:8080 bash -c \
  'source "$1"; kp_validate_inputs' _ "${kp_root}/scripts/kubernetes/lib.sh"
kp_expect_failure invalid-run-id \
  env KUBERPLOY_E2E_RUN_ID='INVALID!' bash -c \
  'source "$1"; kp_validate_inputs' _ "${kp_root}/scripts/kubernetes/lib.sh"
kp_expect_failure server-mismatch \
  env KUBERPLOY_TEST_SERVER=https://other.example.invalid:6443 bash -c \
  'source "$1"; kp_validate_inputs; kp_assert_cluster_identity' _ \
  "${kp_root}/scripts/kubernetes/lib.sh"

kp_insecure_kubeconfig="${kp_tmp}/insecure-kubeconfig"
printf 'fixture only\n' >"${kp_insecure_kubeconfig}"
chmod 644 "${kp_insecure_kubeconfig}"
kp_expect_failure insecure-kubeconfig \
  env KUBECONFIG="${kp_insecure_kubeconfig}" bash -c \
  'source "$1"; kp_validate_inputs' _ "${kp_root}/scripts/kubernetes/lib.sh"

kp_owned_namespace_json="$(jq -n \
  --arg kp_run "${KUBERPLOY_E2E_RUN_ID}" \
  --arg kp_managed "${KP_MANAGED_BY_LABEL_VALUE}" \
  '{metadata:{labels:{"kuberploy.io/test-run":$kp_run,"app.kubernetes.io/managed-by":$kp_managed}}}')"
kp_assert_owned_namespace_json "${kp_owned_namespace_json}"
kp_unowned_namespace_json='{"metadata":{"labels":{"kuberploy.io/test-run":"another-run"}}}'
kp_expect_failure ownership-mismatch \
  kp_assert_owned_namespace_json "${kp_unowned_namespace_json}"

if rg -n '^[[:space:]]*(kubectl|helm)[[:space:]]' \
  "${kp_root}/scripts/kubernetes/preflight.sh" \
  "${kp_root}/scripts/kubernetes/smoke.sh" \
  "${kp_root}/scripts/kubernetes/cleanup-run.sh"; then
  printf 'cluster command bypasses the explicit selector wrappers\n' >&2
  exit 1
fi

if rg -n 'kubectl[[:space:]]+config[[:space:]]+use-context|delete[^\n]*(--all|\*)' \
  "${kp_root}/scripts/kubernetes"; then
  printf 'ambient-context mutation or broad deletion found\n' >&2
  exit 1
fi

for kp_policy_marker in \
  'name: smoke-server-default-deny' \
  'name: smoke-server-allow-probe' \
  'kuberploy.io/smoke-access: allowed' \
  'the selected cluster did not enforce the default-deny ingress NetworkPolicy' \
  'the selected cluster did not enforce the exact NetworkPolicy allow rule'; do
  rg -F "${kp_policy_marker}" "${kp_root}/scripts/kubernetes/smoke.sh" >/dev/null || {
    printf 'missing NetworkPolicy enforcement marker: %s\n' "${kp_policy_marker}" >&2
    exit 1
  }
done

printf 'Kubernetes harness input, identity, selector, and ownership tests passed.\n'
