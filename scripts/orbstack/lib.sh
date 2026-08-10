#!/usr/bin/env bash

set -Eeuo pipefail

readonly KP_EXPECTED_CONTEXT="orbstack"
readonly KP_NAMESPACE_PREFIX="kuberploy-e2e-"

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
    command -v "${kp_tool}" >/dev/null 2>&1 || kp_die "required tool not found: ${kp_tool}"
  done
}

kp_assert_orbstack_context() {
  kp_require_tools kubectl
  local kp_context
  kp_context="$(kubectl config current-context 2>/dev/null || true)"
  [[ "${kp_context}" == "${KP_EXPECTED_CONTEXT}" ]] || \
    kp_die "refusing Kubernetes mutation/read: current context is '${kp_context:-<none>}', expected exactly '${KP_EXPECTED_CONTEXT}'"
}

kp_validate_run_id() {
  local kp_run_id="${1:-}"
  [[ "${kp_run_id}" =~ ^[a-z0-9][a-z0-9-]{0,19}$ ]] || \
    kp_die "run ID must match ^[a-z0-9][a-z0-9-]{0,19}$"
}

kp_run_namespace() {
  local kp_run_id="${1:?run ID required}"
  kp_validate_run_id "${kp_run_id}"
  printf '%s%s\n' "${KP_NAMESPACE_PREFIX}" "${kp_run_id}"
}

kp_require_run_namespace() {
  local kp_run_id="${1:?run ID required}"
  local kp_namespace
  local kp_label
  kp_namespace="$(kp_run_namespace "${kp_run_id}")"
  kubectl get namespace "${kp_namespace}" >/dev/null 2>&1 || \
    kp_die "run namespace does not exist: ${kp_namespace}"
  kp_label="$(kubectl get namespace "${kp_namespace}" -o jsonpath='{.metadata.labels.kuberploy\.io/test-run}')"
  [[ "${kp_label}" == "${kp_run_id}" ]] || \
    kp_die "namespace ${kp_namespace} is not owned by test run ${kp_run_id}"
  printf '%s\n' "${kp_namespace}"
}

kp_release_name() {
  local kp_component="${1:?component required}"
  local kp_run_id="${2:?run ID required}"
  kp_validate_run_id "${kp_run_id}"
  printf '%s-%s\n' "${kp_component}" "${kp_run_id}"
}
