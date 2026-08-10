#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
kp_source="${kp_root}/charts/kuberploy-argocd"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-argocd-render.XXXXXX")"

kp_cleanup() {
  if [[ -n "${kp_tmp:-}" && "${kp_tmp}" == *"/kuberploy-argocd-render."* ]]; then
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

kp_chart="${kp_tmp}/kuberploy-argocd"
cp -R "${kp_source}" "${kp_chart}"
mkdir -p "${kp_chart}/charts"
read -r kp_checksum kp_filename kp_url < <(awk 'NF && $1 !~ /^#/ {print}' "${kp_source}/testdata/upstream-artifacts.lock")
[[ "${kp_checksum}" == "d08882d22d0c76e3174e005cc09abe300c70ba556aec76725a4410d172b9c1f3" ]]
curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 "${kp_url}" -o "${kp_chart}/charts/${kp_filename}"
[[ "$(shasum -a 256 "${kp_chart}/charts/${kp_filename}" | awk '{print $1}')" == "${kp_checksum}" ]]
rg -F "${kp_checksum}" "${kp_root}/DEPENDENCIES.md" >/dev/null
helm dependency list "${kp_chart}" | rg -F 'ok' >/dev/null
python3 -m json.tool "${kp_chart}/values.schema.json" >/dev/null
[[ "$(yq '.dependencies | length' "${kp_chart}/Chart.lock")" == "1" ]]
[[ "$(yq '.dependencies[0].name' "${kp_chart}/Chart.lock")" == "argo-cd" ]]
[[ "$(yq '.dependencies[0].version' "${kp_chart}/Chart.lock")" == "10.3.0" ]]
[[ "$(yq '.digest' "${kp_chart}/Chart.lock")" == "sha256:9be31acc357d78df80e8e0278f487a26d2661f67362ba20765db03c38a986a11" ]]

kp_managed="${kp_source}/testdata/managed-values.yaml"
kp_adopted="${kp_source}/testdata/adopted-values.yaml"
helm lint "${kp_chart}" --namespace argocd -f "${kp_managed}" >/dev/null
helm lint "${kp_chart}" --namespace argocd -f "${kp_adopted}" >/dev/null
helm template argocd "${kp_chart}" --namespace argocd --include-crds --skip-tests -f "${kp_managed}" >"${kp_tmp}/managed.yaml"
helm template argocd "${kp_chart}" --namespace argocd --include-crds --skip-tests -f "${kp_managed}" >"${kp_tmp}/managed-again.yaml"
helm template argocd "${kp_chart}" --namespace argocd --include-crds --skip-tests -f "${kp_adopted}" >"${kp_tmp}/adopted.yaml"
diff -u "${kp_tmp}/managed.yaml" "${kp_tmp}/managed-again.yaml" >/dev/null

[[ "$(yq eval-all '[select(.kind == "Namespace")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "StatefulSet")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "3" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "2" ]]
[[ "$(yq eval-all '[select(.kind == "Secret")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Ingress" or .kind == "HTTPRoute" or .kind == "GRPCRoute")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Service" and .spec.type != "ClusterIP")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Application")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "AppProject")] | length' "${kp_tmp}/managed.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .metadata.name] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "kuberploy-platform-root" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .metadata.namespace] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "argocd" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .metadata.labels."app.kubernetes.io/part-of"] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "kuberploy" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .metadata.annotations."kuberploy.io/repository-secret"] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "kuberploy-repo-71111111111141118111111111111111" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec | keys | sort | join(",")] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "destination,project,source,syncPolicy" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec.project] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "kuberploy-platform-bootstrap" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec.source | keys | sort | join(",")] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "directory,path,repoURL,targetRevision" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec.source.repoURL] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "https://github.com/kuberploy/platform-gitops.git" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec.source.targetRevision] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "refs/heads/main" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec.source.path] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "clusters/71111111-1111-4111-8111-111111111112/argocd" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec.source.directory | keys | sort | join(",")] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "recurse" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec.source.directory.recurse] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "true" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec.destination | keys | sort | join(",")] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "namespace,server" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec.destination.server] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "https://kubernetes.default.svc" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec.destination.namespace] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "argocd" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec.syncPolicy | keys | sort | join(",")] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "automated,syncOptions" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec.syncPolicy.automated | keys | sort | join(",")] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "allowEmpty,prune,selfHeal" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec.syncPolicy.automated.allowEmpty] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "false" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec.syncPolicy.automated.prune] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "true" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec.syncPolicy.automated.selfHeal] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "true" ]]
[[ "$(yq eval-all '[select(.kind == "Application") | .spec.syncPolicy.syncOptions | join(",")] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "CreateNamespace=false,PruneLast=true,RespectIgnoreDifferences=true,ServerSideApply=true" ]]
[[ "$(yq eval-all '[select(.kind == "AppProject") | .metadata.name] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "kuberploy-platform-bootstrap" ]]
[[ "$(yq eval-all '[select(.kind == "AppProject") | .spec.sourceRepos | join(",")] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "https://github.com/kuberploy/platform-gitops.git" ]]
[[ "$(yq eval-all '[select(.kind == "ConfigMap" and .metadata.name == "argocd-argocd-profile") | .data.rootRepositorySecret] | .[0]' "${kp_tmp}/managed.yaml" | tail -1)" == "kuberploy-repo-71111111111141118111111111111111" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment" or .kind == "StatefulSet") | .spec.template.spec.containers[].image] | unique | length' "${kp_tmp}/managed.yaml" | tail -1)" == "1" ]]
rg -F 'quay.io/argoproj/argocd:v3.5.0@sha256:c298cedbaeb31532ba8d4e9904eba9e4987e067293fbd86400c5194e78f743d5' "${kp_tmp}/managed.yaml" >/dev/null
rg -F 'key: redis.db' "${kp_tmp}/managed.yaml" >/dev/null
rg -F 'name: kuberploy-argocd-valkey-auth' "${kp_tmp}/managed.yaml" >/dev/null
rg -F 'policy.default: ""' "${kp_tmp}/managed.yaml" >/dev/null
rg -F 'admin.enabled: "false"' "${kp_tmp}/managed.yaml" >/dev/null
if rg -n 'image: .*redis|image: .*dex|kind: Secret' "${kp_tmp}/managed.yaml" >/dev/null; then
  printf 'managed Argo CD rendered an embedded cache, Dex, or Secret\n' >&2
  exit 1
fi
[[ "$(yq eval-all '[select(.kind == "Deployment" or .kind == "StatefulSet" or .kind == "NetworkPolicy" or .kind == "Namespace" or .kind == "Application" or .kind == "AppProject")] | length' "${kp_tmp}/adopted.yaml" | tail -1)" == "0" ]]

kp_reject() {
  local kp_reason="$1"
  shift
  if helm template invalid "${kp_chart}" --namespace argocd --skip-tests -f "${kp_managed}" "$@" >/dev/null 2>&1; then
    printf 'unsafe Argo CD render accepted: %s\n' "${kp_reason}" >&2
    exit 1
  fi
}

kp_reject 'mutable image' --set-string argo-cd.global.image.tag=v3.5.0
kp_reject 'bundled Redis' --set argo-cd.redis.enabled=true
kp_reject 'local admin' --set 'argo-cd.configs.cm.admin\.enabled=true'
kp_reject 'default tenant role' --set-string 'argo-cd.configs.rbac.policy\.default=role:readonly'
kp_reject 'insecure server' --set-string 'argo-cd.configs.params.server\.insecure=true'
kp_reject 'public server' --set-string argo-cd.server.service.type=LoadBalancer
kp_reject 'direct ingress' --set argo-cd.server.ingress.enabled=true
kp_reject 'plugin sidecar' --set-string argo-cd.repoServer.extraContainers[0].name=plugin
kp_reject 'inline cache password' --set-string argo-cd.externalRedis.password=plaintext
kp_reject 'arbitrary resource' --set-string argo-cd.extraObjects[0].kind=Pod
kp_reject 'unqualified root ref' --set-string argoFoundation.bootstrap.targetRevision=main
kp_reject 'invalid platform cluster identity' --set-string argoFoundation.bootstrap.clusterID=not-a-uuid
kp_reject 'invalid platform binding identity' --set-string argoFoundation.bootstrap.bindingID=71111111-1111-0111-8111-111111111111
kp_reject 'non-canonical GitHub owner' --set-string argoFoundation.bootstrap.repositoryOwner=attacker.invalid/repo
kp_reject 'ambiguous GitHub repository' --set-string argoFoundation.bootstrap.repositoryName=..
kp_reject 'double-dot root ref' --set-string argoFoundation.bootstrap.targetRevision=refs/heads/release..candidate
kp_reject 'double-slash root ref' --set-string argoFoundation.bootstrap.targetRevision=refs/heads/release//candidate
kp_reject 'locked root ref' --set-string argoFoundation.bootstrap.targetRevision=refs/heads/release.lock
kp_reject 'reflog root ref' --set-string 'argoFoundation.bootstrap.targetRevision=refs/heads/release@{candidate'
kp_reject 'hidden root ref component' --set-string argoFoundation.bootstrap.targetRevision=refs/heads/release/.candidate
kp_reject 'trailing-dot root ref component' --set-string argoFoundation.bootstrap.targetRevision=refs/heads/release/candidate.
kp_reject 'caller-selected root URL' --set-string argoFoundation.bootstrap.repositoryURL=https://attacker.invalid/repo.git
kp_reject 'caller-selected root path' --set-string argoFoundation.bootstrap.path=../platform
kp_reject 'caller-selected credential Secret' --set-string argoFoundation.bootstrap.repositorySecretName=attacker-secret
kp_reject 'partial disabled bootstrap authority' --set argoFoundation.bootstrap.enabled=false
kp_reject 'disabled policy' --set argoFoundation.networkPolicy.enabled=false
kp_reject 'broad API egress' --set-string argoFoundation.networkPolicy.kubeAPIServerCIDRs[0]=0.0.0.0/0

# Template validation remains closed when an operator explicitly skips the
# values schema. These deprecated caller-selected fields must not be accepted
# merely because the fixed template would otherwise ignore them.
kp_reject 'schema-bypassed caller root URL' --skip-schema-validation --set-string argoFoundation.bootstrap.repositoryURL=https://attacker.invalid/repo.git
kp_reject 'schema-bypassed caller root path' --skip-schema-validation --set-string argoFoundation.bootstrap.path=clusters/attacker/argocd
kp_reject 'schema-bypassed caller credential' --skip-schema-validation --set-string argoFoundation.bootstrap.repositorySecretName=attacker-secret
kp_reject 'schema-bypassed invalid cluster UUID' --skip-schema-validation --set-string argoFoundation.bootstrap.clusterID=71111111-1111-0111-8111-111111111111
kp_reject 'schema-bypassed malformed branch' --skip-schema-validation --set-string argoFoundation.bootstrap.targetRevision=refs/heads/release//candidate
kp_reject 'schema-bypassed disabled partial authority' --skip-schema-validation --set argoFoundation.bootstrap.enabled=false
kp_reject 'schema-bypassed string enable flag' --skip-schema-validation --set-string argoFoundation.bootstrap.enabled=true

if helm template invalid "${kp_chart}" --namespace another-namespace --skip-tests -f "${kp_managed}" >/dev/null 2>&1; then
  printf 'Argo CD chart accepted a namespace outside its boundary\n' >&2
  exit 1
fi

printf 'Argo CD chart render and mutation validation passed\n'
