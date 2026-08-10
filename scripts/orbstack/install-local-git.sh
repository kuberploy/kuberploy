#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_assert_orbstack_context
kp_require_tools docker helm kubectl
kp_run_id="${1:?usage: install-local-git.sh <run-id>}"
kp_namespace="$(kp_require_run_namespace "${kp_run_id}")"
kp_root="$(kp_repo_root)"
kp_release="$(kp_release_name local-git "${kp_run_id}")"
readonly KP_LOCAL_GIT_IMAGE="kuberploy-local-git:e2e-2.54.0"

# The upstream alpine/git image intentionally omits the git-daemon subpackage.
# Build our exact, non-root integration image into OrbStack's shared image store
# and fail before Helm mutation if the daemon is not present.
docker build \
  --file "${kp_root}/build/package/local-git.Dockerfile" \
  --tag "${KP_LOCAL_GIT_IMAGE}" \
  "${kp_root}"
docker run --rm --entrypoint /bin/sh "${KP_LOCAL_GIT_IMAGE}" \
  -ceu 'test -x "$(git --exec-path)/git-daemon"'

helm upgrade --install "${kp_release}" "${kp_root}/deploy/orbstack/local-git" \
  --namespace "${kp_namespace}" \
  --values "${kp_root}/deploy/orbstack/local-git/values.yaml" \
  --set-string "testRun=${kp_run_id}" \
  --set-string "image.reference=${KP_LOCAL_GIT_IMAGE}" \
  --set-string "image.pullPolicy=Never" \
  --wait --timeout 5m

kubectl rollout status statefulset/kuberploy-git -n "${kp_namespace}" --timeout=5m
printf 'Local Git is available only inside the cluster at git://kuberploy-git.%s.svc.cluster.local/kuberploy-environments.git\n' "${kp_namespace}"
