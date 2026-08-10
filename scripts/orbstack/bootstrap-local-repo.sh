#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_assert_orbstack_context
kp_require_tools kubectl sed
kp_run_id="${1:?usage: bootstrap-local-repo.sh <run-id>}"
kp_namespace="$(kp_require_run_namespace "${kp_run_id}")"
kp_root="$(kp_repo_root)"
kp_ingress_class="kp-e2e-${kp_run_id}"
kp_git_url="git://kuberploy-git.${kp_namespace}.svc.cluster.local/kuberploy-environments.git"
kp_pod="kuberploy-git-0"
kp_worktree="/git/bootstrap-${kp_run_id}"
kp_bare_repo="/git/repositories/kuberploy-environments.git"
kp_stage="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-e2e-${kp_run_id}.XXXXXX")"

kp_remove_stage() {
  [[ -n "${kp_stage:-}" && "${kp_stage}" == *"/kuberploy-e2e-${kp_run_id}."* ]] && rm -rf -- "${kp_stage}"
}
trap kp_remove_stage EXIT

mkdir -p "${kp_stage}/charts" "${kp_stage}/platform/root" \
  "${kp_stage}/environments/22222222-2222-4222-8222-222222222222/apps/33333333-3333-4333-8333-333333333333"
cp -R "${kp_root}/charts/kuberploy-runtime" "${kp_stage}/charts/kuberploy-runtime"

sed \
  -e "s|@@RUN_ID@@|${kp_run_id}|g" \
  -e "s|@@RUN_NAMESPACE@@|${kp_namespace}|g" \
  -e "s|@@GIT_URL@@|${kp_git_url}|g" \
  "${kp_root}/deploy/gitops/repository/platform/root/applicationset.yaml.tmpl" \
  > "${kp_stage}/platform/root/applicationset.yaml"
sed \
  -e "s|@@INGRESS_CLASS@@|${kp_ingress_class}|g" \
  "${kp_root}/deploy/gitops/repository/environments/22222222-2222-4222-8222-222222222222/apps/33333333-3333-4333-8333-333333333333/app.yaml.tmpl" \
  > "${kp_stage}/environments/22222222-2222-4222-8222-222222222222/apps/33333333-3333-4333-8333-333333333333/app.yaml"

kubectl wait --for=condition=Ready "pod/${kp_pod}" -n "${kp_namespace}" --timeout=5m
kubectl exec -n "${kp_namespace}" "${kp_pod}" -c git-daemon -- \
  /bin/sh -ceu '
    worktree="$1"
    bare_repo="$2"
    case "${worktree}" in /git/bootstrap-*) ;; *) exit 64 ;; esac
    rm -rf -- "${worktree}"
    git clone "${bare_repo}" "${worktree}"
  ' sh "${kp_worktree}" "${kp_bare_repo}"
kubectl cp "${kp_stage}/." "${kp_namespace}/${kp_pod}:${kp_worktree}" -c git-daemon
kubectl exec -n "${kp_namespace}" "${kp_pod}" -c git-daemon -- \
  /bin/sh -ceu '
    worktree="$1"
    git -C "${worktree}" config user.name "Kuberploy E2E"
    git -C "${worktree}" config user.email "e2e@kuberploy.local"
    git -C "${worktree}" add -A
    if ! git -C "${worktree}" diff --cached --quiet; then
      git -C "${worktree}" commit -m "bootstrap: first immutable image slice"
    fi
    git -C "${worktree}" push origin HEAD:main
  ' sh "${kp_worktree}"

printf 'Bootstrapped %s without copying any host or GitHub credentials.\n' "${kp_git_url}"
