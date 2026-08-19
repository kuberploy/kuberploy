#!/usr/bin/env bash

set -Eeuo pipefail

# Render the least-privilege one-shot publisher Job from the live worker
# template.  Keep the worker resource request/limit: installer namespaces may
# enforce ResourceQuota on every Pod, including this bounded recovery probe.
kp_render_outbox_relay_job() {
  local kp_worker_file="${1:?worker snapshot required}" kp_job_name="${2:?job name required}"
  local kp_namespace_value="${3:?namespace required}" kp_run="${4:?run ID required}"
  local kp_managed="${5:?managed-by value required}" kp_output="${6:?output required}"

  jq --arg name "${kp_job_name}" --arg ns "${kp_namespace_value}" \
    --arg run "${kp_run}" --arg managed "${kp_managed}" '
    [.spec.template.spec.containers[] | select(.name=="worker")][0] as $worker |
    [$worker.env[] | select(.name=="KUBERPLOY_DATABASE_URL" or
      .name=="KUBERPLOY_VALKEY_ADDRESSES" or .name=="KUBERPLOY_VALKEY_USERNAME" or
      .name=="KUBERPLOY_VALKEY_PASSWORD" or .name=="KUBERPLOY_VALKEY_PUBLISHER_USERNAME" or
      .name=="KUBERPLOY_VALKEY_PUBLISHER_PASSWORD")] as $env |
    select(any($env[];.name=="KUBERPLOY_DATABASE_URL") and
      any($env[];.name=="KUBERPLOY_VALKEY_ADDRESSES") and
      any($env[];.name=="KUBERPLOY_VALKEY_PUBLISHER_USERNAME") and
      any($env[];.name=="KUBERPLOY_VALKEY_PUBLISHER_PASSWORD")) |
    {apiVersion:"batch/v1",kind:"Job",metadata:{name:$name,namespace:$ns,labels:{
      "kuberploy.io/test-run":$run,"app.kubernetes.io/managed-by":$managed,
      "app.kubernetes.io/name":"kuberploy","app.kubernetes.io/instance":.metadata.labels["app.kubernetes.io/instance"],
      "app.kubernetes.io/component":"worker"}},
     spec:{backoffLimit:0,ttlSecondsAfterFinished:3600,template:{metadata:{labels:{
       "kuberploy.io/test-run":$run,"app.kubernetes.io/managed-by":$managed,
       "app.kubernetes.io/name":"kuberploy","app.kubernetes.io/instance":.metadata.labels["app.kubernetes.io/instance"],
       "app.kubernetes.io/component":"worker"}},spec:{
       serviceAccountName:.spec.template.spec.serviceAccountName,automountServiceAccountToken:false,
       restartPolicy:"Never",securityContext:.spec.template.spec.securityContext,containers:[{
         name:"relay",image:$worker.image,imagePullPolicy:$worker.imagePullPolicy,
         command:["/kuberploy-worker","outbox-relay-once"],env:$env,
         resources:$worker.resources,securityContext:$worker.securityContext}]}}}}
  ' "${kp_worker_file}" >"${kp_output}"
}
