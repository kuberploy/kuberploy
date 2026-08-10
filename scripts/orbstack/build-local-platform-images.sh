#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_assert_orbstack_context
kp_require_tools docker
kp_run_id="${1:?usage: build-local-platform-images.sh <run-id>}"
kp_validate_run_id "${kp_run_id}"
kp_root="$(kp_repo_root)"
kp_version="0.1.0"
kp_revision="local-${kp_run_id}"
kp_build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

for kp_component in api worker upgrader; do
  docker build \
    --file "${kp_root}/build/package/${kp_component}.Dockerfile" \
    --build-arg "VERSION=${kp_version}" \
    --build-arg "REVISION=${kp_revision}" \
    --build-arg "BUILD_DATE=${kp_build_date}" \
    --tag "kuberploy-${kp_component}:e2e-${kp_run_id}" \
    "${kp_root}"
done

docker build \
  --file "${kp_root}/web/Dockerfile" \
  --tag "kuberploy-web:e2e-${kp_run_id}" \
  "${kp_root}/web"

printf 'Built four local control-plane images for run %s.\n' "${kp_run_id}"
