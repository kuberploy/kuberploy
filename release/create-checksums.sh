#!/usr/bin/env bash

set -Eeuo pipefail

kp_output="${1:?usage: create-checksums.sh <output> <asset>...}"
shift
(( "$#" > 0 )) || {
  printf 'at least one asset is required\n' >&2
  exit 64
}

kp_output_dir="$(cd "$(dirname "${kp_output}")" && pwd)"
kp_output="${kp_output_dir}/$(basename "${kp_output}")"
: >"${kp_output}"

for kp_asset in "$@"; do
  [[ -f "${kp_asset}" ]] || {
    printf 'asset is not a file: %s\n' "${kp_asset}" >&2
    exit 66
  }
  kp_name="$(basename "${kp_asset}")"
  [[ "${kp_name}" != *$'\n'* ]] || {
    printf 'asset filename contains a newline\n' >&2
    exit 65
  }
  if command -v sha256sum >/dev/null 2>&1; then
    kp_digest="$(sha256sum "${kp_asset}" | awk '{print $1}')"
  else
    kp_digest="$(shasum -a 256 "${kp_asset}" | awk '{print $1}')"
  fi
  printf '%s  %s\n' "${kp_digest}" "${kp_name}" >>"${kp_output}"
done
