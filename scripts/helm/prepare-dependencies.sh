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

helm dependency build --skip-refresh "${kp_root}/charts/kuberploy-installer"
