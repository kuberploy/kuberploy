#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_assert_orbstack_context
kp_run_id="${1:?usage: status.sh <run-id>}"
kp_namespace="$(kp_require_run_namespace "${kp_run_id}")"

kubectl get applications.argoproj.io,applicationsets.argoproj.io -n "${kp_namespace}" \
  -l "kuberploy.io/test-run=${kp_run_id}" --ignore-not-found
kubectl get deployment,service,ingress,pod -n "${kp_namespace}" \
  -l "kuberploy.io/test-run=${kp_run_id}" --ignore-not-found
kubectl get deployment,service,ingress,pod -n "${kp_namespace}" \
  -l "kuberploy.io/application=33333333-3333-4333-8333-333333333333" --ignore-not-found

printf 'Expected local route: http://hello.e2e.k8s.orb.local\n'
