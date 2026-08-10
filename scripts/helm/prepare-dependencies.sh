#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
kp_charts=(
  kuberploy-argocd
  kuberploy-cert-manager
  kuberploy-edge
  kuberploy-external-dns
  kuberploy-external-secrets
  kuberploy-monitoring
  kuberploy-sealed-secrets
  kuberploy-valkey
  kuberploy-installer
)

for kp_chart in "${kp_charts[@]}"; do
  helm dependency build "${kp_root}/charts/${kp_chart}"
done
