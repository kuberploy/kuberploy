#!/usr/bin/env bash

# Live source-build qualification beyond the basic signed-webhook happy path.
# This file is sourced by github-build-workflow.sh. It accepts only closed
# scenario identities and mode-0600 credential source files validated by
# lib.sh. Secret values are streamed to Kubernetes and never written to an
# evidence or temporary file.

kp_attest_source_build_poll_mode() {
  local kp_out="${KUBERPLOY_E2E_STAGE_DIR}/evidence/source-build-safety-poll-config.json"
  "${KUBERPLOY_E2E_KUBECTL}" get configmaps --all-namespaces \
    --selector "app.kubernetes.io/name=kuberploy,kuberploy.io/test-run=${KUBERPLOY_E2E_RUN_ID}" -o json |
    jq '
      [.items[] | select(.data.KUBERPLOY_GIT_PROJECTION_WEBHOOK_WAKE_ENABLED == "false" and
        (.data.KUBERPLOY_GIT_PROJECTION_POLL_INTERVAL_SECONDS | test("^[0-9]+$") and
          tonumber >= 15 and tonumber <= 60)) |
        {apiVersion,kind,metadata:{name:.metadata.name,namespace:.metadata.namespace,uid:.metadata.uid},
         webhookWakeEnabled:.data.KUBERPLOY_GIT_PROJECTION_WEBHOOK_WAKE_ENABLED,
         safetyPollIntervalSeconds:(.data.KUBERPLOY_GIT_PROJECTION_POLL_INTERVAL_SECONDS|tonumber)}] |
      select(length == 1) | .[0]
    ' >"${kp_out}"
  [[ -s "${kp_out}" ]] ||
    kp_die "qualification requires webhook wake disabled and a 15-60 second safety poll"
  chmod 600 "${kp_out}"
}

kp_create_source_build_credential_secret() {
  local kp_namespace_value="${1:?namespace required}" kp_name="${2:?name required}"
  local kp_username_file="${3:?username file required}" kp_password_file="${4:?password file required}"
  local kp_uid kp_safe_out
  [[ -z "$("${KUBERPLOY_E2E_KUBECTL}" get secret "${kp_name}" --namespace "${kp_namespace_value}" --ignore-not-found -o name)" ]] ||
    kp_die "source-build credential Secret ${kp_namespace_value}/${kp_name} already exists"
  kp_plan_create_inventory v1 Secret "${kp_namespace_value}" "${kp_name}"
  kp_uid="$(jq -n --rawfile username "${kp_username_file}" --rawfile password "${kp_password_file}" \
    --arg name "${kp_name}" --arg ns "${kp_namespace_value}" --arg run "${KUBERPLOY_E2E_RUN_ID}" \
    --arg managed "${KUBERPLOY_E2E_MANAGED_BY_LABEL_VALUE}" '
      ($username|rtrimstr("\n")) as $user | ($password|rtrimstr("\n")) as $pass |
      select(($user|length)>0 and ($pass|length)>0) |
      {apiVersion:"v1",kind:"Secret",metadata:{name:$name,namespace:$ns,labels:{
        "kuberploy.io/test-run":$run,"app.kubernetes.io/managed-by":$managed}},
       type:"Opaque",stringData:{username:$user,password:$pass}}
    ' | "${KUBERPLOY_E2E_KUBECTL}" create -f - -o 'jsonpath={.metadata.uid}')"
  [[ -n "${kp_uid}" ]]
  kp_finalize_create_inventory Secret "${kp_namespace_value}" "${kp_name}" "${kp_uid}"
  kp_safe_out="${KUBERPLOY_E2E_STAGE_DIR}/evidence/credential-${kp_name}.json"
  jq -n --arg uid "${kp_uid}" --arg ns "${kp_namespace_value}" --arg name "${kp_name}" \
    '{apiVersion:"v1",kind:"Secret",metadata:{uid:$uid,namespace:$ns,name:$name},
      keys:["password","username"],valuesPersisted:false}' >"${kp_safe_out}"
  chmod 600 "${kp_safe_out}"
}

kp_patch_source_build_credential_secret() {
  local kp_namespace_value="${1:?namespace required}" kp_name="${2:?name required}"
  local kp_username_file="${3:?username file required}" kp_password_file="${4:?password file required}"
  local kp_expected_uid="${5:?expected uid required}"
  jq -n --rawfile username "${kp_username_file}" --rawfile password "${kp_password_file}" \
    --arg uid "${kp_expected_uid}" --arg run "${KUBERPLOY_E2E_RUN_ID}" \
    --arg managed "${KUBERPLOY_E2E_MANAGED_BY_LABEL_VALUE}" '
    ($username|rtrimstr("\n")) as $user | ($password|rtrimstr("\n")) as $pass |
    select(($user|length)>0 and ($pass|length)>0) |
    [{op:"test",path:"/metadata/uid",value:$uid},
     {op:"test",path:"/metadata/labels/kuberploy.io~1test-run",value:$run},
     {op:"test",path:"/metadata/labels/app.kubernetes.io~1managed-by",value:$managed},
     {op:"replace",path:"/data/username",value:($user|@base64)},
     {op:"replace",path:"/data/password",value:($pass|@base64)}]
  ' | "${KUBERPLOY_E2E_KUBECTL}" patch secret "${kp_name}" --namespace "${kp_namespace_value}" \
    --type=json --patch-file=- -o name >/dev/null
}

kp_prepare_source_build_credentials() {
  local kp_namespace_value kp_push_name kp_cache_name
  kp_namespace_value="$(jq -er '.workflow.sourceBuild.credentials.namespace' "${kp_scenario}")"
  kp_push_name="$(jq -er '.workflow.sourceBuild.credentials.pushSecretName' "${kp_scenario}")"
  kp_cache_name="$(jq -er '.workflow.sourceBuild.credentials.cacheSecretName' "${kp_scenario}")"
  kp_attest_source_build_poll_mode
  [[ "${kp_push_name}" != "${kp_cache_name}" ]] || kp_die "push/cache credential Secrets must be distinct"
  kp_create_source_build_credential_secret "${kp_namespace_value}" "${kp_push_name}" \
    "${KUBERPLOY_E2E_REGISTRY_PUSH_USERNAME_FILE}" "${KUBERPLOY_E2E_REGISTRY_PUSH_PASSWORD_FILE}"
  kp_create_source_build_credential_secret "${kp_namespace_value}" "${kp_cache_name}" \
    "${KUBERPLOY_E2E_REGISTRY_CACHE_USERNAME_FILE}" "${KUBERPLOY_E2E_REGISTRY_CACHE_PASSWORD_FILE}"
}

kp_retry_source_build() {
  local kp_source="${1:?source build required}" kp_action="${2:?retry action required}"
  local kp_out="${3:?output required}" kp_id
  kp_human_post_empty "${kp_action}" "/v1/builds/${kp_source}/retry" 202 "${kp_out}"
  kp_id="$(jq -er --arg source "${kp_source}" '
    select(.id != $source and .state == "queued") | .id | select(test("^[a-f0-9-]{36}$"))
  ' "${kp_out}")"
  printf '%s\n' "${kp_id}"
}

kp_wait_source_build_cancellable_job() {
  local kp_build_id="${1:?build ID required}" kp_attempt_out="${2:?attempt output required}"
  local kp_job_out="${3:?job output required}" kp_safe_out="${4:?safe job output required}"
  local kp_actual kp_state kp_generation kp_operation
  kp_operation="${kp_build_id//-/}"
  [[ "${kp_operation}" =~ ^[a-f0-9]{32}$ ]]
  for _ in {1..120}; do
    kp_actual="$(curl --silent --show-error --output "${kp_attempt_out}" --write-out '%{http_code}' \
      --request GET --header "$(<"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")" \
      "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/builds/${kp_build_id}")"
    [[ "${kp_actual}" == "200" ]]
    kp_state="$(jq -er '.state' "${kp_attempt_out}")"
    case "${kp_state}" in
      preparing|running)
        kp_generation="$(jq -er '.generation | select(type == "number" and floor == . and . >= 1)' \
          "${kp_attempt_out}")"
        "${KUBERPLOY_E2E_KUBECTL}" get jobs.batch --all-namespaces \
          --selector "kuberploy.io/build-operation=${kp_operation},kuberploy.io/build-generation=${kp_generation}" \
          -o json >"${kp_job_out}"
        if jq -e --arg operation "${kp_operation}" --arg generation "${kp_generation}" '
          .items | length == 1 and .[0] as $job |
          ($job.metadata.namespace | type == "string" and length > 0) and
          ($job.metadata.name | type == "string" and length > 0) and
          ($job.metadata.uid | type == "string" and length > 0) and
          $job.metadata.labels["kuberploy.io/build-operation"] == $operation and
          $job.metadata.labels["kuberploy.io/build-generation"] == $generation and
          ($job.metadata.deletionTimestamp == null) and
          ([ $job.status.conditions[]? | select((.type == "Complete" or .type == "Failed") and .status == "True") ] | length == 0)
        ' "${kp_job_out}" >/dev/null; then
          jq '{apiVersion:.items[0].apiVersion,kind:.items[0].kind,
            metadata:{name:.items[0].metadata.name,namespace:.items[0].metadata.namespace,
              uid:.items[0].metadata.uid,labels:{
                "kuberploy.io/build-operation":.items[0].metadata.labels["kuberploy.io/build-operation"],
                "kuberploy.io/build-generation":.items[0].metadata.labels["kuberploy.io/build-generation"]}},
            terminalConditionObserved:false}' "${kp_job_out}" >"${kp_safe_out}"
          chmod 600 "${kp_safe_out}"
          return 0
        fi
        ;;
      succeeded|failed|cancelled) kp_die "cancellation candidate ${kp_build_id} became terminal before cancellation" ;;
      queued|cancelling) ;;
      *) kp_die "cancellation candidate ${kp_build_id} returned an unknown state" ;;
    esac
    sleep 1
  done
  kp_die "cancellation candidate ${kp_build_id} never exposed a nonterminal live Job"
}

kp_wait_source_build_cancelled_and_job_deleted() {
  local kp_build_id="${1:?build ID required}" kp_generation="${2:?generation required}"
  local kp_terminal_out="${3:?terminal output required}" kp_deleted_out="${4:?deletion output required}"
  local kp_actual kp_state kp_operation
  kp_operation="${kp_build_id//-/}"
  [[ "${kp_operation}" =~ ^[a-f0-9]{32}$ && "${kp_generation}" =~ ^[1-9][0-9]*$ ]]
  for _ in {1..120}; do
    kp_actual="$(curl --silent --show-error --output "${kp_terminal_out}" --write-out '%{http_code}' \
      --request GET --header "$(<"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")" \
      "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/builds/${kp_build_id}")"
    [[ "${kp_actual}" == "200" ]]
    kp_state="$(jq -er '.state' "${kp_terminal_out}")"
    case "${kp_state}" in
      cancelled) break ;;
      succeeded|failed) kp_die "cancelled build ${kp_build_id} reached unexpected state ${kp_state}" ;;
      queued|preparing|running|cancelling) ;;
      *) kp_die "cancelled build ${kp_build_id} returned an unknown state" ;;
    esac
    sleep 1
  done
  [[ "${kp_state}" == "cancelled" ]] || kp_die "build ${kp_build_id} did not reach cancelled"
  jq -e --arg id "${kp_build_id}" --argjson generation "${kp_generation}" '
    .id == $id and .generation == $generation and .state == "cancelled" and
    ((.failureCode // "") == "") and (.image == null)
  ' "${kp_terminal_out}" >/dev/null || kp_die "cancelled build terminal projection is invalid"
  for _ in {1..120}; do
    "${KUBERPLOY_E2E_KUBECTL}" get jobs.batch --all-namespaces \
      --selector "kuberploy.io/build-operation=${kp_operation},kuberploy.io/build-generation=${kp_generation}" \
      -o json >"${kp_deleted_out}"
    if jq -e '.items == []' "${kp_deleted_out}" >/dev/null; then
      jq -n --arg operation "${kp_operation}" --arg generation "${kp_generation}" \
        '{selector:{buildOperation:$operation,buildGeneration:$generation},remainingJobs:0,deleted:true}' \
        >"${kp_deleted_out}.safe"
      chmod 600 "${kp_deleted_out}.safe"
      mv -- "${kp_deleted_out}.safe" "${kp_deleted_out}"
      return 0
    fi
    sleep 1
  done
  kp_die "cancelled build Job still exists"
}

kp_wait_source_build_terminal_state() {
  local kp_build_id="${1:?build ID required}" kp_want="${2:?terminal state required}"
  local kp_out="${3:?output required}" kp_actual kp_state
  [[ "${kp_want}" == "succeeded" || "${kp_want}" == "failed" ]]
  for _ in {1..900}; do
    kp_actual="$(curl --silent --show-error --output "${kp_out}" --write-out '%{http_code}' \
      --request GET --header "$(<"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")" \
      "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/builds/${kp_build_id}")"
    [[ "${kp_actual}" == "200" ]]
    kp_state="$(jq -er '.state' "${kp_out}")"
    case "${kp_state}" in
      "${kp_want}") return 0 ;;
      succeeded|failed|cancelled) kp_die "build ${kp_build_id} reached unexpected state ${kp_state}" ;;
      queued|preparing|running|cancelling) ;;
      *) kp_die "build ${kp_build_id} returned an unknown state" ;;
    esac
    sleep 1
  done
  kp_die "build ${kp_build_id} did not reach ${kp_want}"
}

kp_wait_auto_deploy_submission() {
  local kp_policy="${1:?policy required}" kp_attempt="${2:?attempt required}"
  local kp_out="${3:?output required}" kp_actual
  for _ in {1..300}; do
    kp_actual="$(curl --silent --show-error --output "${kp_out}" --write-out '%{http_code}' \
      --request GET --header "$(<"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")" \
      "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/auto-deploy-policies/${kp_policy}/runs?limit=100")"
    [[ "${kp_actual}" == "200" ]]
    if jq -e --arg attempt "${kp_attempt}" '
      [.items[] | select(.attemptId == $attempt and .state == "submitted" and
        (.operationId|test("^[a-f0-9-]{36}$")) and (.deploymentId|test("^[a-f0-9-]{36}$")))] | length == 1
    ' "${kp_out}" >/dev/null; then return 0; fi
    sleep 1
  done
  kp_die "auto-deploy did not persist one submitted run receipt"
}

kp_assert_second_build_cache_hit() {
  local kp_build_id="${1:?build required}" kp_terminal="${2:?terminal evidence required}"
  local kp_logs="${3:?log evidence required}" kp_actual
  # BuildKit's raw progress is intentionally private: an untrusted Dockerfile
  # may print arbitrary process output, so the agent exposes the verified,
  # server-owned cache classification instead of forwarding `CACHED` lines.
  jq -e '.state == "succeeded" and .cacheReuse == "hit" and (.cacheReference|test("^.+:generation-[1-9][0-9]*$")) and
    ((.warnings == null) or (.warnings | type == "array" and length == 0))' "${kp_terminal}" >/dev/null ||
    kp_die "second build did not expose a verified cache hit"
  kp_actual="$(curl --silent --show-error --output "${kp_logs}" --write-out '%{http_code}' \
    --request GET --header "$(<"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/builds/${kp_build_id}/logs?follow=false&tailLines=2000&limitBytes=5242880")"
  [[ "${kp_actual}" == "200" ]]
  jq -e '
    .source as $source |
    ($source.id | test("^build_[a-f0-9]{32}$")) and
    (($source.ready | type) == "boolean") and $source.previous == false and
    (.lines | type == "array" and length > 0) and
    all(.lines[];
      .type == "line" and .source.id == $source.id and
      ((.source.ready | type) == "boolean") and .source.previous == false)
  ' "${kp_logs}" >/dev/null ||
    kp_die "second build did not expose a bounded verified build-log source"
}

kp_run_source_build_extended_workflow() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_state_file kp_application kp_definition kp_first
  local kp_project kp_environment kp_deployment kp_service_actor kp_policy kp_cancelled kp_second kp_cold kp_push_failed
  local kp_namespace_value kp_push_name kp_cache_name kp_push_uid kp_cache_uid kp_body kp_operation kp_auto_deployment
  local kp_cancel_generation kp_cancel_source kp_commit kp_cancel_commit kp_cancel_delivery
  kp_state_file="${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"
  kp_application="$(jq -er '.applicationId' "${kp_state_file}")"
  kp_project="$(jq -er '.projectId' "${kp_state_file}")"
  kp_environment="$(jq -er '.directEnvironmentId' "${kp_state_file}")"
  kp_deployment="$(jq -er '.directDeploymentId' "${kp_state_file}")"
  kp_definition="$(jq -er '.buildDefinitionId' "${kp_state_file}")"
  kp_first="$(jq -er '.successfulBuildId' "${kp_state_file}")"
  kp_commit="$(jq -er '.workflow.sourceBuild.push.afterCommit' "${kp_scenario}")"
  kp_service_actor="$(jq -er '.serviceAccountId' "${KUBERPLOY_E2E_ARTIFACT_DIR}/20-postgresql-valkey/evidence/access-proof.json")"
  kp_namespace_value="$(jq -er '.workflow.sourceBuild.credentials.namespace' "${kp_scenario}")"
  kp_push_name="$(jq -er '.workflow.sourceBuild.credentials.pushSecretName' "${kp_scenario}")"
  kp_cache_name="$(jq -er '.workflow.sourceBuild.credentials.cacheSecretName' "${kp_scenario}")"
  kp_push_uid="$(jq -er '.metadata.uid' "${kp_dir}/credential-${kp_push_name}.json")"
  kp_cache_uid="$(jq -er '.metadata.uid' "${kp_dir}/credential-${kp_cache_name}.json")"

  kp_body="$(jq -cn --arg definition "${kp_definition}" --arg environment "${kp_environment}" \
    --arg deployment "${kp_deployment}" --arg actor "${kp_service_actor}" \
    '{buildDefinitionId:$definition,environmentId:$environment,templateDeploymentId:$deployment,
      serviceActorId:$actor,enabled:true}')"
  kp_human_post create-auto-deploy-policy "/v1/applications/${kp_application}/auto-deploy-policies" \
    "${kp_body}" 201 "${kp_dir}/workflow-auto-deploy-policy.json"
  kp_policy="$(jq -er --arg definition "${kp_definition}" --arg project "${kp_project}" \
    --arg application "${kp_application}" --arg environment "${kp_environment}" '
    select(.buildDefinitionId==$definition and .projectId==$project and .applicationId==$application and
      .environmentId==$environment and .currentRevision==1 and .current.enabled==true) |
      .id | select(test("^[a-f0-9-]{36}$"))' "${kp_dir}/workflow-auto-deploy-policy.json")"

  # A successful attempt is the promotion source, not a retry source. The
  # scenario supplies one distinct commit for the cancellation lane. Every
  # fault lane then retries that same cancelled source; webhook deliveries do
  # not create duplicate attempts for one (definition, commit, ref) source.
  kp_cancel_commit="$(jq -er '.workflow.sourceBuild.cancellationPush.afterCommit' "${kp_scenario}")"
  kp_cancel_delivery="$(jq -er '.workflow.sourceBuild.cancellationPush.deliveryId' "${kp_scenario}")"
  jq --arg after "${kp_cancel_commit}" '.after = $after' "${kp_dir}/workflow-github-push.json" \
    >"${kp_dir}/workflow-github-cancellation-push.json"
  chmod 600 "${kp_dir}/workflow-github-cancellation-push.json"
  kp_post_github_push valid "${kp_cancel_delivery}" "${kp_dir}/workflow-github-cancellation-push.json" \
    "${kp_dir}/workflow-github-cancellation-accepted.json"
  kp_wait_build_for_commit "${kp_application}" "${kp_cancel_commit}" \
    "${kp_dir}/workflow-builds-after-cancellation-push.json"
  kp_cancel_source="$(jq -er --arg commit "${kp_cancel_commit}" '
    [.items[] | select(.commitSha == $commit)] | select(length == 1) | .[0].id
  ' "${kp_dir}/workflow-builds-after-cancellation-push.json")"
  kp_cancelled="${kp_cancel_source}"
  kp_wait_source_build_cancellable_job "${kp_cancel_source}" \
    "${kp_dir}/workflow-cancellable-running.json" "${kp_dir}/workflow-cancellable-live-job.raw.json" \
    "${kp_dir}/workflow-cancellable-live-job.json"
  rm -- "${kp_dir}/workflow-cancellable-live-job.raw.json"
  kp_cancel_generation="$(jq -er '.generation' "${kp_dir}/workflow-cancellable-running.json")"
  kp_human_post_empty cancel-live-build "/v1/builds/${kp_cancel_source}/cancel" 202 \
    "${kp_dir}/workflow-cancel-accepted.json"
  jq -e --arg id "${kp_cancel_source}" --argjson generation "${kp_cancel_generation}" '
    .id == $id and .generation == $generation and .state == "cancelling"
  ' "${kp_dir}/workflow-cancel-accepted.json" >/dev/null || kp_die "live build cancellation was not accepted"
  kp_wait_source_build_cancelled_and_job_deleted "${kp_cancel_source}" "${kp_cancel_generation}" \
    "${kp_dir}/workflow-cancel-terminal.json" "${kp_dir}/workflow-cancel-job-deleted.json"

  kp_second="$(kp_retry_source_build "${kp_cancel_source}" retry-cancelled-build "${kp_dir}/workflow-cache-hit-retry.json")"
  kp_wait_source_build_terminal_state "${kp_second}" succeeded "${kp_dir}/workflow-cache-hit-terminal.json"
  kp_assert_second_build_cache_hit "${kp_second}" "${kp_dir}/workflow-cache-hit-terminal.json" \
    "${kp_dir}/workflow-cache-hit-logs.json"
  kp_wait_auto_deploy_submission "${kp_policy}" "${kp_second}" "${kp_dir}/workflow-auto-deploy-runs.json"
  kp_operation="$(jq -er --arg attempt "${kp_second}" '.items[] | select(.attemptId==$attempt) | .operationId' \
    "${kp_dir}/workflow-auto-deploy-runs.json")"
  kp_auto_deployment="$(jq -er --arg attempt "${kp_second}" '.items[] | select(.attemptId==$attempt) | .deploymentId' \
    "${kp_dir}/workflow-auto-deploy-runs.json")"
  kp_poll_operation "${kp_operation}" "${kp_dir}/workflow-auto-deploy-operation-terminal.json"
  jq -e '.status=="succeeded"' "${kp_dir}/workflow-auto-deploy-operation-terminal.json" >/dev/null

  kp_patch_source_build_credential_secret "${kp_namespace_value}" "${kp_cache_name}" \
    "${KUBERPLOY_E2E_REGISTRY_CACHE_USERNAME_FILE}" "${KUBERPLOY_E2E_REGISTRY_FAULT_PASSWORD_FILE}" "${kp_cache_uid}"
  kp_cold="$(kp_retry_source_build "${kp_cancel_source}" cache-fault-build "${kp_dir}/workflow-cache-fault-retry.json")"
  kp_wait_source_build_terminal_state "${kp_cold}" succeeded "${kp_dir}/workflow-cache-fault-terminal.json"
  kp_patch_source_build_credential_secret "${kp_namespace_value}" "${kp_cache_name}" \
    "${KUBERPLOY_E2E_REGISTRY_CACHE_USERNAME_FILE}" "${KUBERPLOY_E2E_REGISTRY_CACHE_PASSWORD_FILE}" "${kp_cache_uid}"
  jq -e '.state=="succeeded" and (.image.reference|test("@sha256:[a-f0-9]{64}$")) and
    .image.digest == (.image.reference | split("@") | .[1]) and
    (.warnings|sort)==["CacheDegraded","ColdBuild"]' "${kp_dir}/workflow-cache-fault-terminal.json" >/dev/null ||
    kp_die "cache-only credential failure did not cold-degrade while preserving release push"

  kp_patch_source_build_credential_secret "${kp_namespace_value}" "${kp_push_name}" \
    "${KUBERPLOY_E2E_REGISTRY_PUSH_USERNAME_FILE}" "${KUBERPLOY_E2E_REGISTRY_FAULT_PASSWORD_FILE}" "${kp_push_uid}"
  kp_push_failed="$(kp_retry_source_build "${kp_cancel_source}" push-fault-build "${kp_dir}/workflow-push-fault-retry.json")"
  kp_wait_source_build_terminal_state "${kp_push_failed}" failed "${kp_dir}/workflow-push-fault-terminal.json"
  kp_patch_source_build_credential_secret "${kp_namespace_value}" "${kp_push_name}" \
    "${KUBERPLOY_E2E_REGISTRY_PUSH_USERNAME_FILE}" "${KUBERPLOY_E2E_REGISTRY_PUSH_PASSWORD_FILE}" "${kp_push_uid}"
  jq -e '.state=="failed" and (.failureCode|type=="string" and length>0) and (.image==null)' \
    "${kp_dir}/workflow-push-fault-terminal.json" >/dev/null ||
    kp_die "release-push credential failure was not terminal"

  local kp_evidence kp_source
  for kp_evidence in "${kp_dir}"/*; do
    [[ -f "${kp_evidence}" ]] || continue
    for kp_source in \
      "${KUBERPLOY_E2E_REGISTRY_PUSH_USERNAME_FILE}" "${KUBERPLOY_E2E_REGISTRY_PUSH_PASSWORD_FILE}" \
      "${KUBERPLOY_E2E_REGISTRY_CACHE_USERNAME_FILE}" "${KUBERPLOY_E2E_REGISTRY_CACHE_PASSWORD_FILE}" \
      "${KUBERPLOY_E2E_REGISTRY_FAULT_PASSWORD_FILE}"; do
      ! grep -F -f "${kp_source}" "${kp_evidence}" >/dev/null ||
        kp_die "registry credential material appeared in source-build qualification evidence"
    done
  done

  jq --arg policy "${kp_policy}" --arg cancelled "${kp_cancelled}" --arg second "${kp_second}" --arg cold "${kp_cold}" \
    --arg failed "${kp_push_failed}" --arg operation "${kp_operation}" --arg deployment "${kp_auto_deployment}" \
    '. + {autoDeployPolicyId:$policy,cancelledBuildId:$cancelled,cancelRetryBuildId:$second,
      cacheHitBuildId:$second,cacheDegradedBuildId:$cold,
      pushFailureBuildId:$failed,autoDeployOperationId:$operation,autoDeployDeploymentId:$deployment}' "${kp_state_file}" >"${kp_state_file}.tmp"
  chmod 600 "${kp_state_file}.tmp"; mv -- "${kp_state_file}.tmp" "${kp_state_file}"
}
