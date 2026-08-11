#!/usr/bin/env bash

set -Eeuo pipefail
umask 077

kp_obs_die() {
  printf 'observability qualification: %s\n' "$*" >&2
  exit 1
}

kp_obs_require_file() {
  local kp_path="${1:?path required}"
  [[ "${kp_path}" == /* && -f "${kp_path}" && ! -L "${kp_path}" ]] || \
    kp_obs_die "required input is not an absolute regular file: ${kp_path}"
}

kp_obs_get() {
  local kp_path="${1:?path required}" kp_header_file="${2:?header required}"
  local kp_expected="${3:?status required}" kp_output="${4:?output required}" kp_actual
  kp_actual="$(curl --silent --show-error --output "${kp_output}" --write-out '%{http_code}' \
    --request GET --header "$(<"${kp_header_file}")" "${KUBERPLOY_E2E_API_BASE_URL}${kp_path}")"
  [[ "${kp_actual}" == "${kp_expected}" ]] || \
    kp_obs_die "GET ${kp_path} returned ${kp_actual}, expected ${kp_expected}"
}

kp_obs_assert_problem() {
  local kp_path="${1:?problem required}" kp_status="${2:?status required}"
  jq -e --argjson status "${kp_status}" '
    .status == $status and (.code | type == "string" and length > 0) and
    (.title | type == "string" and length > 0) and
    ((keys | index("namespace")) == null) and ((keys | index("provider")) == null)
  ' "${kp_path}" >/dev/null || kp_obs_die "unsafe or malformed denial response"
}

kp_obs_query_metric() {
  local kp_scope_type="${1:?scope type required}" kp_scope_id="${2:?scope id required}"
  local kp_metric="${3:?metric required}" kp_header="${4:?header required}"
  local kp_output="${5:?output required}" kp_expected="${6:-200}" kp_path
  kp_path="/v1/metrics/query-range?scopeType=${kp_scope_type}&scopeId=${kp_scope_id}&metric=${kp_metric}&from=${kp_obs_from}&to=${kp_obs_to}&step=30s"
  kp_obs_get "${kp_path}" "${kp_header}" "${kp_expected}" "${kp_output}"
}

kp_obs_safe_evidence() {
  local kp_path="${1:?evidence required}"
  # Product responses must already be redacted. Keep a second, deliberately
  # conservative gate on persisted qualification artifacts.
  if LC_ALL=C grep -Eia '(authorization[[:space:]]*[:=]|bearer[[:space:]]+[A-Za-z0-9._-]{8,}|password[[:space:]]*[:=]|token[[:space:]]*[:=]|-----BEGIN .*PRIVATE KEY-----|gh[pousr]_[A-Za-z0-9]{20,}|kp_sa_[A-Za-z0-9_-]{20,})' "${kp_path}" >/dev/null; then
    kp_obs_die "credential-shaped material reached persisted observability evidence"
  fi
}

: "${KUBERPLOY_E2E_API_BASE_URL:?required}"
: "${KUBERPLOY_E2E_OBSERVABILITY_EVIDENCE_DIR:?required}"
: "${KUBERPLOY_E2E_WORKFLOW_STATE_FILE:?required}"
: "${KUBERPLOY_E2E_API_AUTH_HEADER_FILE:?required}"
: "${KUBERPLOY_E2E_DENIED_AUTH_HEADER_FILE:?required}"
: "${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE:?required}"
: "${KUBERPLOY_E2E_KUBECTL:?required}"
: "${KUBERPLOY_E2E_RUN_ID:?required}"

[[ "${KUBERPLOY_E2E_API_BASE_URL}" =~ ^https://[^[:space:]@/?#]+(:[0-9]+)?$ ]] || \
  kp_obs_die "API base URL must be one canonical HTTPS origin"
[[ "${KUBERPLOY_E2E_RUN_ID}" =~ ^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$ ]] || \
  kp_obs_die "run ID is not a safe Kubernetes name segment"
for kp_obs_input in "${KUBERPLOY_E2E_WORKFLOW_STATE_FILE}" \
  "${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}" "${KUBERPLOY_E2E_DENIED_AUTH_HEADER_FILE}" \
  "${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}"; do
  kp_obs_require_file "${kp_obs_input}"
done
[[ "${KUBERPLOY_E2E_OBSERVABILITY_EVIDENCE_DIR}" == /* ]] || \
  kp_obs_die "evidence directory must be absolute"
mkdir -p -- "${KUBERPLOY_E2E_OBSERVABILITY_EVIDENCE_DIR}"
chmod 700 "${KUBERPLOY_E2E_OBSERVABILITY_EVIDENCE_DIR}"

kp_obs_state="${KUBERPLOY_E2E_WORKFLOW_STATE_FILE}"
kp_obs_dir="${KUBERPLOY_E2E_OBSERVABILITY_EVIDENCE_DIR}"
kp_obs_deployment_id="$(jq -er '.directDeploymentId | select(test("^[a-f0-9-]{36}$"))' "${kp_obs_state}")"
kp_obs_environment_id="$(jq -er '.directEnvironmentId | select(test("^[a-f0-9-]{36}$"))' "${kp_obs_state}")"
kp_obs_namespace="$(jq -er '.directEnvironmentNamespace | select(test("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"))' "${kp_obs_state}")"
kp_obs_application_id="$(jq -er '.applicationId | select(test("^[a-f0-9-]{36}$"))' "${kp_obs_state}")"
kp_obs_runtime_name="kp-a-$(printf '%s' "${kp_obs_application_id}" | shasum -a 256 | awk '{print substr($1,1,16)}')"

# This object is created only to force one safe, non-empty runtime Event through
# the product's exact UID allowlist. It is deleted before this driver exits.
kp_obs_event_name="kp-observe-${KUBERPLOY_E2E_RUN_ID}"
kp_obs_event_file="${kp_obs_dir}/event-create.json"
kp_obs_cleanup() {
  "${KUBERPLOY_E2E_KUBECTL}" delete event "${kp_obs_event_name}" --namespace "${kp_obs_namespace}" \
    --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
}
trap kp_obs_cleanup EXIT

"${KUBERPLOY_E2E_KUBECTL}" get deployment "${kp_obs_runtime_name}" --namespace "${kp_obs_namespace}" \
  -o json | jq '{name:.metadata.name,namespace:.metadata.namespace,uid:.metadata.uid,
    generation:.metadata.generation,observedGeneration:.status.observedGeneration,
    desiredReplicas:.spec.replicas,availableReplicas:(.status.availableReplicas // 0)}' \
  >"${kp_obs_dir}/runtime-deployment.json"
kp_obs_deployment_uid="$(jq -er '.uid | select(test("^[A-Za-z0-9-]{1,128}$"))' "${kp_obs_dir}/runtime-deployment.json")"
jq -n --arg name "${kp_obs_event_name}" --arg namespace "${kp_obs_namespace}" \
  --arg uid "${kp_obs_deployment_uid}" --arg object "${kp_obs_runtime_name}" \
  --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  '{apiVersion:"v1",kind:"Event",metadata:{name:$name,namespace:$namespace},
    involvedObject:{apiVersion:"apps/v1",kind:"Deployment",name:$object,namespace:$namespace,uid:$uid},
    reason:"QualificationObserved",message:"bounded observability qualification event",
    source:{component:"kuberploy-qualification"},type:"Normal",firstTimestamp:$now,lastTimestamp:$now,count:1}' \
  >"${kp_obs_event_file}"
"${KUBERPLOY_E2E_KUBECTL}" create -f "${kp_obs_event_file}" >/dev/null

kp_obs_get "/v1/monitoring/status" "${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}" 200 \
  "${kp_obs_dir}/monitoring-status.json"
jq -e '
  (keys|sort)==["available","message","mode","observedAt","status"] and
  (.mode == "managed" or .mode == "existing") and .status == "available" and
  .available == true and (.observedAt | fromdateiso8601 > 0)
' "${kp_obs_dir}/monitoring-status.json" >/dev/null || kp_obs_die "monitoring readiness is not live"
kp_obs_mode="$(jq -r '.mode' "${kp_obs_dir}/monitoring-status.json")"

if [[ "${kp_obs_mode}" == "managed" ]]; then
  "${KUBERPLOY_E2E_KUBECTL}" get configmap monitoring-monitoring-profile \
    --namespace kuberploy-monitoring -o json | \
    jq '{immutable,data:{contract:.data.contract,readinessContract:.data.readinessContract,
      metricSeries:.data.metricSeries,operatorArgumentsSHA256:.data.operatorArgumentsSHA256,
      recordingRuleSpecSHA256:.data.recordingRuleSpecSHA256}}' >"${kp_obs_dir}/managed-profile.json"
  "${KUBERPLOY_E2E_KUBECTL}" get deployment kuberploy-prometheus-operator \
    --namespace kuberploy-monitoring -o json | jq '{generation:.metadata.generation,
      observedGeneration:.status.observedGeneration,desiredReplicas:.spec.replicas,
      availableReplicas:(.status.availableReplicas // 0),containers:[.spec.template.spec.containers[]|{name,image}]}' \
    >"${kp_obs_dir}/managed-operator.json"
  "${KUBERPLOY_E2E_KUBECTL}" get prometheusrule monitoring-service-recording-rules \
    --namespace kuberploy-monitoring -o json | jq '{generation:.metadata.generation,
      groups:[.spec.groups[]|{name,records:[.rules[].record]}]}' >"${kp_obs_dir}/managed-rules.json"
  jq -e '
    .immutable == true and .data.contract == "kuberploy-managed-monitoring/v1" and
    .data.readinessContract == "profile+operator+rule-spec+kube-state-monitor+prometheus-rules" and
    .data.metricSeries == "kuberploy:service:cpu_usage_cores,kuberploy:service:memory_working_set_bytes,kuberploy:service:replicas_ready,kuberploy:service:container_restarts_total,kuberploy:service:http_requests_per_second,kuberploy:service:http_5xx_ratio,kuberploy:service:http_latency_seconds:p95"
  ' "${kp_obs_dir}/managed-profile.json" >/dev/null || kp_obs_die "managed profile identity drifted"
  jq -e '
    .generation == .observedGeneration and .desiredReplicas == 1 and
    .availableReplicas == 1 and
    ([.containers[] | select(.name == "kube-prometheus-stack" and
      .image == "quay.io/prometheus-operator/prometheus-operator:v0.93.0")] | length) == 1
  ' "${kp_obs_dir}/managed-operator.json" >/dev/null || kp_obs_die "managed operator identity is not ready"
  jq -e '
    .generation >= 1 and
    ([.groups[] | select(.name == "kuberploy.service.metrics") | .records[]] | sort) ==
      ["kuberploy:service:container_restarts_total","kuberploy:service:cpu_usage_cores",
       "kuberploy:service:http_5xx_ratio","kuberploy:service:http_latency_seconds:p95",
       "kuberploy:service:http_requests_per_second","kuberploy:service:memory_working_set_bytes",
       "kuberploy:service:replicas_ready"]
  ' "${kp_obs_dir}/managed-rules.json" >/dev/null || kp_obs_die "managed rule catalog drifted"
fi

# The merged snapshot must contain concrete lines and sources. Then select one
# returned source and prove exact Pod/container filtering without accepting a
# caller-supplied namespace or Kubernetes object UID.
kp_obs_get "/v1/workloads/${kp_obs_deployment_id}/logs?tailLines=200" \
  "${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}" 200 "${kp_obs_dir}/logs-merged.json"
jq -e '
  (.lines | type == "array" and length > 0) and (.sources | type == "array" and length > 0) and
  all(.lines[]; .type == "line" and (.message | type == "string" and length > 0) and
    (.source.podName | type == "string" and length > 0) and
    (.source.container | type == "string" and length > 0)) and
  (.bytes > 0) and .truncated == false and (.observedAt | fromdateiso8601 > 0)
' "${kp_obs_dir}/logs-merged.json" >/dev/null || kp_obs_die "merged workload logs are empty or malformed"
kp_obs_pod="$(jq -er '.lines[0].source.podName' "${kp_obs_dir}/logs-merged.json")"
kp_obs_container="$(jq -er '.lines[0].source.container' "${kp_obs_dir}/logs-merged.json")"
kp_obs_get "/v1/workloads/${kp_obs_deployment_id}/logs?pod=${kp_obs_pod}&container=${kp_obs_container}&tailLines=50" \
  "${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}" 200 "${kp_obs_dir}/logs-exact-source.json"
jq -e --arg pod "${kp_obs_pod}" --arg container "${kp_obs_container}" '
  (.lines | length > 0) and (.sources | length == 1) and
  all(.lines[]; .source.podName == $pod and .source.container == $container) and
  all(.sources[]; .podName == $pod and .container == $container)
' "${kp_obs_dir}/logs-exact-source.json" >/dev/null || kp_obs_die "exact Pod/container filter escaped"

kp_obs_get "/v1/workloads/${kp_obs_deployment_id}/events?limit=50" \
  "${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}" 200 "${kp_obs_dir}/events.json"
jq -e --arg name "${kp_obs_runtime_name}" '
  (.items | type == "array" and length > 0) and
  any(.items[]; .reason == "QualificationObserved" and .objectKind == "Deployment" and
    .objectName == $name and .message == "bounded observability qualification event" and
    .messageTruncated == false and .count == 1) and
  all(.items[]; (.id | type == "string" and length > 0) and
    (.type == "Normal" or .type == "Warning") and
    (.message | type == "string") and ((keys | index("namespace")) == null))
' "${kp_obs_dir}/events.json" >/dev/null || kp_obs_die "sanitized runtime Event was not observed"

# Follow is human-only by design. A bounded client deadline must still receive
# at least one concrete line and may additionally receive explicit gap events;
# silent disconnects or unconstrained streams do not pass.
set +e
curl --silent --show-error --no-buffer --max-time 20 \
  --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
  "${KUBERPLOY_E2E_API_BASE_URL}/v1/workloads/${kp_obs_deployment_id}/logs/follow?pod=${kp_obs_pod}&container=${kp_obs_container}&tailLines=20" \
  >"${kp_obs_dir}/logs-follow.ndjson"
kp_obs_follow_status=$?
set -e
[[ "${kp_obs_follow_status}" -eq 0 || "${kp_obs_follow_status}" -eq 28 ]] || \
  kp_obs_die "bounded log follow failed with curl status ${kp_obs_follow_status}"
jq -s -e '
  length > 0 and any(.[]; .type == "line" and (.line.message | type == "string" and length > 0)) and
  all(.[]; (.type == "line" and .line != null) or
    (.type == "source-status" and .sourceStatus != null) or
    (.type == "gap" and .gap.droppedLines >= 1) or
    (.type == "heartbeat") or
    (.type == "terminal" and .terminal.code != null))
' "${kp_obs_dir}/logs-follow.ndjson" >/dev/null || kp_obs_die "bounded follow emitted no verified line or invalid events"

# Use a live window, never a stale scenario timestamp. Generate bounded route
# traffic before checking the HTTP recording rules.
if [[ -n "${KUBERPLOY_E2E_HTTP_HOSTNAME:-}" ]]; then
  for _ in {1..20}; do
    curl --silent --show-error --max-time 5 --output /dev/null \
      "http://${KUBERPLOY_E2E_HTTP_HOSTNAME}/" || true
  done
fi
kp_obs_to="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
kp_obs_from="$(date -u -d '10 minutes ago' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || \
  date -u -v-10M +%Y-%m-%dT%H:%M:%SZ)"
kp_obs_metrics='cpu-usage memory-working-set replicas-ready container-restarts http-request-rate http-error-ratio http-latency-p95'
for kp_obs_metric in ${kp_obs_metrics}; do
  for kp_obs_scope in service namespace global; do
    case "${kp_obs_scope}" in
      service) kp_obs_scope_id="${kp_obs_deployment_id}"; kp_obs_header="${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}" ;;
      namespace) kp_obs_scope_id="${kp_obs_environment_id}"; kp_obs_header="${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}" ;;
      global) kp_obs_scope_id=platform; kp_obs_header="${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}" ;;
    esac
    kp_obs_output="${kp_obs_dir}/metric-${kp_obs_scope}-${kp_obs_metric}.json"
    kp_obs_query_metric "${kp_obs_scope}" "${kp_obs_scope_id}" "${kp_obs_metric}" "${kp_obs_header}" "${kp_obs_output}"
    jq -e --arg metric "${kp_obs_metric}" --arg scope "${kp_obs_scope}" '
      (keys|sort)==["metric","observedAt","scope","series"] and
      .metric == $metric and .scope == $scope and
      (.series | type == "array") and
      all(.series[]; (keys|sort)==["labels","samples"] and (.labels|type=="object") and
        (.samples | type == "array") and
        all(.samples[]; (keys|sort)==["timestamp","value"]) and
        all(.samples[]; (.timestamp | fromdateiso8601 > 0) and (.value | type == "number"))) and
      (.observedAt | fromdateiso8601 > 0)
    ' "${kp_obs_output}" >/dev/null || kp_obs_die "${kp_obs_scope}/${kp_obs_metric} returned a malformed catalog response"
    if [[ "${kp_obs_metric}" == "replicas-ready" ]]; then
      jq -e '.series | length > 0 and all(.[]; .samples | length > 0)' "${kp_obs_output}" >/dev/null || \
        kp_obs_die "${kp_obs_scope}/replicas-ready returned no live catalog samples"
    fi
  done
done

kp_obs_get "/v1/workloads/${kp_obs_deployment_id}/logs?tailLines=1" \
  "${KUBERPLOY_E2E_DENIED_AUTH_HEADER_FILE}" 404 "${kp_obs_dir}/denied-logs.json"
kp_obs_assert_problem "${kp_obs_dir}/denied-logs.json" 404
kp_obs_get "/v1/workloads/${kp_obs_deployment_id}/events?limit=1" \
  "${KUBERPLOY_E2E_DENIED_AUTH_HEADER_FILE}" 404 "${kp_obs_dir}/denied-events.json"
kp_obs_assert_problem "${kp_obs_dir}/denied-events.json" 404
kp_obs_query_metric service "${kp_obs_deployment_id}" replicas-ready \
  "${KUBERPLOY_E2E_DENIED_AUTH_HEADER_FILE}" "${kp_obs_dir}/denied-service-metric.json" 404
kp_obs_assert_problem "${kp_obs_dir}/denied-service-metric.json" 404
kp_obs_query_metric namespace "${kp_obs_environment_id}" replicas-ready \
  "${KUBERPLOY_E2E_DENIED_AUTH_HEADER_FILE}" "${kp_obs_dir}/denied-namespace-metric.json" 404
kp_obs_assert_problem "${kp_obs_dir}/denied-namespace-metric.json" 404
kp_obs_query_metric global platform replicas-ready "${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}" \
  "${kp_obs_dir}/denied-global-metric.json" 403
kp_obs_assert_problem "${kp_obs_dir}/denied-global-metric.json" 403

for kp_obs_evidence in "${kp_obs_dir}"/*.json "${kp_obs_dir}"/*.ndjson; do
  kp_obs_safe_evidence "${kp_obs_evidence}"
done

jq -n --arg mode "${kp_obs_mode}" --arg workload "${kp_obs_deployment_id}" \
  --arg namespace "${kp_obs_namespace}" --arg pod "${kp_obs_pod}" --arg container "${kp_obs_container}" \
  --arg from "${kp_obs_from}" --arg to "${kp_obs_to}" \
  '{schemaVersion:1,monitoring:{mode:$mode,available:true,
      identityAttestation:(if $mode=="managed" then "managed-exact-release-and-rules" else "existing-compatible-catalog" end)},
    runtime:{workloadId:$workload,namespace:$namespace,pod:$pod,container:$container,
      mergedSnapshotNonEmpty:true,exactSourceNonEmpty:true,followNonEmpty:true,gapSemantics:"explicit-bounded-events",
      sanitizedEventNonEmpty:true},
    metrics:{catalog:["cpu-usage","memory-working-set","replicas-ready","container-restarts",
      "http-request-rate","http-error-ratio","http-latency-p95"],
      scopes:["service","namespace","global"],liveSeriesMetric:"replicas-ready",from:$from,to:$to},
    denials:{crossTenantLogs:404,crossTenantEvents:404,crossTenantServiceMetric:404,
      crossTenantNamespaceMetric:404,nonAdminGlobalMetric:403},safeEvidence:true}' \
  >"${kp_obs_dir}/observability-proof.json"
chmod 600 "${kp_obs_dir}"/*
kp_obs_cleanup
trap - EXIT
