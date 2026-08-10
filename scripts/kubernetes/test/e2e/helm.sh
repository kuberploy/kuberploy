#!/usr/bin/env bash

set -Eeuo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)/lib.sh"
kp_validate_inputs
kp_assert_cluster_identity
exec helm --kubeconfig "${KUBECONFIG}" \
  --kube-context "${KUBERPLOY_TEST_CONTEXT}" "$@"
