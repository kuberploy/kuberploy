#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-source-build-cache-hit.XXXXXX")"
trap 'rm -rf -- "${kp_tmp}"' EXIT

source "${kp_root}/scripts/kubernetes/test/e2e/source-build-extended-workflow.sh"

kp_die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

KUBERPLOY_E2E_API_AUTH_HEADER_FILE="${kp_tmp}/auth-header"
kp_scenario="${kp_tmp}/scenario.json"
printf 'Authorization: Bearer test-only\n' >"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}"
printf '{"apiBaseURL":"https://kuberploy.test"}\n' >"${kp_scenario}"

curl() {
  local kp_output=""
  while (($# > 0)); do
    case "$1" in
      --output)
        kp_output="${2:?curl output path required}"
        shift 2
        ;;
      *) shift ;;
    esac
  done
  [[ -n "${kp_output}" ]]
  printf '{"source":{"ready":true},"lines":[{"message":"safe lifecycle event"}]}\n' >"${kp_output}"
  printf '200'
}

kp_terminal="${kp_tmp}/terminal.json"
kp_logs="${kp_tmp}/logs.json"
printf '%s\n' '{"state":"succeeded","cacheReuse":"hit","cacheReference":"registry.test/cache:generation-2","warnings":[]}' >"${kp_terminal}"
kp_assert_second_build_cache_hit "00000000-0000-0000-0000-000000000001" "${kp_terminal}" "${kp_logs}"

printf '%s\n' '{"state":"succeeded","cacheReuse":"miss","cacheReference":"registry.test/cache:generation-2","warnings":[]}' >"${kp_terminal}"
if (kp_assert_second_build_cache_hit "00000000-0000-0000-0000-000000000002" "${kp_terminal}" "${kp_logs}") >/dev/null 2>&1; then
  printf 'error: non-hit cache metadata was accepted\n' >&2
  exit 1
fi

printf 'source-build cache-hit qualification tests passed\n'
