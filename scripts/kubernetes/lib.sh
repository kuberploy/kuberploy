#!/usr/bin/env bash

set -Eeuo pipefail

readonly KP_NAMESPACE_PREFIX="kuberploy-e2e-"
readonly KP_RUN_LABEL_KEY="kuberploy.io/test-run"
readonly KP_MANAGED_BY_LABEL_KEY="app.kubernetes.io/managed-by"
readonly KP_MANAGED_BY_LABEL_VALUE="kuberploy-e2e-harness"

kp_repo_root() {
  cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P
}

kp_die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

kp_require_tools() {
  local kp_tool
  for kp_tool in "$@"; do
    command -v "${kp_tool}" >/dev/null 2>&1 || \
      kp_die "required tool not found: ${kp_tool}"
  done
}

kp_file_mode() {
  local kp_path="${1:?path required}"
  if stat -f '%Lp' "${kp_path}" >/dev/null 2>&1; then
    stat -f '%Lp' "${kp_path}"
  else
    stat -c '%a' "${kp_path}"
  fi
}

kp_validate_run_id() {
  local kp_run_id="${1:-}"
  [[ "${kp_run_id}" =~ ^[a-z0-9][a-z0-9-]{0,19}$ ]] || \
    kp_die "KUBERPLOY_E2E_RUN_ID must match ^[a-z0-9][a-z0-9-]{0,19}$"
}

kp_validate_inputs() {
  local kp_mode

  : "${KUBECONFIG:?KUBECONFIG is required}"
  : "${KUBERPLOY_TEST_CONTEXT:?KUBERPLOY_TEST_CONTEXT is required}"
  : "${KUBERPLOY_TEST_SERVER:?KUBERPLOY_TEST_SERVER is required}"
  : "${KUBERPLOY_E2E_RUN_ID:?KUBERPLOY_E2E_RUN_ID is required}"

  [[ "${KUBECONFIG}" == /* ]] || \
    kp_die "KUBECONFIG must be one absolute path"
  [[ "${KUBECONFIG}" != *:* ]] || \
    kp_die "KUBECONFIG path lists are not allowed"
  [[ -f "${KUBECONFIG}" && ! -L "${KUBECONFIG}" ]] || \
    kp_die "KUBECONFIG must identify one regular, non-symlink file"

  kp_mode="$(kp_file_mode "${KUBECONFIG}")"
  [[ "${kp_mode}" =~ ^[0-7]{3,4}$ ]] || \
    kp_die "could not validate KUBECONFIG permissions"
  (( (8#${kp_mode} & 8#077) == 0 )) || \
    kp_die "KUBECONFIG must not be group/world accessible; use mode 0600"

  [[ "${KUBERPLOY_TEST_CONTEXT}" != *$'\n'* && \
     "${KUBERPLOY_TEST_CONTEXT}" != *$'\r'* ]] || \
    kp_die "KUBERPLOY_TEST_CONTEXT must be one line"
  [[ "${KUBERPLOY_TEST_SERVER}" =~ ^https://[^[:space:]]+$ ]] || \
    kp_die "KUBERPLOY_TEST_SERVER must be one explicit HTTPS API URL"
  kp_validate_run_id "${KUBERPLOY_E2E_RUN_ID}"
}

kp_kubectl() {
  kubectl --kubeconfig "${KUBECONFIG}" \
    --context "${KUBERPLOY_TEST_CONTEXT}" "$@"
}

kp_helm() {
  helm --kubeconfig "${KUBECONFIG}" \
    --kube-context "${KUBERPLOY_TEST_CONTEXT}" "$@"
}

kp_assert_cluster_identity() {
  local kp_context
  local kp_server

  kp_context="$(kp_kubectl config get-contexts \
    "${KUBERPLOY_TEST_CONTEXT}" -o name 2>/dev/null || true)"
  [[ "${kp_context}" == "${KUBERPLOY_TEST_CONTEXT}" ]] || \
    kp_die "the explicit kubeconfig does not contain the requested context"

  # Read only the selected server field. Never render --raw kubeconfig data.
  kp_server="$(kp_kubectl config view --minify \
    -o jsonpath='{.clusters[0].cluster.server}')"
  [[ "${kp_server}" == "${KUBERPLOY_TEST_SERVER}" ]] || \
    kp_die "the selected context does not match KUBERPLOY_TEST_SERVER"
}

kp_initialize() {
  kp_require_tools jq kubectl stat
  kp_validate_inputs
  kp_assert_cluster_identity
}

kp_run_namespace() {
  kp_validate_run_id "${KUBERPLOY_E2E_RUN_ID}"
  printf '%s%s\n' "${KP_NAMESPACE_PREFIX}" "${KUBERPLOY_E2E_RUN_ID}"
}

kp_assert_owned_namespace_json() {
  local kp_namespace_json="${1:?namespace JSON required}"
  local kp_run_label
  local kp_managed_by

  kp_run_label="$(jq -r --arg kp_key "${KP_RUN_LABEL_KEY}" \
    '.metadata.labels[$kp_key] // ""' <<<"${kp_namespace_json}")"
  kp_managed_by="$(jq -r --arg kp_key "${KP_MANAGED_BY_LABEL_KEY}" \
    '.metadata.labels[$kp_key] // ""' <<<"${kp_namespace_json}")"

  [[ "${kp_run_label}" == "${KUBERPLOY_E2E_RUN_ID}" && \
     "${kp_managed_by}" == "${KP_MANAGED_BY_LABEL_VALUE}" ]] || \
    kp_die "refusing cleanup: namespace ownership labels do not match this run"
}
