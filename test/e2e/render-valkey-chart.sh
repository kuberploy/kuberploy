#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
source "${kp_root}/scripts/helm/download-locked-artifact.sh"
kp_source="${kp_root}/charts/kuberploy-valkey"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-valkey-render.XXXXXX")"

kp_cleanup() {
  if [[ -n "${kp_tmp:-}" && "${kp_tmp}" == *"/kuberploy-valkey-render."* ]]; then
    rm -rf -- "${kp_tmp}"
  fi
}
trap kp_cleanup EXIT

for kp_tool in curl helm python3 rg shasum yq; do
  command -v "${kp_tool}" >/dev/null 2>&1 || {
    printf 'missing tool: %s\n' "${kp_tool}" >&2
    exit 1
  }
done

kp_chart="${kp_tmp}/kuberploy-valkey"
cp -R "${kp_source}" "${kp_chart}"
mkdir -p "${kp_chart}/charts"
read -r kp_checksum kp_filename kp_url < <(awk 'NF && $1 !~ /^#/ {print}' "${kp_source}/testdata/upstream-artifacts.lock")
[[ "${kp_checksum}" == "6aa9f2e423642cae84ed6a9798cdfd0faf2e347290ce7b3e4c393333a79743f8" ]]
kp_download_locked_artifact "${kp_url}" "${kp_filename}" "${kp_chart}/charts/${kp_filename}"
[[ "$(shasum -a 256 "${kp_chart}/charts/${kp_filename}" | awk '{print $1}')" == "${kp_checksum}" ]]
rg -F "${kp_checksum}" "${kp_root}/DEPENDENCIES.md" >/dev/null
helm dependency list "${kp_chart}" | rg -F 'ok' >/dev/null
python3 -m json.tool "${kp_chart}/values.schema.json" >/dev/null

kp_managed="${kp_source}/testdata/managed-values.yaml"
kp_adopted="${kp_source}/testdata/adopted-values.yaml"
helm lint "${kp_chart}" --namespace kuberploy-system -f "${kp_managed}" >/dev/null
helm lint "${kp_chart}" --namespace kuberploy-system -f "${kp_adopted}" >/dev/null
helm template valkey "${kp_chart}" --namespace kuberploy-system --skip-tests -f "${kp_managed}" >"${kp_tmp}/managed.yaml"
helm template valkey "${kp_chart}" --namespace kuberploy-system --skip-tests -f "${kp_managed}" >"${kp_tmp}/managed-again.yaml"
helm template valkey "${kp_chart}" --namespace kuberploy-system --skip-tests -f "${kp_adopted}" >"${kp_tmp}/adopted.yaml"
diff -u "${kp_tmp}/managed.yaml" "${kp_tmp}/managed-again.yaml" >/dev/null

[[ "$(yq eval-all '[select(.kind == "Namespace")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "PersistentVolumeClaim")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "NetworkPolicy" and .metadata.name == "valkey-default-deny") | .spec.podSelector.matchLabels' "${kp_tmp}/managed.yaml" | jq -cS .)" == '{"app.kubernetes.io/instance":"valkey","app.kubernetes.io/name":"valkey"}' ]]
[[ "$(yq eval-all '[select(.kind == "Deployment") | .spec.template.spec.containers[0].image] | unique | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "docker.io/valkey/valkey:9.1.1" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.automountServiceAccountToken' "${kp_tmp}/managed.yaml" | tail -1)" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem' "${kp_tmp}/managed.yaml" | tail -1)" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Service") | .spec.type' "${kp_tmp}/managed.yaml" | tail -1)" == "ClusterIP" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" or .kind == "PersistentVolumeClaim" or .kind == "NetworkPolicy")] | length' "${kp_tmp}/adopted.yaml" | tail -1)" == "0" ]]
rg -F 'maxmemory-policy noeviction' "${kp_tmp}/managed.yaml" >/dev/null
rg -F 'appendonly yes' "${kp_tmp}/managed.yaml" >/dev/null
rg -F 'secretName: kuberploy-valkey-auth' "${kp_tmp}/managed.yaml" >/dev/null
rg -F 'argocd-password' "${kp_tmp}/managed.yaml" >/dev/null
rg -F 'apiCacheUsername: "api-cache"' "${kp_tmp}/managed.yaml" >/dev/null
rg -F 'apiLimiterUsername: "api-limiter"' "${kp_tmp}/managed.yaml" >/dev/null
rg -F 'outboxPublisherUsername: "outbox-publisher"' "${kp_tmp}/managed.yaml" >/dev/null
rg -F 'workerConsumerUsername: "worker-consumer"' "${kp_tmp}/managed.yaml" >/dev/null
rg -F '~kp:v1:cache:* +get +set +del +ping' "${kp_tmp}/managed.yaml" >/dev/null
rg -F '~kp:v1:limit:* +evalsha +eval +script|load +incrby +pttl +pexpire +ping' "${kp_tmp}/managed.yaml" >/dev/null
rg -F '~kp:v1:work:git-write ~kp:v1:work:dataset-id +xadd +get +set +ping' "${kp_tmp}/managed.yaml" >/dev/null
rg -F '~kp:v1:work:* +xgroup +xreadgroup +xautoclaim +xack +ping' "${kp_tmp}/managed.yaml" >/dev/null
rg -F 'kubernetes.io/metadata.name: argocd' "${kp_tmp}/managed.yaml" >/dev/null

kp_reject() {
  local kp_reason="$1"
  shift
  if helm template invalid "${kp_chart}" --namespace kuberploy-system --skip-tests -f "${kp_managed}" "$@" >/dev/null 2>&1; then
    printf 'unsafe Valkey render accepted: %s\n' "${kp_reason}" >&2
    exit 1
  fi
}

kp_reject 'different image version' --set-string valkey.image.tag=9.1.0
kp_reject 'public service' --set-string valkey.service.type=LoadBalancer
kp_reject 'disabled auth' --set valkey.auth.enabled=false
kp_reject 'inline password' --set-string valkey.auth.aclUsers.default.password=plaintext
kp_reject 'cache can publish work' --set-string valkey.auth.aclUsers.api-cache.permissions='~kp:* &* +@all'
kp_reject 'limiter can read cache' --set-string valkey.auth.aclUsers.api-limiter.permissions='~kp:* +@all'
kp_reject 'publisher can consume work' --set-string valkey.auth.aclUsers.outbox-publisher.permissions='~kp:v1:work:* +xadd +xreadgroup +ping'
kp_reject 'consumer can publish work' --set-string valkey.auth.aclUsers.worker-consumer.permissions='~kp:v1:work:* +xadd +xreadgroup +ping'
kp_reject 'removed Argo CD identity' --set valkey.auth.aclUsers.argocd=null
kp_reject 'broadened Argo CD commands' --set-string valkey.auth.aclUsers.argocd.permissions='~* &* +@all'
kp_reject 'hostPath storage' --set-string valkey.dataStorage.hostPath=/tmp/valkey
kp_reject 'eviction policy change' --set-string valkey.valkeyConfig='maxmemory-policy allkeys-lru'
kp_reject 'injected sidecar' --set-string valkey.extraContainers[0].name=attacker
kp_reject 'disabled network policy' --set valkeyFoundation.networkPolicy.enabled=false
kp_reject 'metrics sidecar' --set valkey.metrics.enabled=true
kp_reject 'TLS mode unsupported by current client' --set valkey.tls.enabled=true

if helm template invalid "${kp_chart}" --namespace another-namespace --skip-tests -f "${kp_managed}" >/dev/null 2>&1; then
  printf 'Valkey chart accepted a namespace outside its boundary\n' >&2
  exit 1
fi

printf 'Valkey chart render and mutation validation passed\n'
