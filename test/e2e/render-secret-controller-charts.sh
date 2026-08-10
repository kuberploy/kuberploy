#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-secret-controllers-render.XXXXXX")"

kp_cleanup() {
  if [[ -n "${kp_tmp:-}" && "${kp_tmp}" == *"/kuberploy-secret-controllers-render."* ]]; then
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

kp_stage_chart() {
  local kp_name="$1"
  local kp_source="${kp_root}/charts/${kp_name}"
  local kp_chart="${kp_tmp}/${kp_name}"
  cp -R "${kp_source}" "${kp_chart}"
  rm -rf -- "${kp_chart}/charts"
  mkdir -p "${kp_chart}/charts"
  read -r kp_checksum kp_filename kp_url < <(awk 'NF && $1 !~ /^#/ {print}' "${kp_source}/testdata/upstream-artifacts.lock")
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 "${kp_url}" -o "${kp_chart}/charts/${kp_filename}"
  [[ "$(shasum -a 256 "${kp_chart}/charts/${kp_filename}" | awk '{print $1}')" == "${kp_checksum}" ]]
  rg -F "${kp_checksum}" "${kp_root}/DEPENDENCIES.md" >/dev/null
  helm dependency list "${kp_chart}" | rg -F 'ok' >/dev/null
  python3 -m json.tool "${kp_chart}/values.schema.json" >/dev/null
  printf '%s\n' "${kp_chart}"
}

kp_eso="$(kp_stage_chart kuberploy-external-secrets)"
kp_eso_managed="${kp_root}/charts/kuberploy-external-secrets/testdata/managed-values.yaml"
kp_eso_adopted="${kp_root}/charts/kuberploy-external-secrets/testdata/adopted-values.yaml"
helm lint "${kp_eso}" --namespace external-secrets -f "${kp_eso_managed}" >/dev/null
helm lint "${kp_eso}" --namespace external-secrets -f "${kp_eso_adopted}" >/dev/null
helm template kuberploy-external-secrets "${kp_eso}" --namespace external-secrets --include-crds --skip-tests -f "${kp_eso_managed}" >"${kp_tmp}/eso-managed.yaml"
helm template kuberploy-external-secrets "${kp_eso}" --namespace external-secrets --include-crds --skip-tests -f "${kp_eso_managed}" >"${kp_tmp}/eso-managed-again.yaml"
helm template kuberploy-external-secrets "${kp_eso}" --namespace external-secrets --skip-tests -f "${kp_eso_adopted}" >"${kp_tmp}/eso-adopted.yaml"
diff -u "${kp_tmp}/eso-managed.yaml" "${kp_tmp}/eso-managed-again.yaml" >/dev/null

[[ "$(yq eval-all '[select(.kind == "Namespace")] | length' "${kp_tmp}/eso-managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment")] | length' "${kp_tmp}/eso-managed.yaml" | tail -1)" == "3" ]]
[[ "$(yq eval-all '[select(.kind == "PodDisruptionBudget")] | length' "${kp_tmp}/eso-managed.yaml" | tail -1)" == "3" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy")] | length' "${kp_tmp}/eso-managed.yaml" | tail -1)" == "3" ]]
[[ "$(yq eval-all '[select(.kind == "Secret")] | length' "${kp_tmp}/eso-managed.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment") | .spec.template.spec.containers[].image] | unique | length' "${kp_tmp}/eso-managed.yaml" | tail -1)" == "1" ]]
rg -F 'ghcr.io/external-secrets/external-secrets:v2.8.0@sha256:24c0dd3699e0988520afd2218612758cd97d1f702757b5b4fcf89adaa33ef679' "${kp_tmp}/eso-managed.yaml" >/dev/null
rg -F -- '--enable-cluster-store-reconciler=false' "${kp_tmp}/eso-managed.yaml" >/dev/null
rg -F -- '--enable-cluster-external-secret-reconciler=false' "${kp_tmp}/eso-managed.yaml" >/dev/null
rg -F -- '--enable-push-secret-reconciler=false' "${kp_tmp}/eso-managed.yaml" >/dev/null
rg -F -- '--controller-class=kuberploy' "${kp_tmp}/eso-managed.yaml" >/dev/null
if rg -n 'clusterexternalsecrets\.external-secrets\.io|clustersecretstores\.external-secrets\.io|pushsecrets\.external-secrets\.io|external-secrets-servicebindings|serviceaccounts/token|rbac.authorization.k8s.io/aggregate-to-' "${kp_tmp}/eso-managed.yaml" >/dev/null; then
  printf 'managed External Secrets rendered forbidden cluster resources or RBAC expansion\n' >&2
  exit 1
fi
[[ "$(yq eval-all '[select(.kind != null)] | length' "${kp_tmp}/eso-adopted.yaml" | tail -1)" == "1" ]]

kp_reject_eso() {
  local kp_reason="$1"
  shift
  if helm template invalid "${kp_eso}" --namespace external-secrets --skip-tests -f "${kp_eso_managed}" "$@" >/dev/null 2>&1; then
    printf 'unsafe External Secrets render accepted: %s\n' "${kp_reason}" >&2
    exit 1
  fi
}

kp_reject_eso 'mutable image' --set-string externalSecrets.image.tag=v2.8.0
kp_reject_eso 'cluster store' --set externalSecrets.processClusterStore=true
kp_reject_eso 'push secret' --set externalSecrets.processPushSecret=true
kp_reject_eso 'generic target' --set externalSecrets.genericTargets.enabled=true
kp_reject_eso 'RBAC aggregation' --set externalSecrets.rbac.aggregateToView=true
kp_reject_eso 'service account token minting' --set externalSecrets.rbac.serviceAccountTokenCreate=true
kp_reject_eso 'arbitrary object' --set-string externalSecrets.extraObjects[0].kind=Pod
kp_reject_eso 'sidecar' --set-string externalSecrets.extraContainers[0].name=sidecar
kp_reject_eso 'webhook fail open' --set-string externalSecrets.webhook.failurePolicy=Ignore
kp_reject_eso 'public webhook service' --set-string externalSecrets.webhook.service.type=LoadBalancer
kp_reject_eso 'disabled wrapper policy' --set secretFoundation.networkPolicy.enabled=false
kp_reject_eso 'broad API CIDR' --set-string secretFoundation.networkPolicy.kubeAPIServerCIDRs[0]=0.0.0.0/0
if helm template invalid "${kp_eso}" --namespace another-namespace --skip-tests -f "${kp_eso_managed}" >/dev/null 2>&1; then
  printf 'External Secrets chart accepted a namespace outside its boundary\n' >&2
  exit 1
fi

kp_sealed="$(kp_stage_chart kuberploy-sealed-secrets)"
kp_sealed_managed="${kp_root}/charts/kuberploy-sealed-secrets/testdata/managed-values.yaml"
kp_sealed_adopted="${kp_root}/charts/kuberploy-sealed-secrets/testdata/adopted-values.yaml"
helm lint "${kp_sealed}" --namespace sealed-secrets -f "${kp_sealed_managed}" >/dev/null
helm lint "${kp_sealed}" --namespace sealed-secrets -f "${kp_sealed_adopted}" >/dev/null
helm template kuberploy-sealed-secrets "${kp_sealed}" --namespace sealed-secrets --include-crds --skip-tests -f "${kp_sealed_managed}" >"${kp_tmp}/sealed-managed.yaml"
helm template kuberploy-sealed-secrets "${kp_sealed}" --namespace sealed-secrets --include-crds --skip-tests -f "${kp_sealed_managed}" >"${kp_tmp}/sealed-managed-again.yaml"
helm template kuberploy-sealed-secrets "${kp_sealed}" --namespace sealed-secrets --skip-tests -f "${kp_sealed_adopted}" >"${kp_tmp}/sealed-adopted.yaml"
diff -u "${kp_tmp}/sealed-managed.yaml" "${kp_tmp}/sealed-managed-again.yaml" >/dev/null

[[ "$(yq eval-all '[select(.kind == "Namespace")] | length' "${kp_tmp}/sealed-managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment")] | length' "${kp_tmp}/sealed-managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy")] | length' "${kp_tmp}/sealed-managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Secret")] | length' "${kp_tmp}/sealed-managed.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Ingress")] | length' "${kp_tmp}/sealed-managed.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Service" and .spec.type != "ClusterIP")] | length' "${kp_tmp}/sealed-managed.yaml" | tail -1)" == "0" ]]
rg -F 'docker.io/bitnami/sealed-secrets-controller:0.38.4@sha256:ab8e4687a97fb097f30ca2f028222f779f231c224555ba05f43d172c61f84497' "${kp_tmp}/sealed-managed.yaml" >/dev/null
rg -F -- '--key-prefix' "${kp_tmp}/sealed-managed.yaml" >/dev/null
rg -F 'kuberploy-sealed-secrets-key' "${kp_tmp}/sealed-managed.yaml" >/dev/null
rg -F 'kuberploy.io/recovery-required=true' "${kp_tmp}/sealed-managed.yaml" >/dev/null
rg -F 'kuberploy.io/sealing-key=true' "${kp_tmp}/sealed-managed.yaml" >/dev/null
if rg -n 'system:authenticated|service-proxier|kind: Ingress|type: (LoadBalancer|NodePort)' "${kp_tmp}/sealed-managed.yaml" >/dev/null; then
  printf 'managed Sealed Secrets rendered public access or the broad proxy binding\n' >&2
  exit 1
fi
[[ "$(yq eval-all '[select(.kind != null)] | length' "${kp_tmp}/sealed-adopted.yaml" | tail -1)" == "1" ]]

kp_reject_sealed() {
  local kp_reason="$1"
  shift
  if helm template invalid "${kp_sealed}" --namespace sealed-secrets --skip-tests -f "${kp_sealed_managed}" "$@" >/dev/null 2>&1; then
    printf 'unsafe Sealed Secrets render accepted: %s\n' "${kp_reason}" >&2
    exit 1
  fi
}

kp_reject_sealed 'mutable image' --set-string sealedSecrets.image.tag=0.38.4
kp_reject_sealed 'broad authenticated proxy' --set sealedSecrets.rbac.serviceProxier.create=true
kp_reject_sealed 'public service' --set-string sealedSecrets.service.type=LoadBalancer
kp_reject_sealed 'ingress' --set sealedSecrets.ingress.enabled=true
kp_reject_sealed 'additional namespace' --set-string sealedSecrets.additionalNamespaces[0]=default
kp_reject_sealed 'command override' --set-string sealedSecrets.command[0]=sh
kp_reject_sealed 'volume injection' --set-string sealedSecrets.additionalVolumes[0].name=host
kp_reject_sealed 'custom probe' --set-string sealedSecrets.customLivenessProbe.exec.command[0]=true
kp_reject_sealed 'missing key recovery' --set secretFoundation.keyRecovery.required=false
kp_reject_sealed 'disabled wrapper policy' --set secretFoundation.networkPolicy.enabled=false
kp_reject_sealed 'broad API CIDR' --set-string secretFoundation.networkPolicy.kubeAPIServerCIDRs[0]=0.0.0.0/0
if helm template invalid "${kp_sealed}" --namespace another-namespace --skip-tests -f "${kp_sealed_managed}" >/dev/null 2>&1; then
  printf 'Sealed Secrets chart accepted a namespace outside its boundary\n' >&2
  exit 1
fi

printf 'External Secrets and Sealed Secrets chart render and mutation validation passed\n'
