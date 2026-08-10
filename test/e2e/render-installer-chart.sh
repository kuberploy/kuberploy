#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
kp_chart="${kp_root}/charts/kuberploy-installer"
kp_managed="${kp_chart}/testdata/managed-values.yaml"
kp_adopted="${kp_chart}/testdata/adopted-values.yaml"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-installer-render.XXXXXX")"
kp_cleanup() {
  [[ -n "${kp_tmp:-}" && "${kp_tmp}" == *kuberploy-installer-render.* ]] && rm -rf -- "${kp_tmp}"
}
trap kp_cleanup EXIT

for kp_tool in helm yq jq rg diff shasum cut tar sort; do
  command -v "${kp_tool}" >/dev/null 2>&1 || { printf 'missing tool: %s\n' "${kp_tool}" >&2; exit 1; }
done

jq -e . "${kp_chart}/values.schema.json" >/dev/null
[[ -f "${kp_chart}/Chart.lock" ]]
[[ -f "${kp_chart}/charts/kuberploy-argocd-0.1.0-rc.3.tgz" ]]
[[ -f "${kp_chart}/charts/kuberploy-valkey-0.1.0-rc.3.tgz" ]]
helm dependency list "${kp_chart}" | rg -F 'ok' >/dev/null

helm lint "${kp_chart}" >/dev/null
[[ -z "$(helm template disabled "${kp_chart}")" ]]

helm lint "${kp_chart}" --namespace kuberploy-system -f "${kp_managed}" >/dev/null
helm template kuberploy-installer "${kp_chart}" --namespace kuberploy-system -f "${kp_managed}" >"${kp_tmp}/managed.yaml"
helm template kuberploy-installer "${kp_chart}" --namespace kuberploy-system -f "${kp_managed}" >"${kp_tmp}/managed-again.yaml"
yq eval-all 'del(select(.kind == "Secret").data)' "${kp_tmp}/managed.yaml" >"${kp_tmp}/managed-normalized.yaml"
yq eval-all 'del(select(.kind == "Secret").data)' "${kp_tmp}/managed-again.yaml" >"${kp_tmp}/managed-again-normalized.yaml"
diff -u "${kp_tmp}/managed-normalized.yaml" "${kp_tmp}/managed-again-normalized.yaml"

[[ "$(yq eval-all '[select(.kind == "Application")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .metadata.name' "${kp_tmp}/managed.yaml")" == "kuberploy-postgresql" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.destination.namespace' "${kp_tmp}/managed.yaml")" == "kuberploy-system" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.source.targetRevision' "${kp_tmp}/managed.yaml")" == "v0.1.0-rc.3" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.source.helm.valuesObject.postgresqlFoundation.managed' "${kp_tmp}/managed.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.source.helm.valuesObject.postgresqlFoundation.adoptExisting' "${kp_tmp}/managed.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.source.helm.valueFiles[0]' "${kp_tmp}/managed.yaml")" == "../../examples/installer/postgresql.yaml" ]]
[[ "$(yq eval-all -o=json 'select(.kind == "Application") | .spec.ignoreDifferences[0].jsonPointers' "${kp_tmp}/managed.yaml" | jq -c .)" == '["/spec/volumeClaimTemplates/0/apiVersion","/spec/volumeClaimTemplates/0/kind","/spec/volumeClaimTemplates/0/spec/volumeMode","/spec/volumeClaimTemplates/0/status"]' ]]
if yq eval-all 'select(.kind == "Application") | .spec.source.helm.valuesObject' "${kp_tmp}/managed.yaml" | rg -q 'kuberploy-postgresql-auth'; then
  printf 'installer copied child configuration into the Application instead of using a pinned value file\n' >&2
  exit 1
fi
[[ "$(yq eval-all 'select(.kind == "Application") | .metadata.annotations."argocd.argoproj.io/sync-wave"' "${kp_tmp}/managed.yaml")" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .metadata.annotations."helm.sh/resource-policy"' "${kp_tmp}/managed.yaml")" == "keep" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | has(.metadata.finalizers)' "${kp_tmp}/managed.yaml")" == "false" ]]
[[ "$(yq eval-all '[select(.kind == "Job")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Secret")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "6" ]]
[[ "$(yq eval-all '[select(.kind == "Namespace" and .metadata.name == "argocd")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.namespace == "argocd")] | length > 0' "${kp_tmp}/managed.yaml" | tail -1)" == "true" ]]

yq eval-all 'select(.kind == "Application") | .spec.source.helm.valuesObject' "${kp_tmp}/managed.yaml" >"${kp_tmp}/postgresql-values.yaml"
helm template postgresql "${kp_root}/charts/kuberploy-postgresql" --namespace kuberploy-system \
  -f "${kp_root}/examples/installer/postgresql.yaml" -f "${kp_tmp}/postgresql-values.yaml" >/dev/null

helm template kuberploy-installer "${kp_chart}" --namespace kuberploy-system -f "${kp_managed}" \
  --set bootstrap.controlPlaneToken.mode=generated \
  --set-string bootstrap.controlPlaneToken.kubeAPIServerCIDRs[0]=10.43.0.1/32 \
  --set components.controlPlane.enabled=true \
  --set components.controlPlane.mode=managed \
  --set-string components.controlPlane.expectedPackageVersion=0.1.0-rc.3 \
  --set components.valkey.enabled=true \
  --set components.valkey.mode=managed \
  --set-string components.valkey.expectedPackageVersion=0.1.0-rc.3 \
  >"${kp_tmp}/control-plane.yaml"
[[ "$(yq eval-all '[select(.kind == "Application")] | length' "${kp_tmp}/control-plane.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.source.helm.valuesObject.global.requireImageDigest' "${kp_tmp}/control-plane.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.source.helm.valuesObject.builder.enabled' "${kp_tmp}/control-plane.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .metadata.annotations."argocd.argoproj.io/sync-wave"' "${kp_tmp}/control-plane.yaml")" == "20" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.source.helm.valuesObject.config.valkey.mode' "${kp_tmp}/control-plane.yaml")" == "managed" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.source.helm.valuesObject.config.valkey.secretRef.apiCacheUsernameKey' "${kp_tmp}/control-plane.yaml")" == "api-cache-username" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.source.helm.valuesObject.config.valkey.secretRef.apiCachePasswordKey' "${kp_tmp}/control-plane.yaml")" == "api-cache-password" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.source.helm.valuesObject.config.valkey.secretRef.apiLimiterUsernameKey' "${kp_tmp}/control-plane.yaml")" == "api-limiter-username" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.source.helm.valuesObject.config.valkey.secretRef.apiLimiterPasswordKey' "${kp_tmp}/control-plane.yaml")" == "api-limiter-password" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.source.helm.valuesObject.config.valkey.secretRef.publisherUsernameKey' "${kp_tmp}/control-plane.yaml")" == "outbox-publisher-username" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.source.helm.valuesObject.config.valkey.secretRef.publisherPasswordKey' "${kp_tmp}/control-plane.yaml")" == "outbox-publisher-password" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.source.helm.valuesObject.config.valkey.secretRef.consumerUsernameKey' "${kp_tmp}/control-plane.yaml")" == "worker-consumer-username" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.source.helm.valuesObject.config.valkey.secretRef.consumerPasswordKey' "${kp_tmp}/control-plane.yaml")" == "worker-consumer-password" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.source.helm.valuesObject.config.bootstrapSecret.generate' "${kp_tmp}/control-plane.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.source.helm.valuesObject.networkPolicy.kubeAPIServerCIDRs[0]' "${kp_tmp}/control-plane.yaml")" == "10.43.0.1/32" ]]

yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.source.helm.valuesObject' \
  "${kp_tmp}/control-plane.yaml" >"${kp_tmp}/control-plane-values.yaml"
helm template kuberploy "${kp_root}/charts/kuberploy" --namespace kuberploy-system \
  -f "${kp_tmp}/control-plane-values.yaml" \
  --set-string components.api.image.reference=example.invalid/api@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
  --set-string components.worker.image.reference=example.invalid/worker@sha256:2222222222222222222222222222222222222222222222222222222222222222 \
  --set-string components.web.image.reference=example.invalid/web@sha256:3333333333333333333333333333333333333333333333333333333333333333 \
  --set-string upgrade.image.reference=example.invalid/upgrader@sha256:4444444444444444444444444444444444444444444444444444444444444444 \
  >"${kp_tmp}/control-plane-child.yaml"
[[ "$(yq eval-all '[select(.kind == "Deployment") | .spec.template.spec.containers[].env[]? | select(.name == "KUBERPLOY_VALKEY_USERNAME" or .name == "KUBERPLOY_VALKEY_PASSWORD")] | length' "${kp_tmp}/control-plane-child.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_VALKEY_CACHE_USERNAME") | .valueFrom.secretKeyRef.key' "${kp_tmp}/control-plane-child.yaml")" == "api-cache-username" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_VALKEY_LIMITER_PASSWORD") | .valueFrom.secretKeyRef.key' "${kp_tmp}/control-plane-child.yaml")" == "api-limiter-password" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_VALKEY_PUBLISHER_USERNAME") | .valueFrom.secretKeyRef.key' "${kp_tmp}/control-plane-child.yaml")" == "outbox-publisher-username" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_VALKEY_CONSUMER_PASSWORD") | .valueFrom.secretKeyRef.key' "${kp_tmp}/control-plane-child.yaml")" == "worker-consumer-password" ]]

kp_all_args=()
for kp_component in controlPlane postgresql valkey edge certManager externalDNS externalSecrets sealedSecrets monitoring builder registry; do
  kp_all_args+=(--set "components.${kp_component}.enabled=true")
  kp_all_args+=(--set "components.${kp_component}.mode=managed")
  kp_all_args+=(--set-string "components.${kp_component}.expectedPackageVersion=0.1.0-rc.3")
done
kp_all_args+=(--set bootstrap.controlPlaneToken.mode=generated)
kp_all_args+=(--set-string bootstrap.controlPlaneToken.kubeAPIServerCIDRs[0]=10.43.0.1/32)
helm template kuberploy-installer "${kp_chart}" --namespace kuberploy-system -f "${kp_managed}" "${kp_all_args[@]}" >"${kp_tmp}/all-components.yaml"
[[ "$(yq eval-all '[select(.kind == "Application")] | length' "${kp_tmp}/all-components.yaml" | tail -1)" == "10" ]]
[[ "$(yq eval-all '[select(.kind == "AppProject")] | length' "${kp_tmp}/all-components.yaml" | tail -1)" == "10" ]]

helm lint "${kp_chart}" --namespace kuberploy-system -f "${kp_adopted}" >/dev/null
helm template kuberploy-installer "${kp_chart}" --namespace kuberploy-system -f "${kp_adopted}" >"${kp_tmp}/adopted.yaml"
[[ "$(yq eval-all '[select(.kind == "Application")] | length' "${kp_tmp}/adopted.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .metadata.annotations."kuberploy.io/installation-mode"' "${kp_tmp}/adopted.yaml")" == "adopted" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.source.helm.valuesObject.edge.traefik.managed' "${kp_tmp}/adopted.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.source.helm.valuesObject.edge.traefik.adoptExisting' "${kp_tmp}/adopted.yaml")" == "true" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.namespace == "argocd")] | length' "${kp_tmp}/adopted.yaml" | tail -1)" == "0" ]]

# Existing standalone wrapper tests render the adopted edge values against its
# checksum-pinned upstream archive. Here the installer test verifies the exact
# fenced values without resolving that second-level dependency.

kp_expect_reject() {
  local kp_reason="${1:?reason required}"
  shift
  if helm template kuberploy-installer "${kp_chart}" --namespace kuberploy-system -f "${kp_managed}" "$@" >/dev/null 2>&1; then
    printf 'installer accepted invalid configuration: %s\n' "${kp_reason}" >&2
    exit 1
  fi
}

kp_expect_reject "mutable source revision" --set-string source.targetRevision=main
kp_expect_reject "opaque hash source revision" --set-string source.targetRevision=1111111111111111111111111111111111111111
kp_expect_reject "unprefixed source revision" --set-string source.targetRevision=0.1.0-rc.3
kp_expect_reject "missing package version" --set-string components.postgresql.expectedPackageVersion=
kp_expect_reject "unsupported adopted monitoring" --set components.postgresql.enabled=false --set components.postgresql.mode=disabled --set-string components.postgresql.expectedPackageVersion= --set-json components.postgresql.valueFiles=[] --set components.monitoring.enabled=true --set components.monitoring.mode=adopted --set components.monitoring.adoptionConfirmed=true --set-string components.monitoring.expectedPackageVersion=0.1.0-rc.3
kp_expect_reject "value file outside pinned installer directory" --set-string components.postgresql.valueFiles[0]=../../secrets.yaml
kp_expect_reject "value file traversal below installer prefix" --set-string components.postgresql.valueFiles[0]=../../examples/installer/../../../secrets.yaml
kp_expect_reject "arbitrary inline child values" --set components.postgresql.values.password=do-not-store
kp_expect_reject "disabled Argo with active child" --set bootstrap.argoCD.enabled=false --set bootstrap.argoCD.mode=disabled
kp_expect_reject "managed Argo without installer-owned Valkey" --set bootstrap.valkey.enabled=false
kp_expect_reject "control plane without explicit bootstrap token authority" --set components.controlPlane.enabled=true --set components.controlPlane.mode=managed --set-string components.controlPlane.expectedPackageVersion=0.1.0-rc.3 --set components.valkey.enabled=true --set components.valkey.mode=managed --set-string components.valkey.expectedPackageVersion=0.1.0-rc.3
kp_expect_reject "generated token without exact API CIDR" --set bootstrap.controlPlaneToken.mode=generated --set components.controlPlane.enabled=true --set components.controlPlane.mode=managed --set-string components.controlPlane.expectedPackageVersion=0.1.0-rc.3 --set components.valkey.enabled=true --set components.valkey.mode=managed --set-string components.valkey.expectedPackageVersion=0.1.0-rc.3
kp_expect_reject "precreated token with dormant API CIDR" --set bootstrap.controlPlaneToken.mode=precreated --set-string bootstrap.controlPlaneToken.kubeAPIServerCIDRs[0]=10.43.0.1/32 --set components.controlPlane.enabled=true --set components.controlPlane.mode=managed --set-string components.controlPlane.expectedPackageVersion=0.1.0-rc.3 --set components.valkey.enabled=true --set components.valkey.mode=managed --set-string components.valkey.expectedPackageVersion=0.1.0-rc.3

if helm template kuberploy-installer "${kp_chart}" --namespace argocd -f "${kp_managed}" >/dev/null 2>&1; then
  printf 'installer accepted the wrong bootstrap namespace\n' >&2
  exit 1
fi

printf 'installer chart render checks passed\n'
