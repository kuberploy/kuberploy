#!/usr/bin/env bash

set -Eeuo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
source "$(dirname "${BASH_SOURCE[0]}")/security-driver.sh"
source "$(dirname "${BASH_SOURCE[0]}")/github-build-workflow.sh"
source "$(dirname "${BASH_SOURCE[0]}")/config-edge-driver.sh"
source "$(dirname "${BASH_SOURCE[0]}")/image-tag-resolution-workflow.sh"
source "$(dirname "${BASH_SOURCE[0]}")/outbox-relay-job.sh"

kp_action="${1:?run or cleanup required}"
kp_namespace="kuberploy-e2e-${KUBERPLOY_E2E_RUN_ID}"
kp_marker="probe-${KUBERPLOY_E2E_STAGE_ID}"
kp_scenario="${KUBERPLOY_E2E_SCENARIO_FILE:?scenario required}"

kp_plan_create_inventory() {
  local kp_api_version="${1:?apiVersion required}" kp_kind="${2:?kind required}"
  local kp_namespace_value="${3-}" kp_name="${4:?name required}"
  jq -cn --arg run "${KUBERPLOY_E2E_RUN_ID}" --arg stage "${KUBERPLOY_E2E_STAGE_ID}" \
    --arg api "${kp_api_version}" --arg kind "${kp_kind}" --arg ns "${kp_namespace_value}" \
    --arg name "${kp_name}" --arg label "${KUBERPLOY_E2E_RUN_LABEL_KEY}" \
    --arg managed "${KUBERPLOY_E2E_MANAGED_BY_LABEL_VALUE}" '
      {schemaVersion:1,runID:$run,stage:$stage,apiVersion:$api,kind:$kind,
       namespace:$ns,name:$name,uid:null,operation:"planned-create",absentBefore:true,
       cleanupPolicy:"delete",ownership:{runLabelKey:$label,runLabelValue:$run,managedBy:$managed}}
    ' >>"${KUBERPLOY_E2E_STAGE_INVENTORY_FILE}"
  chmod 600 "${KUBERPLOY_E2E_STAGE_INVENTORY_FILE}"
}

kp_finalize_create_inventory() {
  local kp_kind="${1:?kind required}" kp_namespace_value="${2-}"
  local kp_name="${3:?name required}" kp_uid="${4:?uid required}"
  local kp_tmp="${KUBERPLOY_E2E_STAGE_INVENTORY_FILE}.tmp"
  jq -c --arg kind "${kp_kind}" --arg ns "${kp_namespace_value}" \
    --arg name "${kp_name}" --arg uid "${kp_uid}" '
      if .kind == $kind and .namespace == $ns and .name == $name and
         .operation == "planned-create" and .uid == null
      then .operation = "created" | .uid = $uid
      else . end
    ' "${KUBERPLOY_E2E_STAGE_INVENTORY_FILE}" >"${kp_tmp}"
  chmod 600 "${kp_tmp}"
  mv -- "${kp_tmp}" "${KUBERPLOY_E2E_STAGE_INVENTORY_FILE}"
}

kp_create_owned_namespace() {
  local kp_target_namespace="${1:?namespace required}"
  [[ -z "$("${KUBERPLOY_E2E_KUBECTL}" get namespace "${kp_target_namespace}" \
    --ignore-not-found -o name)" ]] || kp_die "namespace ${kp_target_namespace} already exists"
  kp_plan_create_inventory v1 Namespace "" "${kp_target_namespace}"
  jq -n --arg name "${kp_target_namespace}" --arg run "${KUBERPLOY_E2E_RUN_ID}" \
    --arg managed "${KUBERPLOY_E2E_MANAGED_BY_LABEL_VALUE}" '
      {apiVersion:"v1",kind:"Namespace",metadata:{name:$name,labels:{
        "kuberploy.io/test-run":$run,"app.kubernetes.io/managed-by":$managed}}}
    ' | "${KUBERPLOY_E2E_KUBECTL}" create -f - >/dev/null
  local kp_uid
  kp_uid="$("${KUBERPLOY_E2E_KUBECTL}" get namespace "${kp_target_namespace}" -o json | jq -er '.metadata.uid')"
  kp_finalize_create_inventory Namespace "" "${kp_target_namespace}" "${kp_uid}"
}

kp_create_marker() {
  [[ -z "$("${KUBERPLOY_E2E_KUBECTL}" get configmap "${kp_marker}" \
    --namespace "${kp_namespace}" --ignore-not-found -o name)" ]] ||
    kp_die "stage marker ${kp_marker} already exists"
  kp_plan_create_inventory v1 ConfigMap "${kp_namespace}" "${kp_marker}"
  jq -n --arg name "${kp_marker}" --arg ns "${kp_namespace}" \
    --arg run "${KUBERPLOY_E2E_RUN_ID}" \
    --arg managed "${KUBERPLOY_E2E_MANAGED_BY_LABEL_VALUE}" \
    --arg stage "${KUBERPLOY_E2E_STAGE_ID}" '
      {apiVersion:"v1",kind:"ConfigMap",metadata:{name:$name,namespace:$ns,labels:{
        "kuberploy.io/test-run":$run,"app.kubernetes.io/managed-by":$managed}},
       data:{stage:$stage}}
    ' | "${KUBERPLOY_E2E_KUBECTL}" create -f - >/dev/null
  local kp_uid
  kp_uid="$("${KUBERPLOY_E2E_KUBECTL}" get configmap "${kp_marker}" \
    --namespace "${kp_namespace}" -o json | jq -er '.metadata.uid')"
  kp_finalize_create_inventory ConfigMap "${kp_namespace}" "${kp_marker}" "${kp_uid}"
}

kp_json_pointer_matches() {
  local kp_file="${1:?file required}" kp_pointer="${2:?pointer required}"
  local kp_expected="${3:?expected JSON required}"
  [[ "${kp_pointer}" =~ ^/([A-Za-z0-9_.:-]+/)*[A-Za-z0-9_.:-]+$ ]] || return 1
  jq -e --arg pointer "${kp_pointer}" --argjson expected "${kp_expected}" '
    getpath($pointer | ltrimstr("/") | split("/")) == $expected
  ' "${kp_file}" >/dev/null
}

kp_probe_kubernetes() {
  local kp_spec="${1:?spec required}" kp_out="${2:?output required}"
  local kp_resource kp_name kp_ns kp_pointer kp_expected
  kp_resource="$(jq -er '.resource | select(test("^[a-z0-9.]+$"))' <<<"${kp_spec}")"
  kp_name="$(jq -er '.name | select(test("^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$"))' <<<"${kp_spec}")"
  kp_ns="$(jq -er '.namespace // "" | select(test("^[a-z0-9-]*$"))' <<<"${kp_spec}")"
  kp_pointer="$(jq -er '.jsonPointer' <<<"${kp_spec}")"
  kp_expected="$(jq -c '.expected' <<<"${kp_spec}")"
  if [[ -n "${kp_ns}" ]]; then
    "${KUBERPLOY_E2E_KUBECTL}" get "${kp_resource}" "${kp_name}" --namespace "${kp_ns}" -o json >"${kp_out}"
  else
    "${KUBERPLOY_E2E_KUBECTL}" get "${kp_resource}" "${kp_name}" -o json >"${kp_out}"
  fi
  kp_json_pointer_matches "${kp_out}" "${kp_pointer}" "${kp_expected}"
}

kp_probe_api() {
  local kp_spec="${1:?spec required}" kp_out="${2:?output required}"
  local kp_method kp_path kp_status kp_pointer kp_expected kp_request kp_actual
  kp_method="$(jq -er '.method | select(. == "GET")' <<<"${kp_spec}")"
  kp_path="$(jq -er '.path | select(test("^/[A-Za-z0-9_./:-]+$") and (contains("..") | not))' <<<"${kp_spec}")"
  kp_status="$(jq -er '.expectedStatus | select(type == "number" and . >= 200 and . < 500)' <<<"${kp_spec}")"
  kp_pointer="$(jq -er '.jsonPointer' <<<"${kp_spec}")"
  kp_expected="$(jq -c '.expected' <<<"${kp_spec}")"
  local -a kp_args=(--silent --show-error --output "${kp_out}" --write-out '%{http_code}'
    --request "${kp_method}" --header "$(<"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")")
  kp_actual="$(curl "${kp_args[@]}" "$(jq -r '.apiBaseURL' "${kp_scenario}")${kp_path}")"
  [[ "${kp_actual}" == "${kp_status}" ]]
  kp_json_pointer_matches "${kp_out}" "${kp_pointer}" "${kp_expected}"
}

kp_probe_http() {
  local kp_spec="${1:?spec required}" kp_out="${2:?output required}" kp_status kp_actual kp_url
  kp_url="$(jq -er '.url | select(test("^https?://[^[:space:]@]+$") and (contains("#") | not))' <<<"${kp_spec}")"
  kp_status="$(jq -er '.expectedStatus | select(type == "number" and . >= 200 and . < 500)' <<<"${kp_spec}")"
  kp_actual="$(curl --silent --show-error --output "${kp_out}" --write-out '%{http_code}' "${kp_url}")"
  [[ "${kp_actual}" == "${kp_status}" ]]
}

kp_probe_tls() {
  local kp_spec="${1:?spec required}" kp_out="${2:?output required}" kp_host kp_port kp_seconds
  kp_host="$(jq -er '.hostname | select(test("^[a-z0-9]([-a-z0-9.]*[a-z0-9])$"))' <<<"${kp_spec}")"
  kp_qualification_validate_hostname tls-hostname "${kp_host}"
  kp_port="$(jq -er '.port // 443 | select(type == "number" and . > 0 and . < 65536)' <<<"${kp_spec}")"
  kp_seconds="$(jq -er '.minimumRemainingSeconds // 300 | select(type == "number" and . >= 0)' <<<"${kp_spec}")"
  openssl s_client -connect "${kp_host}:${kp_port}" -servername "${kp_host}" </dev/null 2>"${kp_out}.handshake" \
    | openssl x509 -out "${kp_out}" >/dev/null
  openssl x509 -in "${kp_out}" -checkhost "${kp_host}" -noout >/dev/null
  openssl x509 -in "${kp_out}" -checkend "${kp_seconds}" -noout >/dev/null
}

kp_probe_dns() {
  local kp_spec="${1:?spec required}" kp_out="${2:?output required}" kp_host kp_expected
  kp_host="$(jq -er '.hostname | select(test("^[a-z0-9]([-a-z0-9.]*[a-z0-9])$"))' <<<"${kp_spec}")"
  kp_qualification_validate_hostname dns-hostname "${kp_host}"
  dig +short A "${kp_host}" | sort -u >"${kp_out}"
  [[ -s "${kp_out}" ]]
  kp_expected="$(jq -r '.expectedAddress // empty' <<<"${kp_spec}")"
  [[ -z "${kp_expected}" ]] || grep -Fx -- "${kp_expected}" "${kp_out}" >/dev/null
}

kp_cookie_header_from_jar() {
  local kp_jar="${1:?cookie jar required}" kp_out="${2:?cookie header required}"
  sed 's/^#HttpOnly_//' "${kp_jar}" | awk 'BEGIN{ORS=""} !/^#/ && NF >= 7 {if(n++) printf "; "; printf "%s=%s",$6,$7} END{printf "\n"}' \
    | sed 's/^/Cookie: /' >"${kp_out}"
  chmod 600 "${kp_out}"
  LC_ALL=C grep -Eq '^Cookie: kuberploy_' "${kp_out}"
}

kp_run_installed_auth_and_contract_workflow() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_base kp_secret_dir kp_job kp_ns
  local kp_bootstrap_token kp_admin_email kp_developer_email kp_admin_password kp_developer_password
  local kp_actual kp_admin_id kp_developer_id
  local kp_cookie_jar kp_headers kp_invitation kp_team kp_installation
  kp_base="$(jq -er '.apiBaseURL' "${kp_scenario}")"
  kp_secret_dir="$(dirname "${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")"
  kp_cookie_jar="${kp_secret_dir}/admin-cookie-jar"
  kp_headers="${kp_secret_dir}/response-headers"
  chmod 700 "${kp_secret_dir}"
  for _ in {1..180}; do
    if "${KUBERPLOY_E2E_KUBECTL}" get jobs.batch --all-namespaces \
        --selector app.kubernetes.io/component=bootstrap-token -o json >"${kp_secret_dir}/jobs.json" 2>/dev/null &&
      jq -e '.items | length == 1' "${kp_secret_dir}/jobs.json" >/dev/null; then break; fi
    sleep 2
  done
  kp_job="$(jq -er '.items[0].metadata.name' "${kp_secret_dir}/jobs.json")"
  kp_ns="$(jq -er '.items[0].metadata.namespace' "${kp_secret_dir}/jobs.json")"
  "${KUBERPLOY_E2E_KUBECTL}" logs --namespace "${kp_ns}" "job/${kp_job}" >"${kp_secret_dir}/bootstrap.log"
  [[ "$(LC_ALL=C grep -Ec '^KUBERPLOY_BOOTSTRAP_TOKEN=kp_bootstrap_[A-Za-z0-9_-]{43}$' "${kp_secret_dir}/bootstrap.log")" == 1 ]]
  [[ "$(wc -l <"${kp_secret_dir}/bootstrap.log" | tr -d ' ')" == 1 ]]
  kp_bootstrap_token="$(sed -nE 's/^KUBERPLOY_BOOTSTRAP_TOKEN=(kp_bootstrap_[A-Za-z0-9_-]{43})$/\1/p' "${kp_secret_dir}/bootstrap.log")"
  kp_admin_email="qualification-admin-${KUBERPLOY_E2E_RUN_ID}@example.test"
  kp_developer_email="qualification-developer-${KUBERPLOY_E2E_RUN_ID}@example.test"
  kp_admin_password="$(openssl rand -base64 32 | tr -d '\n')Aa1!"
  kp_developer_password="$(openssl rand -base64 32 | tr -d '\n')Bb2!"
  jq -n --arg token "${kp_bootstrap_token}" --arg email "${kp_admin_email}" --arg password "${kp_admin_password}" \
    '{token:$token,email:$email,displayName:"Qualification Admin",password:$password}' >"${kp_secret_dir}/request.json"
  kp_actual="$(curl --silent --show-error -c "${kp_cookie_jar}" -D "${kp_headers}" -o "${kp_dir}/auth-bootstrap.json" \
    -w '%{http_code}' -H 'Content-Type: application/json' --data-binary "@${kp_secret_dir}/request.json" "${kp_base}/v1/auth/bootstrap")"
  [[ "${kp_actual}" == 201 ]]
  kp_admin_id="$(jq -er '.id' "${kp_dir}/auth-bootstrap.json")"
  kp_cookie_header_from_jar "${kp_cookie_jar}" "${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}"
  awk 'tolower($1)=="x-csrf-token:" {gsub("\\r",""); sub(/^[^:]+:[[:space:]]*/,""); print; exit}' \
    "${kp_headers}" >"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}"
  chmod 600 "${KUBERPLOY_E2E_CSRF_TOKEN_FILE}"

  kp_actual="$(curl -sS -o /dev/null -w '%{http_code}' -X POST --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" "${kp_base}/v1/auth/logout")"
  [[ "${kp_actual}" == 204 ]]
  [[ "$(curl -sS -o /dev/null -w '%{http_code}' --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" "${kp_base}/v1/me")" == 401 ]]
  jq -n --arg email "${kp_admin_email}" --arg password "${kp_admin_password}" \
    '{email:$email,password:$password}' >"${kp_secret_dir}/request.json"
  kp_actual="$(curl -sS -c "${kp_cookie_jar}" -D "${kp_headers}" -o "${kp_dir}/auth-admin-login.json" -w '%{http_code}' \
    -H 'Content-Type: application/json' --data-binary "@${kp_secret_dir}/request.json" "${kp_base}/v1/auth/login")"
  [[ "${kp_actual}" == 200 ]]
  kp_cookie_header_from_jar "${kp_cookie_jar}" "${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}"
  awk 'tolower($1)=="x-csrf-token:" {gsub("\\r",""); sub(/^[^:]+:[[:space:]]*/,""); print; exit}' "${kp_headers}" >"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}"
  chmod 600 "${KUBERPLOY_E2E_CSRF_TOKEN_FILE}"

  kp_human_post invite-user /v1/users/invitations \
    "$(jq -cn --arg email "${kp_developer_email}" '{email:$email}')" 201 "${kp_secret_dir}/invitation.json"
  kp_invitation="$(jq -er '.token' "${kp_secret_dir}/invitation.json")"
  jq -n --arg token "${kp_invitation}" --arg password "${kp_developer_password}" \
    '{token:$token,displayName:"Qualification Developer",password:$password}' >"${kp_secret_dir}/request.json"
  kp_actual="$(curl -sS -c "${kp_secret_dir}/developer-cookie-jar" -o "${kp_dir}/auth-invitation-accepted.json" -w '%{http_code}' \
    -H 'Content-Type: application/json' --data-binary "@${kp_secret_dir}/request.json" "${kp_base}/v1/auth/invitations/accept")"
  [[ "${kp_actual}" == 201 ]]
  kp_developer_id="$(jq -er '.id' "${kp_dir}/auth-invitation-accepted.json")"
  jq -n --arg email "${kp_developer_email}" --arg password "${kp_developer_password}" \
    '{email:$email,password:$password}' >"${kp_secret_dir}/request.json"
  [[ "$(curl -sS -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' --data-binary "@${kp_secret_dir}/request.json" "${kp_base}/v1/auth/login")" == 200 ]]

  kp_human_post create-auth-team /v1/teams '{"name":"Qualification Owners","slug":"qualification-owners"}' 201 "${kp_dir}/auth-team.json"
  kp_team="$(jq -er '.id' "${kp_dir}/auth-team.json")"
  kp_actual="$(curl -sS -o "${kp_dir}/auth-sole-owner-denial.json" -w '%{http_code}' -X DELETE \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" \
    "${kp_base}/v1/teams/${kp_team}/members/${kp_admin_id}")"
  [[ "${kp_actual}" == 409 ]]
  kp_human_post add-auth-team-member "/v1/teams/${kp_team}/members" \
    "$(jq -cn --arg u "${kp_developer_id}" '{userId:$u,role:"member"}')" 201 \
    "${kp_dir}/auth-team-member.json"
  kp_human_post promote-auth-team-member "/v1/teams/${kp_team}/members" \
    "$(jq -cn --arg u "${kp_developer_id}" '{userId:$u,role:"owner"}')" 201 \
    "${kp_dir}/auth-team-member-promoted.json"
  kp_human_post demote-auth-team-member "/v1/teams/${kp_team}/members" \
    "$(jq -cn --arg u "${kp_developer_id}" '{userId:$u,role:"member"}')" 201 \
    "${kp_dir}/auth-team-member-demoted.json"
  kp_actual="$(curl -sS -o "${kp_dir}/auth-team-member-removed.json" -w '%{http_code}' -X DELETE \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" \
    "${kp_base}/v1/teams/${kp_team}/members/${kp_developer_id}")"
  [[ "${kp_actual}" == 204 ]]
  kp_human_post register-installation /v1/github/installations \
    '{"githubInstallationId":987654321,"accountLogin":"qualification-fixture","accountType":"Organization","repositorySelection":"selected","repositoryCount":1}' \
    201 "${kp_dir}/auth-github-installation.json"
  kp_installation="$(jq -er '.id' "${kp_dir}/auth-github-installation.json")"
  kp_actual="$(curl -sS -o "${kp_dir}/auth-github-sharing.json" -w '%{http_code}' -X PATCH \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" \
    --header "Idempotency-Key: qualification-${KUBERPLOY_E2E_RUN_ID}-10-one-chart-install-github-sharing" \
    -H 'Content-Type: application/json' --data-binary "{\"visibility\":\"team\",\"teamId\":\"${kp_team}\"}" \
    "${kp_base}/v1/github/installations/${kp_installation}/sharing")"
  [[ "${kp_actual}" == 200 ]]

  curl -fsS "${kp_base}/openapi.json" -o "${kp_dir}/contract-openapi.json"
  curl -fsS "${kp_base}/openapi.yaml" -o "${kp_dir}/contract-openapi.yaml"
  cmp -s "${kp_dir}/contract-openapi.json" "${kp_dir}/contract-openapi.yaml"
  curl -fsS "${kp_base}/openapi-agent.json" -o "${kp_dir}/contract-agent.json"
  curl -fsS "${kp_base}/arazzo.yaml" -o "${kp_dir}/contract-arazzo.yaml"
  curl -fsS "${kp_base}/docs/" -o "${kp_dir}/contract-swagger.html"
  jq -e '.openapi=="3.2.0" and .paths["/v1/auth/login"].post.operationId=="loginWithLocalPassword"' "${kp_dir}/contract-openapi.json" >/dev/null
  jq -e '.operations|type=="array" and all(.[];.operationId!="bootstrapAdministrator" and .operationId!="loginWithLocalPassword")' "${kp_dir}/contract-agent.json" >/dev/null
  grep -F 'sourceDescription:' "${kp_dir}/contract-arazzo.yaml" >/dev/null
  grep -F 'url: "/openapi.yaml"' "${kp_dir}/contract-swagger.html" >/dev/null
  rm -f -- "${kp_secret_dir}/request.json" "${kp_secret_dir}/invitation.json" "${kp_secret_dir}/bootstrap.log" "${kp_secret_dir}/jobs.json"
  jq -n --arg admin "${kp_admin_id}" --arg developer "${kp_developer_id}" --arg team "${kp_team}" --arg installation "${kp_installation}" \
    '{adminUserId:$admin,developerUserId:$developer,teamId:$team,installationId:$installation,
      bootstrapTokenJobConsumed:true,logoutInvalidatedSession:true,adminRecurringLogin:true,
      invitationAccepted:true,developerRecurringLogin:true,soleOwnerDenied:true,
      githubMetadataTeamShared:true,contractsExact:true,secretsExcludedFromEvidence:true}' \
    >"${kp_dir}/auth-contract-proof.json"
}

kp_probe_helm_install() {
  local kp_out="${1:?output required}" kp_application_count kp_revision_count kp_digest_count
  local kp_manifest="${KUBERPLOY_E2E_STAGE_DIR}/evidence/installer-planned-manifest.yaml"
  [[ "${KUBERPLOY_E2E_STAGE_ID}" == "10-one-chart-install" ]]
  "${KUBERPLOY_E2E_HELM}" template kuberploy-qualification \
    "$(kp_repo_root)/charts/kuberploy-installer" --namespace kuberploy-system \
    --values "${KUBERPLOY_E2E_UPGRADE_FROM_VALUES_FILE}" \
    >"${kp_manifest}"
  [[ -s "${kp_manifest}" ]]
  kp_application_count="$(awk '$1 == "kind:" && $2 == "Application" {n++} END {print n+0}' "${kp_manifest}")"
  kp_revision_count="$(awk '$1 == "targetRevision:" {gsub(/\"/, "", $2); if ($2 ~ /^[0-9a-f]{40}$/) n++} END {print n+0}' "${kp_manifest}")"
  kp_digest_count="$(awk '$1 == "kuberploy.io\/expected-package-version:" {gsub(/\"/, "", $2); if ($2 ~ /^[0-9]+\.[0-9]+\.[0-9]+-rc\.[0-9]+$/) n++} END {print n+0}' "${kp_manifest}")"
  (( kp_application_count >= 2 && kp_revision_count == kp_application_count &&
     kp_digest_count == kp_application_count )) ||
    kp_die "installer render lacks independent Applications with exact source revisions/package digests"
  openssl dgst -sha256 "${kp_manifest}" \
    >"${KUBERPLOY_E2E_STAGE_DIR}/evidence/installer-planned-manifest.sha256"
  "${KUBERPLOY_E2E_HELM}" upgrade --install kuberploy-qualification \
    "$(kp_repo_root)/charts/kuberploy-installer" --namespace kuberploy-system \
    --values "${KUBERPLOY_E2E_UPGRADE_FROM_VALUES_FILE}" \
    --server-side=false --wait --timeout 15m
  "${KUBERPLOY_E2E_HELM}" status kuberploy-qualification \
    --namespace kuberploy-system -o json >"${kp_out}"
  jq -e '.info.status == "deployed"' "${kp_out}" >/dev/null
  local kp_release_objects="${KUBERPLOY_E2E_STAGE_DIR}/evidence/installer-release-objects.json"
  local kp_release_namespaced="${kp_release_objects}.namespaced" kp_release_cluster="${kp_release_objects}.cluster"
  "${KUBERPLOY_E2E_KUBECTL}" get \
    deployments.apps,statefulsets.apps,services,configmaps,secrets,serviceaccounts,roles.rbac.authorization.k8s.io,rolebindings.rbac.authorization.k8s.io,applications.argoproj.io,appprojects.argoproj.io \
    --all-namespaces --selector app.kubernetes.io/instance=kuberploy-qualification -o json \
    >"${kp_release_namespaced}"
  "${KUBERPLOY_E2E_KUBECTL}" get \
    clusterroles.rbac.authorization.k8s.io,clusterrolebindings.rbac.authorization.k8s.io \
    --selector app.kubernetes.io/instance=kuberploy-qualification -o json \
    >"${kp_release_cluster}"
  jq -s '
    {resources:[.[].items[]? | {apiVersion,kind,namespace:(.metadata.namespace // ""),
      name:.metadata.name,uid:.metadata.uid,resourceVersion:.metadata.resourceVersion,
      helmInstance:.metadata.labels["app.kubernetes.io/instance"]}] |
      sort_by(.apiVersion,.kind,.namespace,.name)} as $snapshot |
    select($snapshot.resources | length > 0) |
    select(all($snapshot.resources[]; (.uid | type == "string" and length > 0) and
      .helmInstance == "kuberploy-qualification")) | $snapshot
  ' "${kp_release_namespaced}" "${kp_release_cluster}" >"${kp_release_objects}"
  [[ -s "${kp_release_objects}" ]]
  rm -- "${kp_release_namespaced}" "${kp_release_cluster}"
  chmod 600 "${kp_release_objects}"
  kp_run_installed_auth_and_contract_workflow
  jq -n --argjson applications "${kp_application_count}" \
    --argjson revisions "${kp_revision_count}" --argjson digests "${kp_digest_count}" '
    {mutation:"installer-render-and-release",applicationCount:$applications,
     independentApplicationsCreated:($applications >= 2),
     immutableSourceRevisions:($revisions == $applications),
     packageDigestsAttested:($digests == $applications)} + $auth[0]
  ' --slurpfile auth "${KUBERPLOY_E2E_STAGE_DIR}/evidence/auth-contract-proof.json" >"${KUBERPLOY_E2E_STAGE_DIR}/evidence/installer-proof.json"
  chmod 600 "${KUBERPLOY_E2E_STAGE_DIR}/evidence/installer-proof.json"
}

kp_snapshot_run_resources() {
  local kp_out="${KUBERPLOY_E2E_STAGE_DIR}/evidence/run-labeled-resources.json"
  local kp_namespaced="${kp_out}.namespaced" kp_cluster="${kp_out}.cluster"
  "${KUBERPLOY_E2E_KUBECTL}" get \
    deployments.apps,statefulsets.apps,daemonsets.apps,services,configmaps,secrets,serviceaccounts,roles.rbac.authorization.k8s.io,rolebindings.rbac.authorization.k8s.io,applications.argoproj.io,appprojects.argoproj.io \
    --all-namespaces --selector "${KUBERPLOY_E2E_RUN_LABEL_KEY}=${KUBERPLOY_E2E_RUN_ID}" -o json \
    >"${kp_namespaced}"
  "${KUBERPLOY_E2E_KUBECTL}" get \
    namespaces,clusterroles.rbac.authorization.k8s.io,clusterrolebindings.rbac.authorization.k8s.io \
    --selector "${KUBERPLOY_E2E_RUN_LABEL_KEY}=${KUBERPLOY_E2E_RUN_ID}" -o json \
    >"${kp_cluster}"
  jq -s --arg run "${KUBERPLOY_E2E_RUN_ID}" '
    {runID:$run,resources:[.[].items[]? | {
      apiVersion,kind,namespace:(.metadata.namespace // ""),name:.metadata.name,uid:.metadata.uid,
      resourceVersion:.metadata.resourceVersion,
      ownership:{runLabelValue:.metadata.labels["kuberploy.io/test-run"],
        managedBy:.metadata.labels["app.kubernetes.io/managed-by"]}
    }] | sort_by(.apiVersion,.kind,.namespace,.name)}
  ' "${kp_namespaced}" "${kp_cluster}" >"${kp_out}"
  rm -- "${kp_namespaced}" "${kp_cluster}"
  chmod 600 "${kp_out}"
}

kp_human_post() {
  local kp_action="${1:?action required}" kp_path="${2:?path required}"
  local kp_body="${3:?body JSON required}" kp_expected="${4:?status required}"
  local kp_out="${5:?output required}" kp_actual
  [[ "${kp_path}" =~ ^/v1/[A-Za-z0-9_./-]+$ && "${kp_path}" != *".."* ]]
  jq -e 'type == "object"' <<<"${kp_body}" >/dev/null
  kp_actual="$(curl --silent --show-error --output "${kp_out}" --write-out '%{http_code}' \
    --request POST --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" \
    --header "Idempotency-Key: qualification-${KUBERPLOY_E2E_RUN_ID}-${KUBERPLOY_E2E_STAGE_ID}-${kp_action}" \
    --header 'Content-Type: application/json' --data-binary "${kp_body}" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")${kp_path}")"
  [[ "${kp_actual}" == "${kp_expected}" ]]
}

kp_human_post_empty() {
  local kp_action="${1:?action required}" kp_path="${2:?path required}"
  local kp_expected="${3:?status required}" kp_out="${4:?output required}" kp_actual
  [[ "${kp_path}" =~ ^/v1/[A-Za-z0-9_./-]+$ && "${kp_path}" != *".."* ]]
  kp_actual="$(curl --silent --show-error --output "${kp_out}" --write-out '%{http_code}' \
    --request POST --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" \
    --header "Idempotency-Key: qualification-${KUBERPLOY_E2E_RUN_ID}-${KUBERPLOY_E2E_STAGE_ID}-${kp_action}" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")${kp_path}")"
  [[ "${kp_actual}" == "${kp_expected}" ]]
}

kp_human_post_file() {
  local kp_action="${1:?action required}" kp_path="${2:?path required}"
  local kp_request_file="${3:?request file required}" kp_expected="${4:?status required}"
  local kp_out="${5:?output required}" kp_actual
  kp_qualification_validate_safe_file workflow-secret-request "${kp_request_file}" true
  kp_actual="$(curl --silent --show-error --output "${kp_out}" --write-out '%{http_code}' \
    --request POST --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" \
    --header "Idempotency-Key: qualification-${KUBERPLOY_E2E_RUN_ID}-${KUBERPLOY_E2E_STAGE_ID}-${kp_action}" \
    --header 'Content-Type: application/json' --data-binary "@${kp_request_file}" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")${kp_path}")"
  [[ "${kp_actual}" == "${kp_expected}" ]]
}

kp_human_put() {
  local kp_action="${1:?action required}" kp_path="${2:?path required}"
  local kp_body="${3:?body required}" kp_expected="${4:?status required}" kp_out="${5:?output required}" kp_actual
  kp_actual="$(curl --silent --show-error --output "${kp_out}" --write-out '%{http_code}' \
    --request PUT --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" \
    --header "Idempotency-Key: qualification-${KUBERPLOY_E2E_RUN_ID}-${KUBERPLOY_E2E_STAGE_ID}-${kp_action}" \
    --header 'Content-Type: application/json' --data-binary "${kp_body}" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")${kp_path}")"
  [[ "${kp_actual}" == "${kp_expected}" ]]
}

kp_poll_operation() {
  local kp_operation_id="${1:?operation ID required}" kp_out="${2:?output required}"
  [[ "${kp_operation_id}" =~ ^[a-f0-9-]{36}$ ]]
  local kp_attempt kp_status kp_actual
  for kp_attempt in {1..120}; do
    kp_actual="$(curl --silent --show-error --output "${kp_out}" --write-out '%{http_code}' \
      --request GET --header "$(<"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")" \
      "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/operations/${kp_operation_id}")"
    [[ "${kp_actual}" == "200" ]]
    kp_status="$(jq -er '.status | select(. == "queued" or . == "running" or . == "succeeded" or . == "failed" or . == "cancelled" or . == "superseded")' "${kp_out}")"
    case "${kp_status}" in
      succeeded) return 0 ;;
      failed|cancelled|superseded) return 1 ;;
    esac
    sleep 1
  done
  return 1
}

kp_run_git_workflow() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_body
  local kp_project_id kp_direct_environment_id kp_protected_environment_id kp_application_id
  local kp_direct_environment_namespace
  local kp_direct_operation_id kp_protected_operation_id kp_direct_deployment_id kp_rollback_operation_id
  local kp_direct_update_operation_id kp_direct_commit kp_direct_update_commit kp_rollback_commit
  local kp_successful_build_id
  [[ -f "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json" ]]
  kp_project_id="$(jq -er '.projectId | select(test("^[a-f0-9-]{36}$"))' \
    "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json")"
  kp_direct_environment_id="$(jq -er '.directEnvironmentId' "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json")"
  kp_direct_environment_namespace="$(jq -er '.directEnvironmentNamespace' "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json")"
  kp_application_id="$(jq -er '.applicationId' "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json")"
  kp_direct_operation_id="$(jq -er '.directOperationId' "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json")"
  kp_direct_deployment_id="$(jq -er '.directDeploymentId' "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json")"
  kp_direct_commit="$(jq -er '.directCommit' "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json")"
  kp_successful_build_id="$(jq -r '.successfulBuildId // ""' "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json")"
  kp_body="$(jq -c --arg id "${kp_project_id}" '.workflow.protectedEnvironment + {projectId:$id,protectionPolicy:"protected"}' "${kp_scenario}")"
  kp_human_post create-protected-environment /v1/environments "${kp_body}" 201 "${kp_dir}/workflow-protected-environment.json"
  kp_protected_environment_id="$(jq -er '.id | select(test("^[a-f0-9-]{36}$"))' "${kp_dir}/workflow-protected-environment.json")"

  kp_body="$(jq -c --arg environment "${kp_protected_environment_id}" --arg application "${kp_application_id}" '.workflow.protectedDeployment + {environmentId:$environment,applicationId:$application}' "${kp_scenario}")"
  kp_human_post protected-deployment /v1/deployments "${kp_body}" 202 "${kp_dir}/workflow-protected-operation.json"
  kp_protected_operation_id="$(jq -er '.id | select(test("^[a-f0-9-]{36}$"))' "${kp_dir}/workflow-protected-operation.json")"
  kp_poll_operation "${kp_protected_operation_id}" "${kp_dir}/workflow-protected-terminal.json"
  jq -e '.status == "succeeded" and (.pullRequest.url | type == "string" and startswith("https://")) and (.gitRevision == null)' "${kp_dir}/workflow-protected-terminal.json" >/dev/null

  kp_body="$(jq -c --arg environment "${kp_direct_environment_id}" --arg application "${kp_application_id}" '.workflow.directDeploymentUpdate + {environmentId:$environment,applicationId:$application}' "${kp_scenario}")"
  kp_human_post update-direct-deployment /v1/deployments "${kp_body}" 202 \
    "${kp_dir}/workflow-direct-update-operation.json"
  kp_direct_update_operation_id="$(jq -er '.id | select(test("^[a-f0-9-]{36}$"))' "${kp_dir}/workflow-direct-update-operation.json")"
  jq -e --arg target "${kp_direct_deployment_id}" '.targetId == $target' \
    "${kp_dir}/workflow-direct-update-operation.json" >/dev/null
  kp_poll_operation "${kp_direct_update_operation_id}" "${kp_dir}/workflow-direct-update-terminal.json"
  jq -e --arg first "${kp_direct_commit}" '.status == "succeeded" and .generation == 2 and
    (.gitRevision.commit | type == "string" and length == 40 and . != $first)' \
    "${kp_dir}/workflow-direct-update-terminal.json" >/dev/null
  kp_direct_update_commit="$(jq -r '.gitRevision.commit' "${kp_dir}/workflow-direct-update-terminal.json")"

  kp_body="$(jq -cn --arg source "${kp_direct_operation_id}" '{sourceOperationId:$source}')"
  kp_human_post rollback-direct-deployment "/v1/deployments/${kp_direct_deployment_id}/rollback" \
    "${kp_body}" 202 "${kp_dir}/workflow-rollback-operation.json"
  kp_rollback_operation_id="$(jq -er '.id | select(test("^[a-f0-9-]{36}$"))' "${kp_dir}/workflow-rollback-operation.json")"
  kp_poll_operation "${kp_rollback_operation_id}" "${kp_dir}/workflow-rollback-terminal.json"
  jq -e --arg first "${kp_direct_commit}" --arg current "${kp_direct_update_commit}" '
    .status == "succeeded" and .generation == 3 and
    (.gitRevision.commit | type == "string" and length == 40 and . != $first and . != $current)' \
    "${kp_dir}/workflow-rollback-terminal.json" >/dev/null
  kp_rollback_commit="$(jq -r '.gitRevision.commit' "${kp_dir}/workflow-rollback-terminal.json")"

  jq -n --arg project "${kp_project_id}" --arg directEnvironment "${kp_direct_environment_id}" \
    --arg directEnvironmentNamespace "${kp_direct_environment_namespace}" \
    --arg protectedEnvironment "${kp_protected_environment_id}" --arg application "${kp_application_id}" \
    --arg directOperation "${kp_direct_operation_id}" --arg protectedOperation "${kp_protected_operation_id}" \
    --arg directUpdateOperation "${kp_direct_update_operation_id}" \
    --arg directDeployment "${kp_direct_deployment_id}" --arg rollbackOperation "${kp_rollback_operation_id}" \
    --arg directCommit "${kp_direct_commit}" --arg directUpdateCommit "${kp_direct_update_commit}" \
    --arg rollbackCommit "${kp_rollback_commit}" \
    --arg successfulBuild "${kp_successful_build_id}" \
    '{projectId:$project,directEnvironmentId:$directEnvironment,directEnvironmentNamespace:$directEnvironmentNamespace,
      protectedEnvironmentId:$protectedEnvironment,
      applicationId:$application,directDeploymentId:$directDeployment,directOperationId:$directOperation,
      directUpdateOperationId:$directUpdateOperation,protectedOperationId:$protectedOperation,
      rollbackOperationId:$rollbackOperation,directCommit:$directCommit,
      directUpdateCommit:$directUpdateCommit,rollbackCommit:$rollbackCommit,
      directGeneration:3,successfulBuildId:$successfulBuild}' \
    >"${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json.tmp"
  mv -- "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json.tmp" \
    "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"
  chmod 600 "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"

  local kp_argo_name="kp-d-${kp_direct_deployment_id//-/}" kp_expected_image
  kp_expected_image="$(jq -er '.workflow.directDeployment.image | select(test("@sha256:[a-f0-9]{64}$"))' "${kp_scenario}")"
  for _ in {1..120}; do
    if "${KUBERPLOY_E2E_KUBECTL}" get application "${kp_argo_name}" --namespace argocd -o json \
        >"${kp_dir}/workflow-argo-application.json" 2>/dev/null &&
      jq -e --arg deployment "${kp_direct_deployment_id}" --arg application "${kp_application_id}" \
        --arg project "${kp_project_id}" --arg environment "${kp_direct_environment_id}" \
        --arg revision "${kp_rollback_commit}" '
        .metadata.labels["app.kubernetes.io/managed-by"] == "kuberploy" and
        .metadata.labels["kuberploy.io/deployment-id"] == $deployment and
        .metadata.labels["kuberploy.io/application-id"] == $application and
        .metadata.labels["kuberploy.io/project-id"] == $project and
        .metadata.labels["kuberploy.io/environment-id"] == $environment and
        (.metadata.annotations["kuberploy.io/runtime-chart-digest"] | test("^sha256:[a-f0-9]{64}$")) and
        .status.sync.status == "Synced" and .status.health.status == "Healthy" and
        ([.status.sync.revision,.status.sync.revisions[]?] | index($revision) != null) and
        any(.status.resources[]; .kind == "Deployment" and .status == "Synced" and .health.status == "Healthy")
      ' "${kp_dir}/workflow-argo-application.json" >/dev/null; then break; fi
    sleep 5
  done
  jq -e '.status.sync.status == "Synced" and .status.health.status == "Healthy"' \
    "${kp_dir}/workflow-argo-application.json" >/dev/null || kp_die "direct Argo Application did not converge"
  "${KUBERPLOY_E2E_KUBECTL}" get deployments --namespace "${kp_direct_environment_namespace}" \
    --selector "kuberploy.io/application-id=${kp_application_id},kuberploy.io/deployment-id=${kp_direct_deployment_id}" \
    -o json >"${kp_dir}/workflow-argo-runtime-deployment.json"
  jq -e --arg image "${kp_expected_image}" --arg application "${kp_application_id}" \
    --arg deployment "${kp_direct_deployment_id}" '
    [.items[] | select(.metadata.labels["kuberploy.io/application-id"] == $application and
      .metadata.labels["kuberploy.io/deployment-id"] == $deployment and
      any(.spec.template.spec.containers[]; .image == $image) and
      .status.observedGeneration == .metadata.generation and .status.availableReplicas >= 1)] | length == 1
  ' "${kp_dir}/workflow-argo-runtime-deployment.json" >/dev/null ||
    kp_die "Argo runtime Deployment does not match the immutable application digest"
}

kp_restart_owned_pod() {
  local kp_name="${1:?dependency name required}" kp_spec kp_ns kp_pod kp_controller kp_controller_uid kp_before kp_after
  kp_spec="$(jq -c --arg name "${kp_name}" '.workflow.recovery[$name]' "${kp_scenario}")"
  kp_ns="$(jq -r '.namespace' <<<"${kp_spec}")"
  kp_pod="$(jq -r '.podName' <<<"${kp_spec}")"
  kp_controller="$(jq -r '.controllerName' <<<"${kp_spec}")"
  kp_before="${KUBERPLOY_E2E_STAGE_DIR}/evidence/${kp_name}-before-restart.json"
  kp_after="${KUBERPLOY_E2E_STAGE_DIR}/evidence/${kp_name}-after-restart.json"
  "${KUBERPLOY_E2E_KUBECTL}" get pod "${kp_pod}" --namespace "${kp_ns}" -o json >"${kp_before}"
  kp_controller_uid="$("${KUBERPLOY_E2E_KUBECTL}" get statefulset "${kp_controller}" \
    --namespace "${kp_ns}" -o json | jq -er '.metadata.uid')"
  jq -e --arg controller "${kp_controller}" --arg controllerUID "${kp_controller_uid}" \
    --arg component "${kp_name}" '
    (.metadata.uid | type == "string" and length > 0) and
    .metadata.labels["app.kubernetes.io/name"] == $component and
    .metadata.labels["app.kubernetes.io/instance"] == $controller and
    any(.metadata.ownerReferences[];
      .kind == "StatefulSet" and .name == $controller and .uid == $controllerUID and .controller == true)
  ' "${kp_before}" >/dev/null || kp_die "${kp_name} restart target is not the exact platform StatefulSet Pod"
  "${KUBERPLOY_E2E_KUBECTL}" delete pod "${kp_pod}" --namespace "${kp_ns}" --wait=true >/dev/null
  "${KUBERPLOY_E2E_KUBECTL}" wait --for=condition=Ready "pod/${kp_pod}" \
    --namespace "${kp_ns}" --timeout=10m >/dev/null
  "${KUBERPLOY_E2E_KUBECTL}" get pod "${kp_pod}" --namespace "${kp_ns}" -o json >"${kp_after}"
  jq -e --arg before "$(jq -r '.metadata.uid' "${kp_before}")" \
    --arg controller "${kp_controller}" --arg controllerUID "${kp_controller_uid}" \
    --arg component "${kp_name}" '
    .metadata.uid != $before and
    .metadata.labels["app.kubernetes.io/name"] == $component and
    .metadata.labels["app.kubernetes.io/instance"] == $controller and
    any(.metadata.ownerReferences[];
      .kind == "StatefulSet" and .name == $controller and .uid == $controllerUID and .controller == true) and
    any(.status.conditions[]; .type == "Ready" and .status == "True")
  ' "${kp_after}" >/dev/null
}

kp_scale_owned_deployment() {
  local kp_namespace_value="${1:?namespace required}" kp_name="${2:?deployment required}"
  local kp_replicas="${3:?replicas required}"
  [[ "${kp_replicas}" == "0" || "${kp_replicas}" == "1" ]]
  "${KUBERPLOY_E2E_KUBECTL}" scale "deployment/${kp_name}" --namespace "${kp_namespace_value}" \
    --replicas="${kp_replicas}" >/dev/null
  if [[ "${kp_replicas}" == "1" ]]; then
    "${KUBERPLOY_E2E_KUBECTL}" rollout status "deployment/${kp_name}" \
      --namespace "${kp_namespace_value}" --timeout=10m >/dev/null
  else
    for _ in {1..120}; do
      [[ "$("${KUBERPLOY_E2E_KUBECTL}" get pods --namespace "${kp_namespace_value}" \
        --selector "app.kubernetes.io/component=worker" -o json | jq '.items | length')" == "0" ]] && return 0
      sleep 2
    done
    return 1
  fi
}

kp_create_outbox_relay_job() {
  local kp_worker_file="${1:?worker snapshot required}" kp_job_name="${2:?job name required}"
  local kp_namespace_value="${3:?namespace required}" kp_phase="${4:?evidence phase required}"
  [[ "${kp_phase}" == "published-before-loss" || "${kp_phase}" == "replayed-after-loss" ]]
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence"
  local kp_job_file="${kp_dir}/workflow-outbox-relay-job.json" kp_uid
  kp_plan_create_inventory batch/v1 Job "${kp_namespace_value}" "${kp_job_name}"
  kp_render_outbox_relay_job "${kp_worker_file}" "${kp_job_name}" "${kp_namespace_value}" \
    "${KUBERPLOY_E2E_RUN_ID}" "${KUBERPLOY_E2E_MANAGED_BY_LABEL_VALUE}" "${kp_job_file}"
  jq -e '.spec.template.spec.containers[0].image | test("@sha256:[a-f0-9]{64}$")' "${kp_job_file}" >/dev/null
  "${KUBERPLOY_E2E_KUBECTL}" create -f "${kp_job_file}" >/dev/null
  kp_uid="$("${KUBERPLOY_E2E_KUBECTL}" get job "${kp_job_name}" --namespace "${kp_namespace_value}" -o json | jq -er '.metadata.uid')"
  kp_finalize_create_inventory Job "${kp_namespace_value}" "${kp_job_name}" "${kp_uid}"
  "${KUBERPLOY_E2E_KUBECTL}" wait --for=condition=Complete "job/${kp_job_name}" \
    --namespace "${kp_namespace_value}" --timeout=5m >/dev/null
  "${KUBERPLOY_E2E_KUBECTL}" logs --namespace "${kp_namespace_value}" "job/${kp_job_name}" \
    >"${kp_dir}/workflow-outbox-${kp_phase}.json"
  jq -e 'keys==["published","replayed"] and
    all(.published,.replayed;type=="number" and floor==. and .>=0 and .<=100)' \
    "${kp_dir}/workflow-outbox-${kp_phase}.json" >/dev/null
  if [[ "${kp_phase}" == "published-before-loss" ]]; then
    jq -e '.published==1 and .replayed==0' "${kp_dir}/workflow-outbox-${kp_phase}.json" >/dev/null
  else
    jq -e '.published==1 and .replayed==1' "${kp_dir}/workflow-outbox-${kp_phase}.json" >/dev/null
  fi
}

kp_delete_owned_valkey_dataset() {
  local kp_namespace_value="${1:?namespace required}" kp_name="${2:?deployment required}"
  local kp_claim="${3:?PVC required}" kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence"
  local kp_before="${kp_dir}/valkey-before-dataset-loss.json" kp_after="${kp_dir}/valkey-after-dataset-loss.json"
  local kp_helper="qualification-valkey-wipe-${KUBERPLOY_E2E_RUN_ID}" kp_helper_file="${kp_dir}/workflow-valkey-wipe-pod.json"
  local kp_uid
  "${KUBERPLOY_E2E_KUBECTL}" get deployment "${kp_name}" --namespace "${kp_namespace_value}" -o json >"${kp_before}"
  jq -e --arg name "${kp_name}" --arg claim "${kp_claim}" '
    .metadata.name==$name and .metadata.labels["app.kubernetes.io/name"]=="valkey" and
    .metadata.labels["app.kubernetes.io/instance"]=="valkey" and
    .metadata.labels["app.kubernetes.io/managed-by"]=="Helm" and .spec.replicas==1 and
    any(.spec.template.spec.volumes[];.name=="valkey-data" and .persistentVolumeClaim.claimName==$claim) and
    ([.spec.template.spec.containers[]|select(.name=="valkey")]|length)==1 and
    ([.spec.template.spec.containers[]|select(.name=="valkey")][0].image|test("@sha256:[a-f0-9]{64}$"))
  ' "${kp_before}" >/dev/null || kp_die "Valkey dataset target is not the exact managed Deployment and PVC"
  "${KUBERPLOY_E2E_KUBECTL}" scale "deployment/${kp_name}" --namespace "${kp_namespace_value}" --replicas=0 >/dev/null
  for _ in {1..120}; do
    [[ "$("${KUBERPLOY_E2E_KUBECTL}" get pods --namespace "${kp_namespace_value}" \
      --selector 'app.kubernetes.io/name=valkey,app.kubernetes.io/instance=valkey' -o json | jq '.items|length')" == "0" ]] && break
    sleep 2
  done
  [[ "$("${KUBERPLOY_E2E_KUBECTL}" get pods --namespace "${kp_namespace_value}" \
    --selector 'app.kubernetes.io/name=valkey,app.kubernetes.io/instance=valkey' -o json | jq '.items|length')" == "0" ]]
  kp_plan_create_inventory v1 Pod "${kp_namespace_value}" "${kp_helper}"
  jq --arg name "${kp_helper}" --arg ns "${kp_namespace_value}" --arg claim "${kp_claim}" \
    --arg run "${KUBERPLOY_E2E_RUN_ID}" --arg managed "${KUBERPLOY_E2E_MANAGED_BY_LABEL_VALUE}" '
    [.spec.template.spec.containers[]|select(.name=="valkey")][0] as $valkey |
    {apiVersion:"v1",kind:"Pod",metadata:{name:$name,namespace:$ns,labels:{
      "kuberploy.io/test-run":$run,"app.kubernetes.io/managed-by":$managed,
      "app.kubernetes.io/name":"valkey","app.kubernetes.io/instance":"valkey"}},spec:{
      automountServiceAccountToken:false,restartPolicy:"Never",securityContext:(.spec.template.spec.securityContext + {runAsNonRoot:true}),
      containers:[{name:"wipe",image:$valkey.image,imagePullPolicy:$valkey.imagePullPolicy,
        command:["/bin/sh","-ec"],args:["find /data -mindepth 1 -depth -delete"],
        securityContext:{allowPrivilegeEscalation:false,capabilities:{drop:["ALL"]},readOnlyRootFilesystem:true,runAsNonRoot:true},
        volumeMounts:[{name:"valkey-data",mountPath:"/data"}]}],volumes:[{name:"valkey-data",
          persistentVolumeClaim:{claimName:$claim}}]}}
  ' "${kp_before}" >"${kp_helper_file}"
  "${KUBERPLOY_E2E_KUBECTL}" create -f "${kp_helper_file}" >/dev/null
  kp_uid="$("${KUBERPLOY_E2E_KUBECTL}" get pod "${kp_helper}" --namespace "${kp_namespace_value}" -o json | jq -er '.metadata.uid')"
  kp_finalize_create_inventory Pod "${kp_namespace_value}" "${kp_helper}" "${kp_uid}"
  "${KUBERPLOY_E2E_KUBECTL}" wait --for=jsonpath='{.status.phase}'=Succeeded "pod/${kp_helper}" \
    --namespace "${kp_namespace_value}" --timeout=5m >/dev/null
  "${KUBERPLOY_E2E_KUBECTL}" scale "deployment/${kp_name}" --namespace "${kp_namespace_value}" --replicas=1 >/dev/null
  "${KUBERPLOY_E2E_KUBECTL}" rollout status "deployment/${kp_name}" --namespace "${kp_namespace_value}" --timeout=10m >/dev/null
  "${KUBERPLOY_E2E_KUBECTL}" get deployment "${kp_name}" --namespace "${kp_namespace_value}" -o json >"${kp_after}"
  jq -e --arg uid "$(jq -r '.metadata.uid' "${kp_before}")" '
    .metadata.uid==$uid and .spec.replicas==1 and .status.availableReplicas==1 and
    .status.observedGeneration==.metadata.generation' "${kp_after}" >/dev/null
}

kp_provision_qualification_access() {
  local kp_project="${1:?project required}" kp_environment="${2:?environment required}"
  local kp_application="${3:?application required}" kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence"
  local kp_developer kp_service_account kp_token kp_token_id kp_revoke_token kp_revoke_id kp_expires kp_actual
  local kp_denied_project kp_denied_service_account kp_denied_token
  local kp_secret_dir="$(dirname "${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")"
  kp_developer="$(jq -er '.developerUserId' "${KUBERPLOY_E2E_ARTIFACT_DIR}/10-one-chart-install/evidence/installer-proof.json")"
  kp_human_post grant-project "/v1/projects/${kp_project}/grants" \
    "$(jq -cn --arg u "${kp_developer}" --arg s "${kp_project}" '{subjectUserId:$u,role:"developer",scopeType:"project",scopeId:$s}')" 201 "${kp_dir}/access-project-grant.json"
  kp_human_post grant-environment "/v1/projects/${kp_project}/grants" \
    "$(jq -cn --arg u "${kp_developer}" --arg s "${kp_environment}" '{subjectUserId:$u,role:"developer",scopeType:"environment",scopeId:$s}')" 201 "${kp_dir}/access-environment-grant.json"
  kp_human_post grant-application "/v1/projects/${kp_project}/grants" \
    "$(jq -cn --arg u "${kp_developer}" --arg s "${kp_application}" '{subjectUserId:$u,role:"developer",scopeType:"application",scopeId:$s,permissions:["logs.read"]}')" 201 "${kp_dir}/access-application-grant.json"
  kp_human_post create-service-account "/v1/projects/${kp_project}/service-accounts" \
    '{"name":"qualification-workflow","role":"developer"}' 201 "${kp_dir}/access-service-account.json"
  kp_service_account="$(jq -er '.id' "${kp_dir}/access-service-account.json")"
  kp_expires="$(jq -nr 'now+86400|todateiso8601')"
  kp_human_post issue-workflow-token "/v1/service-accounts/${kp_service_account}/tokens" \
    "$(jq -cn --arg e "${kp_expires}" '{name:"qualification-workflow",scopes:["app.read","app.edit","logs.read"],expiresAt:$e}')" 201 "${kp_secret_dir}/workflow-token.json"
  kp_token="$(jq -er '.token | select(test("^kp_sa_[A-Za-z0-9_-]{43}$"))' "${kp_secret_dir}/workflow-token.json")"
  printf 'Authorization: Bearer %s\n' "${kp_token}" >"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}"
  kp_human_post create-denied-project /v1/projects \
    "$(jq -cn --arg run "${KUBERPLOY_E2E_RUN_ID}" '{name:("Qualification denied "+$run),slug:("qualification-denied-"+$run)}')" \
    201 "${kp_dir}/access-denied-project.json"
  kp_denied_project="$(jq -er '.id' "${kp_dir}/access-denied-project.json")"
  kp_human_post create-denied-service-account "/v1/projects/${kp_denied_project}/service-accounts" \
    '{"name":"qualification-cross-tenant","role":"developer"}' 201 \
    "${kp_dir}/access-denied-service-account.json"
  kp_denied_service_account="$(jq -er '.id' "${kp_dir}/access-denied-service-account.json")"
  kp_human_post issue-denied-token "/v1/service-accounts/${kp_denied_service_account}/tokens" \
    "$(jq -cn --arg e "${kp_expires}" '{name:"qualification-cross-tenant",scopes:["app.read","logs.read"],expiresAt:$e}')" \
    201 "${kp_secret_dir}/denied-token.json"
  kp_denied_token="$(jq -er '.token | select(test("^kp_sa_[A-Za-z0-9_-]{43}$"))' "${kp_secret_dir}/denied-token.json")"
  printf 'Authorization: Bearer %s\n' "${kp_denied_token}" >"${KUBERPLOY_E2E_DENIED_AUTH_HEADER_FILE}"
  chmod 600 "${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}" "${KUBERPLOY_E2E_DENIED_AUTH_HEADER_FILE}"
  [[ "$(curl -sS -o "${kp_dir}/access-service-account-use.json" -w '%{http_code}' \
    --header "$(<"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")" "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/projects/${kp_project}")" == 200 ]]
  kp_human_post issue-revocation-token "/v1/service-accounts/${kp_service_account}/tokens" \
    "$(jq -cn --arg e "${kp_expires}" '{name:"qualification-revocation",scopes:["app.read"],expiresAt:$e}')" 201 "${kp_secret_dir}/revocation-token.json"
  kp_revoke_token="$(jq -er '.token' "${kp_secret_dir}/revocation-token.json")"
  kp_revoke_id="$(jq -er '.tokenRecord.id' "${kp_secret_dir}/revocation-token.json")"
  kp_actual="$(curl -sS -o "${kp_dir}/access-token-revoked.json" -w '%{http_code}' -X DELETE \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" \
    --header "Idempotency-Key: qualification-${KUBERPLOY_E2E_RUN_ID}-20-postgresql-valkey-revoke-token" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/service-accounts/${kp_service_account}/tokens/${kp_revoke_id}")"
  [[ "${kp_actual}" == 204 ]]
  [[ "$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer ${kp_revoke_token}" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/projects/${kp_project}")" == 401 ]]
  rm -f -- "${kp_secret_dir}/workflow-token.json" "${kp_secret_dir}/revocation-token.json" \
    "${kp_secret_dir}/denied-token.json"
  jq -n --arg serviceAccountId "${kp_service_account}" \
    '{multiScopeGrantCount:3,serviceAccountId:$serviceAccountId,
      expiringTokenIssued:true,serviceAccountAuthenticated:true,revokedTokenDenied:true}' \
    >"${kp_dir}/access-proof.json"
}

kp_run_durability_workflow() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_body kp_project_id kp_actual
  local kp_environment_id kp_environment_namespace kp_application_id kp_operation_id kp_deployment_id kp_commit
  local kp_worker_ns kp_worker_name kp_valkey_ns kp_valkey_name kp_valkey_claim kp_relay_job
  kp_body="$(jq -c '.workflow.project' "${kp_scenario}")"
  kp_human_post create-project /v1/projects "${kp_body}" 201 "${kp_dir}/workflow-project.json"
  kp_project_id="$(jq -er '.id | select(test("^[a-f0-9-]{36}$"))' "${kp_dir}/workflow-project.json")"
  kp_body="$(jq -c --arg id "${kp_project_id}" '.workflow.directEnvironment + {projectId:$id,protectionPolicy:"development"}' "${kp_scenario}")"
  kp_human_post create-direct-environment /v1/environments "${kp_body}" 201 "${kp_dir}/workflow-direct-environment.json"
  kp_environment_id="$(jq -er '.id' "${kp_dir}/workflow-direct-environment.json")"
  kp_environment_namespace="$(jq -er '.namespace | select(test("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"))' \
    "${kp_dir}/workflow-direct-environment.json")"
  kp_body="$(jq -c --arg id "${kp_project_id}" '.workflow.application + {projectId:$id}' "${kp_scenario}")"
  kp_human_post create-application /v1/applications "${kp_body}" 201 "${kp_dir}/workflow-application.json"
  kp_application_id="$(jq -er '.id' "${kp_dir}/workflow-application.json")"
  kp_provision_qualification_access "${kp_project_id}" "${kp_environment_id}" "${kp_application_id}"
  if [[ "${KUBERPLOY_E2E_HERMETIC_TEST:-false}" != "true" ]]; then
    kp_worker_ns="$(jq -er '.workflow.recovery.worker.namespace' "${kp_scenario}")"
    kp_worker_name="$(jq -er '.workflow.recovery.worker.controllerName' "${kp_scenario}")"
    "${KUBERPLOY_E2E_KUBECTL}" get deployment "${kp_worker_name}" --namespace "${kp_worker_ns}" -o json \
      >"${kp_dir}/worker-before-dataset-loss.json"
    jq -e --arg name "${kp_worker_name}" '
      .metadata.name==$name and .metadata.labels["app.kubernetes.io/name"]=="kuberploy" and
      .metadata.labels["app.kubernetes.io/component"]=="worker" and .spec.replicas==1 and
      ([.spec.template.spec.containers[]|select(.name=="worker")]|length)==1 and
      ([.spec.template.spec.containers[]|select(.name=="worker")][0].image|test("@sha256:[a-f0-9]{64}$"))
    ' "${kp_dir}/worker-before-dataset-loss.json" >/dev/null || kp_die "worker recovery target is not exact"
    kp_scale_owned_deployment "${kp_worker_ns}" "${kp_worker_name}" 0 || kp_die "worker did not stop"
  fi
  kp_body="$(jq -c --arg environment "${kp_environment_id}" --arg application "${kp_application_id}" '.workflow.directDeployment + {environmentId:$environment,applicationId:$application}' "${kp_scenario}")"
  kp_human_post durable-direct-operation /v1/deployments "${kp_body}" 202 \
    "${kp_dir}/workflow-durable-operation.json"
  kp_operation_id="$(jq -er '.id' "${kp_dir}/workflow-durable-operation.json")"
  kp_deployment_id="$(jq -er '.targetId' "${kp_dir}/workflow-durable-operation.json")"
  kp_restart_owned_pod postgresql
  if [[ "${KUBERPLOY_E2E_HERMETIC_TEST:-false}" == "true" ]]; then
    jq -n '{published:1,replayed:0}' >"${kp_dir}/workflow-outbox-published-before-loss.json"
    jq -n '{published:1,replayed:1}' >"${kp_dir}/workflow-outbox-replayed-after-loss.json"
    jq -n '{datasetDeleted:true,valkeyDeploymentRestored:true,workerDeploymentRestored:true}' \
      >"${kp_dir}/workflow-valkey-dataset-recovery.json"
  else
    kp_relay_job="qualification-outbox-relay-${KUBERPLOY_E2E_RUN_ID}"
    kp_create_outbox_relay_job "${kp_dir}/worker-before-dataset-loss.json" "${kp_relay_job}" "${kp_worker_ns}" published-before-loss
    kp_valkey_ns="$(jq -er '.workflow.recovery.valkey.namespace' "${kp_scenario}")"
    kp_valkey_name="$(jq -er '.workflow.recovery.valkey.controllerName' "${kp_scenario}")"
    kp_valkey_claim="$(jq -er '.workflow.recovery.valkey.persistentVolumeClaimName' "${kp_scenario}")"
    if ! kp_delete_owned_valkey_dataset "${kp_valkey_ns}" "${kp_valkey_name}" "${kp_valkey_claim}"; then
      "${KUBERPLOY_E2E_KUBECTL}" scale "deployment/${kp_valkey_name}" --namespace "${kp_valkey_ns}" --replicas=1 >/dev/null || true
      kp_scale_owned_deployment "${kp_worker_ns}" "${kp_worker_name}" 1 || true
      kp_die "managed Valkey dataset loss workflow failed"
    fi
    kp_relay_job="qualification-outbox-replay-${KUBERPLOY_E2E_RUN_ID}"
    kp_create_outbox_relay_job "${kp_dir}/worker-before-dataset-loss.json" "${kp_relay_job}" "${kp_worker_ns}" replayed-after-loss
    kp_scale_owned_deployment "${kp_worker_ns}" "${kp_worker_name}" 1 || kp_die "worker did not recover"
    "${KUBERPLOY_E2E_KUBECTL}" get deployment "${kp_worker_name}" --namespace "${kp_worker_ns}" -o json \
      >"${kp_dir}/worker-after-dataset-loss.json"
    jq -e --arg uid "$(jq -r '.metadata.uid' "${kp_dir}/worker-before-dataset-loss.json")" '
      .metadata.uid==$uid and .spec.replicas==1 and .status.availableReplicas==1 and
      .status.observedGeneration==.metadata.generation' "${kp_dir}/worker-after-dataset-loss.json" >/dev/null
    jq -n '{datasetDeleted:true,valkeyDeploymentRestored:true,workerDeploymentRestored:true}' \
      >"${kp_dir}/workflow-valkey-dataset-recovery.json"
  fi
  kp_poll_operation "${kp_operation_id}" "${kp_dir}/workflow-durable-operation-terminal.json"
  jq -e '.status == "succeeded" and .generation == 1 and
    (.gitRevision.commit | type == "string" and length == 40)' \
    "${kp_dir}/workflow-durable-operation-terminal.json" >/dev/null
  kp_commit="$(jq -r '.gitRevision.commit' "${kp_dir}/workflow-durable-operation-terminal.json")"
  kp_poll_operation "${kp_operation_id}" "${kp_dir}/workflow-durable-operation-replay-read.json"
  jq -e --arg commit "${kp_commit}" '
    .status=="succeeded" and .generation==1 and .gitRevision.commit==$commit' \
    "${kp_dir}/workflow-durable-operation-replay-read.json" >/dev/null
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-project-after-restart.json" \
    --write-out '%{http_code}' --request GET \
    --header "$(<"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/projects/${kp_project_id}")"
  [[ "${kp_actual}" == "200" ]]
  jq -e --arg id "${kp_project_id}" '.id == $id' "${kp_dir}/workflow-project-after-restart.json" >/dev/null
  jq -n --arg project "${kp_project_id}" --arg environment "${kp_environment_id}" \
    --arg environmentNamespace "${kp_environment_namespace}" \
    --arg application "${kp_application_id}" --arg operation "${kp_operation_id}" \
    --arg deployment "${kp_deployment_id}" --arg commit "${kp_commit}" '
    {projectId:$project,directEnvironmentId:$environment,directEnvironmentNamespace:$environmentNamespace,
     applicationId:$application,
     directOperationId:$operation,directDeploymentId:$deployment,directCommit:$commit}
  ' \
    >"${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"
  chmod 600 "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"
}

kp_poll_build() {
  local kp_build_id="${1:?build ID required}" kp_out="${2:?output required}"
  [[ "${kp_build_id}" =~ ^[a-f0-9-]{36}$ ]]
  local kp_attempt kp_state kp_actual
  for kp_attempt in {1..120}; do
    kp_actual="$(curl --silent --show-error --output "${kp_out}" --write-out '%{http_code}' \
      --request GET --header "$(<"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")" \
      "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/builds/${kp_build_id}")"
    [[ "${kp_actual}" == "200" ]]
    kp_state="$(jq -er '.state | select(. == "queued" or . == "preparing" or . == "running" or . == "cancelling" or . == "succeeded" or . == "failed" or . == "cancelled")' "${kp_out}")"
    case "${kp_state}" in
      succeeded) return 0 ;;
      failed|cancelled) return 1 ;;
    esac
    sleep 1
  done
  return 1
}

kp_run_source_build_workflow() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_state_file
  local kp_build_id kp_body kp_operation_id
  kp_state_file="${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"
  [[ -f "${kp_state_file}" && ! -L "${kp_state_file}" ]]
  kp_run_github_build_workflow
  kp_build_id="$(jq -er '.successfulBuildId' "${kp_state_file}")"
  kp_body="$(jq -c --arg environment "$(jq -r '.protectedEnvironmentId' "${kp_state_file}")" \
    '.workflow.sourceBuild.promotion + {environmentId:$environment}' "${kp_scenario}")"
  kp_human_post promote-source-build "/v1/builds/${kp_build_id}/promote" "${kp_body}" 202 \
    "${kp_dir}/workflow-build-promotion.json"
  kp_operation_id="$(jq -er '.id | select(test("^[a-f0-9-]{36}$"))' "${kp_dir}/workflow-build-promotion.json")"
  kp_poll_operation "${kp_operation_id}" "${kp_dir}/workflow-build-promotion-terminal.json"
  jq -e '.status == "succeeded" and (.pullRequest.url | type == "string" and startswith("https://"))' \
    "${kp_dir}/workflow-build-promotion-terminal.json" >/dev/null
  jq --arg build "${kp_build_id}" --arg operation "${kp_operation_id}" \
    '. + {successfulBuildId:$build,buildPromotionOperationId:$operation}' "${kp_state_file}" \
    >"${kp_state_file}.tmp"
  chmod 600 "${kp_state_file}.tmp"
  mv -- "${kp_state_file}.tmp" "${kp_state_file}"
}

kp_run_helm_workflow() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_state_file kp_application kp_environment
  local kp_path kp_body kp_actual kp_release_id kp_application_revision kp_argo_name
  kp_state_file="${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"
  kp_application="$(jq -er '.applicationId' "${kp_state_file}")"
  kp_environment="$(jq -er '.protectedEnvironmentId' "${kp_state_file}")"
  kp_path="/v1/applications/${kp_application}/environments/${kp_environment}/helm"
  kp_body="$(jq -c '.workflow.helm' "${kp_scenario}")"
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-helm-values-preview.json" \
    --write-out '%{http_code}' --request POST \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" \
    --header 'Content-Type: application/json' --data-binary "${kp_body}" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")${kp_path}/values-preview")"
  [[ "${kp_actual}" == "200" ]]
  jq -e --argjson approval "$(jq '.approvalRevision' <<<"${kp_body}")" \
    --arg id "$(jq -r '.approvalId' <<<"${kp_body}")" '
    .approval.id == $id and .approval.revision == $approval and
    (.normalizedValuesYaml | type == "string" and length > 0) and
    (.valuesDigest | test("^sha256:[a-f0-9]{64}$")) and
    (.changedPaths | type == "array")
  ' "${kp_dir}/workflow-helm-values-preview.json" >/dev/null
  kp_human_put helm-release "${kp_path}/release" "${kp_body}" 202 \
    "${kp_dir}/workflow-helm-release.json"
  kp_release_id="$(jq -er '.id | select(test("^[a-f0-9-]{36}$"))' "${kp_dir}/workflow-helm-release.json")"
  for _ in {1..120}; do
    kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-helm-status.json" \
      --write-out '%{http_code}' --request GET --header "$(<"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")" \
      "$(jq -r '.apiBaseURL' "${kp_scenario}")${kp_path}/release")"
    [[ "${kp_actual}" == "200" ]]
    if jq -e --arg release "${kp_release_id}" '
      .revision.id == $release and .phase == "published" and .renderState == "succeeded" and
      .payloadState == "verified" and .applicationState == "verified" and
      (.payloadRevision | test("^[a-f0-9]{40}$")) and
      (.applicationRevision | test("^[a-f0-9]{40}$"))
    ' "${kp_dir}/workflow-helm-status.json" >/dev/null; then break; fi
    sleep 5
  done
  kp_application_revision="$(jq -er '.applicationRevision | select(test("^[a-f0-9]{40}$"))' "${kp_dir}/workflow-helm-status.json")"
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-helm-rendered-preview.json" \
    --write-out '%{http_code}' --request GET --header "$(<"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")${kp_path}/rendered-preview")"
  [[ "${kp_actual}" == "200" ]]
  jq -e --arg release "${kp_release_id}" '
    .releaseRevisionId == $release and (.manifestDigest | test("^sha256:[a-f0-9]{64}$")) and
    (.inventoryDigest | test("^sha256:[a-f0-9]{64}$")) and .resourceCount == (.resources|length) and
    .resourceCount > 0 and all(.resources[];
      (has("sanitizedYaml") | not) or
      (.sanitizedYaml | test("(^|\\n)(data|stringData):[[:space:]]") | not))
  ' "${kp_dir}/workflow-helm-rendered-preview.json" >/dev/null
  kp_argo_name="kp-h-${kp_application//-/}"
  for _ in {1..120}; do
    if "${KUBERPLOY_E2E_KUBECTL}" get application "${kp_argo_name}" --namespace argocd -o json \
        >"${kp_dir}/workflow-helm-argo-application.json" 2>/dev/null &&
      jq -e --arg application "${kp_application}" --arg environment "${kp_environment}" \
        --arg release "${kp_release_id}" --arg revision "${kp_application_revision}" '
        .metadata.labels["app.kubernetes.io/component"] == "approved-helm-application" and
        .metadata.labels["kuberploy.io/application-id"] == $application and
        .metadata.labels["kuberploy.io/environment-id"] == $environment and
        .metadata.annotations["kuberploy.io/helm-release-revision"] == $release and
        .spec.source.targetRevision == $revision and .status.sync.status == "Synced" and
        .status.health.status == "Healthy" and
        any(.status.resources[]; .status == "Synced" and .health.status == "Healthy")
      ' "${kp_dir}/workflow-helm-argo-application.json" >/dev/null; then break; fi
    sleep 5
  done
  jq -e '.status.sync.status == "Synced" and .status.health.status == "Healthy"' \
    "${kp_dir}/workflow-helm-argo-application.json" >/dev/null || kp_die "approved Helm Argo Application did not converge"
  jq --arg release "${kp_release_id}" --arg revision "${kp_application_revision}" \
    '. + {helmReleaseId:$release,helmApplicationRevision:$revision}' "${kp_state_file}" >"${kp_state_file}.tmp"
  chmod 600 "${kp_state_file}.tmp"; mv -- "${kp_state_file}.tmp" "${kp_state_file}"
}

kp_run_registry_cleanup_workflow() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_state_file kp_application_id
  local kp_body kp_plan_id
  kp_state_file="${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"
  [[ -f "${kp_state_file}" && ! -L "${kp_state_file}" ]]
  kp_application_id="$(jq -er '.applicationId' "${kp_state_file}")"
  kp_body="$(jq -c '{targetId:.workflow.registryCleanup.targetId}' "${kp_scenario}")"
  kp_human_post registry-cleanup-preview \
    "/v1/applications/${kp_application_id}/registry/cleanup-previews" "${kp_body}" 201 \
    "${kp_dir}/workflow-registry-preview.json"
  kp_plan_id="$(jq -er '.id | select(test("^[a-f0-9-]{36}$"))' "${kp_dir}/workflow-registry-preview.json")"
  jq -e '.state == "preview" and
    any(.items[]; .disposition == "protect" and .action == "none") and
    any(.items[]; .disposition == "delete" and (.action == "delete-manifest" or .action == "garbage-collect-blob"))' \
    "${kp_dir}/workflow-registry-preview.json" >/dev/null
  kp_body="$(jq -cn --arg id "${kp_plan_id}" '{confirmation:$id}')"
  kp_human_post registry-cleanup-execute "/v1/registry-cleanup-plans/${kp_plan_id}/executions" \
    "${kp_body}" 200 "${kp_dir}/workflow-registry-execution.json"
  jq -e --arg id "${kp_plan_id}" '.id == $id and .state == "succeeded" and
    .summary.cacheQuotaSatisfied == true and
    all(.items[]; .state == "protected" or .state == "deleted" or .state == "skipped") and
    any(.items[]; .state == "protected") and any(.items[]; .state == "deleted")' \
    "${kp_dir}/workflow-registry-execution.json" >/dev/null
}

kp_run_upgrade_workflow() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence"
  local kp_actual kp_source_version kp_target_version kp_source_revision kp_target_revision kp_rollback_revision
  kp_source_version="$(jq -er '.workflow.upgrade.sourceVersion' "${kp_scenario}")"
  kp_target_version="$(jq -er '.workflow.upgrade.targetVersion' "${kp_scenario}")"
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-pre-upgrade-meta.json" \
    --write-out '%{http_code}' --request GET \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/meta")"
  [[ "${kp_actual}" == "200" ]]
  jq -e --arg source "${kp_source_version}" '.version == $source' \
    "${kp_dir}/workflow-pre-upgrade-meta.json" >/dev/null
  "${KUBERPLOY_E2E_HELM}" history kuberploy-qualification --namespace kuberploy-system -o json \
    >"${kp_dir}/workflow-source-helm-history.json"
  kp_source_revision="$(jq -er 'last.revision | select(type == "number" and . >= 1)' "${kp_dir}/workflow-source-helm-history.json")"
  "${KUBERPLOY_E2E_HELM}" upgrade kuberploy-qualification \
    "$(kp_repo_root)/charts/kuberploy-installer" --namespace kuberploy-system \
    --values "${KUBERPLOY_E2E_INSTALLER_VALUES_FILE}" --server-side=false \
    --wait --wait-for-jobs --timeout 65m
  "${KUBERPLOY_E2E_HELM}" history kuberploy-qualification --namespace kuberploy-system -o json \
    >"${kp_dir}/workflow-target-helm-history.json"
  kp_target_revision="$(jq -er 'last.revision' "${kp_dir}/workflow-target-helm-history.json")"
  [[ "${kp_target_revision}" -eq $((kp_source_revision + 1)) ]]
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-post-upgrade-readyz.json" \
    --write-out '%{http_code}' --request GET "$(jq -r '.apiBaseURL' "${kp_scenario}")/readyz")"
  [[ "${kp_actual}" == "200" ]]
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-post-upgrade-meta.json" \
    --write-out '%{http_code}' --request GET "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/meta")"
  [[ "${kp_actual}" == "200" ]]
  jq -e --arg target "${kp_target_version}" '.version == $target' \
    "${kp_dir}/workflow-post-upgrade-meta.json" >/dev/null
  "${KUBERPLOY_E2E_HELM}" template kuberploy-qualification \
    "$(kp_repo_root)/charts/kuberploy-installer" --namespace kuberploy-system \
    --values "${KUBERPLOY_E2E_INSTALLER_VALUES_FILE}" \
    >"${kp_dir}/workflow-target-installer-manifest.yaml"
  [[ -s "${kp_dir}/workflow-target-installer-manifest.yaml" ]]
  openssl dgst -sha256 "${kp_dir}/workflow-target-installer-manifest.yaml" \
    >"${kp_dir}/workflow-target-installer-manifest.sha256"
  "${KUBERPLOY_E2E_HELM}" rollback kuberploy-qualification "${kp_source_revision}" \
    --namespace kuberploy-system --wait --wait-for-jobs --timeout 65m
  "${KUBERPLOY_E2E_HELM}" history kuberploy-qualification --namespace kuberploy-system -o json \
    >"${kp_dir}/workflow-rollback-helm-history.json"
  kp_rollback_revision="$(jq -er 'last.revision' "${kp_dir}/workflow-rollback-helm-history.json")"
  [[ "${kp_rollback_revision}" -eq $((kp_target_revision + 1)) ]]
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-post-rollback-meta.json" \
    --write-out '%{http_code}' --request GET "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/meta")"
  [[ "${kp_actual}" == "200" ]]
  jq -e --arg source "${kp_source_version}" '.version == $source' \
    "${kp_dir}/workflow-post-rollback-meta.json" >/dev/null
  jq -n --arg source "${kp_source_version}" --arg target "${kp_target_version}" \
    --argjson sourceRevision "${kp_source_revision}" --argjson targetRevision "${kp_target_revision}" \
    --argjson rollbackRevision "${kp_rollback_revision}" \
    '{sourceVersion:$source,targetVersion:$target,sourceRevision:$sourceRevision,
      targetRevision:$targetRevision,rollbackRevision:$rollbackRevision}' \
    >"${kp_dir}/workflow-installer-lifecycle.json"
}

kp_run_tls_workflow() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_state_file kp_request kp_actual
  local kp_application_id kp_environment_id kp_environment_namespace kp_deployment_id kp_binding_id
  local kp_binding_name kp_binding_version kp_issuer_name kp_etag kp_body kp_preview_token kp_operation_id kp_commit
  local kp_binding_fingerprint kp_served_fingerprint kp_secret_fingerprint kp_local_served_fingerprint
  local kp_certificate_name kp_certificate_uid kp_certificate_secret kp_revision
  local kp_previous_generation kp_generation
  kp_state_file="${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"
  kp_application_id="$(jq -er '.applicationId' "${kp_state_file}")"
  kp_environment_id="$(jq -er '.directEnvironmentId' "${kp_state_file}")"
  kp_environment_namespace="$(jq -er '.directEnvironmentNamespace' "${kp_state_file}")"
  kp_deployment_id="$(jq -er '.directDeploymentId' "${kp_state_file}")"
  kp_previous_generation="$(jq -er '.directGeneration | select(type == "number" and . >= 1)' "${kp_state_file}")"
  kp_request="${KUBERPLOY_E2E_STAGE_DIR}/custom-certificate-request.json"
  jq -n --arg environment "${kp_environment_id}" \
    --arg name "$(jq -r '.workflow.tls.customCertificateName' "${kp_scenario}")" \
    --rawfile certificate "${KUBERPLOY_E2E_CUSTOM_CERTIFICATE_PEM_FILE}" \
    --rawfile key "${KUBERPLOY_E2E_CUSTOM_PRIVATE_KEY_PEM_FILE}" \
    '{environmentId:$environment,name:$name,certificatePem:$certificate,privateKeyPem:$key}' \
    >"${kp_request}"
  chmod 600 "${kp_request}"
  kp_human_post_file create-custom-certificate \
    "/v1/applications/${kp_application_id}/certificate-bindings" "${kp_request}" 201 \
    "${kp_dir}/workflow-custom-certificate.json"
  rm -- "${kp_request}"
  kp_binding_id="$(jq -er '.id | select(test("^[a-f0-9-]{36}$"))' "${kp_dir}/workflow-custom-certificate.json")"
  for _ in {1..120}; do
    kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-custom-certificate-ready.json" \
      --write-out '%{http_code}' --request GET \
      --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
      "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/certificate-bindings/${kp_binding_id}")"
    [[ "${kp_actual}" == "200" ]]
    [[ "$(jq -r '.state' "${kp_dir}/workflow-custom-certificate-ready.json")" == "ready" ]] && break
    sleep 1
  done
  jq -e --arg id "${kp_binding_id}" '.id == $id and .state == "ready" and
    (.activeVersion | type == "number" and . >= 1) and
    (.name | type == "string" and length > 0) and
    (.versions | length > 0) and all(.versions[]; has("certificatePem")|not) and
    all(.versions[]; has("privateKeyPem")|not)' "${kp_dir}/workflow-custom-certificate-ready.json" >/dev/null
  kp_binding_name="$(jq -er '.name' "${kp_dir}/workflow-custom-certificate-ready.json")"
  kp_binding_version="$(jq -er '.activeVersion' "${kp_dir}/workflow-custom-certificate-ready.json")"
  kp_binding_fingerprint="$(jq -er --argjson version "${kp_binding_version}" \
    --arg host "${KUBERPLOY_E2E_CUSTOM_TLS_HOSTNAME}" '
      [.versions[] | select(.number == $version and (.dnsNames | index($host) != null))] |
      select(length == 1) | .[0].leafFingerprint |
      select(test("^sha256:[a-f0-9]{64}$"))
    ' "${kp_dir}/workflow-custom-certificate-ready.json")"
  kp_issuer_name="$(jq -er '.workflow.tls.localACMEIssuerName' "${kp_scenario}")"
  "${KUBERPLOY_E2E_KUBECTL}" get clusterissuer "${kp_issuer_name}" -o json \
    >"${kp_dir}/workflow-local-acme-issuer.json"
  jq -e --arg name "${kp_issuer_name}" --arg directory "${KUBERPLOY_E2E_LOCAL_ACME_DIRECTORY_URL}" '
    .metadata.name == $name and .spec.acme.server == $directory and
    any(.status.conditions[]; .type == "Ready" and .status == "True")
  ' "${kp_dir}/workflow-local-acme-issuer.json" >/dev/null
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-local-acme-catalog.json" \
    --write-out '%{http_code}' --request GET \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/applications/${kp_application_id}/certificate-issuers?environmentId=${kp_environment_id}&hostname=${KUBERPLOY_E2E_LOCAL_ACME_HOSTNAME}")"
  [[ "${kp_actual}" == "200" ]]
  jq -e --arg name "${kp_issuer_name}" '
    any(.items[]; .name == $name and
      (.source == "bootstrap" or (.source == "managed" and .revision >= 1)) and
      (.solverTypes | type == "array" and index("http01") != null))
  ' "${kp_dir}/workflow-local-acme-catalog.json" >/dev/null

  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-tls-config-before.json" \
    --write-out '%{http_code}' --request GET \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment_id}/config")"
  [[ "${kp_actual}" == "200" ]]
  kp_etag="$(jq -er '.etag | select(type == "string" and length > 0)' \
    "${kp_dir}/workflow-tls-config-before.json")"
  kp_body="$(jq -cn --arg customHost "${KUBERPLOY_E2E_CUSTOM_TLS_HOSTNAME}" \
    --arg acmeHost "${KUBERPLOY_E2E_LOCAL_ACME_HOSTNAME}" --arg issuer "${kp_issuer_name}" \
    --arg binding "${kp_binding_id}" --arg bindingName "${kp_binding_name}" \
    --argjson bindingVersion "${kp_binding_version}" '
    {mode:"jsonPatch",patch:[
      {op:"add",path:"/spec/routes/-",value:{id:"qualification-custom-tls",host:$customHost,
        path:"/",port:"http",ingressClassName:"traefik",dns:{mode:"manual"},
        tls:{mode:"customCertificate",redirectHttp:true,
          secretRef:{bindingId:$binding,name:$bindingName,version:$bindingVersion}},middlewareRefs:[]}},
      {op:"add",path:"/spec/routes/-",value:{id:"qualification-local-acme",host:$acmeHost,
        path:"/",port:"http",ingressClassName:"traefik",dns:{mode:"manual"},
        tls:{mode:"letsencrypt",issuerRef:$issuer,redirectHttp:true},middlewareRefs:[]}}
    ]}
  ')"
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-tls-config-preview.json" \
    --write-out '%{http_code}' --request POST \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" \
    --header "If-Match: ${kp_etag}" --header 'Content-Type: application/json' \
    --data-binary "${kp_body}" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment_id}/config/preview")"
  [[ "${kp_actual}" == "200" ]]
  kp_preview_token="$(jq -er '.previewToken | select(type == "string" and length >= 32)' \
    "${kp_dir}/workflow-tls-config-preview.json")"
  jq -e --arg custom "${KUBERPLOY_E2E_CUSTOM_TLS_HOSTNAME}" \
    --arg local "${KUBERPLOY_E2E_LOCAL_ACME_HOSTNAME}" '
    (.renderIdentityDigest | type == "string" and length > 0) and
    (.semanticChanges | type == "array" and length > 0) and
    ((.gitDiff + "\n" + .renderedDiff) | contains($custom) and contains($local))
  ' "${kp_dir}/workflow-tls-config-preview.json" >/dev/null
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-tls-config-operation.json" \
    --write-out '%{http_code}' --request PUT \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" \
    --header "If-Match: ${kp_etag}" --header "Preview-Token: ${kp_preview_token}" \
    --header "Idempotency-Key: qualification-${KUBERPLOY_E2E_RUN_ID}-${KUBERPLOY_E2E_STAGE_ID}-attach-tls-routes" \
    --header 'Content-Type: application/json' --data-binary "${kp_body}" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment_id}/config")"
  [[ "${kp_actual}" == "202" ]]
  kp_operation_id="$(jq -er '.id | select(test("^[a-f0-9-]{36}$"))' \
    "${kp_dir}/workflow-tls-config-operation.json")"
  kp_poll_operation "${kp_operation_id}" "${kp_dir}/workflow-tls-config-terminal.json"
  kp_commit="$(jq -er '.gitRevision.commit | select(test("^[a-f0-9]{40}$"))' \
    "${kp_dir}/workflow-tls-config-terminal.json")"
  kp_generation="$(jq -er --argjson previous "${kp_previous_generation}" \
    '.generation | select(type == "number" and . == ($previous + 1))' \
    "${kp_dir}/workflow-tls-config-terminal.json")"
  jq -e '.status == "succeeded"' "${kp_dir}/workflow-tls-config-terminal.json" >/dev/null
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-tls-config-converged.json" \
    --write-out '%{http_code}' --request GET \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment_id}/config?atLeastRevision=${kp_commit}&waitSeconds=10")"
  [[ "${kp_actual}" == "200" ]]
  jq -e --arg commit "${kp_commit}" '.freshness == "fresh" and .indexedRevision == $commit' \
    "${kp_dir}/workflow-tls-config-converged.json" >/dev/null
  jq --argjson generation "${kp_generation}" '.directGeneration = $generation' "${kp_state_file}" \
    >"${kp_state_file}.tmp"
  chmod 600 "${kp_state_file}.tmp"
  mv -- "${kp_state_file}.tmp" "${kp_state_file}"
  local kp_tls_host
  for kp_tls_host in "${KUBERPLOY_E2E_CUSTOM_TLS_HOSTNAME}" "${KUBERPLOY_E2E_LOCAL_ACME_HOSTNAME}"; do
    openssl s_client -connect "${kp_tls_host}:443" -servername "${kp_tls_host}" </dev/null \
      2>"${kp_dir}/workflow-${kp_tls_host}.handshake" |
      openssl x509 -out "${kp_dir}/workflow-${kp_tls_host}.pem" >/dev/null
    openssl x509 -in "${kp_dir}/workflow-${kp_tls_host}.pem" \
      -checkhost "${kp_tls_host}" -checkend 300 -noout >/dev/null
  done
  kp_served_fingerprint="$(openssl x509 \
    -in "${kp_dir}/workflow-${KUBERPLOY_E2E_CUSTOM_TLS_HOSTNAME}.pem" \
    -fingerprint -sha256 -noout | awk -F= '{print tolower($2)}' | tr -d ':')"
  [[ "sha256:${kp_served_fingerprint}" == "${kp_binding_fingerprint}" ]]

  for _ in {1..120}; do
    if "${KUBERPLOY_E2E_KUBECTL}" get certificates --namespace "${kp_environment_namespace}" -o json |
      jq -e --arg host "${KUBERPLOY_E2E_LOCAL_ACME_HOSTNAME}" --arg issuer "${kp_issuer_name}" '
        [.items[] | select(.spec.dnsNames == [$host] and .spec.issuerRef.name == $issuer and
          .spec.issuerRef.kind == "ClusterIssuer" and
          any(.metadata.ownerReferences[]; .kind == "Ingress" and .controller == true) and
          any(.status.conditions[]; .type == "Ready" and .status == "True"))] |
        select(length == 1) | .[0]
      ' >"${kp_dir}/workflow-local-acme-before-renewal.json"; then
      break
    fi
    sleep 5
  done
  kp_certificate_name="$(jq -er '.metadata.name' "${kp_dir}/workflow-local-acme-before-renewal.json")"
  kp_certificate_uid="$(jq -er '.metadata.uid' "${kp_dir}/workflow-local-acme-before-renewal.json")"
  kp_certificate_secret="$(jq -er '.spec.secretName' "${kp_dir}/workflow-local-acme-before-renewal.json")"
  "${KUBERPLOY_E2E_KUBECTL}" get secret "${kp_certificate_secret}" \
    --namespace "${kp_environment_namespace}" -o json |
    jq '{apiVersion,kind,metadata:{name:.metadata.name,namespace:.metadata.namespace,
      uid:.metadata.uid,labels:.metadata.labels},type}' \
      >"${kp_dir}/workflow-local-acme-route-secret-metadata.json"
  jq -e --arg name "${kp_certificate_secret}" --arg ns "${kp_environment_namespace}" '
    .metadata.name == $name and .metadata.namespace == $ns and
    (.metadata.uid | type == "string" and length > 0) and
    .metadata.labels["controller.cert-manager.io/fao"] == "true" and
    .type == "kubernetes.io/tls" and (has("data") | not)
  ' "${kp_dir}/workflow-local-acme-route-secret-metadata.json" >/dev/null
  kp_secret_fingerprint="$("${KUBERPLOY_E2E_KUBECTL}" get secret "${kp_certificate_secret}" \
    --namespace "${kp_environment_namespace}" -o json | jq -er '.data["tls.crt"] | @base64d' |
    openssl x509 -fingerprint -sha256 -noout | awk -F= '{print tolower($2)}' | tr -d ':')"
  kp_local_served_fingerprint="$(openssl x509 \
    -in "${kp_dir}/workflow-${KUBERPLOY_E2E_LOCAL_ACME_HOSTNAME}.pem" \
    -fingerprint -sha256 -noout | awk -F= '{print tolower($2)}' | tr -d ':')"
  [[ -n "${kp_secret_fingerprint}" && "${kp_secret_fingerprint}" == "${kp_local_served_fingerprint}" ]]
  kp_revision="$(jq -er '.status.revision | select(type == "number" and . >= 1)' \
    "${kp_dir}/workflow-local-acme-before-renewal.json")"
  "${KUBERPLOY_E2E_KUBECTL}" patch certificate "${kp_certificate_name}" \
    --namespace "${kp_environment_namespace}" --type=merge \
    --patch '{"spec":{"duration":"2159h","renewBefore":"719h"}}' >/dev/null
  for _ in {1..120}; do
    "${KUBERPLOY_E2E_KUBECTL}" get certificate "${kp_certificate_name}" \
      --namespace "${kp_environment_namespace}" -o json >"${kp_dir}/workflow-local-acme-renewed.json"
    (( $(jq -r '.status.revision // 0' "${kp_dir}/workflow-local-acme-renewed.json") > kp_revision )) && break
    sleep 5
  done
  jq -e --argjson previous "${kp_revision}" '.status.revision > $previous and
    any(.status.conditions[]; .type == "Ready" and .status == "True")' \
    "${kp_dir}/workflow-local-acme-renewed.json" >/dev/null
}

kp_run_edge_workflow() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_application kp_deployment
  local kp_body kp_actual kp_etag kp_preview_token kp_operation_id kp_commit kp_previous_generation kp_generation
  kp_application="$(jq -r '.applicationId' "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json")"
  kp_deployment="$(jq -r '.directDeploymentId' "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json")"
  kp_previous_generation="$(jq -er '.directGeneration | select(type == "number" and . >= 1)' \
    "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json")"
  kp_body="$(jq -cn --arg application "${kp_application}" '
    {name:"qualification-security-headers",
     spec:{headers:{customResponseHeaders:{"X-Kuberploy-Qualification":"passed"}}},
     assignments:[{scope:"application",id:$application}]}
  ')"
  kp_human_post create-edge-middleware /v1/middlewares "${kp_body}" 201 \
    "${kp_dir}/workflow-edge-middleware.json"
  jq -e '.profile.id | type == "string" and test("^[a-f0-9-]{36}$")' \
    "${kp_dir}/workflow-edge-middleware.json" >/dev/null
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-edge-config-before.json" \
    --write-out '%{http_code}' --request GET \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment}/config")"
  [[ "${kp_actual}" == "200" ]]
  kp_etag="$(jq -er '.etag | select(type == "string" and length > 0)' \
    "${kp_dir}/workflow-edge-config-before.json")"
  jq -e --arg host "${KUBERPLOY_E2E_HTTP_HOSTNAME}" '
    .documents[0].document.spec as $spec |
    ($spec.middlewares // [] | length == 0) and
    ($spec.routes | length == 1 and .[0].host == $host and
      (.[0].middlewareRefs // [] | length == 0))
  ' "${kp_dir}/workflow-edge-config-before.json" >/dev/null
  kp_body="$(jq -c '
    .revision as $revision |
    {mode:"jsonPatch",patch:[
      {op:"add",path:"/spec/middlewares",value:[{
        name:"qualification-security-headers",
        profileRef:{profileId:$revision.profileId,revision:$revision.revision,
          specDigest:$revision.specDigest,assignmentsDigest:$revision.assignmentsDigest},
        spec:$revision.spec}]},
      {op:"add",path:"/spec/routes/0/middlewareRefs",
        value:["qualification-security-headers"]}
    ]}
  ' "${kp_dir}/workflow-edge-middleware.json")"
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-edge-config-preview.json" \
    --write-out '%{http_code}' --request POST \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" \
    --header "If-Match: ${kp_etag}" --header 'Content-Type: application/json' \
    --data-binary "${kp_body}" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment}/config/preview")"
  [[ "${kp_actual}" == "200" ]]
  kp_preview_token="$(jq -er '.previewToken | select(type == "string" and length >= 32)' \
    "${kp_dir}/workflow-edge-config-preview.json")"
  jq -e '(.renderIdentityDigest | type == "string" and length > 0) and
    (.semanticChanges | type == "array" and length > 0) and
    ((.gitDiff + "\n" + .renderedDiff) | contains("qualification-security-headers"))' \
    "${kp_dir}/workflow-edge-config-preview.json" >/dev/null
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-edge-config-operation.json" \
    --write-out '%{http_code}' --request PUT \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" \
    --header "If-Match: ${kp_etag}" --header "Preview-Token: ${kp_preview_token}" \
    --header "Idempotency-Key: qualification-${KUBERPLOY_E2E_RUN_ID}-${KUBERPLOY_E2E_STAGE_ID}-attach-middleware" \
    --header 'Content-Type: application/json' --data-binary "${kp_body}" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment}/config")"
  [[ "${kp_actual}" == "202" ]]
  kp_operation_id="$(jq -er '.id | select(test("^[a-f0-9-]{36}$"))' \
    "${kp_dir}/workflow-edge-config-operation.json")"
  kp_poll_operation "${kp_operation_id}" "${kp_dir}/workflow-edge-config-terminal.json"
  kp_commit="$(jq -er '.gitRevision.commit | select(test("^[a-f0-9]{40}$"))' \
    "${kp_dir}/workflow-edge-config-terminal.json")"
  kp_generation="$(jq -er --argjson previous "${kp_previous_generation}" \
    '.generation | select(type == "number" and . == ($previous + 1))' \
    "${kp_dir}/workflow-edge-config-terminal.json")"
  jq -e '.status == "succeeded"' "${kp_dir}/workflow-edge-config-terminal.json" >/dev/null
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-edge-config-converged.json" \
    --write-out '%{http_code}' --request GET \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment}/config?atLeastRevision=${kp_commit}&waitSeconds=10")"
  [[ "${kp_actual}" == "200" ]]
  jq -e --arg commit "${kp_commit}" '.freshness == "fresh" and .indexedRevision == $commit' \
    "${kp_dir}/workflow-edge-config-converged.json" >/dev/null
  jq --argjson generation "${kp_generation}" '.directGeneration = $generation' \
    "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json" \
    >"${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json.tmp"
  chmod 600 "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json.tmp"
  mv -- "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json.tmp" \
    "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"
  for _ in {1..120}; do
    kp_actual="$(curl --silent --show-error --dump-header "${kp_dir}/workflow-edge-response-headers.txt" \
      --output "${kp_dir}/workflow-edge-response-body.txt" --write-out '%{http_code}' \
      "http://${KUBERPLOY_E2E_HTTP_HOSTNAME}/")"
    if [[ "${kp_actual}" == "200" ]] && \
       grep -Eiq '^X-Kuberploy-Qualification:[[:space:]]*passed[[:space:]]*$' \
         "${kp_dir}/workflow-edge-response-headers.txt"; then
      return 0
    fi
    sleep 2
  done
  return 1
}

kp_fixed_get() {
  local kp_path="${1:?path required}" kp_header_file="${2:?header required}"
  local kp_expected="${3:?status required}" kp_out="${4:?output required}" kp_actual
  kp_actual="$(curl --silent --show-error --output "${kp_out}" --write-out '%{http_code}' \
    --request GET --header "$(<"${kp_header_file}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")${kp_path}")"
  [[ "${kp_actual}" == "${kp_expected}" ]]
}

kp_require_stage_capabilities() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence"
  local kp_out="${kp_dir}/capabilities.json" kp_actual kp_features kp_actions
  case "${KUBERPLOY_E2E_STAGE_ID}" in
    20-postgresql-valkey)
      kp_features='["git","gitops","argo","argoCD"]'
      kp_actions='["projects:create","environments:create","applications:create","deployments:create","operations:read"]' ;;
    25-config-edge)
      kp_features='["git","gitops","argo","argoCD","variableSets","edge","traefik","sslip","externalDNSConfiguration","externalDNS"]'
      kp_actions='["applications:create","deployments:create","deployment-config:read","deployment-config:preview","deployment-config:write","operations:read"]' ;;
    30-git-argo)
      kp_features='["git","gitops","argo","argoCD","deploymentRollbacks"]'
      kp_actions='["environments:create","deployments:create","deployments:update","operations:read"]' ;;
    40-source-build)
      kp_features='["builder","builds","autoDeploy","git","gitops","argo","argoCD","helmDeployments","githubAppSetup"]'
      kp_actions='["builds:read","builds:cancel","builds:retry","build-definitions:write","deployments:create","operations:read","helm-values:preview","helm-releases:read","helm-releases:deploy"]' ;;
    50-runtime-edge)
      kp_features='["git","gitops","argo","argoCD","edge","traefik","traefikMiddlewares","middlewareProfiles"]'
      kp_actions='["deployment-config:read","deployment-config:preview","deployment-config:write","operations:read"]' ;;
    60-local-tls)
      kp_features='["git","gitops","argo","argoCD","edge","traefik","certManager","customCertificates","certificateIssuerCatalog"]'
      kp_actions='["certificate-bindings:read","certificate-bindings:create","deployment-config:read","deployment-config:preview","deployment-config:write","operations:read"]' ;;
    70-registry-retention)
      kp_features='["registry","managedRegistry","imageTagResolution","git","gitops","argo","argoCD"]'
      kp_actions='["deployments:create","operations:read","registry:read","registry-cleanup:preview","registry-cleanup:execute"]' ;;
    80-observability)
      kp_features='["logs","monitoring","metrics"]'
      kp_actions='["logs:read","metrics:read"]' ;;
    90-security)
      kp_features='["git","gitops","argo","argoCD","secretBindings"]'
      kp_actions='["secret-bindings:read","secret-bindings:bind","secret-bindings:create","secret-bindings:rotate","deployment-config:read","deployment-config:preview","deployment-config:write","operations:read"]' ;;
    100-upgrade-rollback)
      kp_features='["git","gitops","argo","argoCD"]'
      kp_actions='[]' ;;
    *) return 0 ;;
  esac
  kp_actual="$(curl --silent --show-error --output "${kp_out}" --write-out '%{http_code}' \
    --request GET --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/capabilities")"
  [[ "${kp_actual}" == "200" ]]
  jq -e --argjson features "${kp_features}" --argjson actions "${kp_actions}" '
    . as $document |
    (.features | type == "object") and (.actions | type == "array") and
    ([$features[] | . as $feature | $document.features[$feature] == true] | all) and
    ([$actions[] | . as $action | $document.actions | index($action) != null] | all)
  ' "${kp_out}" >/dev/null || kp_die "${KUBERPLOY_E2E_STAGE_ID} required capability/action is unavailable or stale"
}

kp_run_observability_workflow() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence"
  if [[ "${KUBERPLOY_E2E_HERMETIC_TEST:-false}" == "true" ]]; then
    KP_OBSERVABILITY_TEST_PROOF_OUTPUT="${kp_dir}/observability-proof.json" \
      bash "$(kp_repo_root)/test/e2e/test-observability-driver.sh" >/dev/null
  else
    KUBERPLOY_E2E_API_BASE_URL="$(jq -r '.apiBaseURL' "${kp_scenario}")" \
    KUBERPLOY_E2E_OBSERVABILITY_EVIDENCE_DIR="${kp_dir}" \
    KUBERPLOY_E2E_WORKFLOW_STATE_FILE="${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json" \
      "$(kp_repo_root)/scripts/kubernetes/test/e2e/observability-driver.sh"
  fi
  jq -e '
    (keys|sort)==["denials","metrics","monitoring","runtime","safeEvidence","schemaVersion"] and
    .schemaVersion==1 and .monitoring.available==true and
    (.monitoring.identityAttestation=="managed-exact-release-and-rules" or
      .monitoring.identityAttestation=="existing-compatible-catalog") and
    .runtime.mergedSnapshotNonEmpty==true and .runtime.exactSourceNonEmpty==true and
    .runtime.followNonEmpty==true and .runtime.sanitizedEventNonEmpty==true and
    (.metrics.catalog|length)==7 and .metrics.scopes==["service","namespace","global"] and
    .metrics.liveSeriesMetric=="replicas-ready" and .safeEvidence==true
  ' "${kp_dir}/observability-proof.json" >/dev/null || kp_die "strict observability proof is invalid"
}

kp_run_browser_workflow() {
  local kp_state="${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"
  node "$(kp_repo_root)/web/scripts/qualification-browser.mjs" \
    "${KUBERPLOY_E2E_BROWSER_EXECUTABLE}" "$(jq -r '.apiBaseURL' "${kp_scenario}")" \
    "$(jq -r '.applicationId' "${kp_state}")" "$(jq -r '.directDeploymentId' "${kp_state}")" \
    "${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}" "${KUBERPLOY_E2E_STAGE_DIR}/evidence/browser-proof.json"
  jq -e '(keys|sort)==["browserCommandInvoked","configPreview","hermeticSeam","logs","metrics","realBrowser","rollback","sourceChooser"] and .browserCommandInvoked==true' \
    "${KUBERPLOY_E2E_STAGE_DIR}/evidence/browser-proof.json" >/dev/null
}

kp_run_security_workflow() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_project
  local kp_rbac_status
  # Prove the scenario-pinned CNI identity and actual deny/allow enforcement
  # before exercising the remaining security workflows. There is no skip path.
  kp_security_network_policy_enforcement
  if "${KUBERPLOY_E2E_KUBECTL}" auth can-i get secrets --namespace "${kp_namespace}" \
      --as "system:serviceaccount:${kp_namespace}:default" \
      >"${kp_dir}/workflow-rbac-deny.txt"; then
    kp_rbac_status=0
  else
    kp_rbac_status=$?
  fi
  [[ "${kp_rbac_status}" -eq 1 && \
     "$(tr -d '[:space:]' <"${kp_dir}/workflow-rbac-deny.txt")" == "no" ]] || \
    kp_die "RBAC denial probe did not return exact no/status-1"
  if jq -n --arg ns "${kp_namespace}" '
      {apiVersion:"v1",kind:"Pod",metadata:{name:"privileged-denial-probe",namespace:$ns},
       spec:{containers:[{name:"probe",image:"registry.k8s.io/pause:3.10",
         securityContext:{privileged:true}}]}}
    ' | "${KUBERPLOY_E2E_KUBECTL}" create --dry-run=server -f - \
      >"${kp_dir}/workflow-admission-deny.stdout" 2>"${kp_dir}/workflow-admission-deny.stderr"; then
    kp_die "server admission accepted a privileged tenant Pod"
  fi
  kp_project="$(jq -r '.projectId' "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json")"
  kp_fixed_get "/v1/projects/${kp_project}" "${KUBERPLOY_E2E_DENIED_AUTH_HEADER_FILE}" 404 \
    "${kp_dir}/workflow-cross-tenant-project-denied.json"
  jq -e '.status == 404 and (.code | type == "string") and
    (tostring | test("token|secret|password|privateKey";"i") | not)' \
    "${kp_dir}/workflow-cross-tenant-project-denied.json" >/dev/null
  kp_security_resource_quota_rejection
  kp_run_runtime_secret_workflow
  kp_security_audit_timeline
}

kp_assert_runtime_secret_metadata_safe() {
  local kp_file="${1:?metadata file required}"
  jq -e '
    [paths as $p | ($p[-1] | tostring) |
      select(test("^(values|value|data|stringData|encryptedData|ciphertext|manifest|providerRevision|contentFingerprint|requestFingerprint|sealedKeyFingerprint)$";"i"))] |
    length == 0
  ' "${kp_file}" >/dev/null
  ! grep -F -f "${KUBERPLOY_E2E_RUNTIME_SECRET_INITIAL_VALUE_FILE}" "${kp_file}" >/dev/null
  ! grep -F -f "${KUBERPLOY_E2E_RUNTIME_SECRET_ROTATED_VALUE_FILE}" "${kp_file}" >/dev/null
}

kp_poll_runtime_secret_version() {
  local kp_binding="${1:?binding required}" kp_version="${2:?version required}"
  local kp_out="${3:?output required}" kp_actual
  for _ in {1..120}; do
    kp_actual="$(curl --silent --show-error --output "${kp_out}" --write-out '%{http_code}' \
      --request GET --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
      "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/secret-bindings/${kp_binding}")"
    [[ "${kp_actual}" == "200" ]]
    kp_assert_runtime_secret_metadata_safe "${kp_out}"
    if jq -e --argjson version "${kp_version}" '.state == "ready" and .activeVersion == $version and
        any(.versions[]; .number == $version and .state == "active" and
          (.readinessObservedAt | type == "string") and (.activatedAt | type == "string"))' \
        "${kp_out}" >/dev/null; then
      return 0
    fi
    sleep 1
  done
  return 1
}

kp_attach_runtime_secret_version() {
  local kp_binding="${1:?binding required}" kp_version="${2:?version required}"
  local kp_secret_name="${3:?secret name required}" kp_state_file kp_deployment kp_application
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_actual kp_etag kp_body kp_preview kp_operation kp_commit
  local kp_previous_generation kp_generation
  kp_state_file="${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"
  kp_deployment="$(jq -r '.directDeploymentId' "${kp_state_file}")"
  kp_application="$(jq -r '.applicationId' "${kp_state_file}")"
  kp_previous_generation="$(jq -er '.directGeneration | select(type == "number" and . >= 1)' "${kp_state_file}")"
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-runtime-secret-config-before-v${kp_version}.json" \
    --write-out '%{http_code}' --request GET --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment}/config")"
  [[ "${kp_actual}" == "200" ]]
  kp_etag="$(jq -er '.etag' "${kp_dir}/workflow-runtime-secret-config-before-v${kp_version}.json")"
  kp_body="$(jq -cn --arg binding "${kp_binding}" --arg name "$(jq -r '.workflow.runtimeSecret.name' "${kp_scenario}")" \
    --arg key "$(jq -r '.workflow.runtimeSecret.key' "${kp_scenario}")" \
    --arg env "$(jq -r '.workflow.runtimeSecret.environmentName' "${kp_scenario}")" --argjson version "${kp_version}" '
      {mode:"jsonPatch",patch:[{op:"add",path:"/spec/runtime/env",value:[{name:$env,
        valueFrom:{secretBindingRef:{bindingId:$binding,name:$name,key:$key,version:$version}}}]}]}' )"
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-runtime-secret-preview-v${kp_version}.json" \
    --write-out '%{http_code}' --request POST --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" --header "If-Match: ${kp_etag}" \
    --header 'Content-Type: application/json' --data-binary "${kp_body}" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment}/config/preview")"
  [[ "${kp_actual}" == "200" ]]
  kp_preview="$(jq -er '.previewToken | select(type == "string" and length >= 32)' "${kp_dir}/workflow-runtime-secret-preview-v${kp_version}.json")"
  jq -e --arg binding "${kp_binding}" --argjson version "${kp_version}" '
    (.semanticChanges | length > 0) and ((.gitDiff + "\n" + .renderedDiff) | contains($binding) and contains(($version|tostring)))
  ' "${kp_dir}/workflow-runtime-secret-preview-v${kp_version}.json" >/dev/null
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-runtime-secret-config-operation-v${kp_version}.json" \
    --write-out '%{http_code}' --request PUT --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" --header "If-Match: ${kp_etag}" \
    --header "Preview-Token: ${kp_preview}" \
    --header "Idempotency-Key: qualification-${KUBERPLOY_E2E_RUN_ID}-${KUBERPLOY_E2E_STAGE_ID}-bind-secret-v${kp_version}" \
    --header 'Content-Type: application/json' --data-binary "${kp_body}" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment}/config")"
  [[ "${kp_actual}" == "202" ]]
  kp_operation="$(jq -er '.id | select(test("^[a-f0-9-]{36}$"))' "${kp_dir}/workflow-runtime-secret-config-operation-v${kp_version}.json")"
  kp_poll_operation "${kp_operation}" "${kp_dir}/workflow-runtime-secret-config-terminal-v${kp_version}.json"
  kp_commit="$(jq -er '.gitRevision.commit | select(test("^[a-f0-9]{40}$"))' "${kp_dir}/workflow-runtime-secret-config-terminal-v${kp_version}.json")"
  kp_generation="$(jq -er --argjson previous "${kp_previous_generation}" \
    '.generation | select(type == "number" and . == ($previous + 1))' \
    "${kp_dir}/workflow-runtime-secret-config-terminal-v${kp_version}.json")"
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-runtime-secret-config-converged-v${kp_version}.json" \
    --write-out '%{http_code}' --request GET --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment}/config?atLeastRevision=${kp_commit}&waitSeconds=10")"
  [[ "${kp_actual}" == "200" ]]
  jq -e --arg commit "${kp_commit}" '.freshness == "fresh" and .indexedRevision == $commit' \
    "${kp_dir}/workflow-runtime-secret-config-converged-v${kp_version}.json" >/dev/null
  jq --argjson generation "${kp_generation}" '.directGeneration = $generation' "${kp_state_file}" >"${kp_state_file}.tmp"
  chmod 600 "${kp_state_file}.tmp"
  mv -- "${kp_state_file}.tmp" "${kp_state_file}"
  for _ in {1..120}; do
    if "${KUBERPLOY_E2E_KUBECTL}" get deployments --namespace "$(jq -r '.directEnvironmentNamespace' "${kp_state_file}")" \
        --selector "kuberploy.io/application-id=${kp_application}" -o json |
      jq -e --arg name "${kp_secret_name}" --arg key "$(jq -r '.workflow.runtimeSecret.key' "${kp_scenario}")" \
        --arg env "$(jq -r '.workflow.runtimeSecret.environmentName' "${kp_scenario}")" '
          select(.items | length == 1) | .items[0] |
          any(.spec.template.spec.containers[0].env[]; .name == $env and
            .valueFrom.secretKeyRef.name == $name and .valueFrom.secretKeyRef.key == $key) and
          (.status.observedGeneration >= .metadata.generation) and .status.availableReplicas >= 1
        ' >"${kp_dir}/workflow-runtime-secret-rollout-v${kp_version}.json"; then
      return 0
    fi
    sleep 2
  done
  return 1
}

kp_run_runtime_secret_workflow() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_state_file kp_application kp_environment
  local kp_request kp_binding kp_secret_name kp_actual
  kp_state_file="${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"
  kp_application="$(jq -r '.applicationId' "${kp_state_file}")"
  kp_environment="$(jq -r '.directEnvironmentId' "${kp_state_file}")"
  kp_request="${KUBERPLOY_E2E_STAGE_DIR}/runtime-secret-request.json"
  jq -n --arg environment "${kp_environment}" --arg name "$(jq -r '.workflow.runtimeSecret.name' "${kp_scenario}")" \
    --arg key "$(jq -r '.workflow.runtimeSecret.key' "${kp_scenario}")" \
    --arg env "$(jq -r '.workflow.runtimeSecret.environmentName' "${kp_scenario}")" \
    --rawfile value "${KUBERPLOY_E2E_RUNTIME_SECRET_INITIAL_VALUE_FILE}" '
      {environmentId:$environment,name:$name,provider:"sealed-secrets",values:{($key):($value|rtrimstr("\n"))},
       deliveries:[{sourceKey:$key,kind:"environment",environmentName:$env}]}' >"${kp_request}"
  chmod 600 "${kp_request}"
  kp_human_post_file create-runtime-secret "/v1/applications/${kp_application}/secret-bindings" "${kp_request}" 201 \
    "${kp_dir}/workflow-runtime-secret-created.json"
  rm -- "${kp_request}"
  kp_assert_runtime_secret_metadata_safe "${kp_dir}/workflow-runtime-secret-created.json"
  kp_binding="$(jq -er '.id | select(test("^[a-f0-9-]{36}$"))' "${kp_dir}/workflow-runtime-secret-created.json")"
  kp_poll_runtime_secret_version "${kp_binding}" 1 "${kp_dir}/workflow-runtime-secret-ready-v1.json"
  "${KUBERPLOY_E2E_KUBECTL}" get sealedsecrets.bitnami.com --namespace "$(jq -r '.directEnvironmentNamespace' "${kp_state_file}")" \
    --selector "kuberploy.io/secret-binding=${kp_binding},kuberploy.io/secret-version=1" -o json |
    jq --arg binding "${kp_binding}" 'select(.items | length == 1) | .items[0] |
      {apiVersion,kind,metadata:{name:.metadata.name,namespace:.metadata.namespace,uid:.metadata.uid,labels:.metadata.labels},
       template:{name:.spec.template.metadata.name,type:.spec.template.type,immutable:.spec.template.immutable},
       encryptedKeys:(.spec.encryptedData|keys)} |
      select(.metadata.labels["kuberploy.io/secret-binding"] == $binding and .template.immutable == true)' \
      >"${kp_dir}/workflow-runtime-secret-sealed-v1.json"
  [[ -s "${kp_dir}/workflow-runtime-secret-sealed-v1.json" ]]
  kp_secret_name="$(jq -er '.metadata.name' "${kp_dir}/workflow-runtime-secret-sealed-v1.json")"
  kp_attach_runtime_secret_version "${kp_binding}" 1 "${kp_secret_name}"
  kp_request="${KUBERPLOY_E2E_STAGE_DIR}/runtime-secret-rotation.json"
  jq -n --arg key "$(jq -r '.workflow.runtimeSecret.key' "${kp_scenario}")" \
    --arg env "$(jq -r '.workflow.runtimeSecret.environmentName' "${kp_scenario}")" \
    --rawfile value "${KUBERPLOY_E2E_RUNTIME_SECRET_ROTATED_VALUE_FILE}" '
      {expectedActiveVersion:1,values:{($key):($value|rtrimstr("\n"))},
       deliveries:[{sourceKey:$key,kind:"environment",environmentName:$env}]}' >"${kp_request}"
  chmod 600 "${kp_request}"
  kp_human_post_file rotate-runtime-secret "/v1/secret-bindings/${kp_binding}/versions" "${kp_request}" 201 \
    "${kp_dir}/workflow-runtime-secret-rotated.json"
  rm -- "${kp_request}"
  kp_assert_runtime_secret_metadata_safe "${kp_dir}/workflow-runtime-secret-rotated.json"
  kp_poll_runtime_secret_version "${kp_binding}" 2 "${kp_dir}/workflow-runtime-secret-ready-v2.json"
  jq -e 'any(.versions[]; .number == 1 and .state == "retained")' \
    "${kp_dir}/workflow-runtime-secret-ready-v2.json" >/dev/null
  "${KUBERPLOY_E2E_KUBECTL}" get sealedsecrets.bitnami.com --namespace "$(jq -r '.directEnvironmentNamespace' "${kp_state_file}")" \
    --selector "kuberploy.io/secret-binding=${kp_binding},kuberploy.io/secret-version=2" -o json |
    jq --arg binding "${kp_binding}" 'select(.items | length == 1) | .items[0] |
      {apiVersion,kind,metadata:{name:.metadata.name,namespace:.metadata.namespace,uid:.metadata.uid,labels:.metadata.labels},
       template:{name:.spec.template.metadata.name,type:.spec.template.type,immutable:.spec.template.immutable},
       encryptedKeys:(.spec.encryptedData|keys)} |
      select(.metadata.labels["kuberploy.io/secret-binding"] == $binding and .template.immutable == true)' \
      >"${kp_dir}/workflow-runtime-secret-sealed-v2.json"
  kp_secret_name="$(jq -er '.metadata.name' "${kp_dir}/workflow-runtime-secret-sealed-v2.json")"
  kp_attach_runtime_secret_version "${kp_binding}" 2 "${kp_secret_name}"
  for kp_actual in "${kp_dir}"/*; do
    [[ -f "${kp_actual}" ]] || continue
    ! grep -F -f "${KUBERPLOY_E2E_RUNTIME_SECRET_INITIAL_VALUE_FILE}" "${kp_actual}" >/dev/null || \
      kp_die "initial runtime-secret material appeared in qualification evidence"
    ! grep -F -f "${KUBERPLOY_E2E_RUNTIME_SECRET_ROTATED_VALUE_FILE}" "${kp_actual}" >/dev/null || \
      kp_die "rotated runtime-secret material appeared in qualification evidence"
  done
}

kp_write_workflow_proof() {
  local kp_out="${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-proof.json"
  case "${KUBERPLOY_E2E_STAGE_ID}" in
    20-postgresql-valkey)
      jq -n --arg project "$(jq -r '.projectId' "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json")" \
        '{mutation:"durable-operation-and-valkey-dataset-loss",projectId:$project,
          postgresqlUIDChanged:true,valkeyDatasetDeleted:$recovery[0].datasetDeleted,
          outboxPublishedBeforeLoss:($published[0].published==1 and $published[0].replayed==0),
          postgresOutboxReplayed:($replayed[0].published==1 and $replayed[0].replayed==1),
          exactlyOnceConverged:true,valkeyDeploymentRestored:$recovery[0].valkeyDeploymentRestored,
          workerDeploymentRestored:$recovery[0].workerDeploymentRestored,postRestartRead:true,
          acceptedOperationCompletedAfterRestarts:true} + $access[0]' \
        --slurpfile published "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-outbox-published-before-loss.json" \
        --slurpfile replayed "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-outbox-replayed-after-loss.json" \
        --slurpfile recovery "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-valkey-dataset-recovery.json" \
        --slurpfile access "${KUBERPLOY_E2E_STAGE_DIR}/evidence/access-proof.json" >"${kp_out}" ;;
    25-config-edge)
      cp -- "${KUBERPLOY_E2E_STAGE_DIR}/evidence/config-edge-proof.json" "${kp_out}" ;;
    30-git-argo)
      jq --slurpfile argo "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-argo-application.json" '
        {mutation:"direct-protected-update-rollback",directOperationId,
         directUpdateOperationId,protectedOperationId,rollbackOperationId,
         argoApplication:$argo[0].metadata.name,argoRevision:$argo[0].status.sync.revision,
         argoSyncedHealthy:true,runtimeDigestVerified:true}' \
        "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json" >"${kp_out}" ;;
    40-source-build)
      jq '{mutation:"github-webhook-build-cancel-cache-fault-auto-deploy-promotion-and-approved-helm",successfulBuildId,
        buildDefinitionId,buildPromotionOperationId,helmReleaseId,helmApplicationRevision,
        autoDeployPolicyId,autoDeployOperationId,autoDeployDeploymentId,
        cancelledBuildId,cancelRetryBuildId,cacheHitBuildId,cacheDegradedBuildId,pushFailureBuildId,
        signedWebhookAccepted:true,invalidWebhookRejected:true,
        duplicateDeliveryDeduplicated:true,liveBuildJobCredentialSplit:true,
        buildCancellationAccepted:true,buildCancellationJobDeleted:true,buildCancellationRetrySucceeded:true,
        webhookWakeDisabled:true,safetyPollRetained:true,durableDeliveryPollingConverged:true,
        secondBuildCacheHit:true,cacheColdDegradedPushSucceeded:true,pushFailureTerminal:true,
        autoDeployReceiptSubmitted:true,credentialValuesExcluded:true,
        helmRenderedPreviewSanitized:true,helmArgoSyncedHealthy:true}' \
        "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json" >"${kp_out}" ;;
    50-runtime-edge)
      jq -n --arg hostname "${KUBERPLOY_E2E_HTTP_HOSTNAME}" \
        --arg middleware "$(jq -r '.profile.id' "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-edge-middleware.json")" \
        '{mutation:"http-route-and-middleware",hostname:$hostname,middlewareId:$middleware,
          profileAttachedThroughConfigSave:true,responseHeaderVerified:true}' >"${kp_out}" ;;
    60-local-tls)
      jq -n --arg hostname "${KUBERPLOY_E2E_LOCAL_ACME_HOSTNAME}" \
        --arg customHostname "${KUBERPLOY_E2E_CUSTOM_TLS_HOSTNAME}" \
        --arg directoryURL "${KUBERPLOY_E2E_LOCAL_ACME_DIRECTORY_URL}" \
        --arg binding "$(jq -r '.id' "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-custom-certificate-ready.json")" \
        '{mutation:"attached-custom-certificate-and-local-acme-routes",hostname:$hostname,
          customHostname:$customHostname,acmeDirectoryURL:$directoryURL,
          customCertificateBindingId:$binding,customRouteAttached:true,localACMERouteAttached:true,
          configuredDirectoryVerified:true,issuerCatalogAuthorized:true,
          attachedHostsTLSVerified:true,attachedCertificateReissued:true}' >"${kp_out}" ;;
    70-registry-retention)
      jq --slurpfile image "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-image-tag-proof.json" '
        {mutation:"registry-preview-execution-and-existing-image",planId:.id,state,
        protected:([.items[]|select(.state=="protected")]|length),
        deleted:([.items[]|select(.state=="deleted")]|length),imageTagResolution:$image[0]}' \
        "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-registry-execution.json" >"${kp_out}" ;;
    80-observability)
      jq '. + {mutation:"authorized-observability-and-cross-tenant-denial"}' \
        "${KUBERPLOY_E2E_STAGE_DIR}/evidence/observability-proof.json" >"${kp_out}" ;;
    90-security)
      jq -n \
        --arg binding "$(jq -r '.id' "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-runtime-secret-ready-v2.json")" \
        --arg sealedV1 "$(jq -r '.metadata.uid' "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-runtime-secret-sealed-v1.json")" \
        --arg sealedV2 "$(jq -r '.metadata.uid' "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-runtime-secret-sealed-v2.json")" \
        --arg networkProviderUID "$(jq -r '.uid' "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-network-provider.json")" \
        --arg resourceQuotaUID "$(jq -r '.uid' "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-resource-quota.json")" \
        --argjson auditEventIds "$(jq -c '[.items[].id]' "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-audit-timeline.json")" \
        '{mutation:"security-controls-and-runtime-secret-lifecycle",rbacDenied:true,
          privilegedAdmissionDenied:true,crossTenantDenied:true,secretNondisclosure:true,
          quotaRejected:true,networkProviderIdentityAttested:true,networkProviderUID:$networkProviderUID,
          networkDenied:true,networkAllowed:true,resourceQuotaUID:$resourceQuotaUID,
          auditActorResourceOutcome:true,auditEventIds:$auditEventIds,
          secretBindingId:$binding,initialSealedSecretUID:$sealedV1,rotatedSealedSecretUID:$sealedV2,
          initialVersionActivated:true,initialVersionAttached:true,rotatedVersionActivated:true,
          priorVersionRetained:true,rotatedVersionAttached:true,rolloutsReady:true}' >"${kp_out}" ;;
    100-upgrade-rollback)
      jq '{mutation:"installer-helm-upgrade-and-rollback",helmRelease:"kuberploy-qualification",
        sourceVersion,targetVersion,sourceRevision,targetRevision,rollbackRevision,
        targetIdentityReady:true,rollbackSourceReady:true}' \
        "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-installer-lifecycle.json" >"${kp_out}" ;;
    *) return 0 ;;
  esac
  chmod 600 "${kp_out}"
}

kp_run() {
  if [[ "${KUBERPLOY_E2E_STAGE_ID}" == "10-one-chart-install" ]]; then
    kp_create_owned_namespace "${kp_namespace}"
    # The installer and Argo have fixed namespace boundaries. `create` is
    # intentional: pre-existing shared state fails this disposable qualification
    # before Helm can mutate it.
    kp_create_owned_namespace kuberploy-system
    kp_create_owned_namespace argocd
  else
    kp_create_marker
    kp_require_stage_capabilities
  fi
  if [[ "${KUBERPLOY_E2E_STAGE_ID}" == "20-postgresql-valkey" ]]; then
    kp_run_durability_workflow
  elif [[ "${KUBERPLOY_E2E_STAGE_ID}" == "25-config-edge" ]]; then
    kp_run_config_edge_workflow
  elif [[ "${KUBERPLOY_E2E_STAGE_ID}" == "30-git-argo" ]]; then
    kp_run_git_workflow
  elif [[ "${KUBERPLOY_E2E_STAGE_ID}" == "40-source-build" ]]; then
    kp_run_source_build_workflow
    kp_run_helm_workflow
  elif [[ "${KUBERPLOY_E2E_STAGE_ID}" == "70-registry-retention" ]]; then
    kp_run_existing_image_tag_workflow
    kp_run_registry_cleanup_workflow
  elif [[ "${KUBERPLOY_E2E_STAGE_ID}" == "60-local-tls" ]]; then
    kp_run_tls_workflow
  elif [[ "${KUBERPLOY_E2E_STAGE_ID}" == "50-runtime-edge" ]]; then
    kp_run_edge_workflow
  elif [[ "${KUBERPLOY_E2E_STAGE_ID}" == "100-upgrade-rollback" ]]; then
    kp_run_upgrade_workflow
  elif [[ "${KUBERPLOY_E2E_STAGE_ID}" == "80-observability" ]]; then
    kp_run_observability_workflow
    kp_run_browser_workflow
  elif [[ "${KUBERPLOY_E2E_STAGE_ID}" == "90-security" ]]; then
    kp_run_security_workflow
  fi
  kp_write_workflow_proof
  local kp_assertion kp_spec kp_probe kp_evidence
  IFS=',' read -r -a kp_assertions <<<"${KUBERPLOY_E2E_STAGE_ASSERTIONS}"
  for kp_assertion in "${kp_assertions[@]}"; do
    kp_spec="$(jq -cer --arg s "${KUBERPLOY_E2E_STAGE_ID}" --arg a "${kp_assertion}" '.stages[$s].assertions[$a]' "${kp_scenario}")"
    kp_probe="$(jq -r '.probe' <<<"${kp_spec}")"
    kp_evidence="${KUBERPLOY_E2E_STAGE_DIR}/evidence/${kp_assertion}.evidence"
    case "${kp_probe}" in
      helm-install) kp_probe_helm_install "${kp_evidence}" ;;
      installer-proof)
        [[ -s "${KUBERPLOY_E2E_STAGE_DIR}/evidence/installer-proof.json" &&
           ! -L "${KUBERPLOY_E2E_STAGE_DIR}/evidence/installer-proof.json" ]] ||
          kp_die "installer proof is missing; helm-install must run first"
        cp -- "${KUBERPLOY_E2E_STAGE_DIR}/evidence/installer-proof.json" "${kp_evidence}"
        chmod 600 "${kp_evidence}"
        ;;
      workflow-proof)
        [[ -s "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-proof.json" &&
           ! -L "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-proof.json" ]] ||
          kp_die "workflow proof is missing for ${KUBERPLOY_E2E_STAGE_ID}"
        cp -- "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-proof.json" "${kp_evidence}"
        chmod 600 "${kp_evidence}"
        ;;
      browser-proof)
        cp -- "${KUBERPLOY_E2E_STAGE_DIR}/evidence/browser-proof.json" "${kp_evidence}"
        chmod 600 "${kp_evidence}"
        ;;
      kubernetes) kp_probe_kubernetes "${kp_spec}" "${kp_evidence}" ;;
      api) kp_probe_api "${kp_spec}" "${kp_evidence}" ;;
      http) kp_probe_http "${kp_spec}" "${kp_evidence}" ;;
      tls) kp_probe_tls "${kp_spec}" "${kp_evidence}" ;;
      dns) kp_probe_dns "${kp_spec}" "${kp_evidence}" ;;
      *) kp_die "unsupported repository probe ${kp_probe}" ;;
    esac
  done
  kp_snapshot_run_resources
  local kp_workflow_evidence="false"
  [[ ! -f "${KUBERPLOY_E2E_STAGE_DIR}/evidence/workflow-proof.json" ]] || kp_workflow_evidence="true"
  jq -n --arg run "${KUBERPLOY_E2E_RUN_ID}" --arg stage "${KUBERPLOY_E2E_STAGE_ID}" \
    --argjson workflowEvidence "${kp_workflow_evidence}" \
    --arg csv "${KUBERPLOY_E2E_STAGE_ASSERTIONS}" '
      {schemaVersion:1,runID:$run,stage:$stage,status:"passed",
       assertions:($csv|split(",")|map({id:.,status:"passed",
         evidenceFiles:([("evidence/"+.+".evidence")] +
           (if $workflowEvidence then ["evidence/workflow-proof.json"] else [] end))}))}
    ' >"${KUBERPLOY_E2E_STAGE_RESULT_FILE}"
}

kp_cleanup() {
  local kp_count=0 kp_api kp_kind kp_ns kp_name kp_uid kp_operation kp_current kp_resource
  if [[ "${KUBERPLOY_E2E_STAGE_ID}" == "25-config-edge" ]]; then
    kp_cleanup_config_edge_workflow
  fi
  if [[ "${KUBERPLOY_E2E_STAGE_ID}" == "20-postgresql-valkey" ]]; then
    local kp_restore_component kp_restore_file kp_restore_ns kp_restore_name kp_restore_current
    for kp_restore_component in worker valkey; do
      kp_restore_file="${KUBERPLOY_E2E_STAGE_DIR}/evidence/${kp_restore_component}-before-dataset-loss.json"
      [[ -f "${kp_restore_file}" && ! -L "${kp_restore_file}" ]] || continue
      kp_restore_ns="$(jq -er --arg component "${kp_restore_component}" '.workflow.recovery[$component].namespace' "${kp_scenario}")"
      kp_restore_name="$(jq -er --arg component "${kp_restore_component}" '.workflow.recovery[$component].controllerName' "${kp_scenario}")"
      kp_restore_current="$("${KUBERPLOY_E2E_KUBECTL}" get deployment "${kp_restore_name}" --namespace "${kp_restore_ns}" -o json)"
      jq -e --arg uid "$(jq -er '.metadata.uid' "${kp_restore_file}")" --arg name "${kp_restore_name}" '
        .metadata.uid==$uid and .metadata.name==$name and (.spec.replicas==0 or .spec.replicas==1)
      ' <<<"${kp_restore_current}" >/dev/null || kp_die "refusing to restore a replaced stage20 Deployment"
      if [[ "$(jq -r '.spec.replicas' <<<"${kp_restore_current}")" == "0" ]]; then
        "${KUBERPLOY_E2E_KUBECTL}" scale "deployment/${kp_restore_name}" --namespace "${kp_restore_ns}" --replicas=1 >/dev/null
        "${KUBERPLOY_E2E_KUBECTL}" rollout status "deployment/${kp_restore_name}" \
          --namespace "${kp_restore_ns}" --timeout=10m >/dev/null
      fi
    done
  fi
  # Validate every target before the first cleanup mutation. A later forged or
  # replaced identity can therefore never cause earlier resources to be acted on.
  while IFS= read -r kp_record; do
    kp_kind="$(jq -r '.kind' <<<"${kp_record}")"
    kp_ns="$(jq -r '.namespace' <<<"${kp_record}")"
    kp_name="$(jq -r '.name' <<<"${kp_record}")"
    kp_uid="$(jq -r '.uid // ""' <<<"${kp_record}")"
    kp_operation="$(jq -r '.operation' <<<"${kp_record}")"
    if [[ "${kp_kind}" == "Namespace" ]]; then
      kp_current="$("${KUBERPLOY_E2E_KUBECTL}" get namespace "${kp_name}" \
        --ignore-not-found -o json)"
    elif [[ "${kp_kind}" == "Certificate" ]]; then
      kp_current="$("${KUBERPLOY_E2E_KUBECTL}" get certificate "${kp_name}" \
        --namespace "${kp_ns}" --ignore-not-found -o json)"
    elif [[ "${kp_kind}" == "Job" ]]; then
      kp_current="$("${KUBERPLOY_E2E_KUBECTL}" get job "${kp_name}" \
        --namespace "${kp_ns}" --ignore-not-found -o json)"
    elif [[ "${kp_kind}" == "Pod" || "${kp_kind}" == "Service" || "${kp_kind}" == "NetworkPolicy" || "${kp_kind}" == "Secret" ]]; then
      kp_resource="$(kp_security_resource_for_kind "${kp_kind}")"
      kp_current="$("${KUBERPLOY_E2E_KUBECTL}" get "${kp_resource}" "${kp_name}" \
        --namespace "${kp_ns}" --ignore-not-found -o json)"
    else
      kp_current="$("${KUBERPLOY_E2E_KUBECTL}" get configmap "${kp_name}" \
        --namespace "${kp_ns}" --ignore-not-found -o json)"
    fi
    if [[ -z "${kp_current}" && "${kp_operation}" == "planned-create" ]]; then
      continue
    fi
    [[ -n "${kp_current}" ]]
    jq -e --arg uid "${kp_uid}" --arg run "${KUBERPLOY_E2E_RUN_ID}" --arg managed "${KUBERPLOY_E2E_MANAGED_BY_LABEL_VALUE}" '
      (($uid == "" and (.metadata.uid | type == "string" and length > 0)) or .metadata.uid == $uid) and
      .metadata.labels["kuberploy.io/test-run"] == $run and
      .metadata.labels["app.kubernetes.io/managed-by"] == $managed
    ' <<<"${kp_current}" >/dev/null
  done <"${KUBERPLOY_E2E_STAGE_INVENTORY_FILE}"

  while IFS= read -r kp_record; do
    kp_api="$(jq -r '.apiVersion' <<<"${kp_record}")"
    kp_kind="$(jq -r '.kind' <<<"${kp_record}")"
    kp_ns="$(jq -r '.namespace' <<<"${kp_record}")"
    kp_name="$(jq -r '.name' <<<"${kp_record}")"
    kp_uid="$(jq -r '.uid // ""' <<<"${kp_record}")"
    kp_operation="$(jq -r '.operation' <<<"${kp_record}")"
    if [[ "${kp_operation}" == "planned-create" ]]; then
      if [[ "${kp_kind}" == "Namespace" ]]; then
        kp_current="$("${KUBERPLOY_E2E_KUBECTL}" get namespace "${kp_name}" \
          --ignore-not-found -o json)"
      elif [[ "${kp_kind}" == "Certificate" ]]; then
        kp_current="$("${KUBERPLOY_E2E_KUBECTL}" get certificate "${kp_name}" \
          --namespace "${kp_ns}" --ignore-not-found -o json)"
      elif [[ "${kp_kind}" == "Job" ]]; then
        kp_current="$("${KUBERPLOY_E2E_KUBECTL}" get job "${kp_name}" \
          --namespace "${kp_ns}" --ignore-not-found -o json)"
      elif [[ "${kp_kind}" == "Pod" || "${kp_kind}" == "Service" || "${kp_kind}" == "NetworkPolicy" ]]; then
        kp_resource="$(kp_security_resource_for_kind "${kp_kind}")"
        kp_current="$("${KUBERPLOY_E2E_KUBECTL}" get "${kp_resource}" "${kp_name}" \
          --namespace "${kp_ns}" --ignore-not-found -o json)"
      else
        kp_current="$("${KUBERPLOY_E2E_KUBECTL}" get configmap "${kp_name}" \
          --namespace "${kp_ns}" --ignore-not-found -o json)"
      fi
      if [[ -z "${kp_current}" ]]; then
        kp_count=$((kp_count + 1))
        continue
      fi
    fi
    if [[ "${kp_kind}" == "Namespace" ]]; then
      if [[ "${kp_name}" == "argocd" ]]; then
        "${KUBERPLOY_E2E_HELM}" uninstall kuberploy-qualification --namespace kuberploy-system
      fi
      "${KUBERPLOY_E2E_KUBECTL}" delete namespace "${kp_name}" --ignore-not-found=true --wait=true >/dev/null
      "${KUBERPLOY_E2E_KUBECTL}" wait --for=delete "namespace/${kp_name}" --timeout=5m >/dev/null
    elif [[ "${kp_kind}" == "Certificate" ]]; then
      "${KUBERPLOY_E2E_KUBECTL}" delete certificate "${kp_name}" --namespace "${kp_ns}" \
        --ignore-not-found=true --wait=true >/dev/null
      "${KUBERPLOY_E2E_KUBECTL}" wait --for=delete "certificate/${kp_name}" \
        --namespace "${kp_ns}" --timeout=2m >/dev/null
    elif [[ "${kp_kind}" == "Job" ]]; then
      "${KUBERPLOY_E2E_KUBECTL}" delete job "${kp_name}" --namespace "${kp_ns}" \
        --ignore-not-found=true --wait=true >/dev/null
      "${KUBERPLOY_E2E_KUBECTL}" wait --for=delete "job/${kp_name}" \
        --namespace "${kp_ns}" --timeout=2m >/dev/null
    elif [[ "${kp_kind}" == "Pod" || "${kp_kind}" == "Service" || "${kp_kind}" == "NetworkPolicy" ]]; then
      kp_resource="$(kp_security_resource_for_kind "${kp_kind}")"
      "${KUBERPLOY_E2E_KUBECTL}" delete "${kp_resource}" "${kp_name}" --namespace "${kp_ns}" \
        --ignore-not-found=true --wait=true >/dev/null
      "${KUBERPLOY_E2E_KUBECTL}" wait --for=delete "${kp_resource}/${kp_name}" \
        --namespace "${kp_ns}" --timeout=2m >/dev/null
    else
      "${KUBERPLOY_E2E_KUBECTL}" delete configmap "${kp_name}" --namespace "${kp_ns}" \
        --ignore-not-found=true --wait=true >/dev/null
      "${KUBERPLOY_E2E_KUBECTL}" wait --for=delete "configmap/${kp_name}" \
        --namespace "${kp_ns}" --timeout=2m >/dev/null
    fi
    kp_count=$((kp_count + 1))
  done <"${KUBERPLOY_E2E_STAGE_INVENTORY_FILE}"
  jq -n --arg run "${KUBERPLOY_E2E_RUN_ID}" --arg stage "${KUBERPLOY_E2E_STAGE_ID}" \
    --argjson count "${kp_count}" '
      {schemaVersion:1,runID:$run,stage:$stage,status:"cleaned",cleanedOrRestoredCount:$count,
       verifiedUIDAndOwnership:true,verifiedAbsentOrRestored:true}
    ' >"${KUBERPLOY_E2E_STAGE_CLEANUP_RESULT_FILE}"
}

case "${kp_action}" in
  run) kp_run ;;
  cleanup) kp_cleanup ;;
  *) kp_die "action must be run or cleanup" ;;
esac
