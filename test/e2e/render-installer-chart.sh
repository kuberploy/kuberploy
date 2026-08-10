#!/usr/bin/env bash

set -Eeuo pipefail

kp_failure() {
  printf 'installer chart check failed at line %s: %s\n' "${1}" "${2}" >&2
}
trap 'kp_failure "${LINENO}" "${BASH_COMMAND}"' ERR

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
[[ -f "${kp_chart}/charts/kuberploy-argocd-0.1.0-rc.17.tgz" ]]
[[ -f "${kp_chart}/charts/kuberploy-valkey-0.1.0-rc.17.tgz" ]]
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
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.sources[0].targetRevision' "${kp_tmp}/managed.yaml")" == "0.1.0-rc.17" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.sources[0].repoURL' "${kp_tmp}/managed.yaml")" == "ghcr.io/kuberploy/charts" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.sources[0].chart' "${kp_tmp}/managed.yaml")" == "kuberploy-postgresql" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.sources[1].repoURL' "${kp_tmp}/managed.yaml")" == "https://github.com/kuberploy/kuberploy.git" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.sources[1].targetRevision' "${kp_tmp}/managed.yaml")" == "v0.1.0-rc.17" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.sources[1].ref' "${kp_tmp}/managed.yaml")" == "values" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | has(.spec.source)' "${kp_tmp}/managed.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.sources[0].helm.valuesObject.postgresqlFoundation.managed' "${kp_tmp}/managed.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.sources[0].helm.valuesObject.postgresqlFoundation.adoptExisting' "${kp_tmp}/managed.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.sources[0].helm.valueFiles[0]' "${kp_tmp}/managed.yaml")" == '$values/examples/installer/postgresql.yaml' ]]
[[ "$(yq eval-all -o=json 'select(.kind == "Application") | .spec.ignoreDifferences[0].jsonPointers' "${kp_tmp}/managed.yaml" | jq -c .)" == '["/spec/volumeClaimTemplates/0/apiVersion","/spec/volumeClaimTemplates/0/kind","/spec/volumeClaimTemplates/0/metadata/labels/helm.sh~1chart","/spec/volumeClaimTemplates/0/spec/volumeMode","/spec/volumeClaimTemplates/0/status"]' ]]
if yq eval-all 'select(.kind == "Application") | .spec.sources[0].helm.valuesObject' "${kp_tmp}/managed.yaml" | rg -q 'kuberploy-postgresql-auth'; then
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

yq eval-all 'select(.kind == "Application") | .spec.sources[0].helm.valuesObject' "${kp_tmp}/managed.yaml" >"${kp_tmp}/postgresql-values.yaml"
helm template postgresql "${kp_root}/charts/kuberploy-postgresql" --namespace kuberploy-system \
  -f "${kp_root}/examples/installer/postgresql.yaml" -f "${kp_tmp}/postgresql-values.yaml" >/dev/null

helm template kuberploy-installer "${kp_chart}" --namespace kuberploy-system -f "${kp_managed}" \
  --set bootstrap.controlPlaneToken.mode=generated \
  --set-string bootstrap.controlPlaneToken.kubeAPIServerCIDRs[0]=10.43.0.1/32 \
  --set components.controlPlane.enabled=true \
  --set components.controlPlane.mode=managed \
  --set-string components.controlPlane.expectedPackageVersion=0.1.0-rc.17 \
  --set components.valkey.enabled=true \
  --set components.valkey.mode=managed \
  --set-string components.valkey.expectedPackageVersion=0.1.0-rc.17 \
  >"${kp_tmp}/control-plane.yaml"
[[ "$(yq eval-all '[select(.kind == "Application")] | length' "${kp_tmp}/control-plane.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.global.requireImageDigest' "${kp_tmp}/control-plane.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.builder.enabled' "${kp_tmp}/control-plane.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .metadata.annotations."argocd.argoproj.io/sync-wave"' "${kp_tmp}/control-plane.yaml")" == "20" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.valkey.mode' "${kp_tmp}/control-plane.yaml")" == "managed" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.valkey.secretRef.apiCacheUsernameKey' "${kp_tmp}/control-plane.yaml")" == "api-cache-username" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.valkey.secretRef.apiCachePasswordKey' "${kp_tmp}/control-plane.yaml")" == "api-cache-password" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.valkey.secretRef.apiLimiterUsernameKey' "${kp_tmp}/control-plane.yaml")" == "api-limiter-username" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.valkey.secretRef.apiLimiterPasswordKey' "${kp_tmp}/control-plane.yaml")" == "api-limiter-password" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.valkey.secretRef.publisherUsernameKey' "${kp_tmp}/control-plane.yaml")" == "outbox-publisher-username" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.valkey.secretRef.publisherPasswordKey' "${kp_tmp}/control-plane.yaml")" == "outbox-publisher-password" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.valkey.secretRef.consumerUsernameKey' "${kp_tmp}/control-plane.yaml")" == "worker-consumer-username" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.valkey.secretRef.consumerPasswordKey' "${kp_tmp}/control-plane.yaml")" == "worker-consumer-password" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.bootstrapSecret.generate' "${kp_tmp}/control-plane.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.networkPolicy.kubeAPIServerCIDRs[0]' "${kp_tmp}/control-plane.yaml")" == "10.43.0.1/32" ]]

yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject' \
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
  kp_all_args+=(--set-string "components.${kp_component}.expectedPackageVersion=0.1.0-rc.17")
done
kp_all_args+=(--set bootstrap.controlPlaneToken.mode=generated)
kp_all_args+=(--set-string bootstrap.controlPlaneToken.kubeAPIServerCIDRs[0]=10.43.0.1/32)
kp_all_args+=(--set-string cluster.kubeAPIServerCIDRs[0]=10.43.0.1/32)
kp_all_args+=(--set publicEndpoint.enabled=true)
kp_all_args+=(--set-string publicEndpoint.hostname=kuberploy.example.com)
kp_all_args+=(--set publicEndpoint.tls.enabled=true)
kp_all_args+=(--set-string publicEndpoint.tls.secretName=kuberploy-platform-tls)
kp_all_args+=(--set-string publicEndpoint.tls.clusterIssuerName=kuberploy-letsencrypt-production)
kp_all_args+=(--set-string publicEndpoint.tls.accountEmail=platform@example.com)
kp_all_args+=(--set integrations.registry.enabled=true)
kp_all_args+=(--set-string integrations.registry.authSecretName=registry-auth)
kp_all_args+=(--set-string integrations.registry.secretRevision=v1)
kp_all_args+=(--set-string integrations.registry.exposureMode=ingress)
kp_all_args+=(--set-string integrations.registry.endpoint=registry.example.com)
kp_all_args+=(--set-string integrations.registry.tlsSecretName=registry-tls)
kp_all_args+=(--set-string integrations.registry.clusterIssuerName=kuberploy-letsencrypt-production)
helm template kuberploy-installer "${kp_chart}" --namespace kuberploy-system -f "${kp_managed}" "${kp_all_args[@]}" >"${kp_tmp}/all-components.yaml"
[[ "$(yq eval-all '[select(.kind == "Application")] | length' "${kp_tmp}/all-components.yaml" | tail -1)" == "10" ]]
[[ "$(yq eval-all '[select(.kind == "AppProject")] | length' "${kp_tmp}/all-components.yaml" | tail -1)" == "10" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-registry") | .spec.sources[0].helm.valuesObject.auth.mode' "${kp_tmp}/all-components.yaml")" == "htpasswd" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-registry") | .spec.sources[0].helm.valuesObject.auth.existingSecret' "${kp_tmp}/all-components.yaml")" == "registry-auth" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-registry") | .spec.sources[0].helm.valuesObject.auth.secretRevision' "${kp_tmp}/all-components.yaml")" == "v1" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-registry") | .spec.sources[0].helm.valuesObject.exposure.endpoint' "${kp_tmp}/all-components.yaml")" == "registry.example.com" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-registry") | .spec.sources[0].helm.valuesObject.exposure.clusterIssuerName' "${kp_tmp}/all-components.yaml")" == "kuberploy-letsencrypt-production" ]]
[[ "$(yq eval-all -o=json 'select(.kind == "Application" and .metadata.name == "kuberploy-registry") | .spec.sources[0].helm.valuesObject.networkPolicy.allowedNamespaces' "${kp_tmp}/all-components.yaml" | jq -c .)" == '["kuberploy-build-dind","kuberploy-system"]' ]]

helm template kuberploy-installer "${kp_chart}" --namespace kuberploy-system \
  -f "${kp_managed}" \
  -f "${kp_chart}/testdata/registry-loadbalancer-values.yaml" \
  >"${kp_tmp}/registry-loadbalancer.yaml"
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-registry") | .spec.sources[0].helm.valuesObject.exposure.mode' "${kp_tmp}/registry-loadbalancer.yaml")" == "loadBalancer" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-registry") | .spec.sources[0].helm.valuesObject.exposure.loadBalancer.class' "${kp_tmp}/registry-loadbalancer.yaml")" == "example.com/private" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-registry") | .spec.sources[0].helm.valuesObject.exposure.loadBalancer.annotations."example.com/internal-load-balancer"' "${kp_tmp}/registry-loadbalancer.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-registry") | .spec.sources[0].helm.valuesObject.exposure.loadBalancer.sourceRanges[0]' "${kp_tmp}/registry-loadbalancer.yaml")" == "10.20.0.0/16" ]]

kp_platform_args=(
  --set bootstrap.controlPlaneToken.mode=generated
  --set-string bootstrap.controlPlaneToken.kubeAPIServerCIDRs[0]=10.43.0.1/32
  --set-string cluster.kubeAPIServerCIDRs[0]=10.43.0.1/32
  --set components.controlPlane.enabled=true
  --set components.controlPlane.mode=managed
  --set-string components.controlPlane.expectedPackageVersion=0.1.0-rc.17
  --set components.valkey.enabled=true
  --set components.valkey.mode=managed
  --set-string components.valkey.expectedPackageVersion=0.1.0-rc.17
  --set components.edge.enabled=true
  --set components.edge.mode=managed
  --set-string components.edge.expectedPackageVersion=0.1.0-rc.17
  --set components.certManager.enabled=true
  --set components.certManager.mode=managed
  --set-string components.certManager.expectedPackageVersion=0.1.0-rc.17
  --set components.sealedSecrets.enabled=true
  --set components.sealedSecrets.mode=managed
  --set-string components.sealedSecrets.expectedPackageVersion=0.1.0-rc.17
  --set components.monitoring.enabled=true
  --set components.monitoring.mode=managed
  --set-string components.monitoring.expectedPackageVersion=0.1.0-rc.17
  --set publicEndpoint.enabled=true
  --set-string publicEndpoint.hostname=kuberploy.example.com
)
helm template kuberploy-installer "${kp_chart}" --namespace kuberploy-system -f "${kp_managed}" "${kp_platform_args[@]}" >"${kp_tmp}/platform.yaml"
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-edge") | .spec.sources[0].helm.valuesObject.edge.networkPolicy.kubeAPIServerCIDRs[0]' "${kp_tmp}/platform.yaml")" == "10.43.0.1/32" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-edge") | .spec.sources[0].helm.valuesObject.edge.namespace.create' "${kp_tmp}/platform.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-cert-manager") | .spec.sources[0].helm.valuesObject.foundation.networkPolicy.kubeAPIServerCIDRs[0]' "${kp_tmp}/platform.yaml")" == "10.43.0.1/32" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-sealed-secrets") | .spec.sources[0].helm.valuesObject.secretFoundation.networkPolicy.kubeAPIServerCIDRs[0]' "${kp_tmp}/platform.yaml")" == "10.43.0.1/32" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.publicURL' "${kp_tmp}/platform.yaml")" == "http://kuberploy.example.com" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.ingress.enabled' "${kp_tmp}/platform.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.monitoring.mode' "${kp_tmp}/platform.yaml")" == "managed" ]]
[[ "$(yq eval-all 'select(.kind == "AppProject" and .metadata.name == "kuberploy-cert-manager") | .spec.sourceRepos[0]' "${kp_tmp}/platform.yaml")" == "ghcr.io/kuberploy/charts" ]]
[[ "$(yq eval-all 'select(.kind == "AppProject" and .metadata.name == "kuberploy-cert-manager") | .spec.sourceRepos[1]' "${kp_tmp}/platform.yaml")" == "https://github.com/kuberploy/kuberploy.git" ]]
[[ "$(yq eval-all 'select(.kind == "AppProject" and .metadata.name == "kuberploy-cert-manager") | .spec.destinations[1].namespace' "${kp_tmp}/platform.yaml")" == "kube-system" ]]
[[ "$(yq eval-all 'select(.kind == "AppProject" and .metadata.name == "kuberploy-control-plane") | .spec.destinations[1].namespace' "${kp_tmp}/platform.yaml")" == "kuberploy-monitoring" ]]
[[ "$(yq eval-all 'select(.kind == "AppProject" and .metadata.name == "kuberploy-edge") | .spec.orphanedResources.warn' "${kp_tmp}/platform.yaml")" == "false" ]]

helm template kuberploy-installer "${kp_chart}" --namespace kuberploy-system -f "${kp_managed}" "${kp_platform_args[@]}" \
  --set publicEndpoint.tls.enabled=true \
  --set-string publicEndpoint.tls.secretName=kuberploy-platform-tls \
  --set-string publicEndpoint.tls.clusterIssuerName=kuberploy-letsencrypt-production \
  --set-string publicEndpoint.tls.accountEmail=platform@example.com \
  >"${kp_tmp}/platform-tls.yaml"
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.publicURL' "${kp_tmp}/platform-tls.yaml")" == "https://kuberploy.example.com" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.ingress.tls.issuerName' "${kp_tmp}/platform-tls.yaml")" == "kuberploy-letsencrypt-production" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-cert-manager") | .spec.sources[0].helm.valuesObject.foundation.issuers.production.email' "${kp_tmp}/platform-tls.yaml")" == "platform@example.com" ]]

yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject' \
  "${kp_tmp}/platform-tls.yaml" >"${kp_tmp}/platform-tls-control-values.yaml"
helm template kuberploy "${kp_root}/charts/kuberploy" --namespace kuberploy-system \
  -f "${kp_tmp}/platform-tls-control-values.yaml" >"${kp_tmp}/platform-tls-control.yaml"
[[ "$(yq eval-all 'select(.kind == "Ingress" and .metadata.name == "kuberploy") | .metadata.annotations."cert-manager.io/cluster-issuer"' "${kp_tmp}/platform-tls-control.yaml")" == "kuberploy-letsencrypt-production" ]]

kp_github_args=(
  --set components.builder.enabled=true
  --set components.builder.mode=managed
  --set-string components.builder.expectedPackageVersion=0.1.0-rc.17
  --set integrations.github.enabled=true
  --set integrations.github.appID=123456
  --set-string integrations.github.clientID=Iv1_KuberployClient
  --set-string integrations.github.appSlug=kuberploy-test
  --set-string integrations.github.secretName=kuberploy-github-app
  --set-string integrations.github.clusterID=22222222-2222-4222-8222-222222222222
  --set-string integrations.github.controlPlaneEgressCIDRs[0]=192.0.2.10/32
  --set-string integrations.github.sourceEgressCIDRs[0]=192.0.2.11/32
  --set-string integrations.github.registryEgressCIDRs[0]=192.0.2.12/32
)
helm template kuberploy-installer "${kp_chart}" --namespace kuberploy-system -f "${kp_managed}" "${kp_platform_args[@]}" \
  --set publicEndpoint.tls.enabled=true \
  --set-string publicEndpoint.tls.secretName=kuberploy-platform-tls \
  --set-string publicEndpoint.tls.clusterIssuerName=kuberploy-letsencrypt-production \
  --set-string publicEndpoint.tls.accountEmail=platform@example.com \
  "${kp_github_args[@]}" >"${kp_tmp}/github-platform.yaml"
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.githubApp.enabled' "${kp_tmp}/github-platform.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.buildLogs.enabled' "${kp_tmp}/github-platform.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.githubApp.secretRef.name' "${kp_tmp}/github-platform.yaml")" == "kuberploy-github-app" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.gitProjection.chartVersion' "${kp_tmp}/github-platform.yaml")" == "0.1.0-rc.17" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.config.platformGitBinding.clusterID' "${kp_tmp}/github-platform.yaml")" == "22222222-2222-4222-8222-222222222222" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.builder.enabled' "${kp_tmp}/github-platform.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject.builder.networkPolicy.registryEgressCIDRs[0]' "${kp_tmp}/github-platform.yaml")" == "192.0.2.12/32" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-builder") | .spec.sources[0].targetRevision' "${kp_tmp}/github-platform.yaml")" == "0.1.0-rc.17" ]]
[[ "$(yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-builder") | .spec.sources[0].helm.valuesObject.enabled' "${kp_tmp}/github-platform.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "AppProject" and .metadata.name == "kuberploy-control-plane") | [.spec.destinations[].namespace] | contains(["kuberploy-build-dind"])' "${kp_tmp}/github-platform.yaml")" == "true" ]]
yq eval-all 'select(.kind == "Application" and .metadata.name == "kuberploy-control-plane") | .spec.sources[0].helm.valuesObject' \
  "${kp_tmp}/github-platform.yaml" >"${kp_tmp}/github-control-values.yaml"
# Source tests supply the readable RC reference; release packaging replaces
# the OCI chart default with the immutable multi-platform image identity.
helm template kuberploy "${kp_root}/charts/kuberploy" --namespace kuberploy-system \
  -f "${kp_tmp}/github-control-values.yaml" \
  --set-string components.api.image.reference=ghcr.io/kuberploy/kuberploy-api@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
  --set-string builder.builderAgentImage=ghcr.io/kuberploy/kuberploy-builder-agent:0.1.0-rc.17 \
  >"${kp_tmp}/github-control.yaml"
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GITHUB_BUILDS_ENABLED' "${kp_tmp}/github-control.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_BUILD_LOGS_ENABLED' "${kp_tmp}/github-control.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GIT_PROJECTION_ENABLED' "${kp_tmp}/github-control.yaml")" == "true" ]]

for kp_entry in \
  'kuberploy-edge|edge|kuberploy-system|kuberploy-edge' \
  'kuberploy-cert-manager|cert|cert-manager|kuberploy-cert-manager' \
  'kuberploy-sealed-secrets|sealed-secrets|sealed-secrets|kuberploy-sealed-secrets' \
  'kuberploy-monitoring|monitoring|kuberploy-monitoring|kuberploy-monitoring'; do
  IFS='|' read -r kp_application kp_release kp_namespace kp_child_chart <<<"${kp_entry}"
  yq eval-all "select(.kind == \"Application\" and .metadata.name == \"${kp_application}\") | .spec.sources[0].helm.valuesObject" \
    "${kp_tmp}/platform.yaml" >"${kp_tmp}/${kp_application}-values.yaml"
  helm template "${kp_release}" "${kp_root}/charts/${kp_child_chart}" --namespace "${kp_namespace}" \
    -f "${kp_tmp}/${kp_application}-values.yaml" >/dev/null
done

helm lint "${kp_chart}" --namespace kuberploy-system -f "${kp_adopted}" >/dev/null
helm template kuberploy-installer "${kp_chart}" --namespace kuberploy-system -f "${kp_adopted}" >"${kp_tmp}/adopted.yaml"
[[ "$(yq eval-all '[select(.kind == "Application")] | length' "${kp_tmp}/adopted.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .metadata.annotations."kuberploy.io/installation-mode"' "${kp_tmp}/adopted.yaml")" == "adopted" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.sources[0].helm.valuesObject.edge.traefik.managed' "${kp_tmp}/adopted.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Application") | .spec.sources[0].helm.valuesObject.edge.traefik.adoptExisting' "${kp_tmp}/adopted.yaml")" == "true" ]]
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

kp_expect_reject "mutable source revision" --set-string source.valuesRevision=main
kp_expect_reject "opaque hash source revision" --set-string source.valuesRevision=1111111111111111111111111111111111111111
kp_expect_reject "unprefixed source revision" --set-string source.valuesRevision=0.1.0-rc.17
kp_expect_reject "missing package version" --set-string components.postgresql.expectedPackageVersion=
kp_expect_reject "unsupported adopted monitoring" --set components.postgresql.enabled=false --set components.postgresql.mode=disabled --set-string components.postgresql.expectedPackageVersion= --set-json components.postgresql.valueFiles=[] --set components.monitoring.enabled=true --set components.monitoring.mode=adopted --set components.monitoring.adoptionConfirmed=true --set-string components.monitoring.expectedPackageVersion=0.1.0-rc.17
kp_expect_reject "value file outside pinned installer directory" --set-string components.postgresql.valueFiles[0]=secrets.yaml
kp_expect_reject "value file traversal below installer prefix" --set-string components.postgresql.valueFiles[0]=examples/installer/../../../secrets.yaml
kp_expect_reject "arbitrary inline child values" --set components.postgresql.values.password=do-not-store
kp_expect_reject "disabled Argo with active child" --set bootstrap.argoCD.enabled=false --set bootstrap.argoCD.mode=disabled
kp_expect_reject "managed Argo without installer-owned Valkey" --set bootstrap.valkey.enabled=false
kp_expect_reject "control plane without explicit bootstrap token authority" --set components.controlPlane.enabled=true --set components.controlPlane.mode=managed --set-string components.controlPlane.expectedPackageVersion=0.1.0-rc.17 --set components.valkey.enabled=true --set components.valkey.mode=managed --set-string components.valkey.expectedPackageVersion=0.1.0-rc.17
kp_expect_reject "generated token without exact API CIDR" --set bootstrap.controlPlaneToken.mode=generated --set components.controlPlane.enabled=true --set components.controlPlane.mode=managed --set-string components.controlPlane.expectedPackageVersion=0.1.0-rc.17 --set components.valkey.enabled=true --set components.valkey.mode=managed --set-string components.valkey.expectedPackageVersion=0.1.0-rc.17
kp_expect_reject "precreated token with dormant API CIDR" --set bootstrap.controlPlaneToken.mode=precreated --set-string bootstrap.controlPlaneToken.kubeAPIServerCIDRs[0]=10.43.0.1/32 --set components.controlPlane.enabled=true --set components.controlPlane.mode=managed --set-string components.controlPlane.expectedPackageVersion=0.1.0-rc.17 --set components.valkey.enabled=true --set components.valkey.mode=managed --set-string components.valkey.expectedPackageVersion=0.1.0-rc.17
kp_expect_reject "managed edge without exact cluster API CIDR" --set components.postgresql.enabled=false --set components.postgresql.mode=disabled --set-string components.postgresql.expectedPackageVersion= --set-json components.postgresql.valueFiles=[] --set components.edge.enabled=true --set components.edge.mode=managed --set-string components.edge.expectedPackageVersion=0.1.0-rc.17
kp_expect_reject "public endpoint without edge" --set publicEndpoint.enabled=true --set-string publicEndpoint.hostname=kuberploy.example.com
kp_expect_reject "disabled public endpoint with dormant hostname" --set-string publicEndpoint.hostname=kuberploy.example.com
kp_expect_reject "public TLS without Secret" "${kp_platform_args[@]}" --set publicEndpoint.tls.enabled=true
kp_expect_reject "broad cluster API CIDR" --set-string cluster.kubeAPIServerCIDRs[0]=0.0.0.0/0
kp_expect_reject "dormant GitHub identity" --set integrations.github.appID=123456
kp_expect_reject "GitHub without builder component" "${kp_platform_args[@]}" --set publicEndpoint.tls.enabled=true --set-string publicEndpoint.tls.secretName=kuberploy-platform-tls --set-string publicEndpoint.tls.clusterIssuerName=kuberploy-letsencrypt-production --set-string publicEndpoint.tls.accountEmail=platform@example.com --set integrations.github.enabled=true --set integrations.github.appID=123456 --set-string integrations.github.clientID=Iv1_KuberployClient --set-string integrations.github.appSlug=kuberploy-test --set-string integrations.github.secretName=kuberploy-github-app --set-string integrations.github.clusterID=22222222-2222-4222-8222-222222222222 --set-string integrations.github.controlPlaneEgressCIDRs[0]=192.0.2.10/32 --set-string integrations.github.sourceEgressCIDRs[0]=192.0.2.11/32 --set-string integrations.github.registryEgressCIDRs[0]=192.0.2.12/32
kp_expect_reject "dormant registry identity" --set-string integrations.registry.authSecretName=registry-auth
kp_expect_reject "registry integration without registry component" --set integrations.registry.enabled=true --set-string integrations.registry.authSecretName=registry-auth --set-string integrations.registry.secretRevision=v1 --set-string integrations.registry.exposureMode=ingress --set-string integrations.registry.endpoint=registry.example.com --set-string integrations.registry.tlsSecretName=registry-tls --set-string integrations.registry.clusterIssuerName=kuberploy-letsencrypt-production
kp_expect_reject "managed registry without registry integration" --set components.registry.enabled=true --set components.registry.mode=managed --set-string components.registry.expectedPackageVersion=0.1.0-rc.17

if helm template kuberploy-installer "${kp_chart}" --namespace argocd -f "${kp_managed}" >/dev/null 2>&1; then
  printf 'installer accepted the wrong bootstrap namespace\n' >&2
  exit 1
fi

printf 'installer chart render checks passed\n'
