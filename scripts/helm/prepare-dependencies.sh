#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "${kp_root}/scripts/helm/download-locked-artifact.sh"
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
  [[ "$(shasum -a 256 "${kp_destination}" | awk '{print $1}')" == "${kp_checksum}" ]]
  helm dependency list "${kp_source}" | rg -F 'ok' >/dev/null
done

helm dependency update --skip-refresh "${kp_root}/charts/kuberploy-installer"
