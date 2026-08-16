#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
source "$(dirname "${BASH_SOURCE[0]}")/security-driver.sh"
source "$(dirname "${BASH_SOURCE[0]}")/public-provider-workflow.sh"

kp_root="$(kp_repo_root)"
kp_events_file=""
kp_artifacts_ready="false"
kp_active_stage=""
kp_final_status="failed"
kp_cleanup_failed="false"
kp_secret_state_dir=""
kp_public_provider_active="false"
kp_public_provider_stage_dir=""
kp_stage_ids=()
kp_stage_drivers=()
kp_stage_assertions=()
kp_stage_dirs=()

kp_append_event() {
  local kp_stage="${1:?stage required}"
  local kp_phase="${2:?phase required}"
  local kp_status="${3:?status required}"
  jq -cn --arg kp_stage "${kp_stage}" --arg kp_phase "${kp_phase}" \
    --arg kp_status "${kp_status}" \
    '{stage:$kp_stage,phase:$kp_phase,status:$kp_status}' >>"${kp_events_file}"
  chmod 600 "${kp_events_file}"
}

kp_export_stage_contract() {
  local kp_stage="${1:?stage required}"
  local kp_assertions="${2:?assertions required}"
  local kp_dir="${3:?stage directory required}"
  export KUBERPLOY_E2E_STAGE_ID="${kp_stage}"
  export KUBERPLOY_E2E_STAGE_DIR="${kp_dir}"
  export KUBERPLOY_E2E_STAGE_RESULT_FILE="${kp_dir}/result.json"
  export KUBERPLOY_E2E_STAGE_INVENTORY_FILE="${kp_dir}/inventory.ndjson"
  export KUBERPLOY_E2E_STAGE_CLEANUP_RESULT_FILE="${kp_dir}/cleanup-result.json"
  export KUBERPLOY_E2E_STAGE_ASSERTIONS="${kp_assertions}"
  export KUBERPLOY_E2E_KUBECTL="${kp_root}/scripts/kubernetes/test/e2e/kubectl.sh"
  export KUBERPLOY_E2E_HELM="${kp_root}/scripts/kubernetes/test/e2e/helm.sh"
  export KUBERPLOY_E2E_RUN_LABEL_KEY="${KP_RUN_LABEL_KEY}"
  export KUBERPLOY_E2E_MANAGED_BY_LABEL_KEY="${KP_MANAGED_BY_LABEL_KEY}"
  export KUBERPLOY_E2E_MANAGED_BY_LABEL_VALUE="${KP_MANAGED_BY_LABEL_VALUE}"
}

kp_write_report() {
  local kp_status="${1:?status required}"
  local kp_report="${KUBERPLOY_E2E_ARTIFACT_DIR}/qualification-report.json"
  local kp_teardown_authority kp_infrastructure_id kp_teardown_key_digest
  kp_teardown_authority="$(jq -er '.teardown.authority' "${KUBERPLOY_E2E_SCENARIO_FILE}")"
  kp_infrastructure_id="$(jq -er '.teardown.infrastructureId' "${KUBERPLOY_E2E_SCENARIO_FILE}")"
  kp_teardown_key_digest="$(jq -er '.teardown.publicKeySHA256' "${KUBERPLOY_E2E_SCENARIO_FILE}")"
  jq -s --arg kp_run "${KUBERPLOY_E2E_RUN_ID}" \
    --arg kp_context "${KUBERPLOY_TEST_CONTEXT}" \
    --arg kp_server "${KUBERPLOY_TEST_SERVER}" \
    --arg kp_status "${kp_status}" \
    --arg kp_teardown_authority "${kp_teardown_authority}" \
    --arg kp_infrastructure_id "${kp_infrastructure_id}" \
    --arg kp_teardown_key_digest "${kp_teardown_key_digest}" \
    --argjson kp_public "${KUBERPLOY_E2E_PUBLIC_PROVIDER_TESTS:-false}" '
      group_by(.stage) | sort_by(.[0].stage | split("-")[0] | tonumber) |
      {
        schemaVersion: 1,
        runID: $kp_run,
        target: {context: $kp_context, server: $kp_server},
        status: $kp_status,
        qualifiedAt: (now | todateiso8601),
        disposableCluster: {
          acknowledged: true,
          teardownRequired: true,
          retainedState: "product records, retained installer objects, and Argo-created descendants",
          authority: $kp_teardown_authority,
          infrastructureId: $kp_infrastructure_id,
          publicKeySHA256: $kp_teardown_key_digest
        },
        publicProviderTestsRequested: $kp_public,
        stages: map({stage: .[0].stage, events: map({phase, status})})
      }
    ' "${kp_events_file}" >"${kp_report}.tmp"
  chmod 600 "${kp_report}.tmp"
  mv -- "${kp_report}.tmp" "${kp_report}"
}

kp_cleanup_completed_stages() {
  local kp_index
  local kp_stage
  local kp_driver
  local kp_assertions
  local kp_dir
  local kp_inventory
  local kp_cleanup_result

  for ((kp_index=${#kp_stage_ids[@]}-1; kp_index>=0; kp_index--)); do
    kp_stage="${kp_stage_ids[${kp_index}]}"
    kp_driver="${kp_stage_drivers[${kp_index}]}"
    kp_assertions="${kp_stage_assertions[${kp_index}]}"
    kp_dir="${kp_stage_dirs[${kp_index}]}"
    kp_inventory="${kp_dir}/inventory.ndjson"
    kp_cleanup_result="${kp_dir}/cleanup-result.json"
    kp_export_stage_contract "${kp_stage}" "${kp_assertions}" "${kp_dir}"

    if [[ ! -s "${kp_inventory}" || -L "${kp_inventory}" ]]; then
      kp_append_event "${kp_stage}" cleanup failed
      printf 'error: cannot safely clean %s because its exact inventory is missing\n' \
        "${kp_stage}" >&2
      kp_cleanup_failed="true"
      continue
    fi
    if ! "${kp_driver}" cleanup \
        >"${kp_dir}/evidence/cleanup.stdout" \
        2>"${kp_dir}/evidence/cleanup.stderr"; then
      kp_append_event "${kp_stage}" cleanup failed
      printf 'error: cleanup driver failed for %s; inventory retained at %s\n' \
        "${kp_stage}" "${kp_inventory}" >&2
      kp_cleanup_failed="true"
      continue
    fi
    if ! (kp_qualification_validate_cleanup_result \
        "${kp_stage}" "${kp_inventory}" "${kp_cleanup_result}"); then
      kp_append_event "${kp_stage}" cleanup failed
      kp_cleanup_failed="true"
      continue
    fi
    kp_append_event "${kp_stage}" cleanup passed
  done
}

kp_on_exit() {
  local kp_status=$?
  trap - EXIT INT TERM
  if [[ "${kp_artifacts_ready}" == "true" ]]; then
    if [[ "${kp_public_provider_active}" == "true" ]]; then
      if kp_public_provider_cleanup \
          >"${kp_public_provider_stage_dir}/evidence/cleanup.stdout" \
          2>"${kp_public_provider_stage_dir}/evidence/cleanup.stderr"; then
        kp_append_event 110-public-provider cleanup passed
        kp_public_provider_active="false"
      else
        kp_append_event 110-public-provider cleanup failed
        kp_cleanup_failed="true"
      fi
    fi
    kp_cleanup_completed_stages
    if [[ "${kp_status}" -eq 0 && "${kp_cleanup_failed}" == "false" ]]; then
      kp_final_status="qualified-teardown-required"
    else
      kp_final_status="failed"
      kp_status=1
    fi
    kp_write_report "${kp_final_status}"
    if [[ "${kp_final_status}" == "qualified-teardown-required" ]]; then
      printf 'Full MVP qualification passed; destroy the acknowledged disposable cluster to remove retained product and Argo state.\n'
      printf 'Report: %s\n' \
        "${KUBERPLOY_E2E_ARTIFACT_DIR}/qualification-report.json"
    else
      printf 'error: qualification failed; inspect the report and exact per-stage inventories in %s\n' \
        "${KUBERPLOY_E2E_ARTIFACT_DIR}" >&2
    fi
  fi
  if [[ -n "${kp_secret_state_dir}" && -d "${kp_secret_state_dir}" ]]; then
    find "${kp_secret_state_dir}" -type f -maxdepth 1 -exec rm -- {} +
    rmdir -- "${kp_secret_state_dir}"
  fi
  exit "${kp_status}"
}
trap kp_on_exit EXIT INT TERM

kp_initialize
kp_require_tools find helm curl openssl node
[[ "${KUBERPLOY_E2E_PUBLIC_PROVIDER_TESTS:-false}" != "true" ]] || kp_require_tools dig
kp_qualification_validate_common_inputs

kp_catalog="$(kp_qualification_stage_catalog)"

kp_driver="${kp_root}/scripts/kubernetes/test/e2e/builtin-driver.sh"
[[ -x "${kp_driver}" ]] || kp_die "repository-owned qualification driver is not executable"
kp_qualification_validate_scenario "${kp_catalog}"

kp_qualification_prepare_artifacts
kp_artifacts_ready="true"
kp_secret_state_dir="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-qualification-auth.XXXXXX")"
chmod 700 "${kp_secret_state_dir}"
export KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE="${kp_secret_state_dir}/human-cookie-header"
export KUBERPLOY_E2E_CSRF_TOKEN_FILE="${kp_secret_state_dir}/csrf-token"
export KUBERPLOY_E2E_API_AUTH_HEADER_FILE="${kp_secret_state_dir}/api-auth-header"
export KUBERPLOY_E2E_DENIED_AUTH_HEADER_FILE="${kp_secret_state_dir}/denied-auth-header"
kp_events_file="${KUBERPLOY_E2E_ARTIFACT_DIR}/events.ndjson"
: >"${kp_events_file}"
chmod 600 "${kp_events_file}"

# Preflight is read-only and repeats target identity validation before the
# repository-owned substantive stages begin.
kp_preflight_dir="${KUBERPLOY_E2E_ARTIFACT_DIR}/00-preflight"
mkdir -p "${kp_preflight_dir}/evidence"
chmod 700 "${kp_preflight_dir}" "${kp_preflight_dir}/evidence"
kp_append_event 00-preflight run started
if ! "${kp_root}/scripts/kubernetes/preflight.sh" \
    >"${kp_preflight_dir}/evidence/preflight.txt" \
    2>"${kp_preflight_dir}/evidence/preflight.stderr"; then
  kp_append_event 00-preflight run failed
  kp_die "read-only conforming-cluster preflight failed"
fi
export KUBERPLOY_E2E_STAGE_DIR="${kp_preflight_dir}"
export KUBERPLOY_E2E_KUBECTL="${kp_root}/scripts/kubernetes/test/e2e/kubectl.sh"
kp_security_attest_network_provider
jq -n --arg kp_run "${KUBERPLOY_E2E_RUN_ID}" '
  {
    schemaVersion: 1,
    runID: $kp_run,
    stage: "00-preflight",
    status: "passed",
    assertions: [
      "explicit-target", "version-window", "required-apis", "nodes-ready",
      "default-storage", "dependency-inventory", "network-policy-provider"
    ] | map({id: ., status: "passed", evidenceFiles:
      [if . == "network-policy-provider" then "evidence/workflow-network-provider.json"
       else "evidence/preflight.txt" end]})
  }
' >"${kp_preflight_dir}/result.json"
kp_qualification_validate_result 00-preflight \
  explicit-target,version-window,required-apis,nodes-ready,default-storage,dependency-inventory,network-policy-provider \
  "${kp_preflight_dir}"
kp_append_event 00-preflight run passed

if [[ "${KUBERPLOY_E2E_PUBLIC_PROVIDER_TESTS:-false}" == "true" ]]; then
  kp_public_provider_stage_dir="${KUBERPLOY_E2E_ARTIFACT_DIR}/110-public-provider"
  mkdir -p "${kp_public_provider_stage_dir}/evidence"
  chmod 700 "${kp_public_provider_stage_dir}" "${kp_public_provider_stage_dir}/evidence"
  export KUBERPLOY_E2E_PUBLIC_PROVIDER_INVENTORY_FILE="${kp_public_provider_stage_dir}/inventory.ndjson"
  export KUBERPLOY_E2E_PUBLIC_PROVIDER_EVIDENCE_FILE="${kp_public_provider_stage_dir}/evidence/public-provider.json"
  export KUBERPLOY_E2E_PUBLIC_PROVIDER_CLEANUP_RESULT_FILE="${kp_public_provider_stage_dir}/cleanup-result.json"
  : >"${KUBERPLOY_E2E_PUBLIC_PROVIDER_INVENTORY_FILE}"
  chmod 600 "${KUBERPLOY_E2E_PUBLIC_PROVIDER_INVENTORY_FILE}"
  kp_public_provider_active="true"
  kp_append_event 110-public-provider run started
  if ! kp_public_provider_run; then
    kp_append_event 110-public-provider run failed
    kp_die "public-provider workflow failed"
  fi
  kp_append_event 110-public-provider run passed
fi

while IFS='|' read -r kp_stage kp_mutating kp_assertions; do
  [[ -n "${kp_stage}" ]] || continue
  kp_stage_dir="${KUBERPLOY_E2E_ARTIFACT_DIR}/${kp_stage}"
  mkdir -p "${kp_stage_dir}/evidence"
  chmod 700 "${kp_stage_dir}" "${kp_stage_dir}/evidence"
  : >"${kp_stage_dir}/inventory.ndjson"
  chmod 600 "${kp_stage_dir}/inventory.ndjson"
  kp_export_stage_contract "${kp_stage}" "${kp_assertions}" "${kp_stage_dir}"

  # Register cleanup before invoking the driver. Drivers must append inventory
  # before each mutation so a partial failure still has an exact cleanup set.
  kp_stage_ids+=("${kp_stage}")
  kp_stage_drivers+=("${kp_driver}")
  kp_stage_assertions+=("${kp_assertions}")
  kp_stage_dirs+=("${kp_stage_dir}")
  kp_active_stage="${kp_stage}"
  kp_append_event "${kp_stage}" run started
  if ! "${kp_driver}" run \
      >"${kp_stage_dir}/evidence/driver.stdout" \
      2>"${kp_stage_dir}/evidence/driver.stderr"; then
    kp_append_event "${kp_stage}" run failed
    kp_die "stage ${kp_stage} driver failed"
  fi
  kp_qualification_validate_result "${kp_stage}" "${kp_assertions}" \
    "${kp_stage_dir}"
  kp_qualification_validate_inventory "${kp_stage}" \
    "${kp_stage_dir}/inventory.ndjson"
  kp_append_event "${kp_stage}" run passed
  kp_active_stage=""
done <<<"${kp_catalog}"

if [[ "${KUBERPLOY_E2E_PUBLIC_PROVIDER_TESTS:-false}" == "true" ]]; then
  export KUBERPLOY_E2E_PUBLIC_PROVIDER_HTTPS_EVIDENCE_FILE="${kp_public_provider_stage_dir}/evidence/public-https.json"
  kp_append_event 110-public-provider observe started
  if ! kp_public_provider_verify_https; then
    kp_append_event 110-public-provider observe failed
    kp_die "public HTTPS workflow failed"
  fi
  kp_append_event 110-public-provider observe passed
fi

# The EXIT trap performs reverse-order, inventory-bound cleanup and writes the
# final report. Reaching this line means every requested assertion passed.
exit 0
