#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-observability-driver.XXXXXX")"
trap 'rm -rf -- "${kp_tmp}"' EXIT
mkdir -p "${kp_tmp}/bin" "${kp_tmp}/evidence"

cat >"${kp_tmp}/state.json" <<'EOF'
{"projectId":"11111111-1111-4111-8111-111111111111","directEnvironmentId":"22222222-2222-4222-8222-222222222222","directEnvironmentNamespace":"tenant-safe","applicationId":"33333333-3333-4333-8333-333333333333","directDeploymentId":"44444444-4444-4444-8444-444444444444"}
EOF
printf '%s\n' 'Authorization: Bearer authorized-fixture' >"${kp_tmp}/authorized.header"
printf '%s\n' 'Authorization: Bearer denied-fixture' >"${kp_tmp}/denied.header"
printf '%s\n' 'Cookie: kuberploy_session=human-fixture' >"${kp_tmp}/human.header"
chmod 600 "${kp_tmp}"/*.header

kp_runtime_name="kp-a-$(printf '%s' '33333333-3333-4333-8333-333333333333' | shasum -a 256 | awk '{print substr($1,1,16)}')"

cat >"${kp_tmp}/bin/kubectl" <<EOF
#!/usr/bin/env bash
set -Eeuo pipefail
case "\$*" in
  "get deployment ${kp_runtime_name} --namespace tenant-safe -o json")
    printf '%s\n' '{"metadata":{"name":"${kp_runtime_name}","namespace":"tenant-safe","uid":"deployment-uid-safe","generation":2},"spec":{"replicas":1},"status":{"observedGeneration":2,"availableReplicas":1}}' ;;
  "get configmap monitoring-monitoring-profile --namespace kuberploy-monitoring -o json")
    printf '%s\n' '{"immutable":true,"data":{"contract":"kuberploy-managed-monitoring/v1","readinessContract":"profile+operator+rule-spec+prometheus-scrape-policy+prometheus-rules","metricSeries":"kuberploy:service:cpu_usage_cores,kuberploy:service:memory_working_set_bytes,kuberploy:service:replicas_ready,kuberploy:service:container_restarts_total,kuberploy:service:http_requests_per_second,kuberploy:service:http_5xx_ratio,kuberploy:service:http_latency_seconds:p95","operatorArgumentsSHA256":"sha256:safe","recordingRuleSpecSHA256":"sha256:safe"}}' ;;
  "get deployment kuberploy-prometheus-operator --namespace kuberploy-monitoring -o json")
    printf '%s\n' '{"metadata":{"generation":3},"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"kube-prometheus-stack","image":"quay.io/prometheus-operator/prometheus-operator:v0.93.0"}]}}},"status":{"observedGeneration":3,"availableReplicas":1}}' ;;
  "get prometheusrule monitoring-service-recording-rules --namespace kuberploy-monitoring -o json")
    printf '%s\n' '{"metadata":{"generation":1},"spec":{"groups":[{"name":"kuberploy.service.metrics","rules":[{"record":"kuberploy:service:cpu_usage_cores"},{"record":"kuberploy:service:memory_working_set_bytes"},{"record":"kuberploy:service:replicas_ready"},{"record":"kuberploy:service:container_restarts_total"},{"record":"kuberploy:service:http_requests_per_second"},{"record":"kuberploy:service:http_5xx_ratio"},{"record":"kuberploy:service:http_latency_seconds:p95"}]}]}}' ;;
  create\ -f\ *) printf '%s\n' 'event/created' ;;
  delete\ event\ *) exit 0 ;;
  *) printf 'unexpected kubectl call: %s\n' "\$*" >&2; exit 2 ;;
esac
EOF
chmod +x "${kp_tmp}/bin/kubectl"

cat >"${kp_tmp}/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
kp_output="" kp_write=false kp_header="" kp_url=""
while (( $# )); do
  case "$1" in
    --output) kp_output="$2"; shift 2 ;;
    --write-out) kp_write=true; shift 2 ;;
    --header) kp_header="$2"; shift 2 ;;
    --request|--max-time) shift 2 ;;
    --silent|--show-error|--no-buffer) shift ;;
    http://*) exit 0 ;;
    https://*) kp_url="$1"; shift ;;
    *) printf 'unexpected curl argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done
if [[ "${kp_url}" == */logs/follow* ]]; then
  printf '%s\n' '{"type":"line","line":{"type":"line","source":{"podId":"pod-uid","podName":"pod-safe","container":"app","containerKind":"regular","restartCount":0,"ready":true,"terminating":false,"previous":false},"message":"safe live line","truncated":false},"at":"2026-08-10T01:00:00Z"}'
  exit 28
fi
kp_status=200
case "${kp_url}" in
  */v1/monitoring/status) kp_body='{"mode":"managed","status":"available","available":true,"message":"ready","observedAt":"2026-08-10T01:00:00Z"}' ;;
  */logs\?tailLines=200) kp_body='{"lines":[{"type":"line","timestamp":"2026-08-10T01:00:00Z","source":{"podId":"pod-uid","podName":"pod-safe","container":"app","containerKind":"regular","restartCount":0,"revision":"1","ready":true,"terminating":false,"previous":false},"message":"safe snapshot line","truncated":false}],"sources":[{"podId":"pod-uid","podName":"pod-safe","container":"app","containerKind":"regular","restartCount":0,"revision":"1","ready":true,"terminating":false,"previous":false}],"bytes":18,"truncated":false,"observedAt":"2026-08-10T01:00:00Z"}' ;;
  */logs\?pod=pod-safe*) kp_body='{"lines":[{"type":"line","source":{"podId":"pod-uid","podName":"pod-safe","container":"app","containerKind":"regular","restartCount":0,"ready":true,"terminating":false,"previous":false},"message":"safe exact line","truncated":false}],"sources":[{"podId":"pod-uid","podName":"pod-safe","container":"app","containerKind":"regular","restartCount":0,"ready":true,"terminating":false,"previous":false}],"bytes":15,"truncated":false,"observedAt":"2026-08-10T01:00:00Z"}' ;;
  */events\?limit=50) kp_body='{"items":[{"id":"event-uid","type":"Normal","reason":"QualificationObserved","message":"bounded observability qualification event","messageTruncated":false,"objectKind":"Deployment","objectName":"RUNTIME_NAME","count":1,"firstSeen":"2026-08-10T01:00:00Z","lastSeen":"2026-08-10T01:00:00Z"}],"truncated":false,"observedAt":"2026-08-10T01:00:00Z"}' ;;
  */logs\?tailLines=1|*/events\?limit=1) kp_status=404; kp_body='{"status":404,"code":"NotFound","title":"Not found","detail":"not found"}' ;;
  */v1/metrics/query-range*)
    kp_metric="$(sed -n 's/.*[?&]metric=\([^&]*\).*/\1/p' <<<"${kp_url}")"
    kp_scope="$(sed -n 's/.*[?&]scopeType=\([^&]*\).*/\1/p' <<<"${kp_url}")"
    if [[ "${kp_header}" == *denied-fixture* && "${kp_scope}" != global ]]; then
      kp_status=404; kp_body='{"status":404,"code":"NotFound","title":"Not found","detail":"not found"}'
    elif [[ "${kp_scope}" == global && "${kp_header}" == *authorized-fixture* ]]; then
      kp_status=403; kp_body='{"status":403,"code":"Forbidden","title":"Forbidden","detail":"forbidden"}'
    elif [[ "${kp_metric}" == replicas-ready ]]; then
      kp_body="{\"metric\":\"${kp_metric}\",\"scope\":\"${kp_scope}\",\"series\":[{\"labels\":{},\"samples\":[{\"timestamp\":\"2026-08-10T01:00:00Z\",\"value\":1}]}],\"observedAt\":\"2026-08-10T01:00:00Z\"}"
    else
      kp_body="{\"metric\":\"${kp_metric}\",\"scope\":\"${kp_scope}\",\"series\":[],\"observedAt\":\"2026-08-10T01:00:00Z\"}"
    fi ;;
  *) printf 'unexpected curl URL: %s\n' "${kp_url}" >&2; exit 2 ;;
esac
kp_body="${kp_body/RUNTIME_NAME/__RUNTIME_NAME__}"
kp_body="${kp_body/__RUNTIME_NAME__/${KP_TEST_RUNTIME_NAME:?}}"
[[ -z "${kp_output}" || "${kp_output}" == /dev/null ]] || printf '%s\n' "${kp_body}" >"${kp_output}"
${kp_write} && printf '%s' "${kp_status}"
EOF
chmod +x "${kp_tmp}/bin/curl"

PATH="${kp_tmp}/bin:${PATH}" KP_TEST_RUNTIME_NAME="${kp_runtime_name}" \
KUBERPLOY_E2E_API_BASE_URL=https://qualification.invalid \
KUBERPLOY_E2E_OBSERVABILITY_EVIDENCE_DIR="${kp_tmp}/evidence" \
KUBERPLOY_E2E_WORKFLOW_STATE_FILE="${kp_tmp}/state.json" \
KUBERPLOY_E2E_API_AUTH_HEADER_FILE="${kp_tmp}/authorized.header" \
KUBERPLOY_E2E_DENIED_AUTH_HEADER_FILE="${kp_tmp}/denied.header" \
KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE="${kp_tmp}/human.header" \
KUBERPLOY_E2E_KUBECTL="${kp_tmp}/bin/kubectl" KUBERPLOY_E2E_RUN_ID=fixture \
KUBERPLOY_E2E_HTTP_HOSTNAME=route.invalid \
  "${kp_root}/scripts/kubernetes/test/e2e/observability-driver.sh"

jq -e '
  (keys|sort)==["denials","metrics","monitoring","runtime","safeEvidence","schemaVersion"] and
  .schemaVersion==1 and .monitoring=={mode:"managed",available:true,
    identityAttestation:"managed-exact-release-and-rules"} and
  .runtime.mergedSnapshotNonEmpty==true and .runtime.exactSourceNonEmpty==true and
  .runtime.followNonEmpty==true and .runtime.sanitizedEventNonEmpty==true and
  .metrics.liveSeriesMetric=="replicas-ready" and (.metrics.catalog|length)==7 and
  .metrics.scopes==["service","namespace","global"] and
  .denials=={crossTenantLogs:404,crossTenantEvents:404,crossTenantServiceMetric:404,
    crossTenantNamespaceMetric:404,nonAdminGlobalMetric:403} and .safeEvidence==true
' "${kp_tmp}/evidence/observability-proof.json" >/dev/null
if [[ -n "${KP_OBSERVABILITY_TEST_PROOF_OUTPUT:-}" ]]; then
  [[ "${KP_OBSERVABILITY_TEST_PROOF_OUTPUT}" == /* ]] || {
    printf 'KP_OBSERVABILITY_TEST_PROOF_OUTPUT must be absolute\n' >&2
    exit 1
  }
  cp -- "${kp_tmp}/evidence/observability-proof.json" "${KP_OBSERVABILITY_TEST_PROOF_OUTPUT}"
  chmod 600 "${KP_OBSERVABILITY_TEST_PROOF_OUTPUT}"
fi

# Adversarial fixture: a credential-shaped log must be rejected before proof.
sed -i.bak 's/safe snapshot line/token=kp_sa_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA/' \
  "${kp_tmp}/bin/curl"
rm -f -- "${kp_tmp}/bin/curl.bak"
rm -rf -- "${kp_tmp}/evidence"
mkdir -p "${kp_tmp}/evidence"
if PATH="${kp_tmp}/bin:${PATH}" KP_TEST_RUNTIME_NAME="${kp_runtime_name}" \
  KUBERPLOY_E2E_API_BASE_URL=https://qualification.invalid \
  KUBERPLOY_E2E_OBSERVABILITY_EVIDENCE_DIR="${kp_tmp}/evidence" \
  KUBERPLOY_E2E_WORKFLOW_STATE_FILE="${kp_tmp}/state.json" \
  KUBERPLOY_E2E_API_AUTH_HEADER_FILE="${kp_tmp}/authorized.header" \
  KUBERPLOY_E2E_DENIED_AUTH_HEADER_FILE="${kp_tmp}/denied.header" \
  KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE="${kp_tmp}/human.header" \
  KUBERPLOY_E2E_KUBECTL="${kp_tmp}/bin/kubectl" KUBERPLOY_E2E_RUN_ID=fixture \
  KUBERPLOY_E2E_HTTP_HOSTNAME=route.invalid \
    "${kp_root}/scripts/kubernetes/test/e2e/observability-driver.sh" >/dev/null 2>&1; then
  printf 'credential-shaped observability evidence was accepted\n' >&2
  exit 1
fi

printf 'Isolated observability driver strict proof and adversarial fixture passed.\n'
