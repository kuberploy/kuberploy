#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "${kp_root}/scripts/helm/download-locked-artifact.sh"

kp_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}
kp_charts=(
  kuberploy-argocd
  kuberploy-cert-manager
  kuberploy-edge
  kuberploy-external-dns
  kuberploy-external-secrets
  kuberploy-monitoring
  kuberploy-sealed-secrets
  kuberploy-valkey
)

for kp_chart in "${kp_charts[@]}"; do
  kp_source="${kp_root}/charts/${kp_chart}"
  read -r kp_checksum kp_filename kp_url < <(awk 'NF && $1 !~ /^#/ {print}' "${kp_source}/testdata/upstream-artifacts.lock")
  kp_destination="${kp_source}/charts/${kp_filename}"
  kp_download_locked_artifact "${kp_url}" "${kp_filename}" "${kp_destination}"
  [[ "$(kp_sha256 "${kp_destination}")" == "${kp_checksum}" ]]
  helm dependency list "${kp_source}" | grep -F 'ok' >/dev/null
done

kp_installer_root="${kp_root}/charts/kuberploy-installer"
[[ -d "${kp_installer_root}" && ! -L "${kp_installer_root}" ]]
kp_installer_tmp="$(mktemp -d "${kp_installer_root}/.installer-dependencies.XXXXXX")"
kp_cleanup_installer_tmp() {
  [[ -n "${kp_installer_tmp:-}" &&
     "$(dirname "${kp_installer_tmp}")" == "${kp_installer_root}" &&
     "$(basename "${kp_installer_tmp}")" == .installer-dependencies.* ]] &&
    rm -rf -- "${kp_installer_tmp}"
}
trap kp_cleanup_installer_tmp EXIT

python3 "${kp_root}/scripts/helm/package-installer-dependencies.py" \
  --root "${kp_root}" \
  --destination "${kp_installer_tmp}/new" \
  --lock "${kp_root}/charts/kuberploy-installer/dependencies.lock" \
  --source-date-epoch-file "${kp_root}/charts/kuberploy-installer/dependencies.source-date-epoch"

python3 "${kp_root}/scripts/helm/replace-installer-dependencies.py" \
  --installer-root "${kp_installer_root}" \
  --staged-charts "${kp_installer_tmp}/new"
