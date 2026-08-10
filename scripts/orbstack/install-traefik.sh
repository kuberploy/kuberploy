#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_assert_orbstack_context
kp_require_tools helm jq kubectl
kp_run_id="${1:?usage: install-traefik.sh <run-id>}"
kp_namespace="$(kp_require_run_namespace "${kp_run_id}")"
kp_root="$(kp_repo_root)"
kp_release="$(kp_release_name traefik "${kp_run_id}")"
kp_ingress_class="kp-e2e-${kp_run_id}"

readonly KP_TRAEFIK_REPOSITORY="https://traefik.github.io/charts"
readonly KP_TRAEFIK_CHART_VERSION="41.1.1"

kp_preexisting="false"
if kubectl get crd middlewares.traefik.io >/dev/null 2>&1; then
  kp_preexisting="true"
  kubectl get crd middlewares.traefik.io -o json | \
    jq -e '.spec.versions[] | select(.name == "v1alpha1" and .served == true)' >/dev/null || \
    kp_die "pre-existing Traefik Middleware CRD does not serve v1alpha1"
fi
kubectl create configmap traefik-crd-inventory -n "${kp_namespace}" \
  --from-literal="middlewares.traefik.io.preexisting=${kp_preexisting}" \
  --from-literal="lockedChartVersion=${KP_TRAEFIK_CHART_VERSION}" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl label configmap traefik-crd-inventory -n "${kp_namespace}" --overwrite \
  "kuberploy.io/test-run=${kp_run_id}"

helm upgrade --install "${kp_release}" traefik \
  --repo "${KP_TRAEFIK_REPOSITORY}" \
  --version "${KP_TRAEFIK_CHART_VERSION}" \
  --namespace "${kp_namespace}" \
  --values "${kp_root}/deploy/orbstack/traefik-values.yaml" \
  --set-string "commonLabels.kuberploy\.io/test-run=${kp_run_id}" \
  --set-string "ingressClass.name=${kp_ingress_class}" \
  --set-string "providers.kubernetesCRD.namespaces[0]=${kp_namespace}" \
  --set-string "providers.kubernetesIngress.ingressClass=${kp_ingress_class}" \
  --set-string "providers.kubernetesIngress.namespaces[0]=${kp_namespace}" \
  --wait --timeout 10m

printf 'Installed locked Traefik chart %s with IngressClass %s.\n' \
  "${KP_TRAEFIK_CHART_VERSION}" "${kp_ingress_class}"
