#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
kp_chart="${kp_root}/charts/kuberploy-builder"
kp_values="${kp_chart}/testdata/enabled-values.yaml"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-builder-render.XXXXXX")"

kp_cleanup() {
  if [[ -n "${kp_tmp:-}" && "${kp_tmp}" == *"/kuberploy-builder-render."* ]]; then
    rm -rf -- "${kp_tmp}"
  fi
}
trap kp_cleanup EXIT

for kp_tool in helm rg yq; do
  command -v "${kp_tool}" >/dev/null 2>&1 || {
    printf 'missing tool: %s\n' "${kp_tool}" >&2
    exit 1
  }
done

helm lint "${kp_chart}"
helm lint "${kp_chart}" -f "${kp_values}"

kp_disabled="$(helm template disabled "${kp_chart}")"
[[ -z "${kp_disabled}" ]] || {
  printf 'disabled builder chart rendered resources\n' >&2
  exit 1
}

kp_render="${kp_tmp}/enabled.yaml"
helm template boundary "${kp_chart}" -f "${kp_values}" >"${kp_render}"
yq eval-all 'true' "${kp_render}" >/dev/null

[[ "$(yq eval-all 'select(.kind == "Namespace") | .metadata.labels."pod-security.kubernetes.io/enforce"' "${kp_render}")" == "privileged" ]]
[[ "$(yq eval-all 'select(.kind == "ServiceAccount") | .automountServiceAccountToken' "${kp_render}")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "RoleBinding") | .subjects[0].namespace' "${kp_render}")" == "kuberploy-system" ]]
[[ "$(yq eval-all 'select(.kind == "RoleBinding") | .subjects[0].name' "${kp_render}")" == "kuberploy-controller" ]]
[[ "$(yq eval-all '[select(.kind == "RoleBinding") | .subjects[] | select(.name == "kuberploy-build-pod")] | length' "${kp_render}" | tail -1)" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "NetworkPolicy") | .spec.ingress | length' "${kp_render}")" == "0" ]]
kp_default_deny_expression="$(yq eval-all 'select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name | test("-default-deny$"))) | .spec.validations[0].expression' "${kp_render}")"
grep -F '!has(object.spec.ingress) || object.spec.ingress.size() == 0' <<<"${kp_default_deny_expression}" >/dev/null
grep -F '!has(object.spec.egress) || object.spec.egress.size() == 0' <<<"${kp_default_deny_expression}" >/dev/null
[[ "$(yq eval-all 'select(.kind == "NetworkPolicy") | .spec.egress | length' "${kp_render}")" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "ValidatingAdmissionPolicy")] | length' "${kp_render}" | tail -1)" == "6" ]]
[[ "$(yq eval-all '[select(.kind == "ValidatingAdmissionPolicy" and .spec.failurePolicy != "Fail")] | length' "${kp_render}" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "ValidatingAdmissionPolicyBinding")] | length' "${kp_render}" | tail -1)" == "6" ]]
[[ "$(yq eval-all '[select(.kind == "ValidatingAdmissionPolicyBinding" and .spec.validationActions[0] != "Deny")] | length' "${kp_render}" | tail -1)" == "0" ]]

for kp_required in \
  'request.userInfo.username' \
  'automountServiceAccountToken == false' \
  "object.spec.parallelism == 1" \
  "object.spec.podReplacementPolicy == 'Failed'" \
  "object.spec.template.spec.restartPolicy == 'Never'" \
  "object.spec.template.spec.securityContext.runAsUser == 65532" \
  "!has(object.spec.template.spec.hostNetwork)" \
  "!has(object.spec.template.spec.hostPID)" \
  "!has(object.spec.template.spec.hostIPC)" \
  "c.terminationMessagePath == '/result/result.json'" \
  "object.metadata.annotations['kuberploy.io/build-input-digest']" \
  "v.name in ['workspace', 'docker-socket', 'docker-data', 'result']" \
  "v.name in ['source-credentials', 'registry-push-credentials', 'registry-cache-credentials', 'build-secrets', 'ssh-secrets']" \
  "v.name == 'registry-push-credentials' && v.mountPath == '/var/run/secrets/kuberploy/registry-push'" \
  "v.name == 'registry-cache-credentials' && v.mountPath == '/var/run/secrets/kuberploy/registry-cache'" \
  "!has(v.readOnly) || v.readOnly == false" \
  "c.image == 'registry.example.test/kuberploy/builder-agent:0.1.0-rc.107'" \
  "c.name == 'checkout'" \
  "c.name == 'dind'" \
  "c.command == ['/usr/local/bin/docker-init', '--', '/usr/local/bin/dockerd']" \
  "!has(c.env[0].value)" \
  '!has(c.lifecycle)' \
  'c.securityContext.privileged == true' \
  "c.restartPolicy == 'Always'" \
  "v.name == 'workspace' && v.readOnly == true" \
  "nodeSelector['kuberploy.io/node-class'] == 'dind-builder'" \
  "cidr.endsWith('/32')" \
  "string(object.data['username']) == 'eC1hY2Nlc3MtdG9rZW4='" \
  "object.data['token'].size() <= 2732" \
  "object.metadata.name.startsWith('source-credentials-')" \
  "object.metadata.name == 'default-deny'" \
  "kuberploy.io/static-registry-egress" \
  "'system:masters' in request.userInfo.groups"; do
  rg -F "${kp_required}" "${kp_render}" >/dev/null || {
    printf 'admission render lacks required invariant: %s\n' "${kp_required}" >&2
    exit 1
  }
done

if rg -F "v.name == 'registry-credentials'" "${kp_render}" >/dev/null; then
  printf 'builder admission still accepts the obsolete shared registry credential volume\n' >&2
  exit 1
fi

kp_secret_verbs="$(yq eval-all 'select(.kind == "Role") | .rules[] | select(.resources[] == "secrets") | .verbs | sort | join(",")' "${kp_render}")"
[[ "${kp_secret_verbs}" == "create,delete,get" ]] || {
  printf 'builder controller Secret RBAC is not exact: %s\n' "${kp_secret_verbs}" >&2
  exit 1
}

if yq eval-all 'select(.kind == "Secret" or .kind == "Pod" or .kind == "Job" or .kind == "DaemonSet" or .kind == "Deployment") | .kind' "${kp_render}" | grep -q .; then
  printf 'builder boundary chart rendered credentials or a workload\n' >&2
  exit 1
fi
if rg -n '/var/run/docker\.sock|NodePort|LoadBalancer|0\.0\.0\.0/0' "${kp_render}"; then
  printf 'builder boundary render contains a forbidden host or public capability\n' >&2
  exit 1
fi
if helm template invalid "${kp_chart}" -f "${kp_values}" --set admissionPolicy.enabled=false >/dev/null 2>&1; then
  printf 'builder chart allowed admission enforcement to be disabled\n' >&2
  exit 1
fi
if helm template invalid "${kp_chart}" --set enabled=true >/dev/null 2>&1; then
  printf 'enabled builder chart accepted an empty trusted agent digest\n' >&2
  exit 1
fi
# A different administrator-selected digest is valid chart configuration, but
# the rendered Job policy must bind controllers to that exact value.
kp_alt="${kp_tmp}/alternate-image.yaml"
helm template alternate "${kp_chart}" -f "${kp_values}" --set builderAgentImage=attacker.test/agent:2.0.0 >"${kp_alt}"
rg -F "c.image == 'attacker.test/agent:2.0.0'" "${kp_alt}" >/dev/null
if helm template invalid "${kp_chart}" -f "${kp_values}" --set builderAgentImage=attacker.test/agent:latest >/dev/null 2>&1; then
  printf 'builder chart accepted a floating agent image\n' >&2
  exit 1
fi
if helm template invalid "${kp_chart}" -f "${kp_values}" --set unsupportedField=true >/dev/null 2>&1; then
  printf 'builder values schema accepted an unknown field\n' >&2
  exit 1
fi
if ! helm template parent-alias "${kp_chart}" -f "${kp_values}" \
  --set-string networkPolicy.sourceEgressCIDRs[0]=192.0.2.30/32 \
  --set-string networkPolicy.registryEgressCIDRs[0]=192.0.2.31/32 >/dev/null; then
  printf 'builder chart rejected the parent alias egress contract\n' >&2
  exit 1
fi
if helm template invalid "${kp_chart}" -f "${kp_values}" \
  --set-string networkPolicy.sourceEgressCIDRs[0]=192.0.2.0/24 >/dev/null 2>&1; then
  printf 'builder chart accepted a non-host source egress CIDR\n' >&2
  exit 1
fi

printf 'Builder boundary chart lint, admission, RBAC, quota, and default-deny checks passed.\n'
