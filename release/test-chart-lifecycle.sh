#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-chart-lifecycle.XXXXXX")"
trap 'rm -rf -- "${kp_tmp}"' EXIT

kp_version="$(python3 "${kp_root}/release/validate_source.py" --root "${kp_root}")"

kp_stage_chart() {
  local destination="$1"
  local digit="$2"
  local digest
  digest="sha256:$(printf "${digit}%.0s" {1..64})"
  local chart_args=(
    --source "${kp_root}/charts/kuberploy"
    --builder-chart "${kp_root}/charts/kuberploy-builder"
    --destination "${destination}"
    --version "${kp_version}"
    --api-image "ghcr.io/kuberploy/kuberploy-api@${digest}"
    --worker-image "ghcr.io/kuberploy/kuberploy-worker@${digest}"
    --web-image "ghcr.io/kuberploy/kuberploy-web@${digest}"
    --migration-image "ghcr.io/kuberploy/kuberploy-migration@${digest}"
    --upgrader-image "ghcr.io/kuberploy/kuberploy-upgrader@${digest}"
    --builder-agent-image "ghcr.io/kuberploy/kuberploy-builder-agent@${digest}"
  )
  python3 "${kp_root}/release/package_chart.py" "${chart_args[@]}" >/dev/null
}

kp_stage_chart "${kp_tmp}/install-chart" 6
kp_stage_chart "${kp_tmp}/upgrade-chart" 7

kp_network_args=(--set-string networkPolicy.kubeAPIServerCIDRs[0]=10.43.0.1/32)
helm lint "${kp_tmp}/install-chart" "${kp_network_args[@]}" >/dev/null
helm lint "${kp_tmp}/upgrade-chart" "${kp_network_args[@]}" >/dev/null
helm template kuberploy "${kp_tmp}/install-chart" --namespace kuberploy-system "${kp_network_args[@]}" >"${kp_tmp}/install.yaml"
helm template kuberploy "${kp_tmp}/upgrade-chart" --namespace kuberploy-system --is-upgrade "${kp_network_args[@]}" >"${kp_tmp}/upgrade.yaml"
helm template kuberploy "${kp_tmp}/install-chart" --namespace kuberploy-system "${kp_network_args[@]}" >"${kp_tmp}/rollback.yaml"
helm template kuberploy "${kp_tmp}/install-chart" \
  --namespace kuberploy-system \
  "${kp_network_args[@]}" \
  --set builder.enabled=true >"${kp_tmp}/builder-enabled.yaml"
helm template kuberploy "${kp_tmp}/install-chart" \
  --namespace kuberploy-system \
  "${kp_network_args[@]}" \
  --set-string networkPolicy.externalEgressCIDRs[0]=192.0.2.1/32 \
  --set-string builder.networkPolicy.sourceEgressCIDRs[0]=192.0.2.2/32 \
  --set-string builder.networkPolicy.registryEgressCIDRs[0]=192.0.2.3/32 \
  --set builder.enabled=true \
  --set-string config.publicURL=https://kuberploy.example.test \
  --set config.githubApp.enabled=true \
  --set config.githubApp.appID=12345 \
  --set-string config.githubApp.clientID=Iv1_KuberployClient \
  --set-string config.githubApp.appSlug=kuberploy \
  --set config.githubApp.secretRef.name=kuberploy-github-app >"${kp_tmp}/github-builder-enabled.yaml"
diff -u "${kp_tmp}/install.yaml" "${kp_tmp}/rollback.yaml" >/dev/null

[[ "$(grep -c '^kind: Deployment$' "${kp_tmp}/install.yaml")" -eq 3 ]]
[[ "$(grep -c '^kind: PodDisruptionBudget$' "${kp_tmp}/install.yaml")" -eq 3 ]]
[[ "$(grep -c '^kind: NetworkPolicy$' "${kp_tmp}/install.yaml")" -eq 7 ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "NetworkPolicy" and (.metadata.name | test("-private-egress$"))) | [.spec.egress[0].to[].ipBlock.cidr]' "${kp_tmp}/install.yaml")" == '["10.0.0.0/8","172.16.0.0/12","192.168.0.0/16"]' ]]
[[ "$(grep -Ec '^kind: (ClusterRole|ClusterRoleBinding|Namespace|Application|ApplicationSet|AppProject)$' "${kp_tmp}/install.yaml")" -eq 0 ]]
[[ "$(grep -Ec '^kind: (Namespace|ResourceQuota|ValidatingAdmissionPolicy|ValidatingAdmissionPolicyBinding)$' "${kp_tmp}/install.yaml")" -eq 0 ]]
[[ "$(grep -Ec 'image: ".+@sha256:[a-f0-9]{64}"' "${kp_tmp}/install.yaml")" -eq 4 ]]
grep -q 'ghcr.io/kuberploy/kuberploy-migration@sha256:' "${kp_tmp}/install.yaml"
grep -q 'helm.sh/hook: pre-install,pre-upgrade' "${kp_tmp}/install.yaml"
! grep -q 'KUBERPLOY_UPGRADER' "${kp_tmp}/install.yaml"
! grep -q 'kuberploy-upgrader' "${kp_tmp}/install.yaml"
! grep -q 'name: kuberploy-upgrade' "${kp_tmp}/install.yaml"
! grep -q 'app.kubernetes.io/component: upgrade' "${kp_tmp}/install.yaml"

[[ "$(grep -c '^kind: Namespace$' "${kp_tmp}/builder-enabled.yaml")" -eq 1 ]]
[[ "$(grep -c '^kind: ResourceQuota$' "${kp_tmp}/builder-enabled.yaml")" -eq 1 ]]
[[ "$(grep -c '^kind: ValidatingAdmissionPolicy$' "${kp_tmp}/builder-enabled.yaml")" -eq 7 ]]
[[ "$(grep -c '^kind: ValidatingAdmissionPolicyBinding$' "${kp_tmp}/builder-enabled.yaml")" -eq 7 ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name | test("-private-egress$"))) | [.spec.validations[].expression]' "${kp_tmp}/builder-enabled.yaml" | grep -c '10.0.0.0/8.*172.16.0.0/12.*192.168.0.0/16')" -eq 1 ]]
[[ "$(grep -c '^kind: Job$' "${kp_tmp}/builder-enabled.yaml")" -eq 1 ]]
grep -q 'ghcr.io/kuberploy/kuberploy-builder-agent@sha256:' "${kp_tmp}/builder-enabled.yaml"
grep -q 'KUBERPLOY_GITHUB_BUILDS_ENABLED: "true"' "${kp_tmp}/github-builder-enabled.yaml"
grep -q 'secretName: kuberploy-github-app' "${kp_tmp}/github-builder-enabled.yaml"
grep -q 'path: runtime/private-key.pem' "${kp_tmp}/github-builder-enabled.yaml"
[[ "$(grep -c 'name: github-app-private-key' "${kp_tmp}/github-builder-enabled.yaml")" -eq 2 ]]

helm upgrade --help | grep -q -- '--rollback-on-failure'
if helm upgrade --help | grep -q -- '--atomic'; then
  printf 'Helm 4 unexpectedly exposes deprecated --atomic; review release flags\n' >&2
  exit 1
fi
printf 'chart install/upgrade/rollback render validation passed\n'
