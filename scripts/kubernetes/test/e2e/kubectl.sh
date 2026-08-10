#!/usr/bin/env bash

set -Eeuo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)/lib.sh"
kp_validate_inputs
kp_assert_cluster_identity

if [[ "${1:-}" == "config" && "${2:-}" == "use-context" ]]; then
  kp_die "qualification commands may not change the ambient kubectl context"
fi
if [[ "${1:-}" == "delete" ]]; then
  for kp_argument in "$@"; do
    case "${kp_argument}" in
      --all|--all=*|-l|--selector|--selector=*|*'*'*)
        kp_die "qualification cleanup requires exact inventoried identities"
        ;;
    esac
  done
fi
exec kubectl --kubeconfig "${KUBECONFIG}" \
  --context "${KUBERPLOY_TEST_CONTEXT}" "$@"
