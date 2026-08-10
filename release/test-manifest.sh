#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-release-manifest.XXXXXX")"
trap 'rm -rf -- "${kp_tmp}"' EXIT

kp_version="$(python3 "${kp_root}/release/validate_source.py" --root "${kp_root}")"
kp_tag="v${kp_version}"
kp_commit="$(printf 'a%.0s' {1..40})"
kp_assets="${kp_tmp}/assets"
mkdir -p "${kp_assets}"

kp_api_digest="sha256:$(printf '1%.0s' {1..64})"
kp_worker_digest="sha256:$(printf '2%.0s' {1..64})"
kp_web_digest="sha256:$(printf '3%.0s' {1..64})"
kp_upgrader_digest="sha256:$(printf '4%.0s' {1..64})"
kp_builder_agent_digest="sha256:$(printf '5%.0s' {1..64})"
kp_chart_digest="sha256:$(printf '6%.0s' {1..64})"
kp_summary="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["summary"])' "${kp_root}/release/metadata.json")"
kp_upgrade_range="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["supportedUpgradeFrom"])' "${kp_root}/release/metadata.json")"
kp_breaking="$(python3 -c 'import json,sys; print(str(json.load(open(sys.argv[1], encoding="utf-8"))["breakingChanges"]).lower())' "${kp_root}/release/metadata.json")"

kp_package_args=(
  --source "${kp_root}/charts/kuberploy"
  --builder-chart "${kp_root}/charts/kuberploy-builder"
  --destination "${kp_tmp}/chart"
  --version "${kp_version}"
  --api-image "ghcr.io/kuberploy/kuberploy-api@${kp_api_digest}"
  --worker-image "ghcr.io/kuberploy/kuberploy-worker@${kp_worker_digest}"
  --web-image "ghcr.io/kuberploy/kuberploy-web@${kp_web_digest}"
  --upgrader-image "ghcr.io/kuberploy/kuberploy-upgrader@${kp_upgrader_digest}"
  --builder-agent-image "ghcr.io/kuberploy/kuberploy-builder-agent@${kp_builder_agent_digest}"
)
python3 "${kp_root}/release/package_chart.py" "${kp_package_args[@]}" >/dev/null
kp_chart_package="${kp_assets}/kuberploy-${kp_version}.tgz"
kp_source_date_epoch="1785974400"
python3 "${kp_root}/release/package_chart_archive.py" \
  --source "${kp_tmp}/chart" \
  --output "${kp_chart_package}" \
  --source-date-epoch "${kp_source_date_epoch}" >/dev/null
python3 "${kp_root}/release/package_chart_archive.py" \
  --source "${kp_tmp}/chart" \
  --output "${kp_tmp}/reproducible-chart.tgz" \
  --source-date-epoch "${kp_source_date_epoch}" >/dev/null
cmp --silent "${kp_chart_package}" "${kp_tmp}/reproducible-chart.tgz" || {
  printf 'release chart packaging is not byte reproducible\n' >&2
  exit 1
}
kp_archive_listing="$(tar -tzf "${kp_chart_package}")"
grep -qx "kuberploy/charts/kuberploy-builder/Chart.yaml" <<<"${kp_archive_listing}"
grep -qx "kuberploy/charts/kuberploy-builder/values.schema.json" <<<"${kp_archive_listing}"
if grep -q '/testdata/' <<<"${kp_archive_listing}"; then
  printf 'release chart contains builder test fixtures\n' >&2
  exit 1
fi
kp_chart_digest="$(python3 "${kp_root}/release/chart_oci_digest.py" \
  --chart "${kp_tmp}/chart/Chart.yaml" \
  --package "${kp_chart_package}" \
  --created-at "2026-08-06T00:00:00Z")"
[[ "${kp_chart_digest}" =~ ^sha256:[a-f0-9]{64}$ ]]

python3 "${kp_root}/release/package_component_charts.py" \
  --root "${kp_root}" \
  --destination "${kp_tmp}/component-charts" \
  --version "${kp_version}" \
  --source-date-epoch "${kp_source_date_epoch}" \
  --builder-agent-image "ghcr.io/kuberploy/kuberploy-builder-agent@${kp_builder_agent_digest}" >/dev/null
python3 "${kp_root}/release/package_component_charts.py" \
  --root "${kp_root}" \
  --destination "${kp_tmp}/component-charts-again" \
  --version "${kp_version}" \
  --source-date-epoch "${kp_source_date_epoch}" \
  --builder-agent-image "ghcr.io/kuberploy/kuberploy-builder-agent@${kp_builder_agent_digest}" >/dev/null
diff -qr "${kp_tmp}/component-charts" "${kp_tmp}/component-charts-again" >/dev/null || {
  printf 'component chart staging is not reproducible\n' >&2
  exit 1
}
helm dependency list "${kp_tmp}/component-charts/kuberploy-installer" | grep -Eq '^kuberploy-argocd[[:space:]]+[^[:space:]]+[[:space:]]+[^[:space:]]+[[:space:]]+ok[[:space:]]*$'
cmp --silent \
  "${kp_tmp}/component-charts/kuberploy-installer/charts/kuberploy-argocd-${kp_version}.tgz" \
  "${kp_tmp}/component-charts-again/kuberploy-installer/charts/kuberploy-argocd-${kp_version}.tgz"
kp_component_chart_args=()
kp_component_packages=()
for kp_name in \
  kuberploy-argocd \
  kuberploy-installer \
  kuberploy-builder \
  kuberploy-cert-manager \
  kuberploy-edge \
  kuberploy-external-dns \
  kuberploy-external-secrets \
  kuberploy-monitoring \
  kuberploy-postgresql \
  kuberploy-registry \
  kuberploy-runtime \
  kuberploy-sealed-secrets \
  kuberploy-valkey; do
  kp_component_package="${kp_assets}/${kp_name}-${kp_version}.tgz"
  python3 "${kp_root}/release/package_chart_archive.py" \
    --source "${kp_tmp}/component-charts/${kp_name}" \
    --output "${kp_component_package}" \
    --source-date-epoch "${kp_source_date_epoch}" >/dev/null
  python3 "${kp_root}/release/package_chart_archive.py" \
    --source "${kp_tmp}/component-charts/${kp_name}" \
    --output "${kp_tmp}/${kp_name}-reproducible.tgz" \
    --source-date-epoch "${kp_source_date_epoch}" >/dev/null
  cmp --silent "${kp_component_package}" "${kp_tmp}/${kp_name}-reproducible.tgz" || {
    printf '%s release chart packaging is not byte reproducible\n' "${kp_name}" >&2
    exit 1
  }
  kp_component_digest="$(python3 "${kp_root}/release/chart_oci_digest.py" \
    --chart "${kp_tmp}/component-charts/${kp_name}/Chart.yaml" \
    --package "${kp_component_package}" \
    --created-at "2026-08-06T00:00:00Z")"
  [[ "${kp_component_digest}" =~ ^sha256:[a-f0-9]{64}$ ]]
  kp_component_chart_args+=(--component-chart "${kp_name}" "${kp_component_package}" "${kp_component_digest}")
  kp_component_packages+=("${kp_component_package}")
done

kp_manifest_args=(
  --root "${kp_root}"
  --tag "${kp_tag}"
  --commit "${kp_commit}"
  --created-at "2026-08-06T00:00:00Z"
  --notes-url "https://github.com/kuberploy/kuberploy/releases/tag/${kp_tag}"
  --summary "${kp_summary}"
  --supported-upgrade-from "${kp_upgrade_range}"
  --kubernetes-constraint ">=1.34.0-0 <1.37.0-0"
  --api-reference ghcr.io/kuberploy/kuberploy-api
  --api-digest "${kp_api_digest}"
  --worker-reference ghcr.io/kuberploy/kuberploy-worker
  --worker-digest "${kp_worker_digest}"
  --web-reference ghcr.io/kuberploy/kuberploy-web
  --web-digest "${kp_web_digest}"
  --upgrader-reference ghcr.io/kuberploy/kuberploy-upgrader
  --upgrader-digest "${kp_upgrader_digest}"
  --builder-agent-reference ghcr.io/kuberploy/kuberploy-builder-agent
  --builder-agent-digest "${kp_builder_agent_digest}"
  --chart-oci-reference "ghcr.io/kuberploy/charts/kuberploy:${kp_version}"
  --chart-oci-digest "${kp_chart_digest}"
  --chart-package "${kp_chart_package}"
  "${kp_component_chart_args[@]}"
  --output "${kp_assets}/release-manifest.json"
)
if [[ "${kp_breaking}" == "true" ]]; then
  kp_manifest_args+=(--breaking-changes)
fi
python3 "${kp_root}/release/generate_manifest.py" "${kp_manifest_args[@]}"

"${kp_root}/release/validate-manifest.sh" "${kp_assets}/release-manifest.json" "${kp_assets}" "${kp_tag}" "${kp_commit}"
"${kp_root}/release/create-checksums.sh" \
  "${kp_assets}/SHA256SUMS" \
  "${kp_assets}/release-manifest.json" \
  "${kp_chart_package}" \
  "${kp_component_packages[@]}"
(
  cd "${kp_assets}"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum --check SHA256SUMS >/dev/null
  else
    shasum -a 256 --check SHA256SUMS >/dev/null
  fi
)

cp "${kp_assets}/release-manifest.json" "${kp_assets}/invalid-manifest.json"
python3 - "${kp_assets}/invalid-manifest.json" <<'PY'
import json
import sys
from pathlib import Path

path = Path(sys.argv[1])
value = json.loads(path.read_text(encoding="utf-8"))
value["artifacts"]["images"][0]["digest"] = "sha256:short"
path.write_text(json.dumps(value), encoding="utf-8")
PY
if (
  cd "${kp_root}/release/tools"
  go run ./cmd/validate-manifest "${kp_root}/release/release-manifest.schema.json" "${kp_assets}/invalid-manifest.json" >/dev/null 2>&1
); then
  printf 'JSON Schema validator accepted a malformed digest\n' >&2
  exit 1
fi

cp "${kp_assets}/release-manifest.json" "${kp_assets}/builder-mismatch-manifest.json"
python3 - "${kp_assets}/builder-mismatch-manifest.json" <<'PY'
import json
import sys
from pathlib import Path

path = Path(sys.argv[1])
value = json.loads(path.read_text(encoding="utf-8"))
builder = next(image for image in value["artifacts"]["images"] if image["component"] == "builder-agent")
builder["digest"] = "sha256:" + ("9" * 64)
path.write_text(json.dumps(value), encoding="utf-8")
PY
if "${kp_root}/release/validate-manifest.sh" \
  "${kp_assets}/builder-mismatch-manifest.json" "${kp_assets}" "${kp_tag}" "${kp_commit}" >/dev/null 2>&1; then
  printf 'release validator accepted a builder digest that differs from the embedded chart\n' >&2
  exit 1
fi

kp_mutable_args=(
  --source "${kp_root}/charts/kuberploy"
  --builder-chart "${kp_root}/charts/kuberploy-builder"
  --destination "${kp_tmp}/mutable-chart"
  --version "${kp_version}"
  --api-image ghcr.io/kuberploy/kuberploy-api:latest
  --worker-image "ghcr.io/kuberploy/kuberploy-worker@${kp_worker_digest}"
  --web-image "ghcr.io/kuberploy/kuberploy-web@${kp_web_digest}"
  --upgrader-image "ghcr.io/kuberploy/kuberploy-upgrader@${kp_upgrader_digest}"
  --builder-agent-image "ghcr.io/kuberploy/kuberploy-builder-agent@${kp_builder_agent_digest}"
)
if python3 "${kp_root}/release/package_chart.py" "${kp_mutable_args[@]}" >/dev/null 2>&1; then
  printf 'release chart packager accepted a mutable image\n' >&2
  exit 1
fi

kp_mutable_builder_args=(
  --source "${kp_root}/charts/kuberploy"
  --builder-chart "${kp_root}/charts/kuberploy-builder"
  --destination "${kp_tmp}/mutable-builder-chart"
  --version "${kp_version}"
  --api-image "ghcr.io/kuberploy/kuberploy-api@${kp_api_digest}"
  --worker-image "ghcr.io/kuberploy/kuberploy-worker@${kp_worker_digest}"
  --web-image "ghcr.io/kuberploy/kuberploy-web@${kp_web_digest}"
  --upgrader-image "ghcr.io/kuberploy/kuberploy-upgrader@${kp_upgrader_digest}"
  --builder-agent-image ghcr.io/kuberploy/kuberploy-builder-agent:latest
)
if python3 "${kp_root}/release/package_chart.py" "${kp_mutable_builder_args[@]}" >/dev/null 2>&1; then
  printf 'release chart packager accepted a mutable builder-agent image\n' >&2
  exit 1
fi

cp -R "${kp_root}/charts/kuberploy" "${kp_tmp}/enabled-source-chart"
python3 - "${kp_tmp}/enabled-source-chart/values.yaml" <<'PY'
import sys
from pathlib import Path

path = Path(sys.argv[1])
text = path.read_text(encoding="utf-8")
old = "builder:\n  enabled: false\n"
if text.count(old) != 1:
    raise SystemExit("could not locate builder default")
path.write_text(text.replace(old, "builder:\n  enabled: true\n"), encoding="utf-8")
PY
kp_enabled_source_args=(
  --source "${kp_tmp}/enabled-source-chart"
  --builder-chart "${kp_root}/charts/kuberploy-builder"
  --destination "${kp_tmp}/unexpected-enabled-chart"
  --version "${kp_version}"
  --api-image "ghcr.io/kuberploy/kuberploy-api@${kp_api_digest}"
  --worker-image "ghcr.io/kuberploy/kuberploy-worker@${kp_worker_digest}"
  --web-image "ghcr.io/kuberploy/kuberploy-web@${kp_web_digest}"
  --upgrader-image "ghcr.io/kuberploy/kuberploy-upgrader@${kp_upgrader_digest}"
  --builder-agent-image "ghcr.io/kuberploy/kuberploy-builder-agent@${kp_builder_agent_digest}"
)
if python3 "${kp_root}/release/package_chart.py" "${kp_enabled_source_args[@]}" >/dev/null 2>&1; then
  printf 'release chart packager accepted a privileged builder enabled by default\n' >&2
  exit 1
fi

printf 'release manifest dry-run validation passed\n'
