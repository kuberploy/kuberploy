#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

: "${KUBERPLOY_E2E_ARTIFACT_DIR:?artifact directory required}"
: "${KUBERPLOY_E2E_TEARDOWN_RECEIPT_FILE:?teardown receipt required}"
: "${KUBERPLOY_E2E_TEARDOWN_SIGNATURE_FILE:?teardown signature required}"
: "${KUBERPLOY_E2E_TEARDOWN_PUBLIC_KEY_FILE:?teardown public key required}"

kp_report="${KUBERPLOY_E2E_ARTIFACT_DIR}/qualification-report.json"
kp_marker="${KUBERPLOY_E2E_ARTIFACT_DIR}/.teardown-finalized"
[[ "${KUBERPLOY_E2E_ARTIFACT_DIR}" == /* && -d "${KUBERPLOY_E2E_ARTIFACT_DIR}" && \
   ! -L "${KUBERPLOY_E2E_ARTIFACT_DIR}" ]] || kp_die "artifact directory is unsafe"
[[ -f "${kp_report}" && ! -L "${kp_report}" && ! -e "${kp_marker}" ]] || \
  kp_die "qualification report is missing, unsafe, or already finalized"
kp_qualification_validate_safe_file KUBERPLOY_E2E_TEARDOWN_RECEIPT_FILE \
  "${KUBERPLOY_E2E_TEARDOWN_RECEIPT_FILE}" false
kp_qualification_validate_safe_file KUBERPLOY_E2E_TEARDOWN_SIGNATURE_FILE \
  "${KUBERPLOY_E2E_TEARDOWN_SIGNATURE_FILE}" false
kp_qualification_validate_safe_file KUBERPLOY_E2E_TEARDOWN_PUBLIC_KEY_FILE \
  "${KUBERPLOY_E2E_TEARDOWN_PUBLIC_KEY_FILE}" false

kp_report_digest="$(openssl dgst -sha256 -r "${kp_report}" | awk '{print $1}')"
[[ "${kp_report_digest}" =~ ^[a-f0-9]{64}$ ]]
kp_public_key_digest="$(openssl pkey -pubin -in "${KUBERPLOY_E2E_TEARDOWN_PUBLIC_KEY_FILE}" \
  -outform DER 2>/dev/null | openssl dgst -sha256 -r | awk '{print $1}')"
jq -e --arg digest "${kp_public_key_digest}" \
  '.disposableCluster.publicKeySHA256 == $digest' "${kp_report}" >/dev/null || \
  kp_die "teardown verifier key does not match the qualified report"
jq -e --arg digest "${kp_report_digest}" --slurpfile report "${kp_report}" '
  (keys | sort) == ["authority","destroyedAt","infrastructureId","qualificationReportSHA256",
    "runID","schemaVersion","status","target"] and
  .schemaVersion == 1 and .status == "destroyed" and
  .runID == $report[0].runID and .target == $report[0].target and
  .authority == $report[0].disposableCluster.authority and
  .infrastructureId == $report[0].disposableCluster.infrastructureId and
  .qualificationReportSHA256 == $digest and
  (.destroyedAt | type == "string" and
    (fromdateiso8601 >= ($report[0].qualifiedAt | fromdateiso8601)) and
    fromdateiso8601 <= (now + 300)) and
  $report[0].status == "qualified-teardown-required" and
  $report[0].disposableCluster.teardownRequired == true
' "${KUBERPLOY_E2E_TEARDOWN_RECEIPT_FILE}" >/dev/null || \
  kp_die "teardown receipt does not exactly bind this qualified run and infrastructure"
openssl dgst -sha256 -verify "${KUBERPLOY_E2E_TEARDOWN_PUBLIC_KEY_FILE}" \
  -signature "${KUBERPLOY_E2E_TEARDOWN_SIGNATURE_FILE}" \
  "${KUBERPLOY_E2E_TEARDOWN_RECEIPT_FILE}" >/dev/null || \
  kp_die "teardown receipt signature is invalid"
kp_receipt_digest="$(openssl dgst -sha256 -r "${KUBERPLOY_E2E_TEARDOWN_RECEIPT_FILE}" | awk '{print $1}')"
jq --arg receiptDigest "${kp_receipt_digest}" \
  '.status="passed" | .disposableCluster.teardownRequired=false |
   .disposableCluster.teardownVerified=true |
   .disposableCluster.receiptSHA256=$receiptDigest' \
  "${kp_report}" >"${kp_report}.tmp"
chmod 600 "${kp_report}.tmp"
mv -- "${kp_report}.tmp" "${kp_report}"
printf '%s\n' "${kp_receipt_digest}" >"${kp_marker}"
chmod 600 "${kp_marker}"
printf 'Qualification finalized: signed infrastructure teardown verified.\n'
