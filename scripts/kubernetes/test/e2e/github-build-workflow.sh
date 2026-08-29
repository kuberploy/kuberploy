#!/usr/bin/env bash

# Repository-owned GitHub/source-build qualification primitives. This file is
# sourced by builtin-driver.sh; it accepts identities and payload field values,
# never caller-supplied commands.

source "$(dirname "${BASH_SOURCE[0]}")/source-build-extended-workflow.sh"

kp_github_webhook_signature() {
  local kp_body_file="${1:?body file required}" kp_secret
  kp_secret="$(<"${KUBERPLOY_E2E_GITHUB_WEBHOOK_SECRET_FILE}")"
  [[ -n "${kp_secret}" && "${kp_secret}" != *$'\n'* && "${kp_secret}" != *$'\r'* ]]
  openssl dgst -sha256 -hmac "${kp_secret}" "${kp_body_file}" |
    awk '{print "sha256=" $NF}'
  unset kp_secret
}

kp_post_github_push() {
  local kp_mode="${1:?mode required}" kp_delivery="${2:?delivery required}"
  local kp_body_file="${3:?body file required}" kp_out="${4:?output required}"
  local kp_signature kp_actual
  case "${kp_mode}" in
    valid) kp_signature="$(kp_github_webhook_signature "${kp_body_file}")" ;;
    invalid) kp_signature="sha256=$(printf '0%.0s' {1..64})" ;;
    *) kp_die "unsupported fixed GitHub webhook mode" ;;
  esac
  kp_actual="$(curl --silent --show-error --output "${kp_out}" --write-out '%{http_code}' \
    --request POST --header "X-Hub-Signature-256: ${kp_signature}" \
    --header 'X-GitHub-Event: push' --header "X-GitHub-Delivery: ${kp_delivery}" \
    --header 'Content-Type: application/json' --data-binary "@${kp_body_file}" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/webhooks/github")"
  if [[ "${kp_mode}" == "valid" ]]; then
    [[ "${kp_actual}" == "202" ]] && jq -e '. == {accepted:true}' "${kp_out}" >/dev/null
  else
    [[ "${kp_actual}" == "401" ]] && jq -e '.status == 401' "${kp_out}" >/dev/null
  fi
}

kp_wait_build_for_commit() {
  local kp_application="${1:?application required}" kp_commit="${2:?commit required}"
  local kp_out="${3:?output required}" kp_actual
  for _ in {1..120}; do
    kp_actual="$(curl --silent --show-error --output "${kp_out}" --write-out '%{http_code}' \
      --request GET --header "$(<"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")" \
      "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/applications/${kp_application}/builds")"
    [[ "${kp_actual}" == "200" ]]
    if jq -e --arg commit "${kp_commit}" '
      [.items[] | select(.commitSha == $commit)] | length == 1
    ' "${kp_out}" >/dev/null; then return 0; fi
    sleep 5
  done
  return 1
}

kp_inspect_live_build_job() {
  local kp_attempt_file="${1:?attempt file required}" kp_out="${2:?output required}"
  local kp_attempt kp_generation kp_operation
  kp_attempt="$(jq -er '.id' "${kp_attempt_file}")"
  kp_generation="$(jq -er '.generation | select(type == "number" and . >= 1)' "${kp_attempt_file}")"
  kp_operation="${kp_attempt//-/}"
  for _ in {1..60}; do
    "${KUBERPLOY_E2E_KUBECTL}" get jobs.batch --all-namespaces \
      --selector "kuberploy.io/build-operation=${kp_operation},kuberploy.io/build-generation=${kp_generation}" \
      -o json >"${kp_out}"
    if jq -e --argjson pool "$(jq '.workflow.sourceBuild.builderPool.nodeSelector' "${kp_scenario}")" \
      --argjson isolated "$(jq '.workflow.sourceBuild.builderPool.nodeIsolation' "${kp_scenario}")" \
      --arg expectedPush "$(jq -r '.workflow.sourceBuild.credentials.pushSecretName' "${kp_scenario}")" \
      --arg expectedCache "$(jq -r '.workflow.sourceBuild.credentials.cacheSecretName' "${kp_scenario}")" '
    .items | length == 1 and .[0] as $job |
    ($job.spec.template.spec.volumes | map(select(.name == "registry-push-credentials"))[0].secret.secretName) as $push |
    ($job.spec.template.spec.volumes | map(select(.name == "registry-cache-credentials"))[0].secret.secretName) as $cache |
    $push == $expectedPush and $cache == $expectedCache and $push != $cache and
    (($job.spec.template.spec.hostNetwork // false) == false) and
    (($job.spec.template.spec.hostPID // false) == false) and
    (($job.spec.template.spec.hostIPC // false) == false) and
    ([ $job.spec.template.spec.volumes[] | select(has("hostPath")) ] | length == 0) and
    ([ $job.spec.template.spec.initContainers[], $job.spec.template.spec.containers[] |
      .volumeMounts[]? | select(.mountPath == "/var/run/docker.sock") ] | length == 0) and
    (if $isolated then $job.spec.template.spec.nodeSelector == $pool
      else (($job.spec.template.spec | has("nodeSelector")) == false and
        (($job.spec.template.spec.tolerations // []) | length == 0)) end) and
    any($job.spec.template.spec.initContainers[]; .name == "dind" and .securityContext.privileged == true) and
    any($job.spec.template.spec.initContainers[]; .name == "checkout" and
      ([.volumeMounts[].name] | index("registry-push-credentials") == null and index("registry-cache-credentials") == null)) and
    any($job.spec.template.spec.containers[]; .name == "agent" and
      ([.volumeMounts[] | select(.name == "registry-push-credentials" and .mountPath == "/var/run/secrets/kuberploy/registry-push" and .readOnly == true)] | length == 1) and
      ([.volumeMounts[] | select(.name == "registry-cache-credentials" and .mountPath == "/var/run/secrets/kuberploy/registry-cache" and .readOnly == true)] | length == 1))
    ' "${kp_out}" >/dev/null; then return 0; fi
    sleep 2
  done
  kp_die "live build Job does not satisfy the configured DinD scheduling and credential split contract"
}

kp_run_github_build_workflow() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_state_file kp_application
  local kp_installation kp_repository kp_commit kp_delivery kp_body kp_definition kp_build
  local kp_actual
  kp_state_file="${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"
  kp_application="$(jq -er '.applicationId' "${kp_state_file}")"
  kp_installation="$(jq -er '.workflow.sourceBuild.github.installationId' "${kp_scenario}")"
  kp_repository="$(jq -er '.workflow.sourceBuild.github.repositoryId' "${kp_scenario}")"
  kp_commit="$(jq -er '.workflow.sourceBuild.push.afterCommit' "${kp_scenario}")"
  kp_delivery="$(jq -er '.workflow.sourceBuild.push.deliveryId' "${kp_scenario}")"

  kp_prepare_source_build_credentials

  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-github-installations.json" \
    --write-out '%{http_code}' --request GET \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/github/installations")"
  [[ "${kp_actual}" == "200" ]]
  jq -e --arg id "${kp_installation}" --argjson provider "$(jq '.workflow.sourceBuild.github.githubInstallationId' "${kp_scenario}")" '
    [.items[] | select(.id == $id and .githubInstallationId == $provider)] | length == 1
  ' "${kp_dir}/workflow-github-installations.json" >/dev/null || kp_die "exact prelinked GitHub installation is unavailable"
  kp_actual="$(curl --silent --show-error --output "${kp_dir}/workflow-github-repositories.json" \
    --write-out '%{http_code}' --request GET \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/github/installations/${kp_installation}/repositories")"
  [[ "${kp_actual}" == "200" ]]
  jq -e --arg id "${kp_repository}" --arg installation "${kp_installation}" \
    --argjson provider "$(jq '.workflow.sourceBuild.github.githubRepositoryId' "${kp_scenario}")" '
    [.items[] | select(.id == $id and .installationId == $installation and
      .githubRepositoryId == $provider and .lifecycle == "active")] | length == 1
  ' "${kp_dir}/workflow-github-repositories.json" >/dev/null || kp_die "exact active GitHub repository is unavailable"

  kp_body="$(jq -c '.workflow.sourceBuild.definition' "${kp_scenario}")"
	kp_human_put save-app-source "/v1/applications/${kp_application}/source" \
		"${kp_body}" 200 "${kp_dir}/workflow-build-definition.json"
  kp_definition="$(jq -er --arg application "${kp_application}" --arg installation "${kp_installation}" \
    --arg repository "${kp_repository}" '
    select(.applicationId == $application and .installationId == $installation and
      .repositoryId == $repository) | .id | select(test("^[a-f0-9-]{36}$"))
  ' "${kp_dir}/workflow-build-definition.json")"

  jq -n --arg ref "$(jq -r '.workflow.sourceBuild.definition.triggerRef' "${kp_scenario}")" \
    --arg after "${kp_commit}" \
    --argjson installation "$(jq '.workflow.sourceBuild.github.githubInstallationId' "${kp_scenario}")" \
    --argjson repository "$(jq '.workflow.sourceBuild.github.githubRepositoryId' "${kp_scenario}")" \
    --argjson owner "$(jq '.workflow.sourceBuild.github.ownerId' "${kp_scenario}")" \
    --arg ownerLogin "$(jq -r '.workflow.sourceBuild.github.ownerLogin' "${kp_scenario}")" \
    --arg repositoryName "$(jq -r '.workflow.sourceBuild.github.repositoryName' "${kp_scenario}")" \
    --argjson sender "$(jq '.workflow.sourceBuild.github.senderId' "${kp_scenario}")" \
    --arg senderLogin "$(jq -r '.workflow.sourceBuild.github.senderLogin' "${kp_scenario}")" '
    {ref:$ref,after:$after,created:false,deleted:false,forced:false,
     repository:{id:$repository,name:$repositoryName,full_name:($ownerLogin+"/"+$repositoryName),
       owner:{id:$owner,login:$ownerLogin,type:"Organization"}},
     installation:{id:$installation},sender:{id:$sender,login:$senderLogin,type:"User"}}
  ' >"${kp_dir}/workflow-github-push.json"
  chmod 600 "${kp_dir}/workflow-github-push.json"
  kp_post_github_push invalid "${kp_delivery}-invalid" "${kp_dir}/workflow-github-push.json" \
    "${kp_dir}/workflow-github-invalid.json"
  kp_post_github_push valid "${kp_delivery}" "${kp_dir}/workflow-github-push.json" \
    "${kp_dir}/workflow-github-valid.json"
  kp_wait_build_for_commit "${kp_application}" "${kp_commit}" "${kp_dir}/workflow-builds-after-push.json"
  jq -e --arg commit "${kp_commit}" --arg definition "${kp_definition}" '
    [.items[] | select(.commitSha == $commit and .sourceId == $definition)] | length == 1
  ' "${kp_dir}/workflow-builds-after-push.json" >/dev/null
  kp_build="$(jq -er --arg commit "${kp_commit}" '.items[] | select(.commitSha == $commit) | .id' \
    "${kp_dir}/workflow-builds-after-push.json")"
  kp_post_github_push valid "${kp_delivery}" "${kp_dir}/workflow-github-push.json" \
    "${kp_dir}/workflow-github-duplicate.json"
  kp_wait_build_for_commit "${kp_application}" "${kp_commit}" "${kp_dir}/workflow-builds-after-duplicate.json"
  jq -e --arg commit "${kp_commit}" '[.items[] | select(.commitSha == $commit)] | length == 1' \
    "${kp_dir}/workflow-builds-after-duplicate.json" >/dev/null || kp_die "duplicate GitHub delivery created another build"
  jq -c --arg commit "${kp_commit}" '.items[] | select(.commitSha == $commit)' \
    "${kp_dir}/workflow-builds-after-push.json" >"${kp_dir}/workflow-build-attempt.json"
  kp_inspect_live_build_job "${kp_dir}/workflow-build-attempt.json" \
    "${kp_dir}/workflow-live-build-job.json"
  kp_poll_build "${kp_build}" "${kp_dir}/workflow-build-terminal.json"
  jq -e --arg definition "${kp_definition}" '.state == "succeeded" and .sourceId == $definition and
    (.image.reference | test("@sha256:[a-f0-9]{64}$")) and
    .image.digest == (.image.reference | split("@") | .[1])' \
    "${kp_dir}/workflow-build-terminal.json" >/dev/null
  jq --arg build "${kp_build}" --arg definition "${kp_definition}" \
    '. + {successfulBuildId:$build,buildDefinitionId:$definition}' "${kp_state_file}" >"${kp_state_file}.tmp"
  chmod 600 "${kp_state_file}.tmp"; mv -- "${kp_state_file}.tmp" "${kp_state_file}"
  kp_run_source_build_extended_workflow
}
