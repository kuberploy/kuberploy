#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

python3 -m json.tool "${kp_root}/release/release-manifest.schema.json" >/dev/null
kp_python_files=(
  "${kp_root}/release/check_public_source.py"
  "${kp_root}/release/chart_oci_digest.py"
  "${kp_root}/release/generate_manifest.py"
  "${kp_root}/release/package_chart.py"
  "${kp_root}/release/package_chart_archive.py"
  "${kp_root}/release/package_component_charts.py"
  "${kp_root}/release/test_validate_source.py"
  "${kp_root}/release/validate_semantics.py"
  "${kp_root}/release/validate_source.py"
)
python3 -m py_compile "${kp_python_files[@]}"
python3 "${kp_root}/release/check_public_source.py" --root "${kp_root}"
python3 "${kp_root}/release/validate_source.py" --root "${kp_root}" >/dev/null
python3 "${kp_root}/release/test_validate_source.py"
(
  cd "${kp_root}/release/tools"
  go test ./...
  go vet ./...
)
"${kp_root}/release/test-manifest.sh"
"${kp_root}/release/test-chart-lifecycle.sh"
"${kp_root}/test/e2e/render-argocd-chart.sh"
"${kp_root}/test/e2e/render-postgresql-chart.sh"
"${kp_root}/test/e2e/render-secret-controller-charts.sh"
"${kp_root}/test/e2e/render-valkey-chart.sh"

printf 'all local release checks passed; nothing was published\n'
