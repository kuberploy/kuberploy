#!/usr/bin/env bash

set -Eeuo pipefail

# This workflow owns only one run-scoped Cloudflare DNS record. It never
# accepts a pre-existing record and never deletes by hostname without first
# proving the provider object still has this run's exact identity.
if ! declare -F kp_qualification_validate_hostname >/dev/null 2>&1; then
  source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
fi

readonly KP_PUBLIC_PROVIDER_API_VERSION="cloudflare.kuberploy.io/v1"
readonly KP_PUBLIC_PROVIDER_KIND="DNSRecord"
readonly KP_PUBLIC_PROVIDER_DEFAULT_API="https://api.cloudflare.com/client/v4"

kp_public_provider_api_base="${KUBERPLOY_E2E_CLOUDFLARE_API_BASE_URL:-${KP_PUBLIC_PROVIDER_DEFAULT_API}}"
kp_public_provider_zone="${KUBERPLOY_E2E_PUBLIC_PROVIDER_ZONE:-}"
kp_public_provider_curl_config=""
kp_public_provider_tmp=""

kp_public_provider_validate_target() {
  local kp_type="${KUBERPLOY_E2E_PUBLIC_DNS_RECORD_TYPE:-A}"
  local kp_target="${KUBERPLOY_E2E_PUBLIC_DNS_TARGET:-}"
  [[ "${kp_type}" == "A" ]] ||
    kp_die "KUBERPLOY_E2E_PUBLIC_DNS_RECORD_TYPE must be A"
  [[ "${kp_target}" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] ||
    kp_die "KUBERPLOY_E2E_PUBLIC_DNS_TARGET must be one IPv4 address"
  local kp_octet
  IFS='.' read -r -a kp_octets <<<"${kp_target}"
  for kp_octet in "${kp_octets[@]}"; do
    (( 10#${kp_octet} <= 255 )) ||
      kp_die "KUBERPLOY_E2E_PUBLIC_DNS_TARGET contains an invalid IPv4 octet"
  done
}

kp_public_provider_validate_inputs() {
  : "${KUBERPLOY_E2E_RUN_ID:?KUBERPLOY_E2E_RUN_ID is required}"
  : "${KUBERPLOY_E2E_PUBLIC_HOSTNAME:?KUBERPLOY_E2E_PUBLIC_HOSTNAME is required}"
  : "${KUBERPLOY_E2E_PUBLIC_PROVIDER_ZONE:?KUBERPLOY_E2E_PUBLIC_PROVIDER_ZONE is required}"
  : "${KUBERPLOY_E2E_DNS_PROVIDER_CREDENTIAL_FILE:?KUBERPLOY_E2E_DNS_PROVIDER_CREDENTIAL_FILE is required}"
  kp_validate_run_id "${KUBERPLOY_E2E_RUN_ID}"
  kp_qualification_validate_hostname public-provider-zone "${kp_public_provider_zone}"
  kp_qualification_validate_hostname public-provider-hostname "${KUBERPLOY_E2E_PUBLIC_HOSTNAME}"
  [[ "${KUBERPLOY_E2E_PUBLIC_HOSTNAME}" == \
     "kuberploy-${KUBERPLOY_E2E_RUN_ID}.${kp_public_provider_zone}" ]] ||
    kp_die "KUBERPLOY_E2E_PUBLIC_HOSTNAME must be the run-scoped provider-zone name"
  [[ "${KUBERPLOY_E2E_PUBLIC_EFFECTS_ACK:-}" == \
     "public-provider:${KUBERPLOY_E2E_RUN_ID}:${KUBERPLOY_E2E_PUBLIC_HOSTNAME}" ]] ||
    kp_die "KUBERPLOY_E2E_PUBLIC_EFFECTS_ACK does not match this run and hostname"
  kp_qualification_validate_safe_file KUBERPLOY_E2E_DNS_PROVIDER_CREDENTIAL_FILE \
    "${KUBERPLOY_E2E_DNS_PROVIDER_CREDENTIAL_FILE}" true
  awk 'END {exit !(NR == 1 && length($0) > 0)}' \
    "${KUBERPLOY_E2E_DNS_PROVIDER_CREDENTIAL_FILE}" ||
    kp_die "Cloudflare credential file must contain exactly one non-empty line"
  LC_ALL=C grep -Eq '^[^[:cntrl:][:space:]]+$' \
    "${KUBERPLOY_E2E_DNS_PROVIDER_CREDENTIAL_FILE}" ||
    kp_die "Cloudflare credential file contains a forbidden character"
  [[ "${kp_public_provider_api_base}" =~ ^https://[^[:space:]@]+/client/v4$ ]] ||
    kp_die "Cloudflare API base must be one explicit HTTPS client/v4 URL"
  kp_public_provider_validate_target
}

kp_public_provider_cleanup_tmp() {
  if [[ -n "${kp_public_provider_tmp}" &&
        "${kp_public_provider_tmp}" == *"/kuberploy-public-provider."* &&
        -d "${kp_public_provider_tmp}" ]]; then
    find "${kp_public_provider_tmp}" -type f -maxdepth 1 -delete
    rmdir -- "${kp_public_provider_tmp}"
  fi
  kp_public_provider_tmp=""
  kp_public_provider_curl_config=""
}

kp_public_provider_setup() {
  kp_public_provider_cleanup_tmp
  kp_public_provider_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-public-provider.XXXXXX")"
  chmod 700 "${kp_public_provider_tmp}"
  kp_public_provider_curl_config="${kp_public_provider_tmp}/curl.conf"
  local kp_token
  kp_token="$(<"${KUBERPLOY_E2E_DNS_PROVIDER_CREDENTIAL_FILE}")"
  # The config is mode 0600 and is removed after every workflow. The token is
  # never written to evidence, command logs, or user-facing output.
  printf 'header = "Authorization: Bearer %s"\nheader = "Content-Type: application/json"\n' \
    "${kp_token}" >"${kp_public_provider_curl_config}"
  chmod 600 "${kp_public_provider_curl_config}"
}

kp_public_provider_api() {
  local kp_method="${1:?HTTP method required}"
  local kp_path="${2:?API path required}"
  local kp_output="${3:?output path required}"
  local kp_body="${4:-}"
  local kp_status
  local -a kp_args=(--config "${kp_public_provider_curl_config}" --silent --show-error
    --retry 2 --request "${kp_method}" --output "${kp_output}" --write-out '%{http_code}')
  [[ -n "${kp_body}" ]] && kp_args+=(--data-binary "@${kp_body}")
  kp_status="$(curl "${kp_args[@]}" "${kp_public_provider_api_base}${kp_path}")" ||
    kp_die "Cloudflare API request failed"
  printf '%s\n' "${kp_status}"
}

kp_public_provider_zone_id() {
  local kp_out="${kp_public_provider_tmp}/zone.json"
  [[ "$(kp_public_provider_api GET "/zones?name=${kp_public_provider_zone}&status=active&per_page=2" \
    "${kp_out}")" == "200" ]] || kp_die "Cloudflare zone lookup failed"
  jq -e --arg zone "${kp_public_provider_zone}" \
    '.success == true and (.result | length == 1) and .result[0].name == $zone and
     .result[0].status == "active"' "${kp_out}" >/dev/null ||
    kp_die "Cloudflare zone lookup did not return exactly the configured provider zone"
  jq -er '.result[0].id | select(test("^[a-f0-9]{32}$"))' "${kp_out}"
}

kp_public_provider_record_list() {
  local kp_zone_id="${1:?zone ID required}"
  local kp_out="${2:?output path required}"
  local kp_host="${KUBERPLOY_E2E_PUBLIC_HOSTNAME}"
  local kp_query="/zones/${kp_zone_id}/dns_records?type=A&name=${kp_host}&per_page=2"
  [[ "$(kp_public_provider_api GET "${kp_query}" "${kp_out}")" == "200" ]] ||
    kp_die "Cloudflare DNS record lookup failed"
  jq -e '.success == true and (.result | type == "array")' "${kp_out}" >/dev/null ||
    kp_die "Cloudflare DNS record lookup returned an invalid response"
}

kp_public_provider_record_matches() {
  local kp_record_file="${1:?record file required}" kp_record_id="${2:?record ID required}"
  local kp_comment="kuberploy qualification ${KUBERPLOY_E2E_RUN_ID}"
  jq -e --arg id "${kp_record_id}" --arg host "${KUBERPLOY_E2E_PUBLIC_HOSTNAME}" \
    --arg target "${KUBERPLOY_E2E_PUBLIC_DNS_TARGET}" --arg comment "${kp_comment}" '
    .success == true and .result.id == $id and .result.type == "A" and
    .result.name == $host and .result.content == $target and
    .result.proxied == false and .result.comment == $comment
  ' "${kp_record_file}" >/dev/null
}

kp_public_provider_get_record() {
  local kp_zone_id="${1:?zone ID required}" kp_record_id="${2:?record ID required}"
  local kp_out="${3:?output path required}" kp_status
  kp_status="$(kp_public_provider_api GET \
    "/zones/${kp_zone_id}/dns_records/${kp_record_id}" "${kp_out}")"
  printf '%s\n' "${kp_status}"
}

kp_public_provider_wait_dns() {
  local kp_expected="${KUBERPLOY_E2E_PUBLIC_DNS_TARGET}" kp_actual kp_ns kp_nameservers
  kp_nameservers="$(dig +short NS "${kp_public_provider_zone}" | sed '/^[[:space:]]*$/d' | sort -u || true)"
  for _ in {1..30}; do
    while IFS= read -r kp_ns; do
      [[ -n "${kp_ns}" ]] || continue
      kp_actual="$(dig +short "@${kp_ns}" A "${KUBERPLOY_E2E_PUBLIC_HOSTNAME}" | sed '/^[[:space:]]*$/d' | sort -u || true)"
      [[ "${kp_actual}" == "${kp_expected}" ]] && return 0
    done <<<"${kp_nameservers}"
    kp_actual="$(dig +short @1.1.1.1 A "${KUBERPLOY_E2E_PUBLIC_HOSTNAME}" | sed '/^[[:space:]]*$/d' | sort -u || true)"
    [[ "${kp_actual}" == "${kp_expected}" ]] && return 0
    sleep 2
  done
  return 1
}

kp_public_provider_wait_dns_absent() {
  local kp_actual kp_ns kp_nameservers
  kp_nameservers="$(dig +short NS "${kp_public_provider_zone}" | sed '/^[[:space:]]*$/d' | sort -u || true)"
  [[ -n "${kp_nameservers}" ]] || return 1
  for _ in {1..30}; do
    local kp_present="false"
    while IFS= read -r kp_ns; do
      [[ -n "${kp_ns}" ]] || continue
      kp_actual="$(dig +short "@${kp_ns}" A "${KUBERPLOY_E2E_PUBLIC_HOSTNAME}" | sed '/^[[:space:]]*$/d' | sort -u || true)"
      [[ -n "${kp_actual}" ]] && kp_present="true"
    done <<<"${kp_nameservers}"
    [[ "${kp_present}" == "false" ]] && return 0
    sleep 2
  done
  return 1
}

kp_public_provider_append_planned() {
  local kp_inventory="${KUBERPLOY_E2E_PUBLIC_PROVIDER_INVENTORY_FILE:?inventory file required}"
  jq -cn --arg run "${KUBERPLOY_E2E_RUN_ID}" \
    --arg stage "110-public-provider" --arg zone "${kp_public_provider_zone}" \
    --arg host "${KUBERPLOY_E2E_PUBLIC_HOSTNAME}" --arg label "${KP_RUN_LABEL_KEY}" \
    --arg managed "${KP_MANAGED_BY_LABEL_VALUE}" '
    {schemaVersion:1,runID:$run,stage:$stage,apiVersion:"cloudflare.kuberploy.io/v1",
     kind:"DNSRecord",namespace:$zone,name:$host,uid:null,operation:"planned-create",
     absentBefore:true,cleanupPolicy:"delete",
     ownership:{runLabelKey:$label,runLabelValue:$run,managedBy:$managed}}
  ' >>"${kp_inventory}"
  chmod 600 "${kp_inventory}"
}

kp_public_provider_finalize_inventory() {
  local kp_inventory="${KUBERPLOY_E2E_PUBLIC_PROVIDER_INVENTORY_FILE:?inventory file required}"
  local kp_record_id="${1:?record ID required}" kp_tmp="${kp_inventory}.tmp"
  jq -c --arg id "${kp_record_id}" '
    if .kind == "DNSRecord" and .operation == "planned-create" and .uid == null
    then .operation = "created" | .uid = $id
    else . end
  ' "${kp_inventory}" >"${kp_tmp}"
  chmod 600 "${kp_tmp}"
  mv -- "${kp_tmp}" "${kp_inventory}"
}

kp_public_provider_write_evidence() {
  local kp_zone_id="${1:?zone ID required}" kp_record_id="${2:?record ID required}"
  local kp_evidence="${KUBERPLOY_E2E_PUBLIC_PROVIDER_EVIDENCE_FILE:?evidence file required}"
  jq -n --arg zone "${kp_public_provider_zone}" --arg host "${KUBERPLOY_E2E_PUBLIC_HOSTNAME}" \
    --arg type "A" --arg target "${KUBERPLOY_E2E_PUBLIC_DNS_TARGET}" --arg id "${kp_record_id}" \
    '{schemaVersion:1,provider:"cloudflare",zone:$zone,hostname:$host,recordType:$type,
      target:$target,recordId:$id,exactProviderRecord:true,publicDNSObserved:true}' >"${kp_evidence}"
  chmod 600 "${kp_evidence}"
}

kp_public_provider_run() {
  kp_public_provider_validate_inputs
  kp_public_provider_setup
  trap kp_public_provider_cleanup_tmp RETURN
  local kp_zone_id kp_records kp_body kp_create kp_record_id
  kp_zone_id="$(kp_public_provider_zone_id)"
  kp_records="${kp_public_provider_tmp}/records.json"
  kp_public_provider_record_list "${kp_zone_id}" "${kp_records}"
  [[ "$(jq -er '.result | length' "${kp_records}")" == "0" ]] ||
    kp_die "run-scoped Cloudflare record already exists; refusing to adopt it"

  kp_public_provider_append_planned
  kp_body="${kp_public_provider_tmp}/create.json"
  jq -n --arg host "${KUBERPLOY_E2E_PUBLIC_HOSTNAME}" \
    --arg target "${KUBERPLOY_E2E_PUBLIC_DNS_TARGET}" \
    --arg comment "kuberploy qualification ${KUBERPLOY_E2E_RUN_ID}" \
    '{type:"A",name:$host,content:$target,ttl:60,proxied:false,comment:$comment}' >"${kp_body}"
  kp_create="${kp_public_provider_tmp}/create-response.json"
  [[ "$(kp_public_provider_api POST "/zones/${kp_zone_id}/dns_records" \
    "${kp_create}" "${kp_body}")" == "200" ]] || kp_die "Cloudflare DNS record creation failed"
  jq -e '.success == true' "${kp_create}" >/dev/null ||
    kp_die "Cloudflare DNS record creation returned an invalid response"
  kp_record_id="$(jq -er '.result.id | select(test("^[a-f0-9]{32}$"))' "${kp_create}")"
  kp_public_provider_finalize_inventory "${kp_record_id}"

  local kp_record="${kp_public_provider_tmp}/record.json"
  [[ "$(kp_public_provider_get_record "${kp_zone_id}" "${kp_record_id}" "${kp_record}")" == "200" ]] ||
    kp_die "Cloudflare DNS record disappeared after creation"
  kp_public_provider_record_matches "${kp_record}" "${kp_record_id}" ||
    kp_die "Cloudflare returned a different DNS record than the run requested"
  kp_public_provider_wait_dns || kp_die "public DNS did not resolve to the requested run target"
  kp_public_provider_write_evidence "${kp_zone_id}" "${kp_record_id}"
}

kp_public_provider_cleanup() {
  kp_public_provider_validate_inputs
  kp_public_provider_setup
  trap kp_public_provider_cleanup_tmp RETURN
  local kp_inventory="${KUBERPLOY_E2E_PUBLIC_PROVIDER_INVENTORY_FILE:?inventory file required}"
  local kp_zone_id kp_record_id kp_record kp_status kp_count
  [[ -f "${kp_inventory}" && ! -L "${kp_inventory}" ]] ||
    kp_die "public-provider inventory is missing"
  kp_count="$(wc -l <"${kp_inventory}" | tr -d ' ')"
  [[ "${kp_count}" == "1" ]] || kp_die "public-provider inventory must contain exactly one record"
  kp_zone_id="$(kp_public_provider_zone_id)"
  kp_record_id="$(jq -er '.uid // empty' "${kp_inventory}")"
  kp_record="${kp_public_provider_tmp}/record.json"

  if [[ -n "${kp_record_id}" ]]; then
    kp_status="$(kp_public_provider_get_record "${kp_zone_id}" "${kp_record_id}" "${kp_record}")"
    if [[ "${kp_status}" == "200" ]]; then
      kp_public_provider_record_matches "${kp_record}" "${kp_record_id}" ||
        kp_die "refusing to delete a Cloudflare record whose identity changed"
    elif [[ "${kp_status}" != "404" ]]; then
      kp_die "Cloudflare DNS record cleanup lookup failed"
    fi
  else
    kp_records="${kp_public_provider_tmp}/records.json"
    kp_public_provider_record_list "${kp_zone_id}" "${kp_records}"
    [[ "$(jq -er '.result | length' "${kp_records}")" == "0" ]] ||
      kp_die "refusing to clean an unfinalized Cloudflare record without identity proof"
  fi

  if [[ -n "${kp_record_id}" && "${kp_status}" == "200" ]]; then
    [[ "$(kp_public_provider_api DELETE \
      "/zones/${kp_zone_id}/dns_records/${kp_record_id}" "${kp_record}")" == "200" ]] ||
      kp_die "Cloudflare DNS record deletion failed"
    for _ in {1..30}; do
      kp_status="$(kp_public_provider_get_record "${kp_zone_id}" "${kp_record_id}" "${kp_record}")"
      [[ "${kp_status}" == "404" ]] && break
      sleep 2
    done
    [[ "${kp_status}" == "404" ]] || kp_die "Cloudflare DNS record remained after cleanup"
  fi
  kp_public_provider_wait_dns_absent ||
    kp_die "public DNS still exposed the run-scoped record after cleanup"
  jq -n --arg run "${KUBERPLOY_E2E_RUN_ID}" \
    '{schemaVersion:1,runID:$run,stage:"110-public-provider",status:"cleaned",
      cleanedOrRestoredCount:1,verifiedUIDAndOwnership:true,verifiedAbsentOrRestored:true}' \
    >"${KUBERPLOY_E2E_PUBLIC_PROVIDER_CLEANUP_RESULT_FILE:?cleanup result file required}"
  chmod 600 "${KUBERPLOY_E2E_PUBLIC_PROVIDER_CLEANUP_RESULT_FILE}"
}

kp_public_provider_verify_https() {
  local kp_evidence="${KUBERPLOY_E2E_PUBLIC_PROVIDER_HTTPS_EVIDENCE_FILE:?HTTPS evidence file required}"
  local kp_host="${KUBERPLOY_E2E_PUBLIC_HOSTNAME}" kp_target="${KUBERPLOY_E2E_PUBLIC_DNS_TARGET:-}" kp_http_status
  local kp_cert="${kp_evidence}.cert" kp_handshake="${kp_evidence}.handshake"
  if [[ -n "${kp_target}" ]]; then
    kp_public_provider_validate_target
  else
    kp_target="${kp_host}"
  fi
  openssl s_client -connect "${kp_target}:443" -servername "${kp_host}" </dev/null 2>"${kp_handshake}" |
    openssl x509 -out "${kp_cert}" >/dev/null
  openssl x509 -in "${kp_cert}" -checkhost "${kp_host}" -checkend 300 -noout >/dev/null
  if [[ "${kp_target}" == "${kp_host}" ]]; then
    kp_http_status="$(curl --silent --show-error --output "${kp_evidence}.body" \
      --write-out '%{http_code}' "https://${kp_host}/")"
  else
    kp_http_status="$(curl --silent --show-error --resolve "${kp_host}:443:${kp_target}" \
      --output "${kp_evidence}.body" --write-out '%{http_code}' "https://${kp_host}/")"
  fi
  [[ "${kp_http_status}" == "200" ]] || kp_die "public HTTPS route returned HTTP ${kp_http_status}"
  jq -n --arg host "${kp_host}" --arg status "${kp_http_status}" \
    '{schemaVersion:1,hostname:$host,tlsHostnameVerified:true,tlsExpiryWindowSeconds:300,
      httpStatus:($status|tonumber),publicHTTPSObserved:true}' >"${kp_evidence}"
  chmod 600 "${kp_evidence}"
  find "${kp_cert}" "${kp_handshake}" "${kp_evidence}.body" -type f -delete
}
