#!/usr/bin/env bash

set -Eeuo pipefail

kp_report_error() {
  local kp_status=$?
  printf 'platform render check failed at line %s: %s\n' "${BASH_LINENO[0]}" "${BASH_COMMAND}" >&2
  return "${kp_status}"
}
trap kp_report_error ERR

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-render.XXXXXX")"
kp_remove_tmp() {
  [[ -n "${kp_tmp:-}" && "${kp_tmp}" == *"/kuberploy-render."* ]] && rm -rf -- "${kp_tmp}"
}
trap kp_remove_tmp EXIT

for kp_tool in helm yq diff rg; do
  command -v "${kp_tool}" >/dev/null 2>&1 || { printf 'missing tool: %s\n' "${kp_tool}" >&2; exit 1; }
done

for kp_private_policy in \
  charts/kuberploy/templates/networkpolicies.yaml \
  charts/kuberploy-argocd/templates/networkpolicy.yaml \
  charts/kuberploy-builder/templates/networkpolicy.yaml \
  charts/kuberploy-cert-manager/templates/networkpolicy.yaml \
  charts/kuberploy-edge/templates/networkpolicy.yaml \
  charts/kuberploy-external-dns/templates/networkpolicy.yaml \
  charts/kuberploy-external-secrets/templates/networkpolicy.yaml \
  charts/kuberploy-monitoring/templates/networkpolicy.yaml \
  charts/kuberploy-postgresql/templates/networkpolicy.yaml \
  charts/kuberploy-registry/templates/networkpolicy.yaml \
  charts/kuberploy-runtime/templates/networkpolicy.yaml \
  charts/kuberploy-sealed-secrets/templates/networkpolicy.yaml \
  charts/kuberploy-valkey/templates/networkpolicy.yaml; do
  kp_private_source="${kp_root}/${kp_private_policy}"
  [[ "$(rg -c 'ipBlock: \{cidr: (10\.0\.0\.0/8|172\.16\.0\.0/12|192\.168\.0\.0/16)\}' "${kp_private_source}")" == "3" ]] || {
    printf 'chart lacks exact RFC1918 private egress: %s\n' "${kp_private_policy}" >&2
    exit 1
  }
done

# Production Argo injects these locked Helm parameters from server-owned
# project/environment/application identities. Keep AppConfig fixtures pure by
# supplying the same operator-only fence at render time.
kp_runtime_identity=(
  --set-string kuberployExpectedIdentity.projectId=11111111-1111-4111-8111-111111111111
  --set-string kuberployExpectedIdentity.environmentId=22222222-2222-4222-8222-222222222222
  --set-string kuberployExpectedIdentity.applicationId=33333333-3333-4333-8333-333333333333
)

kp_expect_platform_reject() {
  local kp_reason="${1:?rejection reason is required}"
  shift
  if helm template invalid-platform "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render \
    -f "${kp_root}/test/e2e/fixtures/platform-values.yaml" "$@" >/dev/null 2>&1; then
    printf 'platform chart accepted %s\n' "${kp_reason}" >&2
    exit 1
  fi
}

helm lint "${kp_root}/charts/kuberploy-runtime"
helm lint "${kp_root}/charts/kuberploy-runtime" -f "${kp_root}/test/e2e/fixtures/appconfig-backend.yaml" "${kp_runtime_identity[@]}"
helm lint "${kp_root}/charts/kuberploy-runtime" -f "${kp_root}/test/e2e/fixtures/appconfig-hello.yaml" "${kp_runtime_identity[@]}"
helm lint "${kp_root}/charts/kuberploy-runtime" -f "${kp_root}/charts/kuberploy-runtime/testdata/custom-certificate.yaml" "${kp_runtime_identity[@]}"
helm lint "${kp_root}/charts/kuberploy"
helm lint "${kp_root}/charts/kuberploy" -f "${kp_root}/test/e2e/fixtures/platform-values.yaml"

helm template runtime "${kp_root}/charts/kuberploy-runtime" \
  --namespace kuberploy-e2e-render \
  -f "${kp_root}/test/e2e/fixtures/appconfig-backend.yaml" "${kp_runtime_identity[@]}" > "${kp_tmp}/runtime.yaml"
helm template runtime "${kp_root}/charts/kuberploy-runtime" \
  --namespace kuberploy-e2e-render \
  -f "${kp_root}/test/e2e/fixtures/appconfig-backend.yaml" "${kp_runtime_identity[@]}" > "${kp_tmp}/runtime-again.yaml"
diff -u "${kp_tmp}/runtime.yaml" "${kp_tmp}/runtime-again.yaml"
yq eval-all 'true' "${kp_tmp}/runtime.yaml" >/dev/null

helm template recreate "${kp_root}/charts/kuberploy-runtime" \
  --namespace kuberploy-e2e-render \
  -f "${kp_root}/test/e2e/fixtures/appconfig-backend.yaml" "${kp_runtime_identity[@]}" \
  --set-string spec.runtime.strategy.type=Recreate > "${kp_tmp}/runtime-recreate.yaml"
[[ "$(yq eval 'select(.kind == "Deployment") | .spec.strategy.type' "${kp_tmp}/runtime-recreate.yaml")" == "Recreate" ]]
[[ "$(yq eval 'select(.kind == "Deployment") | .spec.strategy | has("rollingUpdate")' "${kp_tmp}/runtime-recreate.yaml")" == "false" ]]
if helm template invalid-strategy "${kp_root}/charts/kuberploy-runtime" \
  -f "${kp_root}/test/e2e/fixtures/appconfig-backend.yaml" "${kp_runtime_identity[@]}" \
  --set-string spec.runtime.strategy.type=OnDelete >/dev/null 2>&1; then
  printf 'runtime chart accepted an unsupported Deployment strategy\n' >&2
  exit 1
fi

helm template stateful "${kp_root}/charts/kuberploy-runtime" \
  --namespace kuberploy-e2e-render \
  -f "${kp_root}/test/e2e/fixtures/appconfig-backend.yaml" "${kp_runtime_identity[@]}" \
  --set-string spec.runtime.workloadType=StatefulSet \
  --set-string spec.runtime.strategy.type=OnDelete > "${kp_tmp}/runtime-stateful.yaml"
kp_stateful_service="$(yq eval-all 'select(.kind == "StatefulSet") | .spec.serviceName' "${kp_tmp}/runtime-stateful.yaml")"
[[ -n "${kp_stateful_service}" ]]
export kp_stateful_service
[[ "$(yq eval-all 'select(.kind == "Service" and .metadata.name == strenv(kp_stateful_service)) | .spec.clusterIP' "${kp_tmp}/runtime-stateful.yaml")" == "None" ]]
[[ "$(yq eval-all 'select(.kind == "Service" and .metadata.name == strenv(kp_stateful_service)) | .spec.publishNotReadyAddresses' "${kp_tmp}/runtime-stateful.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "StatefulSet") | .spec.updateStrategy.type' "${kp_tmp}/runtime-stateful.yaml")" == "OnDelete" ]]
if helm template invalid-stateful-strategy "${kp_root}/charts/kuberploy-runtime" \
  -f "${kp_root}/test/e2e/fixtures/appconfig-backend.yaml" "${kp_runtime_identity[@]}" \
  --set-string spec.runtime.workloadType=StatefulSet \
  --set-string spec.runtime.strategy.type=Recreate >/dev/null 2>&1; then
  printf 'runtime chart accepted an unsupported StatefulSet strategy\n' >&2
  exit 1
fi

# Even when Helm values-schema validation is deliberately bypassed, the
# runtime template rejects unreviewed Traefik families and nested fields.
if helm template invalid-middleware "${kp_root}/charts/kuberploy-runtime" \
  --skip-schema-validation \
  --set-json 'spec.middlewares=[{"name":"evil","spec":{"plugin":{"evil":{"command":"id"}}}}]' >/dev/null 2>&1; then
  printf 'runtime chart rendered a schema-bypassed Traefik plugin\n' >&2
  exit 1
fi
if helm template invalid-middleware "${kp_root}/charts/kuberploy-runtime" \
  --skip-schema-validation \
  --set-json 'spec.middlewares=[{"name":"evil","spec":{"headers":{"forwardAuth":{"address":"http://evil"}}}}]' >/dev/null 2>&1; then
  printf 'runtime chart rendered a schema-bypassed nested middleware field\n' >&2
  exit 1
fi
helm template basic-auth "${kp_root}/charts/kuberploy-runtime" \
  --skip-schema-validation \
  --set-json 'spec.middlewares=[{"name":"login","spec":{"basicAuth":{"secretBindingRef":{"bindingId":"77777777-7777-4777-8777-777777777777","name":"auth-users","key":"users","version":3},"removeHeader":true}}}]' > "${kp_tmp}/basic-auth.yaml"
[[ "$(yq eval-all 'select(.kind == "Middleware") | .spec.basicAuth.secret' "${kp_tmp}/basic-auth.yaml")" == kp-auth-users-v3-* ]]
if rg -q 'bindingId|secretBindingRef|77777777-7777' "${kp_tmp}/basic-auth.yaml"; then
  printf 'runtime chart leaked BasicAuth binding metadata\n' >&2
  exit 1
fi

helm template custom-certificate "${kp_root}/charts/kuberploy-runtime" \
  --namespace kuberploy-e2e-render \
  -f "${kp_root}/charts/kuberploy-runtime/testdata/custom-certificate.yaml" "${kp_runtime_identity[@]}" > "${kp_tmp}/custom-certificate.yaml"
helm template custom-certificate "${kp_root}/charts/kuberploy-runtime" \
  --namespace kuberploy-e2e-render \
  -f "${kp_root}/charts/kuberploy-runtime/testdata/custom-certificate.yaml" "${kp_runtime_identity[@]}" > "${kp_tmp}/custom-certificate-again.yaml"
diff -u "${kp_tmp}/custom-certificate.yaml" "${kp_tmp}/custom-certificate-again.yaml"
[[ "$(yq eval-all 'select(.kind == "Ingress" and .spec.tls != null) | .spec.tls[0].secretName' "${kp_tmp}/custom-certificate.yaml")" == "kp-route-certificate-v7-57b5b21825" ]] || {
  printf 'custom certificate did not render the deterministic runtime-secret target\n' >&2
  exit 1
}

for kp_custom_certificate_mutation in \
  '.spec.routes[0].tls.secretRef = "caller-selected-secret"' \
  'del(.spec.routes[0].tls.secretRef.version)' \
  '.spec.routes[0].tls.secretRef.secretName = "caller-selected-secret"' \
  '.spec.routes[0].tls.secretRef.bindingId = "not-a-uuid"' \
  '.spec.routes[0].tls.secretRef.name = "Caller_Selected_Secret"' \
  '.spec.routes[0].tls.secretRef.version = 0' \
  '.spec.routes[0].tls.secretRef.version = "7"'; do
  yq "${kp_custom_certificate_mutation}" \
    "${kp_root}/charts/kuberploy-runtime/testdata/custom-certificate.yaml" > "${kp_tmp}/custom-certificate-invalid.yaml"
  if helm template invalid-custom-certificate "${kp_root}/charts/kuberploy-runtime" \
    -f "${kp_tmp}/custom-certificate-invalid.yaml" "${kp_runtime_identity[@]}" >/dev/null 2>&1; then
    printf 'runtime chart accepted invalid custom certificate identity: %s\n' "${kp_custom_certificate_mutation}" >&2
    exit 1
  fi
done

sed 's/version: 7/version: 9223372036854775808/' \
  "${kp_root}/charts/kuberploy-runtime/testdata/custom-certificate.yaml" > "${kp_tmp}/custom-certificate-invalid.yaml"
if helm template invalid-custom-certificate "${kp_root}/charts/kuberploy-runtime" \
  -f "${kp_tmp}/custom-certificate-invalid.yaml" "${kp_runtime_identity[@]}" >/dev/null 2>&1; then
  printf 'runtime chart accepted a custom certificate version above positive int64\n' >&2
  exit 1
fi

# The template itself remains closed if an operator bypasses values-schema
# validation: a legacy string, incomplete object, or caller-selected target
# name can never reach spec.tls.secretName.
for kp_custom_certificate_mutation in \
  '.spec.routes[0].tls.secretRef = "caller-selected-secret"' \
  'del(.spec.routes[0].tls.secretRef.version)' \
  '.spec.routes[0].tls.secretRef.secretName = "caller-selected-secret"'; do
  yq "${kp_custom_certificate_mutation}" \
    "${kp_root}/charts/kuberploy-runtime/testdata/custom-certificate.yaml" > "${kp_tmp}/custom-certificate-invalid.yaml"
  if helm template invalid-custom-certificate "${kp_root}/charts/kuberploy-runtime" \
    -f "${kp_tmp}/custom-certificate-invalid.yaml" "${kp_runtime_identity[@]}" --skip-schema-validation >/dev/null 2>&1; then
    printf 'runtime template accepted unsafe custom certificate identity with schema validation bypassed: %s\n' "${kp_custom_certificate_mutation}" >&2
    exit 1
  fi
done

helm template platform-default "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-system > "${kp_tmp}/platform-default.yaml"
[[ "$(yq eval-all 'select(.kind == "Role" and (.metadata.name | test("-api-observer$"))) | .rules[] | select(.resources | contains(["statefulsets"])) | .verbs | sort | join(",")' "${kp_tmp}/platform-default.yaml")" == "get,list,watch" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy") | .spec.egress[]?.to[]?.ipBlock.cidr | select(. == "0.0.0.0/0" or . == "::/0")] | length' "${kp_tmp}/platform-default.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.name == "platform-default-upgrade")] | length' "${kp_tmp}/platform-default.yaml" | tail -1)" == "0" ]]

kp_image="$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.containers[0].image' "${kp_tmp}/runtime.yaml")"
[[ "${kp_image}" =~ @sha256:[a-f0-9]{64}$ ]] || { printf 'runtime image is not digest-pinned\n' >&2; exit 1; }
[[ "$(yq eval-all 'select(.kind == "Service") | .spec.type' "${kp_tmp}/runtime.yaml")" == "ClusterIP" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .immutable' "${kp_tmp}/runtime.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.securityContext.runAsUser' "${kp_tmp}/runtime.yaml")" == "65532" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .metadata.labels."kuberploy.io/test-run"' "${kp_tmp}/runtime.yaml")" == "render" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | (.spec.template.spec.imagePullSecrets // []) | length' "${kp_tmp}/runtime.yaml")" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and (.metadata.name | test("-edge$"))) | select(.spec.podSelector.matchLabels."kuberploy.io/application" == "33333333-3333-4333-8333-333333333333" and (.spec.ingress | length) == 1 and .spec.ingress[0].from[0].namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "kuberploy-system" and .spec.ingress[0].from[0].podSelector.matchLabels."app.kubernetes.io/name" == "traefik" and (.spec.ingress[0].ports | length) == 1 and .spec.ingress[0].ports[0].port == "http" and .spec.ingress[0].ports[0].protocol == "TCP")] | length' "${kp_tmp}/runtime.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and (.metadata.name | test("-edge$"))) | .spec.ingress[0].ports[] | select(.port != "http" or .protocol != "TCP")] | length' "${kp_tmp}/runtime.yaml" | tail -1)" == "0" ]]

yq '.spec.delivery.registryPull = {"targetId":"44444444-4444-4444-8444-444444444444","profileName":"managed-main","profileRevision":7}' \
  "${kp_root}/test/e2e/fixtures/appconfig-backend.yaml" > "${kp_tmp}/private-pull.yaml"
helm template private-pull "${kp_root}/charts/kuberploy-runtime" \
  --namespace kuberploy-e2e-render -f "${kp_tmp}/private-pull.yaml" "${kp_runtime_identity[@]}" > "${kp_tmp}/private-pull-render.yaml"
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.imagePullSecrets[0].name' "${kp_tmp}/private-pull-render.yaml")" == "kuberploy-pull-25e8bfde8b028b690c352c64" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.imagePullSecrets | length' "${kp_tmp}/private-pull-render.yaml")" == "1" ]]
yq '.spec.delivery.registryPull.secretName = "attacker-selected"' "${kp_tmp}/private-pull.yaml" > "${kp_tmp}/private-pull-secret-name.yaml"
if helm template invalid "${kp_root}/charts/kuberploy-runtime" -f "${kp_tmp}/private-pull-secret-name.yaml" "${kp_runtime_identity[@]}" >/dev/null 2>&1; then
  printf 'runtime chart accepted a caller-selected imagePullSecret name\n' >&2
  exit 1
fi

yq '.metadata.name = "hello-renamed"' "${kp_root}/test/e2e/fixtures/appconfig-backend.yaml" > "${kp_tmp}/renamed.yaml"
helm template renamed "${kp_root}/charts/kuberploy-runtime" -f "${kp_tmp}/renamed.yaml" "${kp_runtime_identity[@]}" > "${kp_tmp}/renamed-render.yaml"
kp_original_name="$(yq eval-all 'select(.kind == "Deployment") | .metadata.name' "${kp_tmp}/runtime.yaml")"
kp_renamed_name="$(yq eval-all 'select(.kind == "Deployment") | .metadata.name' "${kp_tmp}/renamed-render.yaml")"
[[ "${kp_original_name}" == "${kp_renamed_name}" ]] || { printf 'display rename changed immutable workload identity\n' >&2; exit 1; }

yq '.spec.runtime.env[0].value = "changed"' "${kp_root}/test/e2e/fixtures/appconfig-backend.yaml" > "${kp_tmp}/config-changed.yaml"
helm template changed "${kp_root}/charts/kuberploy-runtime" -f "${kp_tmp}/config-changed.yaml" "${kp_runtime_identity[@]}" > "${kp_tmp}/config-changed-render.yaml"
kp_original_config="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/runtime.yaml")"
kp_changed_config="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/config-changed-render.yaml")"
[[ "${kp_original_config}" != "${kp_changed_config}" ]] || { printf 'ordinary config did not produce a new immutable ConfigMap name\n' >&2; exit 1; }

yq '.spec.delivery.release.digest = "latest"' "${kp_root}/test/e2e/fixtures/appconfig-backend.yaml" > "${kp_tmp}/mutable-image.yaml"
if helm template invalid "${kp_root}/charts/kuberploy-runtime" -f "${kp_tmp}/mutable-image.yaml" "${kp_runtime_identity[@]}" >/dev/null 2>&1; then
  printf 'runtime chart accepted a mutable image\n' >&2
  exit 1
fi
yq '.spec.runtime.unknownField = true' "${kp_root}/test/e2e/fixtures/appconfig-backend.yaml" > "${kp_tmp}/unknown.yaml"
if helm template invalid "${kp_root}/charts/kuberploy-runtime" -f "${kp_tmp}/unknown.yaml" "${kp_runtime_identity[@]}" >/dev/null 2>&1; then
  printf 'runtime chart accepted an unknown AppConfig field\n' >&2
  exit 1
fi

helm template platform "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_root}/test/e2e/fixtures/platform-values.yaml" > "${kp_tmp}/platform.yaml"
yq eval-all 'true' "${kp_tmp}/platform.yaml" >/dev/null
if yq eval-all 'select(.kind == "ClusterRole" or .kind == "ClusterRoleBinding") | .kind' "${kp_tmp}/platform.yaml" | grep -q .; then
  printf 'platform chart rendered cluster-wide RBAC\n' >&2
  exit 1
fi
[[ "$(yq eval-all 'select(.kind == "ServiceAccount" and .metadata.labels."app.kubernetes.io/component" == "web") | .automountServiceAccountToken' "${kp_tmp}/platform.yaml")" == "false" ]]
[[ "$(yq eval-all '[select(.kind == "PodDisruptionBudget" and .spec.maxUnavailable == 0)] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "3" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .spec.strategy.type == "RollingUpdate" and .spec.strategy.rollingUpdate.maxUnavailable == 0)] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "3" ]]
[[ "$(yq eval-all '[select((.kind == "Deployment" or .kind == "PodDisruptionBudget") and .metadata.labels."app.kubernetes.io/component" == "migration")] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.annotations."kuberploy.io/migrations" == "prisma-schema-verified-read-only")] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "3" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GITHUB_BUILDS_ENABLED' "${kp_tmp}/platform.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GIT_PROJECTION_ENABLED' "${kp_tmp}/platform.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GIT_PROJECTION_AUTH_MODE' "${kp_tmp}/platform.yaml")" == "disabled" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GIT_PROJECTION_CACHE_MAX_BYTES' "${kp_tmp}/platform.yaml")" == "536870912" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_RUNTIME_SECRETS_ENABLED' "${kp_tmp}/platform.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_CERTIFICATE_OBSERVATION_ENABLED' "${kp_tmp}/platform.yaml")" == "false" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_CERTIFICATE_OBSERVATION"))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "web") | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_CERTIFICATE_OBSERVATION"))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_ENABLED' "${kp_tmp}/platform.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | [.data | keys[] | select(test("^KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER"))] | length' "${kp_tmp}/platform.yaml")" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER"))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "web") | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER"))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_RUNTIME_SECRET"))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "web") | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_RUNTIME_SECRET"))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment") | .spec.template.spec.volumes[]? | select(.name | test("^runtime-secret-"))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Role" or .kind == "RoleBinding") | select(.metadata.name | test("-runtime-secrets-(api|worker)$"))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "ValidatingAdmissionPolicy" or .kind == "ValidatingAdmissionPolicyBinding") | select(.metadata.labels."app.kubernetes.io/component" == "runtime-secrets-admission")] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_RUNTIME_REGISTRY_PULLS_ENABLED' "${kp_tmp}/platform.yaml")" == "false" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_RUNTIME_REGISTRY_PULL"))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "web") | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_RUNTIME_REGISTRY_PULL"))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment") | .spec.template.spec.volumes[]? | select(.name == "runtime-registry-pull-credentials")] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Role" or .kind == "RoleBinding") | select(.metadata.labels."app.kubernetes.io/component" == "runtime-registry-pulls")] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "ValidatingAdmissionPolicy" or .kind == "ValidatingAdmissionPolicyBinding") | select(.metadata.labels."app.kubernetes.io/component" == "runtime-registry-pulls-admission")] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_EDGE_RUNTIME_ENABLED' "${kp_tmp}/platform.yaml")" == "false" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_EDGE_RUNTIME"))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "web") | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_EDGE_RUNTIME"))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Role" or .kind == "RoleBinding" or .kind == "ClusterRole" or .kind == "ClusterRoleBinding") | select(.metadata.labels."app.kubernetes.io/component" == "edge-runtime")] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_ARGO_OBSERVATION_ENABLED' "${kp_tmp}/platform.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_ARGO_DESIRED_STATE_ENABLED' "${kp_tmp}/platform.yaml")" == "false" ]]
[[ "$(yq eval-all '[select(.kind == "Role" or .kind == "RoleBinding") | select(.metadata.labels."app.kubernetes.io/component" == "argo-desired-state")] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "ValidatingAdmissionPolicy" or .kind == "ValidatingAdmissionPolicyBinding") | select(.metadata.labels."app.kubernetes.io/component" == "argo-repository-credentials-admission")] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "ValidatingAdmissionPolicy" or .kind == "ValidatingAdmissionPolicyBinding") | select(.metadata.labels."app.kubernetes.io/component" == "argo-root-refresh-admission")] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_MANAGED_REGISTRY_RUNTIME_ENABLED' "${kp_tmp}/platform.yaml")" == "false" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_MANAGED_REGISTRY_"))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_MANAGED_REGISTRY_"))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[]? | select(.name == "managed-registry-credentials")] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select((.kind == "Role" or .kind == "RoleBinding" or .kind == "ServiceAccount" or .kind == "NetworkPolicy") and (.metadata.name | test("registry-maintenance")))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Role" and (.metadata.name | test("-argo-observer$")))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Role" or .kind == "RoleBinding") | select(.metadata.name | test("managed-monitoring-observer$"))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_VALKEY_ADDRESSES" and .valueFrom.secretKeyRef.name == "external-valkey" and .valueFrom.secretKeyRef.key == "addresses" and (.valueFrom.secretKeyRef.optional != true))] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_VALKEY_USERNAME" and .valueFrom.secretKeyRef.key == "username" and .valueFrom.secretKeyRef.name == "external-valkey" and .valueFrom.secretKeyRef.optional == true)] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_VALKEY_PASSWORD" and .valueFrom.secretKeyRef.key == "password" and .valueFrom.secretKeyRef.name == "external-valkey" and .valueFrom.secretKeyRef.optional == true)] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "2" ]]

yq '.config.valkey.mode = "managed" | .config.valkey.secretRef.name = "kuberploy-valkey" | .networkPolicy.externalValkeyEgressCIDRs = []' \
  "${kp_root}/test/e2e/fixtures/platform-values.yaml" > "${kp_tmp}/managed-valkey-values.yaml"
helm template managed-valkey "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render -f "${kp_tmp}/managed-valkey-values.yaml" > "${kp_tmp}/managed-valkey.yaml"
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_VALKEY_USERNAME" or .name == "KUBERPLOY_VALKEY_PASSWORD")] | length' "${kp_tmp}/managed-valkey.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.template.spec.containers[0].env[] | select((.name == "KUBERPLOY_VALKEY_CACHE_USERNAME" and .valueFrom.secretKeyRef.key == "api-cache-username") or (.name == "KUBERPLOY_VALKEY_CACHE_PASSWORD" and .valueFrom.secretKeyRef.key == "api-cache-password") or (.name == "KUBERPLOY_VALKEY_LIMITER_USERNAME" and .valueFrom.secretKeyRef.key == "api-limiter-username") or (.name == "KUBERPLOY_VALKEY_LIMITER_PASSWORD" and .valueFrom.secretKeyRef.key == "api-limiter-password")) | select(.valueFrom.secretKeyRef.name == "kuberploy-valkey" and (.valueFrom.secretKeyRef.optional != true))] | length' "${kp_tmp}/managed-valkey.yaml" | tail -1)" == "4" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.containers[0].env[] | select((.name == "KUBERPLOY_VALKEY_PUBLISHER_USERNAME" and .valueFrom.secretKeyRef.key == "outbox-publisher-username") or (.name == "KUBERPLOY_VALKEY_PUBLISHER_PASSWORD" and .valueFrom.secretKeyRef.key == "outbox-publisher-password") or (.name == "KUBERPLOY_VALKEY_CONSUMER_USERNAME" and .valueFrom.secretKeyRef.key == "worker-consumer-username") or (.name == "KUBERPLOY_VALKEY_CONSUMER_PASSWORD" and .valueFrom.secretKeyRef.key == "worker-consumer-password")) | select(.valueFrom.secretKeyRef.name == "kuberploy-valkey" and (.valueFrom.secretKeyRef.optional != true))] | length' "${kp_tmp}/managed-valkey.yaml" | tail -1)" == "4" ]]
if helm template invalid-managed-valkey "${kp_root}/charts/kuberploy" --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/managed-valkey-values.yaml" --set-string config.valkey.secretRef.apiCachePasswordKey=password >/dev/null 2>&1; then
  printf 'platform chart accepted managed Valkey cache credential key substitution\n' >&2
  exit 1
fi
if helm template invalid-managed-valkey "${kp_root}/charts/kuberploy" --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/managed-valkey-values.yaml" --set-string config.valkey.secretRef.publisherUsernameKey=username >/dev/null 2>&1; then
  printf 'platform chart accepted managed Valkey publisher credential key substitution\n' >&2
  exit 1
fi

yq '.config.monitoring.mode = "managed" |
    .config.monitoring.prometheusURL = "http://prometheus-operated.kuberploy-monitoring.svc:9090" |
    .config.monitoring.bearerTokenSecret.name = ""' \
  "${kp_root}/test/e2e/fixtures/platform-values.yaml" > "${kp_tmp}/managed-monitoring-values.yaml"
helm template managed-monitoring "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render -f "${kp_tmp}/managed-monitoring-values.yaml" > "${kp_tmp}/managed-monitoring.yaml"
[[ "$(yq eval-all 'select(.kind == "Role" and (.metadata.name | test("managed-monitoring-observer$"))) | .metadata.namespace' "${kp_tmp}/managed-monitoring.yaml")" == "kuberploy-monitoring" ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "Role" and (.metadata.name | test("managed-monitoring-observer$"))) | .rules' "${kp_tmp}/managed-monitoring.yaml")" == '[{"apiGroups":[""],"resources":["configmaps"],"resourceNames":["monitoring-monitoring-profile"],"verbs":["get"]},{"apiGroups":["apps"],"resources":["deployments"],"resourceNames":["kuberploy-prometheus-operator"],"verbs":["get"]},{"apiGroups":["monitoring.coreos.com"],"resources":["prometheusrules"],"resourceNames":["monitoring-service-recording-rules"],"verbs":["get"]},{"apiGroups":["monitoring.coreos.com"],"resources":["prometheuses"],"resourceNames":["monitoring-kube-prometheus-prometheus"],"verbs":["get"]}]' ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "RoleBinding" and (.metadata.name | test("managed-monitoring-observer$"))) | .subjects' "${kp_tmp}/managed-monitoring.yaml")" == '[{"kind":"ServiceAccount","name":"managed-monitoring-api","namespace":"kuberploy-e2e-render"}]' ]]
[[ "$(yq eval-all '[select(.kind == "Role" and (.metadata.name | test("managed-monitoring-observer$"))) | .rules[].verbs[] | select(. != "get")] | length' "${kp_tmp}/managed-monitoring.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Role" and (.metadata.name | test("managed-monitoring-observer$"))) | .rules[].resources[] | select(. == "secrets" or . == "pods")] | length' "${kp_tmp}/managed-monitoring.yaml" | tail -1)" == "0" ]]
kp_expect_platform_reject 'alternate managed Prometheus endpoint' --set config.monitoring.mode=managed --set-string config.monitoring.prometheusURL=http://other.kuberploy-monitoring.svc:9090
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[]? | select(.name == "git-projection-cache")] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[]? | select(.name == "github-app-private-key")] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.template.spec.volumes[]? | select(.name == "github-app-private-key")] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.egress[] | select(.to[0].namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "kuberploy-system" and .to[0].podSelector.matchLabels."app.kubernetes.io/name" == "kuberploy-postgresql" and (.ports | length) == 1 and .ports[0].port == 5432)] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.egress[] | select(.to[0].namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "kuberploy-system" and .to[0].podSelector.matchLabels."app.kubernetes.io/name" == "valkey" and (.ports | length) == 1 and .ports[0].port == 6379)] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.egress[] | select(.to[0].ipBlock.cidr == "10.43.0.1/32" and (.ports | length) == 2 and .ports[0].port == 443 and .ports[1].port == 6443)] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.egress[] | select(.to[0].ipBlock.cidr == "192.0.2.20/32" and (.ports | length) == 1 and .ports[0].port == 5432)] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.egress[] | select(.to[0].ipBlock.cidr == "192.0.2.21/32" and (.ports | length) == 1 and .ports[0].port == 6379)] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.egress[] | select(.to[0].ipBlock.cidr == "192.0.2.10/32" and (.ports | length) == 1 and .ports[0].port == 443)] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.egress[] | select(.to[0].ipBlock.cidr == "192.0.2.10/32" and (.ports | length) == 3 and .ports[0].port == 22 and .ports[1].port == 443 and .ports[2].port == 9418)] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == "upgrade")] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy") | .spec.egress[]? | select(.to[0].ipBlock.cidr == "0.0.0.0/0" or .to[0].ipBlock.cidr == "::/0") | .ports[].port | select(. == 5432 or . == 6379 or . == 6443)] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy") | .spec.egress[]?.to[]?.ipBlock.cidr | select(. == "0.0.0.0/0" or . == "::/0")] | length' "${kp_tmp}/platform.yaml" | tail -1)" == "0" ]]
if [[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and (.metadata.name | test("-private-egress$") | not)) | .spec.egress[]? | select(has("ports") | not)] | length' "${kp_tmp}/platform.yaml" | tail -1)" != "0" ]]; then
  printf 'platform chart rendered an unbounded egress port\n' >&2
  exit 1
fi
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "NetworkPolicy" and (.metadata.name | test("-private-egress$"))) | [.spec.egress[0].to[].ipBlock.cidr]' "${kp_tmp}/platform.yaml")" == '["10.0.0.0/8","172.16.0.0/12","192.168.0.0/16"]' ]]

yq '.rbac.observedNamespaces = []' "${kp_root}/test/e2e/fixtures/platform-values.yaml" > "${kp_tmp}/no-runtime-observer-values.yaml"
helm template no-runtime-observer "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/no-runtime-observer-values.yaml" > "${kp_tmp}/no-runtime-observer.yaml"
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.egress[] | select(.to[0].ipBlock.cidr == "10.43.0.1/32")] | length' "${kp_tmp}/no-runtime-observer.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.egress[] | select(.to[0].ipBlock.cidr == "10.43.0.1/32")] | length' "${kp_tmp}/no-runtime-observer.yaml" | tail -1)" == "1" ]]

kp_expect_platform_reject 'an all-address Kubernetes API CIDR' --set-string 'networkPolicy.kubeAPIServerCIDRs[0]=0.0.0.0/0'
kp_expect_platform_reject 'an expanded all-address Kubernetes API CIDR' --set-string 'networkPolicy.kubeAPIServerCIDRs[0]=0:0:0:0:0:0:0:0/0'
kp_expect_platform_reject 'an all-address external provider CIDR' --set-string 'networkPolicy.externalEgressCIDRs[0]=0.0.0.0/0'
kp_expect_platform_reject 'an all-address external PostgreSQL CIDR' --set-string 'networkPolicy.externalPostgreSQLEgressCIDRs[0]=::/0'
kp_expect_platform_reject 'an all-address external Valkey CIDR' --set-string 'networkPolicy.externalValkeyEgressCIDRs[0]=0.0.0.0/0'
kp_expect_platform_reject 'a provider CIDR equal to the Kubernetes API CIDR' --set-string 'networkPolicy.externalEgressCIDRs[0]=10.43.0.1/32'
kp_expect_platform_reject 'a mutable managed PostgreSQL namespace' --set-string networkPolicy.managedPostgreSQLNamespace=default
kp_expect_platform_reject 'a mutable managed Valkey namespace' --set-string networkPolicy.managedValkeyNamespace=default
kp_expect_platform_reject 'an invalid egress CIDR' --set-string 'networkPolicy.kubeAPIServerCIDRs[0]=not-a-cidr'
kp_expect_platform_reject 'an unknown NetworkPolicy field' --set networkPolicy.attacker=true
kp_expect_platform_reject 'runtime observation without Kubernetes API CIDRs' --set 'networkPolicy.kubeAPIServerCIDRs={}'
kp_expect_platform_reject 'a configured Git remote without provider CIDRs' --set 'networkPolicy.externalEgressCIDRs={}'

yq '.config.argoObservation.enabled = true |
    .config.argoObservation.namespace = "kuberploy-e2e-render" |
    .config.argoObservation.pollIntervalSeconds = 45' \
  "${kp_root}/test/e2e/fixtures/platform-values.yaml" > "${kp_tmp}/argo-observation-values.yaml"
helm template argo-observation "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/argo-observation-values.yaml" > "${kp_tmp}/argo-observation.yaml"
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_ARGO_OBSERVATION_ENABLED' "${kp_tmp}/argo-observation.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_ARGO_NAMESPACE' "${kp_tmp}/argo-observation.yaml")" == "kuberploy-e2e-render" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_ARGO_OBSERVATION_POLL_INTERVAL_SECONDS' "${kp_tmp}/argo-observation.yaml")" == "45" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_ARGO_OBSERVATION_ENABLED" or .name == "KUBERPLOY_ARGO_NAMESPACE" or .name == "KUBERPLOY_ARGO_OBSERVATION_POLL_INTERVAL_SECONDS")] | length' "${kp_tmp}/argo-observation.yaml" | tail -1)" == "3" ]]
[[ "$(yq eval-all 'select(.kind == "Role" and (.metadata.name | test("-argo-observer$"))) | .metadata.namespace' "${kp_tmp}/argo-observation.yaml")" == "kuberploy-e2e-render" ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "Role" and (.metadata.name | test("-argo-observer$"))) | .rules' "${kp_tmp}/argo-observation.yaml")" == '[{"apiGroups":["argoproj.io"],"resources":["applications"],"verbs":["list"]}]' ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "RoleBinding" and (.metadata.name | test("-argo-observer$"))) | .subjects' "${kp_tmp}/argo-observation.yaml")" == '[{"kind":"ServiceAccount","name":"argo-observation-worker","namespace":"kuberploy-e2e-render"}]' ]]
[[ "$(yq eval-all '[select(.kind == "ClusterRole" or .kind == "ClusterRoleBinding") | select(.metadata.name | test("argo-observer"))] | length' "${kp_tmp}/argo-observation.yaml" | tail -1)" == "0" ]]

yq '.config.argoObservation.pollIntervalSeconds = 46' "${kp_tmp}/argo-observation-values.yaml" > "${kp_tmp}/argo-observation-changed-values.yaml"
helm template argo-observation "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/argo-observation-changed-values.yaml" > "${kp_tmp}/argo-observation-changed.yaml"
kp_argo_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/argo-observation.yaml")"
kp_argo_changed_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/argo-observation-changed.yaml")"
[[ "${kp_argo_config_name}" != "${kp_argo_changed_config_name}" ]] || { printf 'Argo observation config mutation did not produce a new immutable ConfigMap name\n' >&2; exit 1; }

for kp_argo_mutation in \
  '.config.argoObservation.namespace = "other-namespace"' \
  '.config.argoObservation.pollIntervalSeconds = 14' \
  '.config.argoObservation.pollIntervalSeconds = 901' \
  '.config.argoObservation.unknownField = true'; do
  yq "${kp_argo_mutation}" "${kp_tmp}/argo-observation-values.yaml" > "${kp_tmp}/argo-observation-invalid.yaml"
  if helm template invalid-argo-observation "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render -f "${kp_tmp}/argo-observation-invalid.yaml" >/dev/null 2>&1; then
    printf 'platform chart accepted invalid Argo observation settings: %s\n' "${kp_argo_mutation}" >&2
    exit 1
  fi
done
yq '.networkPolicy.kubeAPIServerCIDRs = []' "${kp_tmp}/argo-observation-values.yaml" > "${kp_tmp}/argo-observation-no-api-egress.yaml"
if helm template invalid-argo-observation "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render -f "${kp_tmp}/argo-observation-no-api-egress.yaml" >/dev/null 2>&1; then
  printf 'platform chart enabled Argo observation without Kubernetes API egress\n' >&2
  exit 1
fi

yq '.components.worker.image.reference = "ghcr.io/kuberploy/kuberploy-worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" |
    .config.managedRegistry.enabled = true |
    .config.managedRegistry.targetID = "11111111-1111-4111-8111-111111111111" |
    .config.managedRegistry.targetName = "Managed registry" |
    .config.managedRegistry.endpoint = "http://kuberploy-registry.kuberploy-system.svc.cluster.local:5000" |
    .config.managedRegistry.repositoryPrefix = "kuberploy" |
    .config.managedRegistry.pullCredentialRef = "registry-pull" |
    .config.managedRegistry.pushCredentialRef = "registry-push" |
    .config.managedRegistry.cacheCredentialRef = "registry-cache" |
    .config.managedRegistry.lifecycleCredentialRef = "operator/managed-registry" |
    .config.managedRegistry.allowPlainHTTP = true |
    .config.managedRegistry.namespace = "kuberploy-system" |
    .config.managedRegistry.deployment = "kuberploy-registry" |
    .config.managedRegistry.persistentVolumeClaim = "kuberploy-registry" |
    .config.managedRegistry.registryConfigMap = "kuberploy-registry-config-abc123" |
    .config.managedRegistry.credentialSecret.name = "kuberploy-registry-controller"' \
  "${kp_root}/test/e2e/fixtures/platform-values.yaml" > "${kp_tmp}/managed-registry-values.yaml"
helm template managed-registry "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/managed-registry-values.yaml" > "${kp_tmp}/managed-registry.yaml"
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_MANAGED_REGISTRY_RUNTIME_ENABLED' "${kp_tmp}/managed-registry.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_MANAGED_REGISTRY_TARGET_NAME' "${kp_tmp}/managed-registry.yaml")" == "Managed registry" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_MANAGED_REGISTRY_LIFECYCLE_CREDENTIAL_REF' "${kp_tmp}/managed-registry.yaml")" == "operator/managed-registry" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_MANAGED_REGISTRY_"))] | length' "${kp_tmp}/managed-registry.yaml" | tail -1)" == "17" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_MANAGED_REGISTRY_"))] | length' "${kp_tmp}/managed-registry.yaml" | tail -1)" == "17" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.template.spec.volumes[]? | select(.name == "managed-registry-credentials")] | length' "${kp_tmp}/managed-registry.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[] | select(.name == "managed-registry-credentials") | .secret.secretName' "${kp_tmp}/managed-registry.yaml")" == "kuberploy-registry-controller" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[] | select(.name == "managed-registry-credentials") | [.secret.items[].path] | sort | join(",")' "${kp_tmp}/managed-registry.yaml")" == "password,username" ]]
[[ "$(yq eval-all 'select(.kind == "ServiceAccount" and (.metadata.name | test("registry-maintenance$"))) | .metadata.namespace' "${kp_tmp}/managed-registry.yaml")" == "kuberploy-system" ]]
[[ "$(yq eval-all 'select(.kind == "ServiceAccount" and (.metadata.name | test("registry-maintenance$"))) | .automountServiceAccountToken' "${kp_tmp}/managed-registry.yaml")" == "false" ]]
[[ "$(yq eval-all -o=json 'select(.kind == "Role" and (.metadata.name | test("registry-maintenance-controller$")))' "${kp_tmp}/managed-registry.yaml" | jq '[.rules[] | select(.resources == ["secrets"] or .resources == ["pods/log"])] | length')" == "0" ]]
[[ "$(yq eval-all -o=json 'select(.kind == "Role" and (.metadata.name | test("registry-maintenance-controller$")))' "${kp_tmp}/managed-registry.yaml" | jq '[.rules[] | select(.resources == ["deployments"] and .resourceNames == ["kuberploy-registry"] and .verbs == ["get"])] | length')" == "1" ]]
[[ "$(yq eval-all -o=json 'select(.kind == "NetworkPolicy" and (.metadata.name | test("registry-maintenance-deny-network$")))' "${kp_tmp}/managed-registry.yaml" | jq '.metadata.namespace == "kuberploy-system" and ([.spec.ingress[]?] | length) == 0 and ([.spec.egress[]?] | length) == 0')" == "true" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.egress[] | select(.to[0].namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "kuberploy-system" and .to[0].podSelector.matchLabels."app.kubernetes.io/name" == "kuberploy-registry" and (.ports | length) == 1 and .ports[0].port == 5000 and .ports[0].protocol == "TCP")] | length' "${kp_tmp}/managed-registry.yaml" | tail -1)" == "1" ]]

yq '.config.managedRegistry.observationIntervalSeconds = 301' "${kp_tmp}/managed-registry-values.yaml" > "${kp_tmp}/managed-registry-changed-values.yaml"
helm template managed-registry "${kp_root}/charts/kuberploy" --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/managed-registry-changed-values.yaml" > "${kp_tmp}/managed-registry-changed.yaml"
kp_registry_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/managed-registry.yaml")"
kp_registry_changed_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/managed-registry-changed.yaml")"
[[ "${kp_registry_config_name}" != "${kp_registry_changed_config_name}" ]] || { printf 'managed registry mutation did not produce a new immutable ConfigMap name\n' >&2; exit 1; }

for kp_registry_mutation in \
  '.components.worker.image.reference = "kuberploy-worker:mutable"' \
  '.config.managedRegistry.allowPlainHTTP = false' \
  '.config.managedRegistry.credentialSecret.passwordKey = .config.managedRegistry.credentialSecret.usernameKey' \
  '.config.managedRegistry.pullCredentialRef = .config.managedRegistry.pushCredentialRef' \
  '.config.managedRegistry.lifecycleCredentialRef = .config.managedRegistry.cacheCredentialRef' \
  '.config.managedRegistry.targetID = "../../other"' \
  '.config.managedRegistry.servicePort = 5001' \
  '.config.managedRegistry.credentialRef = "ambiguous-legacy-reference"' \
  '.config.managedRegistry.unknownField = true' \
  '.networkPolicy.enabled = false' \
  '.networkPolicy.kubeAPIServerCIDRs = []'; do
  yq "${kp_registry_mutation}" "${kp_tmp}/managed-registry-values.yaml" > "${kp_tmp}/managed-registry-invalid.yaml"
  if helm template invalid-managed-registry "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render -f "${kp_tmp}/managed-registry-invalid.yaml" >/dev/null 2>&1; then
    printf 'platform chart accepted invalid managed registry runtime: %s\n' "${kp_registry_mutation}" >&2
    exit 1
  fi
done

# Template validation remains effective even when an operator bypasses values
# schema validation in Helm.
if helm template invalid-platform "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_root}/test/e2e/fixtures/platform-values.yaml" \
  --skip-schema-validation \
  --set-string 'networkPolicy.kubeAPIServerCIDRs[0]=::/0' >/dev/null 2>&1; then
  printf 'platform template accepted an all-address Kubernetes API CIDR with schema validation bypassed\n' >&2
  exit 1
fi
if rg -n 'KUBERPLOY_GITHUB_WEBHOOK_ENABLED' "${kp_tmp}/platform.yaml"; then
  printf 'platform chart exposed the unfinished GitHub webhook capability\n' >&2
  exit 1
fi

yq '.config.githubApp.enabled = true |
    .config.githubApp.appID = 123456 |
    .config.githubApp.clientID = "Iv1_KuberployClient" |
    .config.githubApp.appSlug = "kuberploy-test" |
    .config.publicURL = "https://kuberploy.example.test" |
    .config.githubApp.secretRef.name = "kuberploy-github-app" |
    .builder.enabled = true |
    .builder.builderAgentImage = "ghcr.io/kuberploy/kuberploy-builder-agent:0.1.0-rc.166" |
    .builder.networkPolicy.sourceEgressCIDRs = ["192.0.2.30/32"] |
    .builder.networkPolicy.registryEgressCIDRs = ["192.0.2.31/32"] |
    .builder.controllerServiceAccount.namespace = "kuberploy-e2e-render" |
    .builder.controllerServiceAccount.name = "github-builds-worker"' \
  "${kp_root}/test/e2e/fixtures/platform-values.yaml" > "${kp_tmp}/github-build-values.yaml"
helm template github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/github-build-values.yaml" > "${kp_tmp}/github-builds.yaml"
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GITHUB_BUILDS_ENABLED' "${kp_tmp}/github-builds.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GITHUB_APP_ID' "${kp_tmp}/github-builds.yaml")" == "123456" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GITHUB_APP_CLIENT_ID' "${kp_tmp}/github-builds.yaml")" == "Iv1_KuberployClient" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GITHUB_APP_SLUG' "${kp_tmp}/github-builds.yaml")" == "kuberploy-test" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GIT_PROJECTION_WEBHOOK_WAKE_ENABLED' "${kp_tmp}/github-builds.yaml")" == "true" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_GIT_PROJECTION_WEBHOOK_WAKE_ENABLED" and .valueFrom.configMapKeyRef.key == "KUBERPLOY_GIT_PROJECTION_WEBHOOK_WAKE_ENABLED")] | length' "${kp_tmp}/github-builds.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_AUTO_DEPLOY_ENABLED" and .valueFrom.configMapKeyRef.key == "KUBERPLOY_AUTO_DEPLOY_ENABLED")] | length' "${kp_tmp}/github-builds.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_AUTO_DEPLOY_ENABLED")] | length' "${kp_tmp}/github-builds.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_BUILDER_NAMESPACE' "${kp_tmp}/github-builds.yaml")" == "kuberploy-build-dind" ]]

yq '.components.worker.image.reference = "ghcr.io/kuberploy/kuberploy-worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" |
    .config.managedRegistry.enabled = true |
    .config.managedRegistry.targetID = "11111111-1111-4111-8111-111111111111" |
    .config.managedRegistry.targetName = "Managed registry" |
    .config.managedRegistry.endpoint = "https://registry.example.test" |
    .config.managedRegistry.repositoryPrefix = "kuberploy" |
    .config.managedRegistry.pullCredentialRef = "registry-pull" |
    .config.managedRegistry.pushCredentialRef = "registry-push" |
    .config.managedRegistry.cacheCredentialRef = "registry-cache" |
    .config.managedRegistry.lifecycleCredentialRef = "operator/managed-registry" |
    .config.managedRegistry.namespace = "kuberploy-system" |
    .config.managedRegistry.deployment = "registry" |
    .config.managedRegistry.persistentVolumeClaim = "registry" |
    .config.managedRegistry.registryConfigMap = "registry-config-abc123" |
    .config.managedRegistry.credentialSecret.name = "registry-lifecycle"' \
  "${kp_tmp}/github-build-values.yaml" > "${kp_tmp}/github-managed-registry-values.yaml"
helm template github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render -f "${kp_tmp}/github-managed-registry-values.yaml" > "${kp_tmp}/github-managed-registry.yaml"
[[ "$(yq eval-all -o=json 'select(.kind == "NetworkPolicy" and (.metadata.name | test("builder-managed-registry$")))' "${kp_tmp}/github-managed-registry.yaml" | jq '.metadata.namespace == "kuberploy-build-dind" and .spec.podSelector.matchLabels["app.kubernetes.io/name"] == "kuberploy-builder" and .spec.podSelector.matchLabels["app.kubernetes.io/component"] == "source-build" and .spec.egress[0].to[0].namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == "kuberploy-system" and .spec.egress[0].to[0].podSelector.matchLabels["app.kubernetes.io/name"] == "kuberploy-registry" and .spec.egress[0].to[0].podSelector.matchLabels["app.kubernetes.io/instance"] == "registry" and .spec.egress[0].ports == [{"port":5000,"protocol":"TCP"}]')" == "true" ]]
[[ "$(yq eval-all -o=json 'select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name | test("builder-managed-registry$")))' "${kp_tmp}/github-managed-registry.yaml" | jq '(.metadata.annotations["argocd.argoproj.io/sync-wave"] == "-1") and (.spec.validations | length) == 5 and ([.spec.validations[].expression] | join("\n") | contains("argocd-application-controller") and contains("source-build") and contains("kuberploy-registry") and contains("port == 5000"))')" == "true" ]]
[[ "$(yq eval-all -o=json 'select(.kind == "ValidatingAdmissionPolicyBinding" and (.metadata.name | test("builder-managed-registry$")))' "${kp_tmp}/github-managed-registry.yaml" | jq '.metadata.annotations["argocd.argoproj.io/sync-wave"] == "-1" and .spec.validationActions == ["Deny"] and .spec.matchResources.namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == "kuberploy-build-dind"')" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.containers[0].volumeMounts[] | select(.name == "github-app-private-key") | .mountPath' "${kp_tmp}/github-builds.yaml")" == "/var/run/secrets/kuberploy/github-app" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[] | select(.name == "github-app-private-key") | .secret.secretName' "${kp_tmp}/github-builds.yaml")" == "kuberploy-github-app" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[] | select(.name == "github-app-private-key") | .secret.defaultMode' "${kp_tmp}/github-builds.yaml")" == "288" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[] | select(.name == "github-app-private-key") | .secret.items | length' "${kp_tmp}/github-builds.yaml")" == "1" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[] | select(.name == "github-app-private-key") | .secret.items[0].key' "${kp_tmp}/github-builds.yaml")" == "private-key.pem" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[] | select(.name == "github-app-private-key") | .secret.items[0].path' "${kp_tmp}/github-builds.yaml")" == "runtime/private-key.pem" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.template.spec.volumes[]? | select(.name == "github-app-private-key")] | length' "${kp_tmp}/github-builds.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.template.spec.volumes[] | select(.name == "github-app-api-runtime") | [.secret.items[].path] | sort | join(",")' "${kp_tmp}/github-builds.yaml")" == "runtime/oauth-client-secret,runtime/private-key.pem,runtime/state-signing-secret,runtime/webhook-secret" ]]
[[ "$(yq eval-all 'select(.kind == "Ingress") | [.spec.rules[].http.paths[] | select((.path == "/v1/github/installations/setup" or .path == "/v1/github/installations/callback" or .path == "/v1/webhooks/github") and .pathType == "Exact" and .backend.service.name == "github-builds-api")] | length' "${kp_tmp}/github-builds.yaml" | tail -1)" == "3" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_BUILDER_AGENT_IMAGE" or .name == "KUBERPLOY_BUILDER_BUILDKIT_IMAGE" or .name == "KUBERPLOY_BUILDER_SOURCE_EGRESS_CIDRS" or .name == "KUBERPLOY_BUILDER_REGISTRY_EGRESS_CIDRS")] | length' "${kp_tmp}/github-builds.yaml" | tail -1)" == "8" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_BUILDER_BUILDKIT_IMAGE" and .valueFrom.configMapKeyRef.key == "KUBERPLOY_BUILDER_BUILDKIT_IMAGE")] | length' "${kp_tmp}/github-builds.yaml" | tail -1)" == "2" ]]

yq '.config.gitProjection.webhookWakeEnabled = false' "${kp_tmp}/github-build-values.yaml" >"${kp_tmp}/github-build-no-webhook-wake-values.yaml"
helm template github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render -f "${kp_tmp}/github-build-no-webhook-wake-values.yaml" \
  >"${kp_tmp}/github-build-no-webhook-wake.yaml"
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GIT_PROJECTION_WEBHOOK_WAKE_ENABLED' "${kp_tmp}/github-build-no-webhook-wake.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GIT_PROJECTION_POLL_INTERVAL_SECONDS' "${kp_tmp}/github-build-no-webhook-wake.yaml")" == "300" ]]

yq '.config.buildLogs.enabled = true |
    .components.api.image.reference = "ghcr.io/kuberploy/kuberploy-api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"' \
  "${kp_tmp}/github-build-values.yaml" > "${kp_tmp}/build-logs-values.yaml"
helm template github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/build-logs-values.yaml" > "${kp_tmp}/build-logs.yaml"
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_BUILD_LOGS_ENABLED' "${kp_tmp}/build-logs.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | [.spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_BUILD_LOGS_ENABLED" and .valueFrom.configMapKeyRef.key == "KUBERPLOY_BUILD_LOGS_ENABLED")] | length' "${kp_tmp}/build-logs.yaml")" == "1" ]]
[[ "$(yq eval-all 'select(.kind == "Role" and (.metadata.name | test("-build-logs$"))) | .metadata.namespace' "${kp_tmp}/build-logs.yaml")" == "kuberploy-build-dind" ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "Role" and (.metadata.name | test("-build-logs$"))) | .rules' "${kp_tmp}/build-logs.yaml")" == '[{"apiGroups":["batch"],"resources":["jobs"],"verbs":["get"]},{"apiGroups":[""],"resources":["pods"],"verbs":["get","list"]},{"apiGroups":[""],"resources":["pods/log"],"verbs":["get"]}]' ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "RoleBinding" and (.metadata.name | test("-build-logs$"))) | .subjects' "${kp_tmp}/build-logs.yaml")" == '[{"kind":"ServiceAccount","name":"github-builds-api","namespace":"kuberploy-e2e-render"}]' ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.egress[] | select(.to[0].ipBlock.cidr == "10.43.0.1/32" and (.ports | length) == 2 and .ports[0].port == 443 and .ports[1].port == 6443)] | length' "${kp_tmp}/build-logs.yaml" | tail -1)" == "1" ]]
kp_build_logs_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/build-logs.yaml")"
kp_build_logs_base_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/github-builds.yaml")"
[[ "${kp_build_logs_config_name}" != "${kp_build_logs_base_config_name}" ]] || { printf 'build-log enablement did not produce a new immutable ConfigMap name\n' >&2; exit 1; }

for kp_build_log_mutation in \
  '.components.api.image.reference = "kuberploy-api:mutable"' \
  '.networkPolicy.enabled = false' \
  '.config.githubApp.enabled = false' \
  '.builder.enabled = false' \
  '.config.buildLogs.attacker = true'; do
  yq "${kp_build_log_mutation}" "${kp_tmp}/build-logs-values.yaml" > "${kp_tmp}/build-logs-invalid.yaml"
  if helm template invalid-build-logs "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render -f "${kp_tmp}/build-logs-invalid.yaml" >/dev/null 2>&1; then
    printf 'platform chart accepted invalid source-build log boundary: %s\n' "${kp_build_log_mutation}" >&2
    exit 1
  fi
done

yq '.config.gitProjection.enabled = true | .config.gitProjection.chartVersion = "0.1.0-rc.166"' \
  "${kp_tmp}/github-build-values.yaml" > "${kp_tmp}/git-projection-values.yaml"
helm template github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/git-projection-values.yaml" > "${kp_tmp}/git-projection.yaml"
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GIT_PROJECTION_ENABLED' "${kp_tmp}/git-projection.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GIT_PROJECTION_AUTH_MODE' "${kp_tmp}/git-projection.yaml")" == "github-app" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GIT_PROJECTION_CACHE_MAX_BYTES' "${kp_tmp}/git-projection.yaml")" == "536870912" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GIT_PROJECTION_POLL_INTERVAL_SECONDS' "${kp_tmp}/git-projection.yaml")" == "300" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GIT_PROJECTION_CHART_VERSION' "${kp_tmp}/git-projection.yaml")" == "0.1.0-rc.166" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_GIT_PROJECTION_POLICY_VERSION' "${kp_tmp}/git-projection.yaml")" == "appconfig-v1alpha1" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.containers[0].volumeMounts[] | select(.name == "git-projection-cache") | .mountPath' "${kp_tmp}/git-projection.yaml")" == "/var/lib/kuberploy/git-projection" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[] | select(.name == "git-projection-cache") | .emptyDir.sizeLimit' "${kp_tmp}/git-projection.yaml")" == "536870912" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[] | select(.name == "github-app-private-key") | .secret.items | length' "${kp_tmp}/git-projection.yaml")" == "1" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[] | select(.name == "github-app-private-key") | .secret.items[0].path' "${kp_tmp}/git-projection.yaml")" == "runtime/private-key.pem" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.template.spec.volumes[]? | select(.name == "git-projection-cache")] | length' "${kp_tmp}/git-projection.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[] | select(.name == "github-app-private-key") | [.secret.items[].path] | join(",")' "${kp_tmp}/git-projection.yaml")" == "runtime/private-key.pem" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_GIT_PROJECTION_AUTH_MODE") | .valueFrom.configMapKeyRef.key' "${kp_tmp}/git-projection.yaml")" == "KUBERPLOY_GIT_PROJECTION_AUTH_MODE" ]]
for kp_component in api worker; do
  [[ "$(kp_component="${kp_component}" yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == strenv(kp_component)) | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_GIT_PROJECTION_CHART_VERSION" or .name == "KUBERPLOY_GIT_PROJECTION_POLICY_VERSION")] | length' "${kp_tmp}/git-projection.yaml" | tail -1)" == "2" ]]
done

# The API creates only the exact operator-owned binding UUID. Argo and the
# foundation may share it from the first installer render.
yq '.config.platformGitBinding.enabled = true |
    .config.platformGitBinding.bindingID = "11111111-1111-4111-8111-111111111111" |
    .config.platformGitBinding.clusterID = "22222222-2222-4222-8222-222222222222"' \
  "${kp_tmp}/git-projection-values.yaml" > "${kp_tmp}/platform-git-bootstrap-values.yaml"
helm template github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/platform-git-bootstrap-values.yaml" > "${kp_tmp}/platform-git-bootstrap.yaml"
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_CLUSTER_ID' "${kp_tmp}/platform-git-bootstrap.yaml")" == "22222222-2222-4222-8222-222222222222" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_PLATFORM_GIT_BINDING_ID' "${kp_tmp}/platform-git-bootstrap.yaml")" == "11111111-1111-4111-8111-111111111111" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_PLATFORM_GIT_BINDING_ID")] | length' "${kp_tmp}/platform-git-bootstrap.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_CLUSTER_ID")] | length' "${kp_tmp}/platform-git-bootstrap.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_CLUSTER_ID")] | length' "${kp_tmp}/platform-git-bootstrap.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | [.data.KUBERPLOY_ARGO_DESIRED_STATE_ENABLED,.data.KUBERPLOY_ENVIRONMENT_FOUNDATION_ENABLED] | join(",")' "${kp_tmp}/platform-git-bootstrap.yaml")" == "false,false" ]]

for kp_platform_bootstrap_mutation in \
  '.config.platformGitBinding.enabled = false' \
  '.config.platformGitBinding.bindingID = "not-a-uuid"' \
  '.config.platformGitBinding.clusterID = "not-a-uuid"' \
  '.config.gitProjection.enabled = false' \
  '.config.githubApp.enabled = false'; do
  yq "${kp_platform_bootstrap_mutation}" "${kp_tmp}/platform-git-bootstrap-values.yaml" > "${kp_tmp}/platform-git-bootstrap-invalid.yaml"
  if helm template github-builds "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render -f "${kp_tmp}/platform-git-bootstrap-invalid.yaml" \
    --skip-schema-validation >/dev/null 2>&1; then
    printf 'platform chart accepted invalid platform Git bootstrap settings: %s\n' "${kp_platform_bootstrap_mutation}" >&2
    exit 1
  fi
done

yq eval-all 'select(fileIndex == 0) * select(fileIndex == 1)' \
  "${kp_tmp}/git-projection-values.yaml" \
  "${kp_root}/test/e2e/fixtures/argo-desired-state-values.yaml" > "${kp_tmp}/argo-desired-state-values.yaml"
helm template argo-desired "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/argo-desired-state-values.yaml" > "${kp_tmp}/argo-desired-state.yaml"
helm template argo-desired "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/argo-desired-state-values.yaml" > "${kp_tmp}/argo-desired-state-again.yaml"
diff -u "${kp_tmp}/argo-desired-state.yaml" "${kp_tmp}/argo-desired-state-again.yaml"
yq eval-all 'true' "${kp_tmp}/argo-desired-state.yaml" >/dev/null

kp_argo_worker_image='ghcr.io/kuberploy/kuberploy-worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_ARGO_DESIRED_STATE_ENABLED' "${kp_tmp}/argo-desired-state.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | [.data.KUBERPLOY_ENVIRONMENT_FOUNDATION_ENABLED,.data.KUBERPLOY_ENVIRONMENT_FOUNDATION_PLATFORM_BINDING_ID,.data.KUBERPLOY_ENVIRONMENT_FOUNDATION_PSA_VERSION,.data.KUBERPLOY_ENVIRONMENT_FOUNDATION_POLL_INTERVAL_SECONDS] | join(",")' "${kp_tmp}/argo-desired-state.yaml")" == "true,11111111-1111-4111-8111-111111111111,v1.31,2" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | [.data.KUBERPLOY_RUNTIME_VIEW_ENABLED,.data.KUBERPLOY_ENVIRONMENT_FOUNDATION_CONTROL_PLANE_NAMESPACE,.data.KUBERPLOY_ENVIRONMENT_FOUNDATION_OBSERVER_SERVICE_ACCOUNT] | join(",")' "${kp_tmp}/argo-desired-state.yaml")" == "true,kuberploy-e2e-render,argo-desired-api" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_ARGO_PLATFORM_BINDING_ID' "${kp_tmp}/argo-desired-state.yaml")" == "11111111-1111-4111-8111-111111111111" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_CLUSTER_ID' "${kp_tmp}/argo-desired-state.yaml")" == "22222222-2222-4222-8222-222222222222" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | [.data.KUBERPLOY_ARGO_NAMESPACE,.data.KUBERPLOY_ARGO_RUNTIME_CHART_REPOSITORY,.data.KUBERPLOY_ARGO_RUNTIME_CHART_VERSION,.data.KUBERPLOY_ARGO_RUNTIME_CHART_DIGEST,.data.KUBERPLOY_ARGO_DESIRED_STATE_POLL_INTERVAL_SECONDS,.data.KUBERPLOY_ARGO_CATALOG_MAX_AGE_SECONDS] | join(",")' "${kp_tmp}/argo-desired-state.yaml")" == "kuberploy-e2e-render,oci://ghcr.io/kuberploy/charts,0.1.0-rc.166,sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc,2,300" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_ARGO_RENDERER_IMAGE' "${kp_tmp}/argo-desired-state.yaml")" == "${kp_argo_worker_image}" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.containers[0].image' "${kp_tmp}/argo-desired-state.yaml")" == "${kp_argo_worker_image}" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | (.data | has("KUBERPLOY_ARGO_ROOT_APPLICATION_NAME")) or (.data | has("KUBERPLOY_ARGO_REPOSITORY_SECRET_NAME"))' "${kp_tmp}/argo-desired-state.yaml")" == "false" ]]
for kp_component in api worker; do
  [[ "$(kp_component="${kp_component}" yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == strenv(kp_component)) | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_ARGO_DESIRED_STATE_ENABLED" or .name == "KUBERPLOY_ARGO_NAMESPACE" or .name == "KUBERPLOY_ARGO_PLATFORM_BINDING_ID" or .name == "KUBERPLOY_CLUSTER_ID" or .name == "KUBERPLOY_ARGO_RUNTIME_CHART_REPOSITORY" or .name == "KUBERPLOY_ARGO_RUNTIME_CHART_VERSION" or .name == "KUBERPLOY_ARGO_RUNTIME_CHART_DIGEST" or .name == "KUBERPLOY_ARGO_RENDERER_IMAGE" or .name == "KUBERPLOY_ARGO_DESIRED_STATE_POLL_INTERVAL_SECONDS" or .name == "KUBERPLOY_ARGO_CATALOG_MAX_AGE_SECONDS")] | length' "${kp_tmp}/argo-desired-state.yaml" | tail -1)" == "10" ]]
  [[ "$(kp_component="${kp_component}" yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == strenv(kp_component)) | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_ENVIRONMENT_FOUNDATION_ENABLED" or .name == "KUBERPLOY_ENVIRONMENT_FOUNDATION_PLATFORM_BINDING_ID" or .name == "KUBERPLOY_ENVIRONMENT_FOUNDATION_PSA_VERSION" or .name == "KUBERPLOY_ENVIRONMENT_FOUNDATION_POLL_INTERVAL_SECONDS" or .name == "KUBERPLOY_ENVIRONMENT_FOUNDATION_CONTROL_PLANE_NAMESPACE" or .name == "KUBERPLOY_ENVIRONMENT_FOUNDATION_OBSERVER_SERVICE_ACCOUNT")] | length' "${kp_tmp}/argo-desired-state.yaml" | tail -1)" == "6" ]]
done
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.egress[] | select(.to[0].ipBlock.cidr == "10.43.0.1/32")] | length' "${kp_tmp}/argo-desired-state.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment") | .spec.template.spec.containers[].env[]? | select(.name == "KUBERPLOY_ARGO_ROOT_APPLICATION_NAME" or .name == "KUBERPLOY_ARGO_REPOSITORY_SECRET_NAME")] | length' "${kp_tmp}/argo-desired-state.yaml" | tail -1)" == "0" ]]

[[ "$(yq eval-all 'select(.kind == "Role" and .metadata.labels."app.kubernetes.io/component" == "argo-desired-state") | .metadata.namespace' "${kp_tmp}/argo-desired-state.yaml")" == "kuberploy-e2e-render" ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "Role" and .metadata.labels."app.kubernetes.io/component" == "argo-desired-state") | .rules' "${kp_tmp}/argo-desired-state.yaml")" == '[{"apiGroups":["argoproj.io"],"resources":["applications"],"resourceNames":["kuberploy-platform-root"],"verbs":["get","patch"]},{"apiGroups":[""],"resources":["secrets"],"verbs":["create","patch","delete"]}]' ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "RoleBinding" and .metadata.labels."app.kubernetes.io/component" == "argo-desired-state") | .subjects' "${kp_tmp}/argo-desired-state.yaml")" == '[{"kind":"ServiceAccount","name":"argo-desired-worker","namespace":"kuberploy-e2e-render"}]' ]]
[[ "$(yq eval-all -o=json 'select(.kind == "Role" and .metadata.labels."app.kubernetes.io/component" == "argo-desired-state")' "${kp_tmp}/argo-desired-state.yaml" | jq '[.rules[] | select(.resources == ["secrets"]) | .verbs[] | select(. == "get" or . == "list" or . == "watch" or . == "update")] | length')" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "ClusterRole" or .kind == "ClusterRoleBinding") | select(.metadata.labels."app.kubernetes.io/component" == "argo-desired-state" or .metadata.labels."app.kubernetes.io/component" == "argo-repository-credentials-admission")] | length' "${kp_tmp}/argo-desired-state.yaml" | tail -1)" == "0" ]]

kp_argo_admission_name='argo-desired-argo-repositories-ff423e40c9'
export kp_argo_admission_name
[[ "$(yq eval-all -o=json 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == strenv(kp_argo_admission_name))' "${kp_tmp}/argo-desired-state.yaml" | jq '.spec.failurePolicy == "Fail" and .spec.matchConstraints.matchPolicy == "Exact" and (.spec.matchConditions | length) == 2 and (.spec.validations | length) == 4')" == "true" ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == strenv(kp_argo_admission_name)) | .spec.matchConstraints.resourceRules' "${kp_tmp}/argo-desired-state.yaml")" == '[{"apiGroups":[""],"apiVersions":["v1"],"operations":["CREATE","UPDATE","DELETE"],"resources":["secrets"],"scope":"Namespaced"}]' ]]
[[ "$(yq eval-all -o=json 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == strenv(kp_argo_admission_name))' "${kp_tmp}/argo-desired-state.yaml" | jq -r '[.spec.matchConditions[].expression] | join(",")')" == "request.userInfo.username == 'system:serviceaccount:kuberploy-e2e-render:argo-desired-worker',request.namespace == 'kuberploy-e2e-render'" ]]
[[ "$(yq eval-all -o=json 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == strenv(kp_argo_admission_name))' "${kp_tmp}/argo-desired-state.yaml" | jq '[.spec.validations[].expression] | join("\n") | contains("object.metadata.name == '\''kuberploy-repo-'\'' + object.metadata.labels['\''kuberploy.io/git-binding-id'\''].replace('\''-'\'', '\'''\'')") and contains("object.metadata.labels.size() == 4") and contains("object.metadata.annotations.size() == 2") and contains("object.data.size() == 5") and contains("githubAppPrivateKey") and contains("oldObject.metadata.name == '\''kuberploy-repo-'\''") and contains("request.operation != '\''DELETE'\''")')" == "true" ]]
[[ "$(yq eval-all -o=json 'select(.kind == "ValidatingAdmissionPolicyBinding" and .metadata.name == strenv(kp_argo_admission_name))' "${kp_tmp}/argo-desired-state.yaml" | jq --arg name "${kp_argo_admission_name}" '.spec.policyName == $name and .spec.validationActions == ["Deny"] and .spec.matchResources.matchPolicy == "Exact" and .spec.matchResources.namespaceSelector == {"matchLabels":{"kubernetes.io/metadata.name":"kuberploy-e2e-render"}}')" == "true" ]]
kp_argo_refresh_admission_name='argo-desired-argo-root-refresh-ff423e40c9'
export kp_argo_refresh_admission_name
[[ "$(yq eval-all -o=json 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == strenv(kp_argo_refresh_admission_name))' "${kp_tmp}/argo-desired-state.yaml" | jq '.spec.failurePolicy == "Fail" and .spec.matchConstraints.matchPolicy == "Exact" and (.spec.matchConditions | length) == 2 and (.spec.validations | length) == 2')" == "true" ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == strenv(kp_argo_refresh_admission_name)) | .spec.matchConstraints.resourceRules' "${kp_tmp}/argo-desired-state.yaml")" == '[{"apiGroups":["argoproj.io"],"apiVersions":["v1alpha1"],"operations":["UPDATE"],"resources":["applications"],"scope":"Namespaced"}]' ]]
[[ "$(yq eval-all -o=json 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == strenv(kp_argo_refresh_admission_name))' "${kp_tmp}/argo-desired-state.yaml" | jq '[.spec.validations[].expression] | join("\n") | contains("request.name == '\''kuberploy-platform-root'\''") and contains("object.spec == oldObject.spec") and contains("argocd.argoproj.io/refresh") and contains("== '\''hard'\''")')" == "true" ]]
[[ "$(yq eval-all -o=json 'select(.kind == "ValidatingAdmissionPolicyBinding" and .metadata.name == strenv(kp_argo_refresh_admission_name))' "${kp_tmp}/argo-desired-state.yaml" | jq --arg name "${kp_argo_refresh_admission_name}" '.spec.policyName == $name and .spec.validationActions == ["Deny"] and .spec.matchResources.namespaceSelector == {"matchLabels":{"kubernetes.io/metadata.name":"kuberploy-e2e-render"}}')" == "true" ]]

yq '.config.argoDesiredState.pollIntervalSeconds = 3' "${kp_tmp}/argo-desired-state-values.yaml" > "${kp_tmp}/argo-desired-state-changed-values.yaml"
helm template argo-desired "${kp_root}/charts/kuberploy" --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/argo-desired-state-changed-values.yaml" > "${kp_tmp}/argo-desired-state-changed.yaml"
kp_argo_desired_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/argo-desired-state.yaml")"
kp_argo_desired_changed_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/argo-desired-state-changed.yaml")"
[[ "${kp_argo_desired_config_name}" != "${kp_argo_desired_changed_config_name}" ]] || { printf 'protected Argo desired-state mutation did not produce a new immutable ConfigMap name\n' >&2; exit 1; }

for kp_argo_desired_mutation in \
  '.config.argoDesiredState.enabled = false' \
  '.config.argoDesiredState.enabled = false | .config.argoDesiredState.platformBindingID = "" | .config.argoDesiredState.clusterID = "" | .config.argoDesiredState.namespace = "" | .config.argoDesiredState.runtimeChartRepository = "" | .config.argoDesiredState.runtimeChartVersion = "" | .config.argoDesiredState.runtimeChartDigest = "" | .config.argoDesiredState.pollIntervalSeconds = 3' \
  '.config.gitProjection.enabled = false' \
  '.config.githubApp.enabled = false' \
  '.config.argoObservation.enabled = false' \
  '.config.environmentFoundation.enabled = false' \
  '.config.environmentFoundation.platformBindingID = "33333333-3333-4333-8333-333333333333"' \
  '.config.environmentFoundation.clusterID = "33333333-3333-4333-8333-333333333333"' \
  '.config.environmentFoundation.psaVersion = "v1.24"' \
  '.config.environmentFoundation.pollIntervalSeconds = 0' \
  '.config.environmentFoundation.pollIntervalSeconds = 61' \
  '.config.environmentFoundation.manifestPath = "clusters/attacker/argocd/foundation.yaml"' \
  '.config.argoObservation.namespace = "other-argo"' \
  '.rbac.argoNamespace = "other-argo"' \
  '.networkPolicy.enabled = false' \
  '.networkPolicy.kubeAPIServerCIDRs = []' \
  '.networkPolicy.kubeAPIServerCIDRs = ["10.43.0.0/24"]' \
  '.components.api.image.reference = "kuberploy-api:mutable"' \
  '.components.worker.image.reference = "kuberploy-worker:mutable"' \
  '.config.argoDesiredState.platformBindingID = "11111111-1111-9111-8111-111111111111"' \
  '.config.argoDesiredState.clusterID = "not-a-uuid"' \
  '.config.argoDesiredState.runtimeChartRepository = "oci://user@ghcr.io/kuberploy/charts"' \
  '.config.argoDesiredState.runtimeChartVersion = "v1.2.3"' \
  '.config.argoDesiredState.runtimeChartDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"' \
  '.config.argoDesiredState.pollIntervalSeconds = 0' \
  '.config.argoDesiredState.pollIntervalSeconds = 61' \
  '.config.argoDesiredState.catalogMaxAgeSeconds = 59' \
  '.config.argoDesiredState.catalogMaxAgeSeconds = 3601' \
  '.config.argoDesiredState.credentialSecretName = "attacker-secret"'; do
  yq "${kp_argo_desired_mutation}" "${kp_tmp}/argo-desired-state-values.yaml" > "${kp_tmp}/argo-desired-state-invalid.yaml"
  if helm template invalid-argo-desired "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render -f "${kp_tmp}/argo-desired-state-invalid.yaml" >/dev/null 2>&1; then
    printf 'platform chart accepted invalid protected Argo desired-state settings: %s\n' "${kp_argo_desired_mutation}" >&2
    exit 1
  fi
  if helm template invalid-argo-desired "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render -f "${kp_tmp}/argo-desired-state-invalid.yaml" \
    --skip-schema-validation >/dev/null 2>&1; then
    printf 'protected Argo desired-state template accepted invalid settings with schema validation bypassed: %s\n' "${kp_argo_desired_mutation}" >&2
    exit 1
  fi
done

yq '.config.helmApplications.enabled = true |
    .config.helmApplications.rendererNamespace = "kuberploy-helm-renderer" |
    .config.helmApplications.ociRegistryHosts = ["ghcr.io"] |
    .config.helmApplications.ociAuthHosts = ["ghcr.io"] |
    .builder.controllerServiceAccount.name = "helm-applications-worker"' \
  "${kp_tmp}/argo-desired-state-values.yaml" > "${kp_tmp}/helm-applications-values.yaml"
helm template helm-applications "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/helm-applications-values.yaml" > "${kp_tmp}/helm-applications.yaml"
helm template helm-applications "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/helm-applications-values.yaml" > "${kp_tmp}/helm-applications-again.yaml"
diff -u "${kp_tmp}/helm-applications.yaml" "${kp_tmp}/helm-applications-again.yaml"
yq eval-all 'true' "${kp_tmp}/helm-applications.yaml" >/dev/null

[[ "$(yq eval-all 'select(.kind == "Namespace" and .metadata.name == "kuberploy-helm-renderer") | [.metadata.labels."app.kubernetes.io/component",.metadata.labels."pod-security.kubernetes.io/enforce"] | join(",")' "${kp_tmp}/helm-applications.yaml")" == "helm-renderer,restricted" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | [.data.KUBERPLOY_HELM_APPLICATIONS_ENABLED,.data.KUBERPLOY_HELM_RENDERER_NAMESPACE,.data.KUBERPLOY_HELM_RENDERER_SERVICE_ACCOUNT,.data.KUBERPLOY_HELM_RENDERER_POLL_MILLISECONDS,.data.KUBERPLOY_HELM_WORK_POLL_MILLISECONDS,.data.KUBERPLOY_HELM_RENDER_LEASE_SECONDS,.data.KUBERPLOY_HELM_PUBLISH_LEASE_SECONDS,.data.KUBERPLOY_HELM_READINESS_LEASE_SECONDS,.data.KUBERPLOY_HELM_OCI_REQUEST_SECONDS,.data.KUBERPLOY_HELM_OCI_REGISTRY_HOSTS,.data.KUBERPLOY_HELM_OCI_AUTH_HOSTS,.data.KUBERPLOY_HELM_OCI_REDIRECT_HOSTS,.data.KUBERPLOY_HELM_ARGO_NAMESPACE] | join(",")' "${kp_tmp}/helm-applications.yaml")" == "true,kuberploy-helm-renderer,helm-applications-helm-renderer,250,1000,60,90,30,15,ghcr.io,ghcr.io,pkg-containers.githubusercontent.com,kuberploy-e2e-render" ]]
[[ "$(yq eval-all -o=json 'select(.kind == "ConfigMap" and .data.KUBERPLOY_HELM_PACKAGE_CACHE_BYTES != null)' "${kp_tmp}/helm-applications.yaml" | jq -r '.data.KUBERPLOY_HELM_PACKAGE_CACHE_BYTES')" == "67108864" ]]
for kp_component in api worker; do
  [[ "$(kp_component="${kp_component}" yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == strenv(kp_component)) | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_HELM_"))] | length' "${kp_tmp}/helm-applications.yaml" | tail -1)" == "14" ]]
done
[[ "$(yq eval-all 'select(.kind == "ServiceAccount" and .metadata.labels."app.kubernetes.io/component" == "helm-renderer") | .metadata.namespace + "," + .metadata.name + "," + (.automountServiceAccountToken | tostring)' "${kp_tmp}/helm-applications.yaml")" == "kuberploy-helm-renderer,helm-applications-helm-renderer,false" ]]
[[ "$(yq eval-all 'select(.kind == "Role" and (.metadata.name | test("-helm-renderer$"))) | .metadata.namespace' "${kp_tmp}/helm-applications.yaml")" == "kuberploy-helm-renderer" ]]
[[ "$(yq eval-all '[select(.kind == "Role" and (.metadata.name | test("-helm-renderer$"))) | .rules[] | select(.resources[] == "secrets" or .verbs[] == "patch" or .verbs[] == "update")] | length' "${kp_tmp}/helm-applications.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "RoleBinding" and (.metadata.name | test("-helm-renderer$"))) | .subjects' "${kp_tmp}/helm-applications.yaml")" == '[{"kind":"ServiceAccount","name":"helm-applications-worker","namespace":"kuberploy-e2e-render"}]' ]]
[[ "$(yq eval-all -o=json 'select(.kind == "NetworkPolicy" and (.metadata.name | test("-helm-renderer-default-deny$")))' "${kp_tmp}/helm-applications.yaml" | jq '.metadata.namespace == "kuberploy-helm-renderer" and ([.spec.ingress[]?] | length) == 0 and ([.spec.egress[]?] | length) == 0')" == "true" ]]

yq '.config.helmApplications.ociRegistryHosts = ["ghcr.io","registry.example.test:5443"] |
    .config.helmApplications.ociAuthHosts = ["ghcr.io","registry.example.test:5443"] |
    .builder.controllerServiceAccount.name = "helm-private-oci-worker" |
    .config.helmApplications.ociCredentialProfiles = [
      {"registryHost":"ghcr.io","authHost":"ghcr.io","name":"ghcr-private","mode":"basic","secretRef":{"name":"helm-ghcr","usernameKey":"username","passwordKey":"password","tokenKey":""}},
      {"registryHost":"registry.example.test:5443","authHost":"registry.example.test:5443","name":"registry-private","mode":"bearer","secretRef":{"name":"helm-registry","usernameKey":"","passwordKey":"","tokenKey":"token"}}
    ]' "${kp_tmp}/helm-applications-values.yaml" > "${kp_tmp}/helm-private-oci-values.yaml"
helm template helm-private-oci "${kp_root}/charts/kuberploy" --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/helm-private-oci-values.yaml" > "${kp_tmp}/helm-private-oci.yaml"
kp_helm_oci_profiles="$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_HELM_OCI_CREDENTIAL_PROFILES_JSON' "${kp_tmp}/helm-private-oci.yaml")"
jq -e 'length == 2 and .[0].registryHost == "ghcr.io" and .[0].authHost == "ghcr.io" and .[0].name == "ghcr-private" and .[0].mode == "basic" and (.[0].projectionDigest | test("^sha256:[a-f0-9]{64}$")) and .[1].mode == "bearer" and (.[1].projectionDigest | test("^sha256:[a-f0-9]{64}$"))' <<<"${kp_helm_oci_profiles}" >/dev/null
[[ "${kp_helm_oci_profiles}" != *"helm-ghcr"* && "${kp_helm_oci_profiles}" != *"usernameKey"* && "${kp_helm_oci_profiles}" != *"tokenKey"* ]]
for kp_component in api worker; do
  [[ "$(kp_component="${kp_component}" yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == strenv(kp_component)) | .spec.template.spec.containers[0].volumeMounts[] | select(.name == "helm-oci-credentials" and .mountPath == "/var/run/secrets/kuberploy/helm-oci" and .readOnly == true)] | length' "${kp_tmp}/helm-private-oci.yaml" | tail -1)" == "1" ]]
  [[ "$(kp_component="${kp_component}" yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == strenv(kp_component)) | .spec.template.spec.volumes[] | select(.name == "helm-oci-credentials") | .projected.defaultMode' "${kp_tmp}/helm-private-oci.yaml")" == "288" ]]
  [[ "$(kp_component="${kp_component}" yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == strenv(kp_component)) | .spec.template.spec.volumes[] | select(.name == "helm-oci-credentials") | [.projected.sources[] | .secret.name + ":" + ([.secret.items[] | .key + "=" + .path] | join(";"))] | join(",")' "${kp_tmp}/helm-private-oci.yaml")" == "helm-ghcr:username=ghcr-private/username;password=ghcr-private/password,helm-registry:token=registry-private/token" ]]
done
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "web") | .spec.template.spec.volumes[]? | select(.name == "helm-oci-credentials")] | length' "${kp_tmp}/helm-private-oci.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select((.kind == "Role" or .kind == "ClusterRole") and (.metadata.name | test("helm-oci")))] | length' "${kp_tmp}/helm-private-oci.yaml" | tail -1)" == "0" ]]
for kp_component in api worker; do
  [[ "$(kp_component="${kp_component}" yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == strenv(kp_component)) | .spec.egress[] | select(.ports[]?.port == 443)] | length > 0' "${kp_tmp}/helm-private-oci.yaml" | tail -1)" == "true" ]]
  [[ "$(kp_component="${kp_component}" yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == strenv(kp_component)) | .spec.egress[] | select(.ports[]?.port == 5443)] | length' "${kp_tmp}/helm-private-oci.yaml" | tail -1)" == "1" ]]
done
kp_helm_private_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/helm-private-oci.yaml")"
kp_helm_public_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/helm-applications.yaml")"
[[ "${kp_helm_private_config_name}" != "${kp_helm_public_config_name}" ]] || { printf 'Helm OCI credential projection identity did not change immutable config\n' >&2; exit 1; }

yq '.config.helmApplications.ociRedirectHosts = ["cdn.example.test"]' "${kp_tmp}/helm-applications-values.yaml" > "${kp_tmp}/helm-redirect-values.yaml"
helm template helm-applications "${kp_root}/charts/kuberploy" --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/helm-redirect-values.yaml" > "${kp_tmp}/helm-redirect.yaml"
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_HELM_OCI_REDIRECT_HOSTS' "${kp_tmp}/helm-redirect.yaml")" == "cdn.example.test,pkg-containers.githubusercontent.com" ]]
kp_helm_redirect_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/helm-redirect.yaml")"
[[ "${kp_helm_redirect_config_name}" != "${kp_helm_public_config_name}" ]] || { printf 'Helm OCI redirect policy did not change immutable config\n' >&2; exit 1; }

yq '.config.helmApplications.workPollMilliseconds = 1100' "${kp_tmp}/helm-applications-values.yaml" > "${kp_tmp}/helm-applications-changed-values.yaml"
helm template helm-applications "${kp_root}/charts/kuberploy" --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/helm-applications-changed-values.yaml" > "${kp_tmp}/helm-applications-changed.yaml"
kp_helm_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/helm-applications.yaml")"
kp_helm_changed_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/helm-applications-changed.yaml")"
[[ "${kp_helm_config_name}" != "${kp_helm_changed_config_name}" ]] || { printf 'Helm operator config mutation did not produce a new immutable ConfigMap name\n' >&2; exit 1; }

for kp_helm_mutation in \
  '.config.helmApplications.enabled = false' \
  '.config.helmApplications.rendererNamespace = "kuberploy-e2e-render"' \
  '.config.helmApplications.ociRegistryHosts = []' \
  '.config.helmApplications.ociRegistryHosts = ["z.example.test","a.example.test"]' \
  '.config.helmApplications.ociAuthHosts = ["ghcr.io","ghcr.io"]' \
  '.config.helmApplications.ociRedirectHosts = ["cdn.example.test","cdn.example.test"]' \
  '.config.helmApplications.ociRedirectHosts = ["z.example.test","a.example.test"]' \
  '.config.helmApplications.ociRedirectHosts = ["https://cdn.example.test/path?query=1"]' \
  '.config.helmApplications.ociRegistryHosts = ["ghcr.io:65536"]' \
  '.config.helmApplications.ociRegistryHosts = ["bad..example.test"]' \
  '.config.helmApplications.renderLeaseSeconds = 30' \
  '.config.helmApplications.workPollMilliseconds = 30000 | .config.helmApplications.readinessLeaseSeconds = 30' \
  '.config.helmApplications.packageCacheBytes = 1024' \
  '.config.helmApplications.attackerCredentialSecret = "caller-secret"' \
  '.config.helmApplications.ociCredentialProfiles = [{"registryHost":"unknown.example.test","authHost":"ghcr.io","name":"private","mode":"basic","secretRef":{"name":"helm-private","usernameKey":"username","passwordKey":"password","tokenKey":""}}]' \
  '.config.helmApplications.ociCredentialProfiles = [{"registryHost":"ghcr.io","authHost":"ghcr.io","name":"same","mode":"basic","secretRef":{"name":"one","usernameKey":"username","passwordKey":"password","tokenKey":""}},{"registryHost":"ghcr.io","authHost":"ghcr.io","name":"same","mode":"bearer","secretRef":{"name":"two","usernameKey":"","passwordKey":"","tokenKey":"token"}}]' \
  '.config.helmApplications.ociRegistryHosts = ["ghcr.io","registry.example.test"] | .config.helmApplications.ociAuthHosts = ["ghcr.io","registry.example.test"] | .config.helmApplications.ociCredentialProfiles = [{"registryHost":"registry.example.test","authHost":"registry.example.test","name":"second","mode":"bearer","secretRef":{"name":"two","usernameKey":"","passwordKey":"","tokenKey":"token"}},{"registryHost":"ghcr.io","authHost":"ghcr.io","name":"first","mode":"basic","secretRef":{"name":"one","usernameKey":"username","passwordKey":"password","tokenKey":""}}]' \
  '.config.helmApplications.ociRegistryHosts = ["ghcr.io","registry.example.test"] | .config.helmApplications.ociAuthHosts = ["ghcr.io","registry.example.test"] | .config.helmApplications.ociCredentialProfiles = [{"registryHost":"ghcr.io","authHost":"ghcr.io","name":"same","mode":"basic","secretRef":{"name":"one","usernameKey":"username","passwordKey":"password","tokenKey":""}},{"registryHost":"registry.example.test","authHost":"registry.example.test","name":"same","mode":"bearer","secretRef":{"name":"two","usernameKey":"","passwordKey":"","tokenKey":"token"}}]' \
  '.config.helmApplications.ociCredentialProfiles = [{"registryHost":"ghcr.io","authHost":"ghcr.io","name":"private","mode":"basic","secretRef":{"name":"helm-private","usernameKey":"username","passwordKey":"password","tokenKey":"token"}}]' \
  '.config.helmApplications.ociCredentialProfiles = [{"registryHost":"ghcr.io","authHost":"ghcr.io","name":"private","mode":"bearer","secretRef":{"name":"helm-private","usernameKey":"","passwordKey":"","tokenKey":"../token"}}]' \
  '.config.helmApplications.ociCredentialProfiles = [{"registryHost":"ghcr.io","authHost":"ghcr.io","name":"private","mode":"bearer","secretRef":{"name":"helm-private","usernameKey":"","passwordKey":"","tokenKey":"token"},"callerCredential":"bad"}]' \
  '.config.gitProjection.enabled = false' \
  '.config.argoDesiredState.enabled = false' \
  '.config.environmentFoundation.enabled = false' \
  '.networkPolicy.enabled = false' \
  '.networkPolicy.externalEgressCIDRs = []' \
  '.networkPolicy.externalEgressCIDRs = ["192.0.2.0/24"]' \
  '.networkPolicy.kubeAPIServerCIDRs = ["10.43.0.0/24"]' \
  '.components.api.image.reference = "kuberploy-api:mutable"' \
  '.components.worker.image.reference = "kuberploy-worker:mutable"'; do
  yq "${kp_helm_mutation}" "${kp_tmp}/helm-applications-values.yaml" > "${kp_tmp}/helm-applications-invalid.yaml"
  if helm template invalid-helm-applications "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render -f "${kp_tmp}/helm-applications-invalid.yaml" >/dev/null 2>&1; then
    printf 'platform chart accepted invalid approved Helm application settings: %s\n' "${kp_helm_mutation}" >&2
    exit 1
  fi
  if helm template invalid-helm-applications "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render -f "${kp_tmp}/helm-applications-invalid.yaml" \
    --skip-schema-validation >/dev/null 2>&1; then
    printf 'approved Helm template accepted invalid settings with schema validation bypassed: %s\n' "${kp_helm_mutation}" >&2
    exit 1
  fi
done

yq '.config.runtimeRegistryPulls.enabled = true |
    .config.runtimeRegistryPulls.namespaces = ["apps-production", "apps-staging"] |
    .config.runtimeRegistryPulls.profiles = [
      {"name":"managed-a","targetId":"11111111-1111-7111-8111-111111111111","registryServer":"registry-a.example.test","credentialRef":"runtime-pull/a","revision":3,"sourceSecretRef":"pull-source-a","sourceSecretKey":".dockerconfigjson"},
      {"name":"managed-b","targetId":"22222222-2222-7222-8222-222222222222","registryServer":"registry-b.example.test:5443","credentialRef":"runtime-pull/b","revision":9,"sourceSecretRef":"pull-source-b","sourceSecretKey":"config.json"}
    ] |
    .components.api.image.reference = "ghcr.io/kuberploy/kuberploy-api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" |
    .components.worker.image.reference = "ghcr.io/kuberploy/kuberploy-worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" |
    .builder.controllerServiceAccount.name = "runtime-pulls-worker" |
    .rbac.observedNamespaces = []' \
  "${kp_tmp}/git-projection-values.yaml" > "${kp_tmp}/runtime-registry-pulls-values.yaml"
helm template runtime-pulls "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/runtime-registry-pulls-values.yaml" > "${kp_tmp}/runtime-registry-pulls.yaml"
yq eval-all 'true' "${kp_tmp}/runtime-registry-pulls.yaml" >/dev/null

kp_runtime_pull_profiles='[{"credentialRef":"runtime-pull/a","name":"managed-a","registryServer":"registry-a.example.test","revision":3,"sourceSecretKey":".dockerconfigjson","sourceSecretRef":"pull-source-a","targetId":"11111111-1111-7111-8111-111111111111"},{"credentialRef":"runtime-pull/b","name":"managed-b","registryServer":"registry-b.example.test:5443","revision":9,"sourceSecretKey":"config.json","sourceSecretRef":"pull-source-b","targetId":"22222222-2222-7222-8222-222222222222"}]'
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_RUNTIME_REGISTRY_PULLS_ENABLED' "${kp_tmp}/runtime-registry-pulls.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_RUNTIME_REGISTRY_PULL_NAMESPACES' "${kp_tmp}/runtime-registry-pulls.yaml")" == "apps-production,apps-staging" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_RUNTIME_REGISTRY_PULL_PROFILES' "${kp_tmp}/runtime-registry-pulls.yaml")" == "${kp_runtime_pull_profiles}" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | [.data.KUBERPLOY_RUNTIME_REGISTRY_PULL_POLL_SECONDS,.data.KUBERPLOY_RUNTIME_REGISTRY_PULL_WORK_LEASE_SECONDS,.data.KUBERPLOY_RUNTIME_REGISTRY_PULL_HEARTBEAT_SECONDS,.data.KUBERPLOY_RUNTIME_REGISTRY_PULL_READINESS_SECONDS,.data.KUBERPLOY_RUNTIME_REGISTRY_PULL_MINIMUM_BACKOFF_SECONDS,.data.KUBERPLOY_RUNTIME_REGISTRY_PULL_MAXIMUM_BACKOFF_SECONDS] | join(",")' "${kp_tmp}/runtime-registry-pulls.yaml")" == "30,120,20,90,5,300" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_RUNTIME_REGISTRY_PULL"))] | length' "${kp_tmp}/runtime-registry-pulls.yaml" | tail -1)" == "18" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "web") | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_RUNTIME_REGISTRY_PULL"))] | length' "${kp_tmp}/runtime-registry-pulls.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].volumeMounts[] | select(.name == "runtime-registry-pull-credentials" and .mountPath == "/var/run/secrets/kuberploy/registry-pulls" and .readOnly == true)] | length' "${kp_tmp}/runtime-registry-pulls.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.volumes[] | select(.name == "runtime-registry-pull-credentials" and .projected.defaultMode == 288 and (.projected.sources | length) == 2)] | length' "${kp_tmp}/runtime-registry-pulls.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all -o=json '.' "${kp_tmp}/runtime-registry-pulls.yaml" | jq -s '[.[] | select(.kind == "Deployment" and (.metadata.labels["app.kubernetes.io/component"] == "api" or .metadata.labels["app.kubernetes.io/component"] == "worker")) | .spec.template.spec.volumes[] | select(.name == "runtime-registry-pull-credentials") | [.projected.sources[] | .secret.name + ":" + .secret.items[0].key + ":" + .secret.items[0].path] | join(",") | select(. == "pull-source-a:.dockerconfigjson:managed-a/dockerconfigjson,pull-source-b:config.json:managed-b/dockerconfigjson")] | length')" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "web") | .spec.template.spec.volumes[]? | select(.name == "runtime-registry-pull-credentials")] | length' "${kp_tmp}/runtime-registry-pulls.yaml" | tail -1)" == "0" ]]

[[ "$(yq eval-all '[select(.kind == "Role" and .metadata.labels."app.kubernetes.io/component" == "runtime-registry-pulls")] | length' "${kp_tmp}/runtime-registry-pulls.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "RoleBinding" and .metadata.labels."app.kubernetes.io/component" == "runtime-registry-pulls")] | length' "${kp_tmp}/runtime-registry-pulls.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "ValidatingAdmissionPolicy" and .metadata.labels."app.kubernetes.io/component" == "runtime-registry-pulls-admission")] | length' "${kp_tmp}/runtime-registry-pulls.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "ValidatingAdmissionPolicyBinding" and .metadata.labels."app.kubernetes.io/component" == "runtime-registry-pulls-admission")] | length' "${kp_tmp}/runtime-registry-pulls.yaml" | tail -1)" == "2" ]]
for kp_runtime_pull_namespace in apps-production apps-staging; do
  case "${kp_runtime_pull_namespace}" in
    apps-production)
      kp_runtime_pull_name_a="kuberploy-pull-35131b85831a592e67c58666"
      kp_runtime_pull_name_b="kuberploy-pull-7368fd1d2c3cbcdede0fa2fe"
      kp_runtime_pull_policy="runtime-pulls-registry-pulls-3a3603529c"
      ;;
    apps-staging)
      kp_runtime_pull_name_a="kuberploy-pull-2bc12d58cf5badee8356f4ce"
      kp_runtime_pull_name_b="kuberploy-pull-45193fc6e15e4ed3b7d0e56b"
      kp_runtime_pull_policy="runtime-pulls-registry-pulls-10ae1a680b"
      ;;
  esac
  kp_runtime_pull_names="${kp_runtime_pull_name_a},${kp_runtime_pull_name_b}"
  kp_runtime_pull_pair_a="(object.metadata.name == '${kp_runtime_pull_name_a}' && object.metadata.labels['kuberploy.io/registry-target-id'] == '11111111-1111-7111-8111-111111111111' && object.metadata.labels['kuberploy.io/profile-revision'] == '3' && object.metadata.annotations['kuberploy.io/pull-credential-ref'] == 'runtime-pull/a')"
  kp_runtime_pull_pair_b="(object.metadata.name == '${kp_runtime_pull_name_b}' && object.metadata.labels['kuberploy.io/registry-target-id'] == '22222222-2222-7222-8222-222222222222' && object.metadata.labels['kuberploy.io/profile-revision'] == '9' && object.metadata.annotations['kuberploy.io/pull-credential-ref'] == 'runtime-pull/b')"
  [[ "$(kp_runtime_pull_namespace="${kp_runtime_pull_namespace}" yq eval-all -o=json 'select(.kind == "Role" and .metadata.labels."app.kubernetes.io/component" == "runtime-registry-pulls" and .metadata.namespace == strenv(kp_runtime_pull_namespace))' "${kp_tmp}/runtime-registry-pulls.yaml" | jq --arg names "${kp_runtime_pull_names}" '(.rules | length) == 2 and .rules[0].apiGroups == [""] and .rules[0].resources == ["secrets"] and .rules[0].verbs == ["get"] and (.rules[0].resourceNames | join(",")) == $names and .rules[1].apiGroups == [""] and .rules[1].resources == ["secrets"] and .rules[1].verbs == ["create"] and (.rules[1] | has("resourceNames") | not)')" == "true" ]]
  [[ "$(kp_runtime_pull_namespace="${kp_runtime_pull_namespace}" yq eval-all -o=json -I=0 'select(.kind == "RoleBinding" and .metadata.labels."app.kubernetes.io/component" == "runtime-registry-pulls" and .metadata.namespace == strenv(kp_runtime_pull_namespace)) | .subjects' "${kp_tmp}/runtime-registry-pulls.yaml")" == '[{"kind":"ServiceAccount","name":"runtime-pulls-worker","namespace":"kuberploy-e2e-render"}]' ]]
  [[ "$(kp_runtime_pull_namespace="${kp_runtime_pull_namespace}" yq eval-all -o=json -I=0 'select(.kind == "RoleBinding" and .metadata.labels."app.kubernetes.io/component" == "runtime-registry-pulls" and .metadata.namespace == strenv(kp_runtime_pull_namespace)) | .roleRef' "${kp_tmp}/runtime-registry-pulls.yaml")" == '{"apiGroup":"rbac.authorization.k8s.io","kind":"Role","name":"runtime-pulls-runtime-registry-pulls"}' ]]
  [[ "$(kp_runtime_pull_policy="${kp_runtime_pull_policy}" yq eval-all -o=json 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == strenv(kp_runtime_pull_policy))' "${kp_tmp}/runtime-registry-pulls.yaml" | jq --arg namespace "${kp_runtime_pull_namespace}" --arg pair_a "${kp_runtime_pull_pair_a}" --arg pair_b "${kp_runtime_pull_pair_b}" '([.spec.validations[].expression] | join("\n")) as $expressions | .spec.failurePolicy == "Fail" and (.spec.validations | length) == 3 and ($expressions | contains("system:serviceaccount:kuberploy-e2e-render:runtime-pulls-worker")) and ($expressions | contains($namespace)) and ($expressions | contains($pair_a)) and ($expressions | contains($pair_b)) and ($expressions | contains("oldObject.metadata.name.startsWith('\''kuberploy-pull-'\'')")) and ($expressions | contains("kubernetes.io/dockerconfigjson")) and ($expressions | contains("object.immutable == true")) and ($expressions | contains("object.stringData.size() == 0")) and ($expressions | contains("<= 87384"))')" == "true" ]]
  [[ "$(kp_runtime_pull_policy="${kp_runtime_pull_policy}" yq eval-all -o=json 'select(.kind == "ValidatingAdmissionPolicyBinding" and .metadata.name == strenv(kp_runtime_pull_policy))' "${kp_tmp}/runtime-registry-pulls.yaml" | jq --arg policy "${kp_runtime_pull_policy}" --arg namespace "${kp_runtime_pull_namespace}" '.spec.policyName == $policy and .spec.validationActions == ["Deny"] and .spec.matchResources.namespaceSelector == {"matchLabels":{"kubernetes.io/metadata.name":$namespace}}')" == "true" ]]
done
[[ "$(yq eval-all '[select(.kind == "Role" and .metadata.labels."app.kubernetes.io/component" == "runtime-registry-pulls") | .rules[].verbs[] | select(. == "delete" or . == "list" or . == "watch" or . == "update" or . == "patch")] | length' "${kp_tmp}/runtime-registry-pulls.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "ClusterRole" or .kind == "ClusterRoleBinding") | select(.metadata.name | test("runtime-registry-pulls|registry-pulls"))] | length' "${kp_tmp}/runtime-registry-pulls.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "ValidatingAdmissionPolicy" and .metadata.labels."app.kubernetes.io/component" == "runtime-registry-pulls-admission") | .spec.matchConstraints.resourceRules[] | select((.apiGroups | join(",")) == "" and (.apiVersions | join(",")) == "v1" and (.operations | join(",")) == "CREATE,UPDATE,DELETE" and (.resources | join(",")) == "secrets" and .scope == "Namespaced")] | length' "${kp_tmp}/runtime-registry-pulls.yaml" | tail -1)" == "2" ]] || { printf 'runtime registry pull admission policies have a broad or incomplete resource match\n' >&2; exit 1; }

yq '.config.runtimeRegistryPulls.readinessSeconds = 91' "${kp_tmp}/runtime-registry-pulls-values.yaml" > "${kp_tmp}/runtime-registry-pulls-changed-values.yaml"
helm template runtime-pulls "${kp_root}/charts/kuberploy" --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/runtime-registry-pulls-changed-values.yaml" > "${kp_tmp}/runtime-registry-pulls-changed.yaml"
kp_runtime_pull_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/runtime-registry-pulls.yaml")"
kp_runtime_pull_changed_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/runtime-registry-pulls-changed.yaml")"
[[ "${kp_runtime_pull_config_name}" != "${kp_runtime_pull_changed_config_name}" ]] || { printf 'runtime registry pull mutation did not produce a new immutable ConfigMap name\n' >&2; exit 1; }

for kp_runtime_pull_mutation in \
  '.config.runtimeRegistryPulls.enabled = false' \
  '.config.runtimeRegistryPulls.namespaces = []' \
  '.config.runtimeRegistryPulls.namespaces = ["apps-production", "apps-production"]' \
  '.config.runtimeRegistryPulls.namespaces = ["apps-staging", "apps-production"]' \
  '.config.runtimeRegistryPulls.profiles = []' \
  '.config.runtimeRegistryPulls.profiles = [.config.runtimeRegistryPulls.profiles[1], .config.runtimeRegistryPulls.profiles[0]]' \
  '.config.runtimeRegistryPulls.profiles[1] = .config.runtimeRegistryPulls.profiles[0]' \
  '.config.runtimeRegistryPulls.profiles[1].name = .config.runtimeRegistryPulls.profiles[0].name' \
  '.config.gitProjection.enabled = false' \
  '.networkPolicy.enabled = false' \
  '.networkPolicy.kubeAPIServerCIDRs = []' \
  '.components.api.image.reference = "kuberploy-api:mutable"' \
  '.components.worker.image.reference = "kuberploy-worker:mutable"' \
  '.config.runtimeRegistryPulls.unknownField = true'; do
  yq "${kp_runtime_pull_mutation}" "${kp_tmp}/runtime-registry-pulls-values.yaml" > "${kp_tmp}/runtime-registry-pulls-invalid.yaml"
  if helm template invalid-runtime-pulls "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render -f "${kp_tmp}/runtime-registry-pulls-invalid.yaml" >/dev/null 2>&1; then
    printf 'platform chart accepted invalid runtime registry pull settings: %s\n' "${kp_runtime_pull_mutation}" >&2
    exit 1
  fi
done

# Template checks remain closed if an operator explicitly bypasses values schema validation.
for kp_runtime_pull_mutation in \
  '.config.runtimeRegistryPulls.enabled = false' \
  '.config.runtimeRegistryPulls.enabled = false | .config.runtimeRegistryPulls.namespaces = [] | .config.runtimeRegistryPulls.profiles = [] | .config.runtimeRegistryPulls.readinessSeconds = 91' \
  '.config.runtimeRegistryPulls.namespaces = ["apps-staging", "apps-production"]' \
  '.config.runtimeRegistryPulls.namespaces = ["apps-production", "apps-production"]' \
  '.config.runtimeRegistryPulls.profiles = [.config.runtimeRegistryPulls.profiles[1], .config.runtimeRegistryPulls.profiles[0]]' \
  '.config.runtimeRegistryPulls.profiles[1].name = .config.runtimeRegistryPulls.profiles[0].name'; do
  yq "${kp_runtime_pull_mutation}" "${kp_tmp}/runtime-registry-pulls-values.yaml" > "${kp_tmp}/runtime-registry-pulls-invalid.yaml"
  if helm template invalid-runtime-pulls "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render -f "${kp_tmp}/runtime-registry-pulls-invalid.yaml" \
    --skip-schema-validation >/dev/null 2>&1; then
    printf 'runtime registry pull template accepted invalid settings with schema validation bypassed: %s\n' "${kp_runtime_pull_mutation}" >&2
    exit 1
  fi
done

yq '.config.imageTagResolution.anonymousTargetIds = ["33333333-3333-7333-8333-333333333333"] |
    .config.imageTagResolution.tokenAuthorities = [{"targetId":"33333333-3333-7333-8333-333333333333","realmUrl":"https://auth.registry.example.test:8443/token","service":"registry.example.test"}] |
    .config.imageTagResolution.platform = "linux/arm64/v8" |
    .builder.controllerServiceAccount.name = "image-tag-resolution-worker"' \
  "${kp_tmp}/runtime-registry-pulls-values.yaml" > "${kp_tmp}/image-tag-resolution-values.yaml"
helm template image-tag-resolution "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/image-tag-resolution-values.yaml" > "${kp_tmp}/image-tag-resolution.yaml"
yq eval-all 'true' "${kp_tmp}/image-tag-resolution.yaml" >/dev/null
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_IMAGE_TAG_RESOLUTION_PLATFORM' "${kp_tmp}/image-tag-resolution.yaml")" == "linux/arm64/v8" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_IMAGE_TAG_RESOLUTION_ANONYMOUS_TARGET_IDS' "${kp_tmp}/image-tag-resolution.yaml")" == "33333333-3333-7333-8333-333333333333" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_IMAGE_TAG_RESOLUTION_TOKEN_AUTHORITIES' "${kp_tmp}/image-tag-resolution.yaml")" == '[{"realmUrl":"https://auth.registry.example.test:8443/token","service":"registry.example.test","targetId":"33333333-3333-7333-8333-333333333333"}]' ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_IMAGE_TAG_RESOLUTION"))] | length' "${kp_tmp}/image-tag-resolution.yaml" | tail -1)" == "3" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" != "api") | .spec.template.spec.containers[0].env[]? | select(.name | test("^KUBERPLOY_IMAGE_TAG_RESOLUTION"))] | length' "${kp_tmp}/image-tag-resolution.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == "api") | [.spec.egress[].ports[]?.port] | unique | sort | join(",")' "${kp_tmp}/image-tag-resolution.yaml")" == *"443"* ]]
[[ "$(yq eval-all 'select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == "api") | [.spec.egress[].ports[]?.port] | unique | sort | join(",")' "${kp_tmp}/image-tag-resolution.yaml")" == *"5443"* ]]
[[ "$(yq eval-all 'select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == "api") | [.spec.egress[].ports[]?.port] | unique | sort | join(",")' "${kp_tmp}/image-tag-resolution.yaml")" == *"8443"* ]]

# Image tag resolution remains fail-closed when an operator bypasses the values schema.
for kp_image_resolution_mutation in \
  '.networkPolicy.enabled = false' \
  '.networkPolicy.externalEgressCIDRs = []' \
  '.config.imageTagResolution.anonymousTargetIds += [.config.imageTagResolution.anonymousTargetIds[0]]' \
  '.config.imageTagResolution.tokenAuthorities[0].targetId = "44444444-4444-7444-8444-444444444444"' \
  '.config.imageTagResolution.tokenAuthorities[0].realmUrl = "http://auth.registry.example.test/token"' \
  '.config.imageTagResolution.tokenAuthorities[0].realmUrl = "https://auth.registry.example.test/token?scope=attacker"' \
  '.config.imageTagResolution.platform = "linux/386"' \
  '.config.imageTagResolution.callerRegistry = "attacker.invalid"' \
  '.config.imageTagResolution.tokenAuthorities[0].credentialRef = "caller-secret"'; do
  yq "${kp_image_resolution_mutation}" "${kp_tmp}/image-tag-resolution-values.yaml" > "${kp_tmp}/image-tag-resolution-invalid.yaml"
  if helm template invalid-image-tag-resolution "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render -f "${kp_tmp}/image-tag-resolution-invalid.yaml" \
    --skip-schema-validation >/dev/null 2>&1; then
    printf 'image tag resolution template accepted invalid settings with schema validation bypassed: %s\n' "${kp_image_resolution_mutation}" >&2
    exit 1
  fi
done

yq '.' "${kp_root}/test/e2e/fixtures/edge-runtime-values.yaml" > "${kp_tmp}/edge-runtime-values.yaml"
helm template edge-runtime "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_root}/test/e2e/fixtures/platform-values.yaml" \
  -f "${kp_tmp}/edge-runtime-values.yaml" > "${kp_tmp}/edge-runtime.yaml"
helm template edge-runtime "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_root}/test/e2e/fixtures/platform-values.yaml" \
  -f "${kp_tmp}/edge-runtime-values.yaml" > "${kp_tmp}/edge-runtime-again.yaml"
diff -u "${kp_tmp}/edge-runtime.yaml" "${kp_tmp}/edge-runtime-again.yaml"
yq eval-all 'true' "${kp_tmp}/edge-runtime.yaml" >/dev/null

# A production-only cert-manager profile is valid. Empty solver arrays must
# match only the disabled branch of the closed schema.
yq '.config.edgeRuntime.profiles.certManager.stagingIssuer = "" |
  .config.edgeRuntime.profiles.certManager.stagingServerClass = "" |
  .config.edgeRuntime.profiles.certManager.stagingSolverTypes = [] |
  .config.edgeRuntime.profiles.certManager.stagingDNS01Profiles = []' \
  "${kp_tmp}/edge-runtime-values.yaml" > "${kp_tmp}/edge-runtime-production-issuer-only.yaml"
helm template edge-runtime-production-issuer-only "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_root}/test/e2e/fixtures/platform-values.yaml" \
  -f "${kp_tmp}/edge-runtime-production-issuer-only.yaml" >/dev/null

kp_edge_profiles="$(yq -o=json -I=0 '.config.edgeRuntime.profiles | sort_keys(..)' "${kp_tmp}/edge-runtime-values.yaml")"
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_EDGE_RUNTIME_ENABLED' "${kp_tmp}/edge-runtime.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_EDGE_RUNTIME_PROFILES_JSON' "${kp_tmp}/edge-runtime.yaml")" == "${kp_edge_profiles}" ]]
[[ "$(yq -o=json -I=0 '.config.edgeRuntime.profiles.traefik.sslip' "${kp_tmp}/edge-runtime-values.yaml")" == '{"mode":"auto-first-ip"}' ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | [.data.KUBERPLOY_EDGE_RUNTIME_POLL_SECONDS,.data.KUBERPLOY_EDGE_RUNTIME_WORK_LEASE_SECONDS,.data.KUBERPLOY_EDGE_RUNTIME_HEARTBEAT_SECONDS,.data.KUBERPLOY_EDGE_RUNTIME_READINESS_SECONDS,.data.KUBERPLOY_EDGE_RUNTIME_MINIMUM_BACKOFF_SECONDS,.data.KUBERPLOY_EDGE_RUNTIME_MAXIMUM_BACKOFF_SECONDS] | join(",")' "${kp_tmp}/edge-runtime.yaml")" == "30,120,20,90,5,300" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .immutable' "${kp_tmp}/edge-runtime.yaml")" == "true" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_EDGE_RUNTIME"))] | length' "${kp_tmp}/edge-runtime.yaml" | tail -1)" == "16" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "web") | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_EDGE_RUNTIME"))] | length' "${kp_tmp}/edge-runtime.yaml" | tail -1)" == "0" ]]
[[ "$(jq -r '.externalDNS[0].credentialSecretRef' <<<"${kp_edge_profiles}")" == "external-dns-cloudflare-primary" ]]
kp_edge_profiles_without_credential_ref="$(jq -c 'del(.externalDNS[].credentialSecretRef)' <<<"${kp_edge_profiles}")"
if rg -qi '(secret|credential|password|token)' <<<"${kp_edge_profiles_without_credential_ref}"; then
  printf 'edge runtime profile JSON contains credential material or an unapproved credential field\n' >&2
  exit 1
fi

[[ "$(yq eval-all '[select(.kind == "Role" and .metadata.labels."app.kubernetes.io/component" == "edge-runtime")] | length' "${kp_tmp}/edge-runtime.yaml" | tail -1)" == "3" ]]
[[ "$(yq eval-all '[select(.kind == "RoleBinding" and .metadata.labels."app.kubernetes.io/component" == "edge-runtime")] | length' "${kp_tmp}/edge-runtime.yaml" | tail -1)" == "3" ]]
[[ "$(yq eval-all '[select(.kind == "ClusterRole" and .metadata.labels."app.kubernetes.io/component" == "edge-runtime")] | length' "${kp_tmp}/edge-runtime.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "ClusterRoleBinding" and .metadata.labels."app.kubernetes.io/component" == "edge-runtime")] | length' "${kp_tmp}/edge-runtime.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Role" and .metadata.labels."kuberploy.io/edge-target" == "traefik") | .metadata.namespace] | join(",")' "${kp_tmp}/edge-runtime.yaml" | tail -1)" == "kuberploy-system" ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "Role" and .metadata.labels."kuberploy.io/edge-target" == "traefik") | .rules' "${kp_tmp}/edge-runtime.yaml")" == '[{"apiGroups":["apps"],"resources":["deployments"],"resourceNames":["traefik"],"verbs":["get"]},{"apiGroups":[""],"resources":["services"],"resourceNames":["traefik"],"verbs":["get"]},{"apiGroups":[""],"resources":["configmaps"],"resourceNames":["edge-foundation-edge-profile"],"verbs":["get"]}]' ]]
[[ "$(yq eval-all -o=json 'select(.kind == "Role" and .metadata.labels."kuberploy.io/edge-target" == "cert-manager")' "${kp_tmp}/edge-runtime.yaml" | jq '.metadata.namespace == "cert-manager" and .rules[0].apiGroups == ["apps"] and .rules[0].resources == ["deployments"] and (.rules[0].resourceNames | join(",")) == "cert-manager,cert-manager-cainjector,cert-manager-webhook" and .rules[0].verbs == ["get"] and .rules[1] == {"apiGroups":[""],"resources":["configmaps"],"resourceNames":["cert-foundation-certificate-profile"],"verbs":["get"]}')" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Role" and .metadata.labels."kuberploy.io/edge-target" == "external-dns") | .metadata.namespace' "${kp_tmp}/edge-runtime.yaml")" == "external-dns" ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "Role" and .metadata.labels."kuberploy.io/edge-target" == "external-dns") | .rules' "${kp_tmp}/edge-runtime.yaml")" == '[{"apiGroups":["apps"],"resources":["deployments"],"resourceNames":["external-dns"],"verbs":["get"]},{"apiGroups":[""],"resources":["configmaps"],"resourceNames":["dns-primary-dns-profile"],"verbs":["get"]}]' ]]
[[ "$(yq eval-all -o=json 'select(.kind == "ClusterRole" and .metadata.labels."app.kubernetes.io/component" == "edge-runtime")' "${kp_tmp}/edge-runtime.yaml" | jq '(.rules | length) == 4 and .rules[0].apiGroups == ["networking.k8s.io"] and .rules[0].resources == ["ingressclasses"] and .rules[0].resourceNames == ["traefik"] and (.rules[1].resourceNames | length) == 10 and (.rules[2].resourceNames | length) == 6 and .rules[3].apiGroups == ["cert-manager.io"] and .rules[3].resources == ["clusterissuers"] and (.rules[3].resourceNames | join(",")) == "kuberploy-letsencrypt-production,kuberploy-letsencrypt-staging"')" == "true" ]]
[[ "$(yq eval-all '[select((.kind == "Role" or .kind == "ClusterRole") and .metadata.labels."app.kubernetes.io/component" == "edge-runtime") | .rules[].verbs[] | select(. != "get")] | length' "${kp_tmp}/edge-runtime.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select((.kind == "Role" or .kind == "ClusterRole") and .metadata.labels."app.kubernetes.io/component" == "edge-runtime") | .rules[].resources[] | select(. == "secrets" or . == "pods" or . == "pods/log")] | length' "${kp_tmp}/edge-runtime.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select((.kind == "RoleBinding" or .kind == "ClusterRoleBinding") and .metadata.labels."app.kubernetes.io/component" == "edge-runtime") | .subjects[] | select(.kind != "ServiceAccount" or .name != "edge-runtime-worker" or .namespace != "kuberploy-e2e-render")] | length' "${kp_tmp}/edge-runtime.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "ValidatingAdmissionPolicy" or .kind == "ValidatingAdmissionPolicyBinding") | select(.metadata.labels."app.kubernetes.io/component" == "edge-runtime")] | length' "${kp_tmp}/edge-runtime.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.egress[] | select(.to[0].ipBlock.cidr == "10.43.0.1/32" and (.ports | length) == 2 and .ports[0].port == 443 and .ports[1].port == 6443)] | length' "${kp_tmp}/edge-runtime.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.egress[] | select(.to[0].ipBlock.cidr == "10.43.0.1/32")] | length' "${kp_tmp}/edge-runtime.yaml" | tail -1)" == "0" ]]

yq '.config.edgeRuntime.readinessSeconds = 91' "${kp_tmp}/edge-runtime-values.yaml" > "${kp_tmp}/edge-runtime-changed-values.yaml"
helm template edge-runtime-changed "${kp_root}/charts/kuberploy" --namespace kuberploy-e2e-render \
  -f "${kp_root}/test/e2e/fixtures/platform-values.yaml" -f "${kp_tmp}/edge-runtime-changed-values.yaml" > "${kp_tmp}/edge-runtime-changed.yaml"
kp_edge_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/edge-runtime.yaml")"
kp_edge_changed_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/edge-runtime-changed.yaml")"
[[ "${kp_edge_config_name}" != "${kp_edge_changed_config_name}" ]] || { printf 'edge runtime mutation did not produce a new immutable ConfigMap name\n' >&2; exit 1; }

yq 'del(.config.edgeRuntime.profiles.traefik.sslip)' "${kp_tmp}/edge-runtime-values.yaml" > "${kp_tmp}/edge-runtime-no-sslip-values.yaml"
helm template edge-runtime-no-sslip "${kp_root}/charts/kuberploy" --namespace kuberploy-e2e-render \
  -f "${kp_root}/test/e2e/fixtures/platform-values.yaml" -f "${kp_tmp}/edge-runtime-no-sslip-values.yaml" > "${kp_tmp}/edge-runtime-no-sslip.yaml"
kp_edge_no_sslip_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/edge-runtime-no-sslip.yaml")"
[[ "${kp_edge_config_name}" != "${kp_edge_no_sslip_config_name}" ]] || { printf 'sslip policy did not participate in the immutable edge runtime identity\n' >&2; exit 1; }

for kp_edge_mutation in \
  '.config.edgeRuntime.enabled = false' \
  '.config.edgeRuntime.profiles.traefik = null | .config.edgeRuntime.profiles.certManager = null | .config.edgeRuntime.profiles.externalDNS = []' \
  '.config.edgeRuntime.workLeaseSeconds = 120 | .config.edgeRuntime.heartbeatSeconds = 60' \
  '.config.edgeRuntime.readinessSeconds = 59' \
  '.config.edgeRuntime.minimumBackoffSeconds = 10 | .config.edgeRuntime.maximumBackoffSeconds = 9' \
  '.config.edgeRuntime.profiles.traefik.mode = "owner"' \
  '.config.edgeRuntime.profiles.traefik.deployment.image = "docker.io/library/traefik:latest"' \
  '.config.edgeRuntime.profiles.traefik.crds |= reverse' \
  '.config.edgeRuntime.profiles.traefik.sslip = null' \
  '.config.edgeRuntime.profiles.traefik.sslip.mode = "caller-ip"' \
  '.config.edgeRuntime.profiles.traefik.sslip.staticPublicIPv4 = "8.8.8.8"' \
  '.config.edgeRuntime.profiles.traefik.sslip.mode = "verified-static-ip"' \
  '.config.edgeRuntime.profiles.traefik.sslip.mode = "verified-static-ip" | .config.edgeRuntime.profiles.traefik.sslip.staticPublicIPv4 = "10.0.0.1"' \
  '.config.edgeRuntime.profiles.traefik.sslip.mode = "verified-static-ip" | .config.edgeRuntime.profiles.traefik.sslip.staticPublicIPv4 = "008.008.008.008"' \
  '.config.edgeRuntime.profiles.traefik.sslip.callerAddress = "8.8.8.8"' \
  '.config.edgeRuntime.profiles.certManager.deployments = [.config.edgeRuntime.profiles.certManager.deployments[1], .config.edgeRuntime.profiles.certManager.deployments[0], .config.edgeRuntime.profiles.certManager.deployments[2]]' \
  '.config.edgeRuntime.profiles.certManager.productionIssuer = .config.edgeRuntime.profiles.certManager.stagingIssuer' \
  '.config.edgeRuntime.profiles.externalDNS += [.config.edgeRuntime.profiles.externalDNS[0]]' \
  '.config.edgeRuntime.profiles.externalDNS[0].domainFilters |= reverse' \
  '.config.edgeRuntime.profiles.externalDNS[0].providerKind = "unknown"' \
  '.config.edgeRuntime.profiles.externalDNS[0].credentialSecretRef = ""' \
  '.config.edgeRuntime.profiles.externalDNS[0].providerConfigRef = ""' \
  '.config.edgeRuntime.profiles.externalDNS[0].egressConfigRef = ""' \
  '.config.edgeRuntime.profiles.externalDNS[0].deployment.image = "registry.k8s.io/external-dns/external-dns:latest"' \
  '.networkPolicy.enabled = false' \
  '.networkPolicy.kubeAPIServerCIDRs = []' \
  '.components.api.image.reference = "kuberploy-api:mutable"' \
  '.components.worker.image.reference = "kuberploy-worker:mutable"' \
  '.config.edgeRuntime.unknownField = true'; do
  yq "${kp_edge_mutation}" "${kp_tmp}/edge-runtime-values.yaml" > "${kp_tmp}/edge-runtime-invalid-values.yaml"
  if helm template invalid-edge-runtime "${kp_root}/charts/kuberploy" --namespace kuberploy-e2e-render \
    -f "${kp_root}/test/e2e/fixtures/platform-values.yaml" -f "${kp_tmp}/edge-runtime-invalid-values.yaml" >/dev/null 2>&1; then
    printf 'platform chart accepted invalid edge runtime settings: %s\n' "${kp_edge_mutation}" >&2
    exit 1
  fi
done

# Critical runtime fences remain render-time closed if schema validation is bypassed.
for kp_edge_mutation in \
  '.config.edgeRuntime.enabled = false' \
  '.config.edgeRuntime.enabled = false | .config.edgeRuntime.profiles.traefik = null | .config.edgeRuntime.profiles.certManager = null | .config.edgeRuntime.profiles.externalDNS = [] | .config.edgeRuntime.readinessSeconds = 91' \
  '.config.edgeRuntime.profiles.traefik = null | .config.edgeRuntime.profiles.certManager = null | .config.edgeRuntime.profiles.externalDNS = []' \
  '.config.edgeRuntime.workLeaseSeconds = 120 | .config.edgeRuntime.heartbeatSeconds = 60' \
  '.config.edgeRuntime.readinessSeconds = 59' \
  '.config.edgeRuntime.profiles.traefik.deployment.image = "docker.io/library/traefik:latest"' \
  '.config.edgeRuntime.profiles.traefik.crds |= reverse' \
  '.config.edgeRuntime.profiles.traefik.sslip = null' \
  '.config.edgeRuntime.profiles.traefik.sslip.staticPublicIPv4 = "8.8.8.8"' \
  '.config.edgeRuntime.profiles.traefik.sslip.mode = "verified-static-ip"' \
  '.config.edgeRuntime.profiles.traefik.sslip.mode = "verified-static-ip" | .config.edgeRuntime.profiles.traefik.sslip.staticPublicIPv4 = "192.0.2.10"' \
  '.config.edgeRuntime.profiles.traefik.sslip.callerAddress = "8.8.8.8"' \
  '.config.edgeRuntime.profiles.certManager.deployments = [.config.edgeRuntime.profiles.certManager.deployments[1], .config.edgeRuntime.profiles.certManager.deployments[0], .config.edgeRuntime.profiles.certManager.deployments[2]]' \
  '.config.edgeRuntime.profiles.externalDNS += [.config.edgeRuntime.profiles.externalDNS[0]]' \
  '.config.edgeRuntime.profiles.externalDNS[0].domainFilters |= reverse' \
  '.config.edgeRuntime.profiles.externalDNS[0].providerKind = "unknown"' \
  '.config.edgeRuntime.profiles.externalDNS[0].credentialSecretRef = ""' \
  '.config.edgeRuntime.profiles.externalDNS[0].providerConfigRef = ""' \
  '.config.edgeRuntime.profiles.externalDNS[0].egressConfigRef = ""' \
  '.networkPolicy.enabled = false' \
  '.components.worker.image.reference = "kuberploy-worker:mutable"'; do
  yq "${kp_edge_mutation}" "${kp_tmp}/edge-runtime-values.yaml" > "${kp_tmp}/edge-runtime-invalid-values.yaml"
  if helm template invalid-edge-runtime "${kp_root}/charts/kuberploy" --namespace kuberploy-e2e-render \
    -f "${kp_root}/test/e2e/fixtures/platform-values.yaml" -f "${kp_tmp}/edge-runtime-invalid-values.yaml" \
    --skip-schema-validation >/dev/null 2>&1; then
    printf 'edge runtime template accepted invalid settings with schema validation bypassed: %s\n' "${kp_edge_mutation}" >&2
    exit 1
  fi
done

yq eval-all 'select(fileIndex == 0) * select(fileIndex == 1) * select(fileIndex == 2)' \
  "${kp_tmp}/argo-desired-state-values.yaml" \
  "${kp_root}/test/e2e/fixtures/edge-runtime-values.yaml" \
  "${kp_root}/test/e2e/fixtures/certificate-issuer-observer-values.yaml" > "${kp_tmp}/certificate-issuer-observer-values.yaml"
helm template argo-desired "${kp_root}/charts/kuberploy" --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/certificate-issuer-observer-values.yaml" > "${kp_tmp}/certificate-issuer-observer.yaml"
yq eval-all 'true' "${kp_tmp}/certificate-issuer-observer.yaml" >/dev/null
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | [.data.KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_ENABLED,.data.KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_BINDING_ID,.data.KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_CLUSTER_ID,.data.KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_NAMESPACE,.data.KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_SERVICE_ACCOUNT,.data.KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_POLL_SECONDS,.data.KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_REQUEST_TIMEOUT_SECONDS,.data.KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_MAXIMUM_AGE_SECONDS,.data.KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_READINESS_LEASE_SECONDS] | join(",")' "${kp_tmp}/certificate-issuer-observer.yaml")" == "true,11111111-1111-4111-8111-111111111111,22222222-2222-4222-8222-222222222222,kuberploy-e2e-render,argo-desired-worker,30,10,120,180" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | [.data | keys[] | select(test("^KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER"))] | length' "${kp_tmp}/certificate-issuer-observer.yaml")" == "9" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER"))] | length' "${kp_tmp}/certificate-issuer-observer.yaml" | tail -1)" == "18" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "web") | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER"))] | length' "${kp_tmp}/certificate-issuer-observer.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select((.name == "KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_NAMESPACE" or .name == "KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_SERVICE_ACCOUNT") and .valueFrom.configMapKeyRef.key == .name)] | length' "${kp_tmp}/certificate-issuer-observer.yaml" | tail -1)" == "4" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.egress[] | select(.to[0].ipBlock.cidr == "10.43.0.1/32" and (.ports | length) == 2 and .ports[0].port == 443 and .ports[1].port == 6443)] | length' "${kp_tmp}/certificate-issuer-observer.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.egress[] | select(.to[0].ipBlock.cidr == "10.43.0.1/32" and (.ports | length) == 2 and .ports[0].port == 443 and .ports[1].port == 6443)] | length' "${kp_tmp}/certificate-issuer-observer.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Role" or .kind == "ClusterRole") | .rules[]? | select(.resources[]? == "clusterissuers") | .verbs[] | select(. != "get")] | length' "${kp_tmp}/certificate-issuer-observer.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Role" or .kind == "RoleBinding" or .kind == "ClusterRole" or .kind == "ClusterRoleBinding") | select(.metadata.labels."app.kubernetes.io/component" == "certificate-issuer-observer")] | length' "${kp_tmp}/certificate-issuer-observer.yaml" | tail -1)" == "0" ]]

yq '.config.certificateIssuerObserver.pollIntervalSeconds = 31' "${kp_tmp}/certificate-issuer-observer-values.yaml" > "${kp_tmp}/certificate-issuer-observer-changed-values.yaml"
helm template argo-desired "${kp_root}/charts/kuberploy" --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/certificate-issuer-observer-changed-values.yaml" > "${kp_tmp}/certificate-issuer-observer-changed.yaml"
kp_issuer_observer_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/certificate-issuer-observer.yaml")"
kp_issuer_observer_changed_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/certificate-issuer-observer-changed.yaml")"
[[ "${kp_issuer_observer_config_name}" != "${kp_issuer_observer_changed_config_name}" ]] || { printf 'certificate issuer observer mutation did not produce a new immutable ConfigMap name\n' >&2; exit 1; }

for kp_issuer_observer_mutation in \
  '.config.certificateIssuerObserver.enabled = false' \
  '.config.certificateIssuerObserver.platformBindingID = "33333333-3333-4333-8333-333333333333"' \
  '.config.certificateIssuerObserver.clusterID = "33333333-3333-4333-8333-333333333333"' \
  '.config.certificateIssuerObserver.pollIntervalSeconds = 10 | .config.certificateIssuerObserver.requestTimeoutSeconds = 10' \
  '.config.certificateIssuerObserver.maximumAgeSeconds = 59' \
  '.config.certificateIssuerObserver.readinessLeaseSeconds = 119' \
  '.config.certificateIssuerObserver.namespace = "attacker"' \
  '.config.certificateIssuerObserver.serviceAccount = "attacker"' \
  '.config.gitProjection.enabled = false' \
  '.config.argoDesiredState.enabled = false' \
  '.config.environmentFoundation.enabled = false' \
  '.config.edgeRuntime.profiles.certManager = null' \
  '.networkPolicy.enabled = false' \
  '.networkPolicy.kubeAPIServerCIDRs = []'; do
  yq "${kp_issuer_observer_mutation}" "${kp_tmp}/certificate-issuer-observer-values.yaml" > "${kp_tmp}/certificate-issuer-observer-invalid.yaml"
  if helm template invalid-certificate-issuer-observer "${kp_root}/charts/kuberploy" --namespace kuberploy-e2e-render \
    -f "${kp_tmp}/certificate-issuer-observer-invalid.yaml" >/dev/null 2>&1; then
    printf 'platform chart accepted invalid certificate issuer observer settings: %s\n' "${kp_issuer_observer_mutation}" >&2
    exit 1
  fi
  if helm template invalid-certificate-issuer-observer "${kp_root}/charts/kuberploy" --namespace kuberploy-e2e-render \
    -f "${kp_tmp}/certificate-issuer-observer-invalid.yaml" --skip-schema-validation >/dev/null 2>&1; then
    printf 'certificate issuer observer template accepted invalid settings with schema validation bypassed: %s\n' "${kp_issuer_observer_mutation}" >&2
    exit 1
  fi
done

yq '.config.runtimeSecrets.enabled = true |
    .config.runtimeSecrets.namespaces = ["apps-production", "apps-staging"] |
    .config.runtimeSecrets.fingerprintSecret.name = "kuberploy-runtime-secret-fingerprint" |
    .config.runtimeSecrets.sealingCertificateSecret.name = "sealed-secrets-key" |
    .components.api.image.reference = "ghcr.io/kuberploy/kuberploy-api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" |
    .components.worker.image.reference = "ghcr.io/kuberploy/kuberploy-worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" |
    .rbac.observedNamespaces = []' \
  "${kp_tmp}/git-projection-values.yaml" > "${kp_tmp}/runtime-secrets-values.yaml"
helm template github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/runtime-secrets-values.yaml" > "${kp_tmp}/runtime-secrets.yaml"
yq eval-all 'true' "${kp_tmp}/runtime-secrets.yaml" >/dev/null
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_RUNTIME_SECRETS_ENABLED' "${kp_tmp}/runtime-secrets.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_RUNTIME_SECRET_NAMESPACES' "${kp_tmp}/runtime-secrets.yaml")" == "apps-production,apps-staging" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_RUNTIME_SECRET_FINGERPRINT_SECRET_KEY' "${kp_tmp}/runtime-secrets.yaml")" == "runtime-secret-fingerprint.key" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_RUNTIME_SECRET_SEALING_CERTIFICATE_SECRET_KEY' "${kp_tmp}/runtime-secrets.yaml")" == "tls.crt" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_RUNTIME_SECRET"))] | length' "${kp_tmp}/runtime-secrets.yaml" | tail -1)" == "26" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "web") | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_RUNTIME_SECRET"))] | length' "${kp_tmp}/runtime-secrets.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.template.spec.containers[0].volumeMounts[] | select(.name == "runtime-secret-fingerprint" and .mountPath == "/var/run/secrets/kuberploy-system" and .readOnly == true)] | length' "${kp_tmp}/runtime-secrets.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.containers[0].volumeMounts[]? | select(.name == "runtime-secret-fingerprint")] | length' "${kp_tmp}/runtime-secrets.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].volumeMounts[] | select(.name == "runtime-secret-sealing-certificate" and .mountPath == "/var/run/secrets/kuberploy-system/sealed-secrets" and .readOnly == true)] | length' "${kp_tmp}/runtime-secrets.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "api") | .spec.template.spec.volumes[] | select(.name == "runtime-secret-fingerprint" and .secret.secretName == "kuberploy-runtime-secret-fingerprint" and .secret.defaultMode == 288 and (.secret.items | length) == 1 and .secret.items[0].key == "runtime-secret-fingerprint.key" and .secret.items[0].path == "runtime-secret-fingerprint.key")] | length' "${kp_tmp}/runtime-secrets.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.volumes[] | select(.name == "runtime-secret-sealing-certificate" and .secret.secretName == "sealed-secrets-key" and .secret.defaultMode == 288 and (.secret.items | length) == 1 and .secret.items[0].key == "tls.crt" and .secret.items[0].path == "tls.crt")] | length' "${kp_tmp}/runtime-secrets.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "Role" and (.metadata.name | test("-runtime-secrets-(api|worker)$")))] | length' "${kp_tmp}/runtime-secrets.yaml" | tail -1)" == "4" ]]
[[ "$(yq eval-all '[select(.kind == "RoleBinding" and (.metadata.name | test("-runtime-secrets-(api|worker)$")))] | length' "${kp_tmp}/runtime-secrets.yaml" | tail -1)" == "4" ]]
for kp_runtime_namespace in apps-production apps-staging; do
  [[ "$(kp_runtime_namespace="${kp_runtime_namespace}" yq eval-all -o=json -I=0 'select(.kind == "Role" and .metadata.namespace == strenv(kp_runtime_namespace) and (.metadata.name | test("-runtime-secrets-api$"))) | .rules' "${kp_tmp}/runtime-secrets.yaml")" == '[{"apiGroups":["bitnami.com"],"resources":["sealedsecrets"],"verbs":["get","create","delete"]}]' ]]
  [[ "$(kp_runtime_namespace="${kp_runtime_namespace}" yq eval-all -o=json -I=0 'select(.kind == "Role" and .metadata.namespace == strenv(kp_runtime_namespace) and (.metadata.name | test("-runtime-secrets-worker$"))) | .rules' "${kp_tmp}/runtime-secrets.yaml")" == '[{"apiGroups":["bitnami.com"],"resources":["sealedsecrets"],"verbs":["get"]}]' ]]
done
[[ "$(yq eval-all '[select(.kind == "Role" and (.metadata.name | test("-runtime-secrets-(api|worker)$"))) | .rules[] | select(.resources[] == "secrets" or (.verbs[] | test("list|watch|update|patch")))] | length' "${kp_tmp}/runtime-secrets.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "ClusterRole" or .kind == "ClusterRoleBinding") | select(.metadata.name | test("runtime-secrets"))] | length' "${kp_tmp}/runtime-secrets.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "ValidatingAdmissionPolicy" and .metadata.labels."app.kubernetes.io/component" == "runtime-secrets-admission")] | length' "${kp_tmp}/runtime-secrets.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.labels."app.kubernetes.io/component" == "runtime-secrets-admission") | .spec.failurePolicy' "${kp_tmp}/runtime-secrets.yaml")" == "Fail" ]]
[[ "$(yq eval-all '[select(.kind == "ValidatingAdmissionPolicy" and .metadata.labels."app.kubernetes.io/component" == "runtime-secrets-admission") | .spec.matchConstraints.resourceRules[] | select((.apiGroups | join(",")) == "bitnami.com" and (.apiVersions | join(",")) == "v1alpha1" and (.operations | join(",")) == "CREATE,DELETE" and (.resources | join(",")) == "sealedsecrets" and .scope == "Namespaced")] | length' "${kp_tmp}/runtime-secrets.yaml" | tail -1)" == "1" ]] || { printf 'runtime-secret admission policy has a broad or incomplete resource match\n' >&2; exit 1; }
[[ "$(yq eval-all 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.labels."app.kubernetes.io/component" == "runtime-secrets-admission") | ([.spec.validations[].expression] | join("\n")) as $expressions | (($expressions | contains("system:serviceaccount:kuberploy-e2e-render:github-builds-api")) and ($expressions | contains("kuberploy.io/secret-binding")) and ($expressions | contains("kuberploy.io/secret-version")) and ($expressions | contains("kuberploy.io/target-secret")) and ($expressions | contains("oldObject")) and ($expressions | contains("spec.template.immutable")))' "${kp_tmp}/runtime-secrets.yaml")" == "true" ]] || { printf 'runtime-secret admission policy omitted an exact identity fence\n' >&2; exit 1; }
[[ "$(yq eval-all '[select(.kind == "ValidatingAdmissionPolicyBinding" and .metadata.labels."app.kubernetes.io/component" == "runtime-secrets-admission")] | length' "${kp_tmp}/runtime-secrets.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "ValidatingAdmissionPolicyBinding" and .metadata.labels."app.kubernetes.io/component" == "runtime-secrets-admission" and (.spec.validationActions | join(",")) == "Deny") | .spec.matchResources.namespaceSelector.matchExpressions[] | select(.key == "kubernetes.io/metadata.name" and .operator == "In" and (.values | join(",")) == "apps-production,apps-staging")] | length' "${kp_tmp}/runtime-secrets.yaml" | tail -1)" == "1" ]] || { printf 'runtime-secret admission binding is not deny-only on the exact namespace allowlist\n' >&2; exit 1; }
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.egress[] | select(.to[0].ipBlock.cidr == "10.43.0.1/32" and (.ports | length) == 2 and .ports[0].port == 443 and .ports[1].port == 6443)] | length' "${kp_tmp}/runtime-secrets.yaml" | tail -1)" == "2" ]]

yq '.config.runtimeSecrets.pollIntervalSeconds = 6' "${kp_tmp}/runtime-secrets-values.yaml" > "${kp_tmp}/runtime-secrets-changed-values.yaml"
helm template github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/runtime-secrets-changed-values.yaml" > "${kp_tmp}/runtime-secrets-changed.yaml"
kp_runtime_secret_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/runtime-secrets.yaml")"
kp_runtime_secret_changed_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/runtime-secrets-changed.yaml")"
[[ "${kp_runtime_secret_config_name}" != "${kp_runtime_secret_changed_config_name}" ]] || { printf 'runtime-secret config mutation did not produce a new immutable ConfigMap name\n' >&2; exit 1; }

for kp_runtime_secret_mutation in \
  '.config.runtimeSecrets.enabled = false' \
  '.config.runtimeSecrets.namespaces = []' \
  '.config.runtimeSecrets.namespaces = ["apps-production", "apps-production"]' \
  '.config.runtimeSecrets.namespaces = ["apps-staging", "apps-production"]' \
  '.config.runtimeSecrets.fingerprintSecret.name = ""' \
  '.config.runtimeSecrets.sealingCertificateSecret.name = ""' \
  '.config.runtimeSecrets.fingerprintSecret.key = "attacker-selected"' \
  '.config.runtimeSecrets.sealingCertificateSecret.key = "attacker.crt"' \
  '.config.runtimeSecrets.fingerprintKeyID = "attacker-key"' \
  '.config.runtimeSecrets.workLeaseSeconds = 20 | .config.runtimeSecrets.heartbeatSeconds = 10' \
  '.config.runtimeSecrets.minimumBackoffSeconds = 10 | .config.runtimeSecrets.maximumBackoffSeconds = 9' \
  '.config.runtimeSecrets.unknownField = true' \
  '.config.gitProjection.enabled = false' \
  '.networkPolicy.enabled = false' \
  '.networkPolicy.kubeAPIServerCIDRs = []' \
  '.components.api.image.reference = "kuberploy-api:mutable"' \
  '.components.worker.image.reference = "kuberploy-worker:mutable"'; do
  yq "${kp_runtime_secret_mutation}" "${kp_tmp}/runtime-secrets-values.yaml" > "${kp_tmp}/runtime-secrets-invalid.yaml"
  if helm template invalid-runtime-secrets "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render -f "${kp_tmp}/runtime-secrets-invalid.yaml" >/dev/null 2>&1; then
    printf 'platform chart accepted invalid runtime-secret settings: %s\n' "${kp_runtime_secret_mutation}" >&2
    exit 1
  fi
done

# Template checks remain closed if schema validation is explicitly bypassed.
if helm template invalid-runtime-secrets "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render -f "${kp_tmp}/runtime-secrets-values.yaml" \
  --skip-schema-validation --set 'config.runtimeSecrets.namespaces={apps-staging,apps-production}' >/dev/null 2>&1; then
  printf 'runtime-secret template accepted an unsorted namespace allowlist with schema validation bypassed\n' >&2
  exit 1
fi

yq '.config.certificateObservation.enabled = true' \
  "${kp_tmp}/runtime-secrets-values.yaml" > "${kp_tmp}/certificate-observation-values.yaml"
helm template github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/certificate-observation-values.yaml" > "${kp_tmp}/certificate-observation.yaml"
yq eval-all 'true' "${kp_tmp}/certificate-observation.yaml" >/dev/null
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_CERTIFICATE_OBSERVATION_ENABLED' "${kp_tmp}/certificate-observation.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_CERTIFICATE_OBSERVATION_POLL_SECONDS' "${kp_tmp}/certificate-observation.yaml")" == "30" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.KUBERPLOY_CERTIFICATE_OBSERVATION_MAXIMUM_AGE_SECONDS' "${kp_tmp}/certificate-observation.yaml")" == "90" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_CERTIFICATE_OBSERVATION"))] | length' "${kp_tmp}/certificate-observation.yaml" | tail -1)" == "16" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "web") | .spec.template.spec.containers[0].env[] | select(.name | test("^KUBERPLOY_CERTIFICATE_OBSERVATION"))] | length' "${kp_tmp}/certificate-observation.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[]? | select(.name == "runtime-secret-fingerprint")] | length' "${kp_tmp}/certificate-observation.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Role" and (.metadata.name | test("-runtime-secrets-worker$"))) | .rules[] | select((.apiGroups | join(",")) == "bitnami.com" and (.resources | join(",")) == "sealedsecrets" and (.verbs | join(",")) == "get")] | length' "${kp_tmp}/certificate-observation.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "Role" and (.metadata.name | test("-runtime-secrets-worker$"))) | .rules[] | select(.resources[] == "secrets" or (.verbs[] | test("list|watch|create|delete|update|patch")))] | length' "${kp_tmp}/certificate-observation.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.labels."app.kubernetes.io/component" == "runtime-secrets-admission") | ([.spec.validations[].expression] | join("\n")) as $expressions | (($expressions | contains("kubernetes.io/tls")) and ($expressions | contains("tls.crt")) and ($expressions | contains("tls.key")) and ($expressions | contains("Opaque")))' "${kp_tmp}/certificate-observation.yaml")" == "true" ]] || { printf 'certificate observation did not retain the exact TLS SealedSecret admission envelope\n' >&2; exit 1; }
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and (.metadata.labels."app.kubernetes.io/component" == "api" or .metadata.labels."app.kubernetes.io/component" == "worker")) | .spec.egress[] | select(.to[0].ipBlock.cidr == "10.43.0.1/32" and (.ports | length) == 2 and .ports[0].port == 443 and .ports[1].port == 6443)] | length' "${kp_tmp}/certificate-observation.yaml" | tail -1)" == "2" ]]

yq '.config.certificateObservation.pollIntervalSeconds = 40 | .config.certificateObservation.maximumAgeSeconds = 120' \
  "${kp_tmp}/certificate-observation-values.yaml" > "${kp_tmp}/certificate-observation-changed-values.yaml"
helm template github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/certificate-observation-changed-values.yaml" > "${kp_tmp}/certificate-observation-changed.yaml"
kp_certificate_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/certificate-observation.yaml")"
kp_certificate_changed_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/certificate-observation-changed.yaml")"
[[ "${kp_certificate_config_name}" != "${kp_certificate_changed_config_name}" ]] || { printf 'certificate observation config mutation did not produce a new immutable ConfigMap name\n' >&2; exit 1; }

for kp_certificate_mutation in \
  '.config.certificateObservation.enabled = false | .config.certificateObservation.pollIntervalSeconds = 31' \
  '.config.runtimeSecrets.enabled = false' \
  '.config.gitProjection.enabled = false' \
  '.networkPolicy.enabled = false' \
  '.networkPolicy.kubeAPIServerCIDRs = []' \
  '.components.api.image.reference = "kuberploy-api:mutable"' \
  '.components.worker.image.reference = "kuberploy-worker:mutable"' \
  '.config.certificateObservation.workLeaseSeconds = 20 | .config.certificateObservation.heartbeatSeconds = 10' \
  '.config.certificateObservation.minimumBackoffSeconds = 10 | .config.certificateObservation.maximumBackoffSeconds = 9' \
  '.config.certificateObservation.pollIntervalSeconds = 60 | .config.certificateObservation.maximumAgeSeconds = 119' \
  '.config.certificateObservation.maximumAgeSeconds = 1801' \
  '.config.certificateObservation.unknownField = true'; do
  yq "${kp_certificate_mutation}" "${kp_tmp}/certificate-observation-values.yaml" > "${kp_tmp}/certificate-observation-invalid.yaml"
  if helm template github-builds "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render -f "${kp_tmp}/certificate-observation-invalid.yaml" >/dev/null 2>&1; then
    printf 'platform chart accepted invalid certificate observation settings: %s\n' "${kp_certificate_mutation}" >&2
    exit 1
  fi
done

for kp_certificate_mutation in \
  '.config.runtimeSecrets.enabled = false' \
  '.config.gitProjection.enabled = false' \
  '.networkPolicy.enabled = false' \
  '.networkPolicy.kubeAPIServerCIDRs = []' \
  '.components.worker.image.reference = "kuberploy-worker:mutable"' \
  '.config.certificateObservation.pollIntervalSeconds = 60 | .config.certificateObservation.maximumAgeSeconds = 119'; do
  yq "${kp_certificate_mutation}" "${kp_tmp}/certificate-observation-values.yaml" > "${kp_tmp}/certificate-observation-invalid.yaml"
  if helm template github-builds "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render -f "${kp_tmp}/certificate-observation-invalid.yaml" \
    --skip-schema-validation >/dev/null 2>&1; then
    printf 'certificate observation template accepted an invalid production fence with schema validation bypassed: %s\n' "${kp_certificate_mutation}" >&2
    exit 1
  fi
done

yq '.config.gitProjection.pollIntervalSeconds = 600' "${kp_tmp}/git-projection-values.yaml" > "${kp_tmp}/git-projection-changed-values.yaml"
helm template github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/git-projection-changed-values.yaml" > "${kp_tmp}/git-projection-changed.yaml"
kp_projection_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/git-projection.yaml")"
kp_projection_changed_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/git-projection-changed.yaml")"
[[ "${kp_projection_config_name}" != "${kp_projection_changed_config_name}" ]] || { printf 'Git projection config mutation did not produce a new immutable ConfigMap name\n' >&2; exit 1; }

yq '.config.gitProjection.enabled = true | .config.gitProjection.chartVersion = "0.1.0-rc.166"' "${kp_root}/test/e2e/fixtures/platform-values.yaml" > "${kp_tmp}/git-projection-no-github.yaml"
if helm template invalid-git-projection "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render -f "${kp_tmp}/git-projection-no-github.yaml" >/dev/null 2>&1; then
  printf 'platform chart enabled Git projection without the GitHub App boundary\n' >&2
  exit 1
fi
for kp_projection_mutation in \
  '.config.gitProjection.cacheMaxBytes = 67108863' \
  '.config.gitProjection.cacheMaxBytes = 2147483649' \
  '.config.gitProjection.pollIntervalSeconds = 14' \
  '.config.gitProjection.pollIntervalSeconds = 86401' \
  '.config.gitProjection.chartVersion = ""' \
  '.config.gitProjection.chartVersion = "sha256:ABC"' \
  '.config.gitProjection.unknownField = true'; do
  yq "${kp_projection_mutation}" "${kp_tmp}/git-projection-values.yaml" > "${kp_tmp}/git-projection-invalid.yaml"
  if helm template invalid-git-projection "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render -f "${kp_tmp}/git-projection-invalid.yaml" >/dev/null 2>&1; then
    printf 'platform chart accepted invalid Git projection settings: %s\n' "${kp_projection_mutation}" >&2
    exit 1
  fi
done

yq '.config.githubApp.clientID = "Iv1_ChangedClient"' "${kp_tmp}/github-build-values.yaml" > "${kp_tmp}/github-build-changed-values.yaml"
helm template github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/github-build-changed-values.yaml" > "${kp_tmp}/github-builds-changed.yaml"
kp_github_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/github-builds.yaml")"
kp_github_changed_config_name="$(yq eval-all 'select(.kind == "ConfigMap") | .metadata.name' "${kp_tmp}/github-builds-changed.yaml")"
[[ "${kp_github_config_name}" != "${kp_github_changed_config_name}" ]] || { printf 'GitHub App config mutation did not produce a new immutable ConfigMap name\n' >&2; exit 1; }

yq '.config.githubApp.enabled = true |
    .config.githubApp.appID = 0 |
    .config.githubApp.clientID = "" |
    .config.githubApp.appSlug = "" |
    .config.githubApp.secretRef.name = "" |
    .builder.enabled = true' \
  "${kp_root}/test/e2e/fixtures/platform-values.yaml" > "${kp_tmp}/github-build-incomplete.yaml"
if helm template invalid-github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render -f "${kp_tmp}/github-build-incomplete.yaml" >/dev/null 2>&1; then
  printf 'platform chart accepted incomplete GitHub App configuration\n' >&2
  exit 1
fi
yq '.builder.enabled = false' "${kp_tmp}/github-build-values.yaml" > "${kp_tmp}/github-build-no-builder.yaml"
if helm template invalid-github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render -f "${kp_tmp}/github-build-no-builder.yaml" >/dev/null 2>&1; then
  printf 'platform chart enabled GitHub builds without the builder boundary\n' >&2
  exit 1
fi
yq '.builder.controllerServiceAccount.name = "unrelated-controller"' "${kp_tmp}/github-build-values.yaml" > "${kp_tmp}/github-build-wrong-service-account.yaml"
if helm template invalid-github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render -f "${kp_tmp}/github-build-wrong-service-account.yaml" >/dev/null 2>&1; then
  printf 'platform chart accepted a mismatched builder controller ServiceAccount\n' >&2
  exit 1
fi
yq '.builder.controllerServiceAccount.namespace = "unrelated-namespace"' "${kp_tmp}/github-build-values.yaml" > "${kp_tmp}/github-build-wrong-controller-namespace.yaml"
if helm template invalid-github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render -f "${kp_tmp}/github-build-wrong-controller-namespace.yaml" >/dev/null 2>&1; then
  printf 'platform chart accepted a cross-namespace builder controller identity\n' >&2
  exit 1
fi
yq '.config.githubApp.unknownField = true' "${kp_tmp}/github-build-values.yaml" > "${kp_tmp}/github-build-unknown.yaml"
if helm template invalid-github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render -f "${kp_tmp}/github-build-unknown.yaml" >/dev/null 2>&1; then
  printf 'platform chart accepted an unknown GitHub App field\n' >&2
  exit 1
fi
for kp_build_mutation in \
  '.builder.networkPolicy.sourceEgressCIDRs = []' \
  '.builder.networkPolicy.registryEgressCIDRs = []' \
  '.builder.networkPolicy.sourceEgressCIDRs = ["0.0.0.0/0"]' \
  '.builder.networkPolicy.registryEgressCIDRs = ["192.0.2.0/24"]' \
  '.config.publicURL = "http://kuberploy.example.test"' \
  '.config.githubApp.secretRef.oauthClientSecretKey = .config.githubApp.secretRef.webhookSecretKey' \
  '.config.githubApp.appSlug = "Invalid Slug"'; do
  yq "${kp_build_mutation}" "${kp_tmp}/github-build-values.yaml" > "${kp_tmp}/github-build-invalid-runtime.yaml"
  if helm template invalid-github-builds "${kp_root}/charts/kuberploy" \
    --namespace kuberploy-e2e-render -f "${kp_tmp}/github-build-invalid-runtime.yaml" >/dev/null 2>&1; then
    printf 'platform chart accepted invalid GitHub build runtime: %s\n' "${kp_build_mutation}" >&2
    exit 1
  fi
done

yq '.builder.networkPolicy.registryEgressCIDRs = [.networkPolicy.kubeAPIServerCIDRs[0]]' \
  "${kp_tmp}/github-build-values.yaml" > "${kp_tmp}/github-build-shared-node-rejected.yaml"
if helm template invalid-github-builds "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render -f "${kp_tmp}/github-build-shared-node-rejected.yaml" >/dev/null 2>&1; then
  printf 'platform chart accepted shared builder and Kubernetes API egress\n' >&2
  exit 1
fi

yq '.config.git.remoteURL = "https://github.example.test/platform/config.git" |
    .config.git.credentialsSecret.name = "git-writer-credentials"' \
  "${kp_root}/test/e2e/fixtures/platform-values.yaml" > "${kp_tmp}/git-auth-values.yaml"
helm template git-auth "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render \
  -f "${kp_tmp}/git-auth-values.yaml" > "${kp_tmp}/git-auth.yaml"
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.containers[0].env[] | select(.name == "KUBERPLOY_GIT_AUTH_MODE") | .value' "${kp_tmp}/git-auth.yaml")" == "secret" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[] | select(.name == "git-credentials") | .secret.defaultMode' "${kp_tmp}/git-auth.yaml")" == "288" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/component" == "worker") | .spec.template.spec.volumes[] | select(.name == "git-credentials") | [.secret.items[].path] | sort | join(",")' "${kp_tmp}/git-auth.yaml")" == "password,username" ]]
if helm template invalid-git-auth "${kp_root}/charts/kuberploy" \
  --set config.git.credentialsSecret.name=git-writer-credentials >/dev/null 2>&1; then
  printf 'Git credential Secret was accepted without an HTTPS remote\n' >&2
  exit 1
fi

yq '.global.requireImageDigest = true' "${kp_root}/test/e2e/fixtures/platform-values.yaml" > "${kp_tmp}/release-images-required.yaml"
if helm template invalid "${kp_root}/charts/kuberploy" -f "${kp_tmp}/release-images-required.yaml" >/dev/null 2>&1; then
  printf 'release packaging accepted non-digest control-plane images\n' >&2
  exit 1
fi
helm template upgrade-seam "${kp_root}/charts/kuberploy" \
  -f "${kp_root}/test/e2e/fixtures/platform-values.yaml" > "${kp_tmp}/upgrade-job-render.yaml"
[[ "$(yq eval-all 'select(.kind == "Job" and .metadata.labels."app.kubernetes.io/component" == "migration") | .metadata.annotations."helm.sh/hook"' "${kp_tmp}/upgrade-job-render.yaml")" == "pre-install,pre-upgrade" ]]
[[ "$(yq eval-all 'select(.kind == "Job" and .metadata.labels."app.kubernetes.io/component" == "migration") | .spec.template.spec.automountServiceAccountToken' "${kp_tmp}/upgrade-job-render.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Job" and .metadata.labels."app.kubernetes.io/component" == "migration") | .spec.template.spec.containers[0].image' "${kp_tmp}/upgrade-job-render.yaml")" == "ghcr.io/kuberploy/kuberploy-migration:0.1.0-rc.166" ]]
[[ "$(yq eval-all 'select(.kind == "Job" and .metadata.labels."app.kubernetes.io/component" == "migration") | .spec.template.spec.containers[0].env[0].name' "${kp_tmp}/upgrade-job-render.yaml")" == "DATABASE_URL" ]]
[[ "$(yq eval-all 'select(.kind == "Job" and .metadata.labels."app.kubernetes.io/component" == "migration") | .spec.template.spec.containers[0].command // "implicit-entrypoint"' "${kp_tmp}/upgrade-job-render.yaml")" == "implicit-entrypoint" ]]
[[ "$(yq eval-all 'select(.kind == "NetworkPolicy" and .metadata.name == "upgrade-seam-migration") | .spec.podSelector.matchLabels."app.kubernetes.io/component"' "${kp_tmp}/upgrade-job-render.yaml")" == "migration" ]]
[[ "$(yq eval-all 'select(.kind == "NetworkPolicy" and .metadata.name == "upgrade-seam-migration") | .metadata.annotations."helm.sh/hook"' "${kp_tmp}/upgrade-job-render.yaml")" == "pre-install,pre-upgrade" ]]
[[ "$(yq eval-all 'select(.kind == "NetworkPolicy" and .metadata.name == "upgrade-seam-migration") | .metadata.annotations."helm.sh/hook-weight"' "${kp_tmp}/upgrade-job-render.yaml")" == "-9" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.name == "upgrade-seam-migration") | .spec.egress[] | select(.to[0].namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "kube-system" and .to[0].podSelector.matchLabels."k8s-app" == "kube-dns" and ([.ports[].port] | sort | join(",")) == "53,53")] | length' "${kp_tmp}/upgrade-job-render.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.name == "upgrade-seam-migration") | .spec.egress[] | select(.to[0].namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "kuberploy-system" and .to[0].podSelector.matchLabels."app.kubernetes.io/name" == "kuberploy-postgresql" and (.ports | length) == 1 and .ports[0].port == 5432)] | length' "${kp_tmp}/upgrade-job-render.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.name == "upgrade-seam-migration") | .spec.egress[] | select(.to[0].ipBlock.cidr == "192.0.2.20/32" and (.ports | length) == 1 and .ports[0].port == 5432)] | length' "${kp_tmp}/upgrade-job-render.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.name == "upgrade-seam-migration") | .spec.egress[] | .ports[].port | select(. != 53 and . != 5432)] | length' "${kp_tmp}/upgrade-job-render.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "ServiceAccount" and .metadata.name == "upgrade-seam-migration")] | length' "${kp_tmp}/upgrade-job-render.yaml" | tail -1)" == "0" ]]

yq '.config.bootstrapSecret.generate = true' \
  "${kp_root}/test/e2e/fixtures/platform-values.yaml" > "${kp_tmp}/bootstrap-token-values.yaml"
helm template bootstrap-token "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render -f "${kp_tmp}/bootstrap-token-values.yaml" \
  > "${kp_tmp}/bootstrap-token.yaml"
[[ "$(yq eval-all 'select(.kind == "Job" and .metadata.name == "bootstrap-token-bootstrap-token") | .spec.template.spec.containers[0].command | join(" ")' "${kp_tmp}/bootstrap-token.yaml")" == "/kuberploy-bootstrap-token" ]]
[[ "$(yq eval-all 'select(.kind == "Job" and .metadata.name == "bootstrap-token-bootstrap-token") | .spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation' "${kp_tmp}/bootstrap-token.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Job" and .metadata.name == "bootstrap-token-bootstrap-token") | .spec.template.spec.containers[0].securityContext.capabilities.drop | join(",")' "${kp_tmp}/bootstrap-token.yaml")" == "ALL" ]]
[[ "$(yq eval-all 'select(.kind == "Role" and .metadata.name == "bootstrap-token-bootstrap-token") | .rules[0].verbs | join(",")' "${kp_tmp}/bootstrap-token.yaml")" == "create" ]]
[[ "$(yq eval-all 'select(.kind == "Role" and .metadata.name == "bootstrap-token-bootstrap-token") | .rules[0].resources | join(",")' "${kp_tmp}/bootstrap-token.yaml")" == "secrets" ]]
[[ "$(yq eval-all 'select(.kind == "NetworkPolicy" and .metadata.name == "bootstrap-token-bootstrap-token") | .spec.egress[0].to[0].ipBlock.cidr' "${kp_tmp}/bootstrap-token.yaml")" == "10.43.0.1/32" ]]
if rg -n 'KUBERPLOY_BOOTSTRAP_TOKEN=|stringData:|^[[:space:]]+token:[[:space:]]' \
  "${kp_tmp}/bootstrap-token.yaml"; then
  printf 'bootstrap token material leaked into the Helm release manifest\n' >&2
  exit 1
fi
yq '.config.bootstrapSecret.generate = true | .networkPolicy.kubeAPIServerCIDRs = []' \
  "${kp_root}/test/e2e/fixtures/platform-values.yaml" > "${kp_tmp}/bootstrap-token-no-api.yaml"
if helm template invalid-bootstrap-token "${kp_root}/charts/kuberploy" \
  --namespace kuberploy-e2e-render -f "${kp_tmp}/bootstrap-token-no-api.yaml" \
  --skip-schema-validation >/dev/null 2>&1; then
  printf 'bootstrap generator accepted NetworkPolicy without exact API CIDR\n' >&2
  exit 1
fi

if rg -n ":latest([@[:space:]\"']|$)" \
  "${kp_root}/charts/kuberploy/values.yaml" \
  "${kp_root}/charts/kuberploy-runtime/values.yaml" \
  "${kp_tmp}"; then
  printf 'floating latest reference found\n' >&2
  exit 1
fi

printf 'All Helm lint, deterministic render, identity, immutability and RBAC checks passed.\n'
