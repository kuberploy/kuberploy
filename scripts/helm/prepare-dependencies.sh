#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
kp_repositories=(
  "argo|https://argoproj.github.io/argo-helm"
  "jetstack|https://charts.jetstack.io"
  "traefik|https://traefik.github.io/charts"
  "external-dns|https://kubernetes-sigs.github.io/external-dns"
  "external-secrets|https://charts.external-secrets.io"
  "prometheus-community|https://prometheus-community.github.io/helm-charts"
  "sealed-secrets|https://bitnami.github.io/sealed-secrets"
  "valkey|https://valkey.io/valkey-helm/"
)
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

for kp_repository in "${kp_repositories[@]}"; do
  kp_name="${kp_repository%%|*}"
  kp_url="${kp_repository#*|}"
  helm repo add "${kp_name}" "${kp_url}" --force-update >/dev/null
done
helm repo update >/dev/null

for kp_chart in "${kp_charts[@]}"; do
  helm dependency build --skip-refresh "${kp_root}/charts/${kp_chart}"
done
