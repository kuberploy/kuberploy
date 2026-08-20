#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
source "${kp_root}/scripts/helm/download-locked-artifact.sh"
kp_source="${kp_root}/charts/kuberploy-monitoring"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-monitoring-render.XXXXXX")"

kp_cleanup() {
  if [[ -n "${kp_tmp:-}" && "${kp_tmp}" == *"/kuberploy-monitoring-render."* ]]; then
    rm -rf -- "${kp_tmp}"
  fi
}
trap kp_cleanup EXIT

for kp_tool in curl diff helm python3 rg shasum yq; do
  command -v "${kp_tool}" >/dev/null 2>&1 || {
    printf 'missing tool: %s\n' "${kp_tool}" >&2
    exit 1
  }
done

kp_chart="${kp_tmp}/kuberploy-monitoring"
kp_lock="${kp_source}/testdata/upstream-artifacts.lock"
kp_values="${kp_source}/testdata/managed-values.yaml"
kp_compact="${kp_source}/testdata/compact-values.yaml"

cp -R "${kp_source}" "${kp_chart}"
mkdir -p "${kp_chart}/charts"

kp_lock_lines="$(awk 'NF && $1 !~ /^#/ {count++} END {print count+0}' "${kp_lock}")"
[[ "${kp_lock_lines}" == "1" ]] || {
  printf 'monitoring must lock exactly one independently owned upstream chart\n' >&2
  exit 1
}

while read -r kp_checksum kp_filename kp_url; do
  [[ -z "${kp_checksum}" || "${kp_checksum}" == \#* ]] && continue
  [[ -n "${kp_checksum}" && -n "${kp_filename}" && "${kp_url}" == https://* ]] || {
    printf 'malformed monitoring upstream lock\n' >&2
    exit 1
  }
  rg -F "${kp_checksum}" "${kp_root}/DEPENDENCIES.md" >/dev/null || {
    printf 'monitoring checksum is absent from DEPENDENCIES.md\n' >&2
    exit 1
  }
  kp_download="${kp_chart}/charts/${kp_filename}"
  kp_download_locked_artifact "${kp_url}" "${kp_filename}" "${kp_download}"
  kp_actual="$(shasum -a 256 "${kp_chart}/charts/${kp_filename}" | awk '{print $1}')"
  [[ "${kp_actual}" == "${kp_checksum}" ]] || {
    printf '%s checksum mismatch: expected %s, got %s\n' "${kp_filename}" "${kp_checksum}" "${kp_actual}" >&2
    exit 1
  }
done <"${kp_lock}"

helm dependency list "${kp_chart}" | rg -F 'ok' >/dev/null
python3 -m json.tool "${kp_source}/values.schema.json" >/dev/null
bash -n "${BASH_SOURCE[0]}"

[[ "$(yq '.annotations."kuberploy.io/kube-prometheus-stack-chart-sha256"' "${kp_source}/Chart.yaml")" == 'b558a852552f809ccce66d5677ca1a55c8010470c44a01dbdc4ab3f678bcdc90' ]]
[[ "$(yq '.dependencies | length' "${kp_source}/Chart.yaml")" == "1" ]]
[[ "$(yq '.dependencies[0].name' "${kp_source}/Chart.yaml")" == "kube-prometheus-stack" ]]
[[ "$(yq '.dependencies[0].version' "${kp_source}/Chart.yaml")" == "88.1.5" ]]
[[ "$(yq '.dependencies[0].repository' "${kp_source}/Chart.yaml")" == 'https://prometheus-community.github.io/helm-charts' ]]
[[ "$(yq '.dependencies[0].condition' "${kp_source}/Chart.yaml")" == "monitoring.managed" ]]
rg -F 'digest: sha256:2691fd86bc6258083c5bed1a6bb61b0343f2e43e48d623a28d54d3a32a547b83' "${kp_source}/Chart.lock" >/dev/null

helm lint "${kp_chart}" --namespace kuberploy-monitoring
helm lint "${kp_chart}" --namespace kuberploy-monitoring -f "${kp_values}"
helm lint "${kp_chart}" --namespace kuberploy-monitoring -f "${kp_compact}"

helm template monitoring "${kp_chart}" --namespace kuberploy-monitoring --include-crds >"${kp_tmp}/default.yaml"
helm template monitoring "${kp_chart}" --namespace kuberploy-monitoring --include-crds -f "${kp_values}" >"${kp_tmp}/managed.yaml"
helm template monitoring "${kp_chart}" --namespace kuberploy-monitoring --include-crds -f "${kp_values}" >"${kp_tmp}/managed-again.yaml"
diff -u "${kp_tmp}/managed.yaml" "${kp_tmp}/managed-again.yaml"
yq eval-all 'true' "${kp_tmp}/managed.yaml" >/dev/null

for kp_kube_version in 1.34.10 1.35.7 1.36.3; do
  helm template monitoring "${kp_chart}" --namespace kuberploy-monitoring --include-crds \
    --kube-version "${kp_kube_version}" -f "${kp_values}" >/dev/null
done

helm template monitoring "${kp_chart}" --namespace kuberploy-monitoring --include-crds -f "${kp_compact}" >"${kp_tmp}/compact.yaml"
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.retention' "${kp_tmp}/compact.yaml")" == "7d" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.retentionSize' "${kp_tmp}/compact.yaml")" == "8GB" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.resources.requests.cpu' "${kp_tmp}/compact.yaml")" == "100m" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.storage.volumeClaimTemplate.spec.storageClassName' "${kp_tmp}/compact.yaml")" == "local-path" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.storage.volumeClaimTemplate.spec.resources.requests.storage' "${kp_tmp}/compact.yaml")" == "10Gi" ]]

kp_count_kind() {
  local kp_kind="$1"
  yq eval-all "[select(.kind == \"${kp_kind}\")] | length" "${kp_tmp}/managed.yaml" | tail -1
}

[[ "$(kp_count_kind Namespace)" == "1" ]]
[[ "$(kp_count_kind Prometheus)" == "1" ]]
[[ "$(kp_count_kind Alertmanager)" == "1" ]]
[[ "$(kp_count_kind PrometheusRule)" == "1" ]]
[[ "$(kp_count_kind DaemonSet)" == "1" ]]
[[ "$(kp_count_kind Deployment)" == "2" ]]
[[ "$(kp_count_kind PodDisruptionBudget)" == "4" ]]
[[ "$(kp_count_kind NetworkPolicy)" == "4" ]]
[[ "$(kp_count_kind Ingress)" == "0" ]]
[[ "$(kp_count_kind HTTPRoute)" == "0" ]]
[[ "$(kp_count_kind Job)" == "0" ]]
[[ "$(kp_count_kind CustomResourceDefinition)" -ge "10" ]]

[[ "$(yq eval-all 'select(.kind == "Namespace") | .metadata.name' "${kp_tmp}/managed.yaml")" == "kuberploy-monitoring" ]]
[[ "$(yq eval-all 'select(.kind == "Namespace") | .metadata.labels."kuberploy.io/monitoring-namespace"' "${kp_tmp}/managed.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Namespace") | .metadata.labels."pod-security.kubernetes.io/enforce"' "${kp_tmp}/managed.yaml")" == "privileged" ]]
[[ "$(yq eval-all 'select(.kind == "Namespace") | .metadata.labels."pod-security.kubernetes.io/audit"' "${kp_tmp}/managed.yaml")" == "restricted" ]]
[[ "$(yq eval-all '[select(.metadata.namespace != null and .metadata.namespace != "kuberploy-monitoring")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]

[[ "$(yq eval-all '[select(.kind == "Service" and .spec.type != "ClusterIP")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Service") | .spec.externalIPs[]?] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "PodDisruptionBudget" and (.spec.minAvailable != null or .spec.maxUnavailable != 1))] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]

kp_operator_image='quay.io/prometheus-operator/prometheus-operator:v0.93.0'
kp_ksm_image='registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.19.1'
kp_node_image='quay.io/prometheus/node-exporter:v1.12.1-distroless'
kp_prometheus_image='quay.io/prometheus/prometheus:v3.13.2-distroless'
kp_alertmanager_image='quay.io/prometheus/alertmanager:v0.33.1'
kp_reloader_image='quay.io/prometheus-operator/prometheus-config-reloader:v0.93.0'
kp_thanos_image='quay.io/thanos/thanos:v0.42.4'

kp_workload_images="$(yq eval-all 'select(.kind == "Deployment" or .kind == "DaemonSet") | .spec.template.spec.containers[].image' "${kp_tmp}/managed.yaml" | rg -v '^---$' | sort -u)"
for kp_image in "${kp_operator_image}" "${kp_ksm_image}" "${kp_node_image}"; do
  grep -Fx -- "${kp_image}" <<<"${kp_workload_images}" >/dev/null
done
[[ "$(wc -l <<<"${kp_workload_images}" | tr -d ' ')" == "3" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.image' "${kp_tmp}/managed.yaml")" == "${kp_prometheus_image}" ]]
[[ "$(yq eval-all 'select(.kind == "Alertmanager") | .spec.image' "${kp_tmp}/managed.yaml")" == "${kp_alertmanager_image}" ]]
kp_operator_args="$(yq eval-all 'select(.kind == "Deployment" and .metadata.name == "kuberploy-prometheus-operator") | .spec.template.spec.containers[0].args[]' "${kp_tmp}/managed.yaml")"
grep -Fx -- "--prometheus-config-reloader=${kp_reloader_image}" <<<"${kp_operator_args}" >/dev/null
grep -Fx -- "--thanos-default-base-image=${kp_thanos_image}" <<<"${kp_operator_args}" >/dev/null
grep -Fx -- '--prometheus-instance-namespaces=kuberploy-monitoring' <<<"${kp_operator_args}" >/dev/null
grep -Fx -- '--alertmanager-instance-namespaces=kuberploy-monitoring' <<<"${kp_operator_args}" >/dev/null
grep -Fx -- '--thanos-ruler-instance-selector=kuberploy.io/monitoring-instance=disabled' <<<"${kp_operator_args}" >/dev/null
kp_operator_args_digest="$(
  yq eval-all -o=json -I=0 'select(.kind == "Deployment" and .metadata.name == "kuberploy-prometheus-operator") | .spec.template.spec.containers[0].args' "${kp_tmp}/managed.yaml" |
    python3 -c 'import json,sys; sys.stdout.write(json.dumps(json.load(sys.stdin), sort_keys=True, separators=(",", ":")))' |
    shasum -a 256 | awk '{print "sha256:" $1}'
)"
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "monitoring-monitoring-profile") | .data.operatorArgumentsSHA256' "${kp_tmp}/managed.yaml")" == "${kp_operator_args_digest}" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.replicas' "${kp_tmp}/managed.yaml")" == "1" ]]
[[ "$(yq eval-all 'select(.kind == "Alertmanager") | .spec.replicas' "${kp_tmp}/managed.yaml")" == "1" ]]
[[ "$(yq eval-all 'select(.kind == "Alertmanager") | .spec.automountServiceAccountToken' "${kp_tmp}/managed.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | (.spec.ignoreNamespaceSelectors // false)' "${kp_tmp}/managed.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.arbitraryFSAccessThroughSMs.deny' "${kp_tmp}/managed.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.serviceMonitorSelector.matchLabels."kuberploy.io/monitoring-source"' "${kp_tmp}/managed.yaml")" == "protected" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.serviceMonitorNamespaceSelector.matchLabels."kuberploy.io/monitoring-namespace"' "${kp_tmp}/managed.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.podMonitorSelector.matchLabels."kuberploy.io/monitoring-source"' "${kp_tmp}/managed.yaml")" == "protected" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.ruleSelector.matchLabels."kuberploy.io/monitoring-rule"' "${kp_tmp}/managed.yaml")" == "protected" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.ruleNamespaceSelector.matchLabels."kubernetes.io/metadata.name"' "${kp_tmp}/managed.yaml")" == "kuberploy-monitoring" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.enforcedSampleLimit' "${kp_tmp}/managed.yaml")" == "10000" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.enforcedTargetLimit' "${kp_tmp}/managed.yaml")" == "1000" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.enforcedLabelLimit' "${kp_tmp}/managed.yaml")" == "40" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.enforcedBodySizeLimit' "${kp_tmp}/managed.yaml")" == "50MB" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.enableAdminAPI' "${kp_tmp}/managed.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.enableRemoteWriteReceiver' "${kp_tmp}/managed.yaml")" == "null" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.storage.volumeClaimTemplate.spec.accessModes | join(",")' "${kp_tmp}/managed.yaml")" == "ReadWriteOnce" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.persistentVolumeClaimRetentionPolicy.whenDeleted' "${kp_tmp}/managed.yaml")" == "Retain" ]]

[[ "$(kp_count_kind ServiceMonitor)" == "5" ]]
[[ "$(yq eval-all '[select(.kind == "ServiceMonitor" and .metadata.labels."kuberploy.io/monitoring-source" != "protected")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]
awk '
  function check_document() {
    if (service_monitor && protected_labels != 1) {
      exit 1
    }
  }
  /^---$/ {
    check_document()
    service_monitor = 0
    protected_labels = 0
    next
  }
  /^kind: ServiceMonitor$/ { service_monitor = 1 }
  service_monitor && /^    kuberploy\.io\/monitoring-source: protected$/ { protected_labels++ }
  END { check_document() }
' "${kp_tmp}/managed.yaml" || {
  printf 'each ServiceMonitor must carry exactly one protected selector label\n' >&2
  exit 1
}
[[ "$(yq eval-all 'select(.kind == "ServiceMonitor" and (.metadata.name | test("kubelet"))) | .spec.endpoints[0].tlsConfig.insecureSkipVerify' "${kp_tmp}/managed.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "ServiceMonitor" and (.metadata.name | test("kubelet"))) | .spec.endpoints[0].metricRelabelings[1].action' "${kp_tmp}/managed.yaml")" == "keep" ]]
[[ "$(yq eval-all 'select(.kind == "ServiceMonitor" and (.metadata.name | test("kubelet"))) | .spec.endpoints[0].metricRelabelings[1].sourceLabels | join(",")' "${kp_tmp}/managed.yaml")" == "pod" ]]
[[ "$(yq eval-all 'select(.kind == "ServiceMonitor" and (.metadata.name | test("kubelet"))) | .spec.endpoints[0].metricRelabelings[1].regex' "${kp_tmp}/managed.yaml")" == ".+" ]]
kp_ksm_args="$(yq eval-all 'select(.kind == "Deployment" and .metadata.name == "kuberploy-kube-state-metrics") | .spec.template.spec.containers[0].args[]' "${kp_tmp}/managed.yaml")"
[[ "$(yq eval-all 'select(.kind == "ServiceMonitor" and .metadata.name == "kuberploy-kube-state-metrics") | (.spec.endpoints[0].honorLabels // false)' "${kp_tmp}/managed.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Prometheus" and .metadata.name == "monitoring-kube-prometheus-prometheus") | .spec.overrideHonorLabels' "${kp_tmp}/managed.yaml")" == "true" ]]
grep -Fx -- '--resources=deployments,ingresses,pods,services' <<<"${kp_ksm_args}" >/dev/null
grep -Fx -- '--metric-allowlist=kube_deployment_labels,kube_deployment_status_replicas_available,kube_ingress_labels,kube_pod_container_status_restarts_total,kube_pod_labels,kube_service_labels' <<<"${kp_ksm_args}" >/dev/null
grep -Fx -- '--metric-labels-allowlist=deployments=[kuberploy.io/project,kuberploy.io/environment,kuberploy.io/application,kuberploy.io/service],ingresses=[kuberploy.io/project,kuberploy.io/environment,kuberploy.io/application,kuberploy.io/service],pods=[kuberploy.io/project,kuberploy.io/environment,kuberploy.io/application,kuberploy.io/service],services=[kuberploy.io/project,kuberploy.io/environment,kuberploy.io/application,kuberploy.io/service]' <<<"${kp_ksm_args}" >/dev/null
if grep -F '*' <<<"${kp_ksm_args}" >/dev/null; then
  printf 'kube-state-metrics rendered a wildcard allowlist\n' >&2
  exit 1
fi

[[ "$(yq eval-all '[select(.kind == "Deployment" or .kind == "DaemonSet") | select(.spec.template.spec.hostNetwork == true or .spec.template.spec.hostPID == true or .spec.template.spec.hostIPC == true)] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" or .kind == "DaemonSet") | .spec.template.spec.containers[] | select(.securityContext.privileged == true or .securityContext.allowPrivilegeEscalation == true)] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" or .kind == "DaemonSet") | .spec.template.spec.containers[] | select(.resources.requests.cpu == null or .resources.requests.memory == null or .resources.limits.cpu == null or .resources.limits.memory == null)] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "DaemonSet") | .spec.template.spec.volumes[] | select(.hostPath != null)] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "DaemonSet") | .spec.template.spec.volumes[] | .hostPath.path] | sort | join(",")' "${kp_tmp}/managed.yaml" | tail -1)" == "/proc,/sys" ]]
[[ "$(yq eval-all '[select(.kind == "DaemonSet") | .spec.template.spec.containers[].volumeMounts[] | select(.name == "proc" or .name == "sys") | select(.readOnly != true)] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "ClusterRoleBinding") | .subjects[] | select(.kind == "ServiceAccount" and .name == "default")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]

kp_records="$(yq eval-all 'select(.kind == "PrometheusRule" and .metadata.labels."kuberploy.io/monitoring-rule" == "protected") | .spec.groups[].rules[].record' "${kp_tmp}/managed.yaml" | sort)"
kp_expected_records="$(printf '%s\n' \
  'kuberploy:service:container_restarts_total' \
  'kuberploy:service:cpu_usage_cores' \
  'kuberploy:service:http_5xx_ratio' \
  'kuberploy:service:http_latency_seconds:p95' \
  'kuberploy:service:http_requests_per_second' \
  'kuberploy:service:memory_working_set_bytes' \
  'kuberploy:service:replicas_ready')"
[[ "${kp_records}" == "${kp_expected_records}" ]]
[[ "$(wc -l <<<"${kp_records}" | tr -d ' ')" == "7" ]]
kp_rule_spec_digest="$(
  yq eval-all -o=json -I=0 'select(.kind == "PrometheusRule" and .metadata.name == "monitoring-service-recording-rules") | .spec' "${kp_tmp}/managed.yaml" |
    python3 -c 'import json,sys; sys.stdout.write(json.dumps(json.load(sys.stdin), sort_keys=True, separators=(",", ":")))' |
    shasum -a 256 | awk '{print "sha256:" $1}'
)"
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "monitoring-monitoring-profile") | .data.recordingRuleSpecSHA256' "${kp_tmp}/managed.yaml")" == "${kp_rule_spec_digest}" ]]
kp_rule_exprs="$(yq eval-all 'select(.kind == "PrometheusRule") | .spec.groups[].rules[].expr' "${kp_tmp}/managed.yaml")"
for kp_label in namespace kuberploy_project kuberploy_environment kuberploy_application kuberploy_service; do
  grep -F -- "${kp_label}" <<<"${kp_rule_exprs}" >/dev/null
done
[[ "$(grep -c 'traefik_service_' <<<"${kp_rule_exprs}")" -ge "3" ]]
[[ "$(grep -c 'exported_namespace' <<<"${kp_rule_exprs}")" -ge "7" ]]
[[ "$(grep -c 'exported_pod' <<<"${kp_rule_exprs}")" -ge "3" ]]
[[ "$(grep -c 'exported_service' <<<"${kp_rule_exprs}")" -ge "3" ]]
[[ "$(grep -c 'exported_service=~' <<<"${kp_rule_exprs}")" -eq "4" ]]
if rg -N 'traefik_service_[^{]+\{[^}]*\bservice=~' <<<"${kp_rule_exprs}" >/dev/null; then
  printf 'HTTP recording rules matched Prometheus target service instead of Traefik exported_service\n' >&2
  exit 1
fi
if grep -E 'or[[:space:]]+vector\(0\)' <<<"${kp_rule_exprs}" >/dev/null; then
  printf 'HTTP recording rules must remain empty when Traefik metrics are absent\n' >&2
  exit 1
fi

kp_query_policy="$(yq eval-all 'select(.kind == "NetworkPolicy" and (.metadata.name | test("-prometheus-query$")))' "${kp_tmp}/managed.yaml")"
[[ "$(yq '.spec.ingress[0].from[0].namespaceSelector.matchLabels."kuberploy.io/control-plane-namespace"' <<<"${kp_query_policy}")" == "true" ]]
[[ "$(yq '.spec.ingress[0].from[0].podSelector.matchLabels."app.kubernetes.io/name"' <<<"${kp_query_policy}")" == "kuberploy" ]]
[[ "$(yq '.spec.ingress[0].from[0].podSelector.matchLabels."app.kubernetes.io/component"' <<<"${kp_query_policy}")" == "api" ]]
[[ "$(yq '.spec.ingress[0].ports | length' <<<"${kp_query_policy}")" == "1" ]]
[[ "$(yq '.spec.ingress[0].ports[0].port' <<<"${kp_query_policy}")" == "9090" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy") | .spec.ingress[].from[]? | select(.namespaceSelector == {})] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy") | .spec.ingress[].from[]?.ipBlock] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "NetworkPolicy" and (.metadata.name | test("-private-egress$"))) | [.spec.egress[0].to[].ipBlock.cidr]' "${kp_tmp}/managed.yaml")" == '["10.0.0.0/8","172.16.0.0/12","192.168.0.0/16"]' ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.name == "monitoring-private-egress") | .spec.egress[] | select(.to[0].ipBlock.cidr == "10.43.0.1/32" and .ports[0].port == 443 and .ports[1].port == 6443)] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "monitoring-monitoring-profile") | .immutable' "${kp_tmp}/managed.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "monitoring-monitoring-profile") | .metadata.annotations."argocd.argoproj.io/sync-options"' "${kp_tmp}/managed.yaml")" == "Force=true,Replace=true" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "monitoring-monitoring-profile") | (.data | length)' "${kp_tmp}/managed.yaml")" == "24" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "monitoring-monitoring-profile") | .data.ignoreNamespaceSelectors + "," + .data.serviceMonitorFilesystemAccess' "${kp_tmp}/managed.yaml")" == "false,kubelet-service-account-token+cluster-ca" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "monitoring-monitoring-profile") | .data.contract + "," + .data.chartName + "," + .data.chartVersion + "," + .data.releaseName + "," + .data.namespace' "${kp_tmp}/managed.yaml")" == "kuberploy-managed-monitoring/v1,kuberploy-monitoring,0.1.0-rc.268,monitoring,kuberploy-monitoring" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "monitoring-monitoring-profile") | .data.operatorArgumentsSHA256' "${kp_tmp}/managed.yaml")" == "sha256:ad7ee73da3828389d76d5f6102dde3c3c6cde35f0345bf8d7cad220a5c6df7a6" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "monitoring-monitoring-profile") | .data.upstreamChartSHA256' "${kp_tmp}/managed.yaml")" == "sha256:b558a852552f809ccce66d5677ca1a55c8010470c44a01dbdc4ab3f678bcdc90" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "monitoring-monitoring-profile") | .data.recordingRuleSpecSHA256' "${kp_tmp}/managed.yaml")" == "sha256:0058f63c0c000cc9e491f3775c830554fa7a1bf10d0b86de7e3f8d61e9b09879" ]]

if rg -n 'kind: (Ingress|HTTPRoute)|type: (LoadBalancer|NodePort)|grafana/grafana' "${kp_tmp}/managed.yaml"; then
  printf 'managed monitoring rendered a public route, public Service, or Grafana\n' >&2
  exit 1
fi

kp_expect_reject() {
  local kp_reason="$1"
  shift
  if helm template invalid "${kp_chart}" --namespace kuberploy-monitoring --include-crds -f "${kp_values}" "$@" \
      >"${kp_tmp}/rejected.stdout" 2>"${kp_tmp}/rejected.stderr"; then
    printf 'unsafe monitoring render was accepted: %s\n' "${kp_reason}" >&2
    exit 1
  fi
}

kp_expect_reject 'wrong release namespace' --namespace other-namespace
kp_expect_reject 'wrong release identity' --name-template other-monitoring
kp_expect_reject 'managed monitoring disabled' --set monitoring.managed=false
helm template monitoring "${kp_chart}" --namespace kuberploy-monitoring --include-crds -f "${kp_values}" \
  --set monitoring.networkPolicy.enabled=false >"${kp_tmp}/no-network-policy.yaml"
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy")] | length' "${kp_tmp}/no-network-policy.yaml" | tail -1)" == "0" ]]
helm template monitoring "${kp_chart}" --namespace kuberploy-monitoring --include-crds -f "${kp_values}" \
  --set-json monitoring.networkPolicy.kubeAPIServerCIDRs=[] >"${kp_tmp}/no-api.yaml"
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.name == "monitoring-private-egress") | .spec.egress[] | select(.to[0].ipBlock.cidr == "0.0.0.0/0" and .ports[0].port == 443 and .ports[1].port == 6443)] | length' "${kp_tmp}/no-api.yaml" | tail -1)" == "1" ]]
kp_expect_reject 'broad Kubernetes API identity' --set-string 'monitoring.networkPolicy.kubeAPIServerCIDRs[0]=10.43.0.0/24'
helm template monitoring "${kp_chart}" --namespace kuberploy-monitoring --include-crds -f "${kp_values}" \
  --set-json 'monitoring.networkPolicy.kubeAPIServerCIDRs=["10.43.0.1/32","10.43.0.1/32"]' >"${kp_tmp}/duplicate-api.yaml"
[[ "$(yq eval-all 'select(.kind == "NetworkPolicy" and .metadata.name == "monitoring-private-egress") | [.spec.egress[].to[]?.ipBlock.cidr] | map(select(. == "10.43.0.1/32")) | length' "${kp_tmp}/duplicate-api.yaml")" == "1" ]]
kp_expect_reject 'schema-bypass broad Kubernetes API identity' --skip-schema-validation --set-string 'monitoring.networkPolicy.kubeAPIServerCIDRs[0]=0.0.0.0/0'
kp_expect_reject 'query namespace trust label changed' --set-string 'monitoring.networkPolicy.queryClient.namespaceLabel.kuberploy\.io/control-plane-namespace=false'
kp_expect_reject 'Grafana enabled' --set 'kube-prometheus-stack.grafana.enabled=true'
kp_expect_reject 'public Prometheus Service' --set-string 'kube-prometheus-stack.prometheus.service.type=LoadBalancer'
kp_expect_reject 'public Alertmanager route' --set 'kube-prometheus-stack.alertmanager.ingress.enabled=true'
helm template monitoring "${kp_chart}" --namespace kuberploy-monitoring --include-crds -f "${kp_values}" \
  --set 'kube-prometheus-stack.prometheus.prometheusSpec.replicas=2' >"${kp_tmp}/scaled.yaml"
[[ "$(yq eval-all 'select(.kind == "Prometheus") | .spec.replicas' "${kp_tmp}/scaled.yaml")" == "2" ]]
kp_expect_reject 'unprotected ServiceMonitor selector' --set-string 'kube-prometheus-stack.prometheus.prometheusSpec.serviceMonitorSelector.matchLabels.kuberploy\.io/monitoring-source=attacker'
kp_expect_reject 'namespace selector suppression' --set 'kube-prometheus-stack.prometheus.prometheusSpec.ignoreNamespaceSelectors=true'
kp_expect_reject 'kubelet service-account scrape disabled' --set 'kube-prometheus-stack.prometheus.prometheusSpec.arbitraryFSAccessThroughSMs.deny=true'
kp_expect_reject 'kubelet TLS verification disabled' --set 'kube-prometheus-stack.kubelet.serviceMonitor.insecureSkipVerify=true'
kp_expect_reject 'Prometheus admin API enabled' --set 'kube-prometheus-stack.prometheus.prometheusSpec.enableAdminAPI=true'
kp_expect_reject 'remote-write receiver enabled' --set 'kube-prometheus-stack.prometheus.prometheusSpec.enableRemoteWriteReceiver=true'
kp_expect_reject 'remote-write exfiltration' --set-string 'kube-prometheus-stack.prometheus.prometheusSpec.remoteWrite[0].url=https://attacker.invalid/write'
kp_expect_reject 'arbitrary scrape configuration' --set-string 'kube-prometheus-stack.prometheus.prometheusSpec.additionalScrapeConfigs[0].job_name=attacker'
kp_expect_reject 'Prometheus sidecar injection' --set-string 'kube-prometheus-stack.prometheus.prometheusSpec.containers[0].name=attacker'
kp_expect_reject 'operator argument injection' --set-string 'kube-prometheus-stack.prometheusOperator.extraArgs[0]=--log-level=debug'
kp_expect_reject 'operator admission hook Jobs' --set 'kube-prometheus-stack.prometheusOperator.admissionWebhooks.enabled=true'
helm template monitoring "${kp_chart}" --namespace kuberploy-monitoring --include-crds -f "${kp_values}" \
  --set-string 'kube-prometheus-stack.prometheus.prometheusSpec.image.sha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' \
  --set-string 'kube-prometheus-stack.prometheusOperator.image.sha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' >/dev/null
kp_expect_reject 'kube-state-metrics wildcard labels' --set-string 'kube-prometheus-stack.kube-state-metrics.metricLabelsAllowlist[0]=pods=[*]'
kp_expect_reject 'Kubernetes annotation projection' --set-string 'kube-prometheus-stack.kube-state-metrics.metricAnnotationsAllowList[0]=pods=[secret]'
kp_expect_reject 'node-exporter host network' --set 'kube-prometheus-stack.prometheus-node-exporter.hostNetwork=true'
kp_expect_reject 'node-exporter host PID' --set 'kube-prometheus-stack.prometheus-node-exporter.hostPID=true'
kp_expect_reject 'node-exporter arbitrary host mount' --set-string 'kube-prometheus-stack.prometheus-node-exporter.extraHostVolumeMounts[0].name=host' --set-string 'kube-prometheus-stack.prometheus-node-exporter.extraHostVolumeMounts[0].hostPath=/'
kp_expect_reject 'retention beyond managed maximum' --set-string 'kube-prometheus-stack.prometheus.prometheusSpec.retention=365d'
kp_expect_reject 'invalid PVC access mode' --set-string 'kube-prometheus-stack.prometheus.prometheusSpec.storageSpec.volumeClaimTemplate.spec.accessModes[0]=ReadWriteMany'
kp_expect_reject 'resource injection' --set-string 'kube-prometheus-stack.prometheus.prometheusSpec.resources.limits.nvidia.com/gpu=1'

printf 'Managed monitoring chart locks, selectors, seven recording rules, images, storage, policies, and mutations passed.\n'
