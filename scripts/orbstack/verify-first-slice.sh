#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_assert_orbstack_context
kp_require_tools curl jq kubectl
kp_run_id="${1:?usage: verify-first-slice.sh <run-id>}"
kp_namespace="$(kp_require_run_namespace "${kp_run_id}")"
kp_selector="kuberploy.io/test-run=${kp_run_id}"
kp_expected_digest="sha256:99c6b4bb4a1e1df3f0b3752168c89358794d02258ebebc26bf21c29399011a85"

kp_deadline=$((SECONDS + 300))
while (( SECONDS < kp_deadline )); do
  kp_apps="$(kubectl get applications.argoproj.io -n "${kp_namespace}" \
    -l "${kp_selector}" -o json 2>/dev/null || true)"
  if [[ -n "${kp_apps}" ]] && jq -e '
      (.items | length) >= 2 and
      all(.items[]; .status.sync.status == "Synced" and .status.health.status == "Healthy")
    ' <<<"${kp_apps}" >/dev/null; then
    break
  fi
  sleep 2
done

[[ -n "${kp_apps:-}" ]] || kp_die "Argo Applications were not created"
jq -e '
  (.items | length) >= 2 and
  all(.items[]; .status.sync.status == "Synced" and .status.health.status == "Healthy")
' <<<"${kp_apps}" >/dev/null || kp_die "Argo Applications did not become Synced and Healthy"

kp_runtime_image="$(kubectl get deployment -n "${kp_namespace}" \
  -l kuberploy.io/application=33333333-3333-4333-8333-333333333333 \
  -o jsonpath='{.items[0].spec.template.spec.containers[0].image}')"
[[ "${kp_runtime_image}" == *@"${kp_expected_digest}" ]] || \
  kp_die "runtime Deployment is not pinned to the expected image digest"

kp_response="$(curl --fail --show-error --silent --max-time 15 \
  http://hello.e2e.k8s.orb.local/hostname)"
[[ "${kp_response}" =~ ^kp-a-[a-z0-9-]+$ ]] || \
  kp_die "Traefik route returned an unexpected payload: ${kp_response}"

printf 'Verified Git -> Argo CD -> digest-pinned Deployment -> Traefik route (%s).\n' \
  "${kp_response}"
