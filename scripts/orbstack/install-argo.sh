#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_assert_orbstack_context
kp_require_tools helm jq kubectl
kp_run_id="${1:?usage: install-argo.sh <run-id>}"
kp_namespace="$(kp_require_run_namespace "${kp_run_id}")"
kp_root="$(kp_repo_root)"
kp_release="$(kp_release_name argocd "${kp_run_id}")"

readonly KP_ARGO_REPOSITORY="https://argoproj.github.io/argo-helm"
readonly KP_ARGO_CHART_VERSION="10.3.0"

kp_preexisting="false"
if kubectl get crd applications.argoproj.io >/dev/null 2>&1; then
  kp_preexisting="true"
  kubectl get crd applications.argoproj.io -o json | \
    jq -e '.spec.versions[] | select(.name == "v1alpha1" and .served == true)' >/dev/null || \
    kp_die "pre-existing Argo Application CRD does not serve v1alpha1"
fi
kubectl create configmap argo-crd-inventory -n "${kp_namespace}" \
  --from-literal="applications.argoproj.io.preexisting=${kp_preexisting}" \
  --from-literal="lockedChartVersion=${KP_ARGO_CHART_VERSION}" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl label configmap argo-crd-inventory -n "${kp_namespace}" --overwrite \
  "kuberploy.io/test-run=${kp_run_id}"

helm upgrade --install "${kp_release}" argo-cd \
  --repo "${KP_ARGO_REPOSITORY}" \
  --version "${KP_ARGO_CHART_VERSION}" \
  --namespace "${kp_namespace}" \
  --values "${kp_root}/deploy/orbstack/argocd-values.yaml" \
  --set-string "global.additionalLabels.kuberploy\.io/test-run=${kp_run_id}" \
  --wait --timeout 10m

kubectl wait --for=condition=Available deployment \
  -l app.kubernetes.io/part-of=argocd \
  -n "${kp_namespace}" --timeout=10m
printf 'Installed locked Argo CD chart %s in %s.\n' "${KP_ARGO_CHART_VERSION}" "${kp_namespace}"
