#!/usr/bin/env bash

set -Eeuo pipefail

kp_manifest="${1:?usage: validate-manifest.sh <manifest> <asset-dir> [expected-tag] [expected-commit]}"
kp_asset_dir="${2:?usage: validate-manifest.sh <manifest> <asset-dir> [expected-tag] [expected-commit]}"
kp_expected_tag="${3:-}"
kp_expected_commit="${4:-}"
kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

kp_manifest="$(cd "$(dirname "${kp_manifest}")" && pwd)/$(basename "${kp_manifest}")"
kp_asset_dir="$(cd "${kp_asset_dir}" && pwd)"

(
  cd "${kp_root}/release/tools"
  go run ./cmd/validate-manifest "${kp_root}/release/release-manifest.schema.json" "${kp_manifest}"
)

kp_semantic_args=(
  "${kp_manifest}"
  --root "${kp_root}"
  --asset-dir "${kp_asset_dir}"
)
if [[ -n "${kp_expected_tag}" ]]; then
  kp_semantic_args+=(--expected-tag "${kp_expected_tag}")
fi
if [[ -n "${kp_expected_commit}" ]]; then
  kp_semantic_args+=(--expected-commit "${kp_expected_commit}")
fi
python3 "${kp_root}/release/validate_semantics.py" "${kp_semantic_args[@]}"
