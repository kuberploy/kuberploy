#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
kp_chart="${kp_root}/charts/kuberploy-postgresql"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-postgresql-render.XXXXXX")"

kp_cleanup() {
  if [[ -n "${kp_tmp:-}" && "${kp_tmp}" == *"/kuberploy-postgresql-render."* ]]; then
    rm -rf -- "${kp_tmp}"
  fi
}
trap kp_cleanup EXIT

for kp_tool in helm python3 rg yq; do
  command -v "${kp_tool}" >/dev/null 2>&1 || {
    printf 'missing tool: %s\n' "${kp_tool}" >&2
    exit 1
  }
done

python3 -m json.tool "${kp_chart}/values.schema.json" >/dev/null
kp_managed="${kp_chart}/testdata/managed-values.yaml"
kp_adopted="${kp_chart}/testdata/adopted-values.yaml"
helm lint "${kp_chart}" --namespace kuberploy-system -f "${kp_managed}" >/dev/null
helm lint "${kp_chart}" --namespace kuberploy-system -f "${kp_adopted}" >/dev/null
helm template postgresql "${kp_chart}" --namespace kuberploy-system -f "${kp_managed}" >"${kp_tmp}/managed.yaml"
[[ "$(yq eval-all 'select(.kind == "Namespace" and .metadata.name == "kuberploy-system") | .metadata.labels."kuberploy.io/control-plane-namespace"' "${kp_tmp}/managed.yaml")" == "true" ]]
helm template postgresql "${kp_chart}" --namespace kuberploy-system -f "${kp_managed}" >"${kp_tmp}/managed-again.yaml"
helm template postgresql "${kp_chart}" --namespace kuberploy-system -f "${kp_adopted}" >"${kp_tmp}/adopted.yaml"
diff -u "${kp_tmp}/managed.yaml" "${kp_tmp}/managed-again.yaml" >/dev/null

[[ "$(yq eval-all '[select(.kind == "StatefulSet")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Service")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Secret")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "NetworkPolicy" and .metadata.name == "kuberploy-postgresql-default-deny") | .spec.podSelector.matchLabels' "${kp_tmp}/managed.yaml" | jq -cS .)" == '{"app.kubernetes.io/instance":"postgresql","app.kubernetes.io/name":"kuberploy-postgresql"}' ]]
[[ "$(yq eval-all 'select(.kind == "Service") | .spec.type' "${kp_tmp}/managed.yaml" | tail -1)" == "ClusterIP" ]]
[[ "$(yq eval-all 'select(.kind == "StatefulSet") | .spec.replicas' "${kp_tmp}/managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all 'select(.kind == "StatefulSet") | .spec.persistentVolumeClaimRetentionPolicy.whenDeleted' "${kp_tmp}/managed.yaml" | tail -1)" == "Retain" ]]
[[ "$(yq eval-all 'select(.kind == "StatefulSet") | .spec.template.spec.automountServiceAccountToken' "${kp_tmp}/managed.yaml" | tail -1)" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "StatefulSet") | .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem' "${kp_tmp}/managed.yaml" | tail -1)" == "true" ]]
rg -F 'docker.io/library/postgres:18.4-alpine3.24' "${kp_tmp}/managed.yaml" >/dev/null
rg -F -- '--data-checksums --auth-host=scram-sha-256' "${kp_tmp}/managed.yaml" >/dev/null
rg -F 'helm.sh/resource-policy: keep' "${kp_tmp}/managed.yaml" >/dev/null
rg -F 'kubernetes.io/metadata.name: kuberploy-system' "${kp_tmp}/managed.yaml" >/dev/null
[[ "$(yq eval-all '[select(.kind == "StatefulSet" or .kind == "Service" or .kind == "ServiceAccount" or .kind == "NetworkPolicy" or .kind == "Namespace")] | length' "${kp_tmp}/adopted.yaml" | tail -1)" == "0" ]]

kp_reject() {
  local kp_reason="$1"
  shift
  if helm template invalid "${kp_chart}" --namespace kuberploy-system -f "${kp_managed}" "$@" >/dev/null 2>&1; then
    printf 'unsafe PostgreSQL render accepted: %s\n' "${kp_reason}" >&2
    exit 1
  fi
}

kp_reject 'different image version' --set-string postgresqlFoundation.image.reference=docker.io/library/postgres:18.4-alpine3.23
kp_reject 'disabled policy' --set postgresqlFoundation.networkPolicy.enabled=false
kp_reject 'wrong client namespace' --set-string postgresqlFoundation.networkPolicy.controlPlaneNamespace=default
kp_reject 'changed port' --set postgresqlFoundation.service.port=15432
kp_reject 'deletable PVC' --set postgresqlFoundation.storage.keepPVC=false
kp_reject 'hostPath-style access mode' --set-string postgresqlFoundation.storage.accessModes[0]=ReadWriteMany
kp_reject 'missing auth secret' --set-string postgresqlFoundation.auth.existingSecret=
kp_reject 'excessive connections' --set postgresqlFoundation.database.maxConnections=10000
kp_reject 'unknown injected value' --set-string postgresqlFoundation.extraContainers[0].name=attacker

if helm template invalid "${kp_chart}" --namespace another-namespace -f "${kp_managed}" >/dev/null 2>&1; then
  printf 'PostgreSQL chart accepted a namespace outside its boundary\n' >&2
  exit 1
fi

printf 'PostgreSQL chart render and mutation validation passed\n'
