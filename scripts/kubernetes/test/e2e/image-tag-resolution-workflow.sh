#!/usr/bin/env bash

# Existing-image qualification is deliberately downstream of the self-contained
# source build: that build populates the managed/local registry without asking
# an operator for a third-party registry credential or image fixture.

kp_run_existing_image_tag_workflow() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence"
  local kp_state="${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"
  local kp_build="${KUBERPLOY_E2E_ARTIFACT_DIR}/40-source-build/evidence/workflow-build-terminal.json"
  local kp_application kp_environment kp_namespace kp_deployment kp_build_id kp_generation kp_commit
  local kp_immutable kp_repository kp_tag kp_body kp_actual kp_operation kp_revision kp_argo

  [[ -f "${kp_state}" && ! -L "${kp_state}" && -f "${kp_build}" && ! -L "${kp_build}" ]]
  kp_application="$(jq -er '.applicationId | select(test("^[a-f0-9-]{36}$"))' "${kp_state}")"
  kp_environment="$(jq -er '.directEnvironmentId | select(test("^[a-f0-9-]{36}$"))' "${kp_state}")"
  kp_namespace="$(jq -er '.directEnvironmentNamespace | select(test("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"))' "${kp_state}")"
  kp_deployment="$(jq -er '.directDeploymentId | select(test("^[a-f0-9-]{36}$"))' "${kp_state}")"
  kp_build_id="$(jq -er '.id | select(test("^[a-f0-9-]{36}$"))' "${kp_build}")"
  kp_generation="$(jq -er '.generation | select(type == "number" and floor == . and . >= 1)' "${kp_build}")"
  kp_commit="$(jq -er '.commitSha | select(test("^[a-f0-9]{40}$"))' "${kp_build}")"
  kp_immutable="$(jq -er '.image.reference | select(test("^[A-Za-z0-9][A-Za-z0-9._:/-]*@sha256:[a-f0-9]{64}$"))' "${kp_build}")"
  jq -e --arg image "${kp_immutable}" '.state == "succeeded" and .image.digest == ($image | split("@") | .[1])' \
    "${kp_build}" >/dev/null
  kp_repository="${kp_immutable%@*}"
  kp_tag="${kp_repository}:candidate-${kp_build_id//-/}-g${kp_generation}-${kp_commit:0:12}"
  [[ "${kp_tag}" =~ ^[A-Za-z0-9][A-Za-z0-9._:/-]*:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]]

  kp_body="$(jq -cn --arg environment "${kp_environment}" --arg application "${kp_application}" \
    --arg image "${kp_tag}" '{environmentId:$environment,applicationId:$application,image:$image}')"
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-image-tag-preview.json" \
    --write-out '%{http_code}' --request POST \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")" \
    --header 'Content-Type: application/json' --data-binary "${kp_body}" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/image-resolution-preview")"
  [[ "${kp_actual}" == "200" ]]
  jq -e --arg requested "${kp_tag}" --arg immutable "${kp_immutable}" '
    (keys | sort) == ["immutableImage","requestedImage","resolved"] and
    .requestedImage == $requested and .immutableImage == $immutable and .resolved == true
  ' "${kp_dir}/workflow-image-tag-preview.json" >/dev/null

  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-image-current-deployment.json" \
    --write-out '%{http_code}' --request GET --header "$(<"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment}")"
  [[ "${kp_actual}" == "200" ]]
  jq -e --arg id "${kp_deployment}" '.id == $id and (.runtime | type == "object")' \
    "${kp_dir}/workflow-image-current-deployment.json" >/dev/null
  kp_body="$(jq -cn --arg environment "${kp_environment}" --arg application "${kp_application}" \
    --arg image "${kp_tag}" --arg expected "${kp_immutable}" \
    --argjson runtime "$(jq '.runtime' "${kp_dir}/workflow-image-current-deployment.json")" \
    '{environmentId:$environment,applicationId:$application,image:$image,
      expectedImmutableImage:$expected,runtime:$runtime}')"
  kp_human_post existing-image-tag-deployment /v1/deployments "${kp_body}" 202 \
    "${kp_dir}/workflow-image-tag-operation.json"
  kp_operation="$(jq -er --arg target "${kp_deployment}" '
    select(.targetId == $target) | .id | select(test("^[a-f0-9-]{36}$"))
  ' "${kp_dir}/workflow-image-tag-operation.json")"
  kp_poll_operation "${kp_operation}" "${kp_dir}/workflow-image-tag-terminal.json"
  jq -e '.status == "succeeded" and (.generation | type == "number" and . >= 1) and
    (.gitRevision.commit | test("^[a-f0-9]{40}$"))' "${kp_dir}/workflow-image-tag-terminal.json" >/dev/null
  kp_revision="$(jq -er '.gitRevision.commit' "${kp_dir}/workflow-image-tag-terminal.json")"

  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-image-persisted-deployment.json" \
    --write-out '%{http_code}' --request GET --header "$(<"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment}")"
  [[ "${kp_actual}" == "200" ]]
  jq -e --arg id "${kp_deployment}" --arg immutable "${kp_immutable}" --arg tag "${kp_tag}" '
    .id == $id and .image == $immutable and (.image != $tag) and
    (.image | test("@sha256:[a-f0-9]{64}$"))
  ' "${kp_dir}/workflow-image-persisted-deployment.json" >/dev/null

  kp_argo="kp-d-${kp_deployment//-/}"
  for _ in {1..120}; do
    if "${KUBERPLOY_E2E_KUBECTL}" get application "${kp_argo}" --namespace argocd -o json \
        >"${kp_dir}/workflow-image-argo-application.json" 2>/dev/null &&
      jq -e --arg revision "${kp_revision}" '
        .status.sync.status == "Synced" and .status.health.status == "Healthy" and
        ([.status.sync.revision,.status.sync.revisions[]?] | index($revision) != null) and
        any(.status.resources[]; .kind == "Deployment" and .status == "Synced" and .health.status == "Healthy")
      ' "${kp_dir}/workflow-image-argo-application.json" >/dev/null; then break; fi
    sleep 5
  done
  jq -e '.status.sync.status == "Synced" and .status.health.status == "Healthy"' \
    "${kp_dir}/workflow-image-argo-application.json" >/dev/null || kp_die "tag-resolved Argo Application did not converge"
  "${KUBERPLOY_E2E_KUBECTL}" get deployments --namespace "${kp_namespace}" \
    --selector "kuberploy.io/application-id=${kp_application},kuberploy.io/deployment-id=${kp_deployment}" \
    -o json >"${kp_dir}/workflow-image-runtime-deployment.json"
  jq -e --arg immutable "${kp_immutable}" --arg tag "${kp_tag}" '
    [.items[] | select(any(.spec.template.spec.containers[]; .image == $immutable and .image != $tag) and
      .status.observedGeneration == .metadata.generation and .status.availableReplicas >= 1)] | length == 1
  ' "${kp_dir}/workflow-image-runtime-deployment.json" >/dev/null

  jq -n --arg requested "${kp_tag}" --arg immutable "${kp_immutable}" \
    --arg operation "${kp_operation}" --arg revision "${kp_revision}" \
    '{requestedTag:$requested,immutableImage:$immutable,operationId:$operation,gitRevision:$revision,
      previewResolved:true,persistedDigestOnly:true,argoSyncedHealthy:true,runtimeDigestOnly:true}' \
    >"${kp_dir}/workflow-image-tag-proof.json"
  chmod 600 "${kp_dir}/workflow-image-tag-proof.json"
}
