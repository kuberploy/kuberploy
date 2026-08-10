#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_initialize
kp_require_tools grep

kp_version_json="$(kp_kubectl version -o json)"
kp_server_version="$(jq -r '.serverVersion.gitVersion // ""' \
  <<<"${kp_version_json}")"
[[ "${kp_server_version}" =~ ^v1\.(34|35|36)\.[0-9]+([+.-][0-9A-Za-z.-]+)*$ ]] || \
  kp_die "Kubernetes server is outside the locked 1.34-1.36 support window"

kp_api_versions="$(kp_kubectl api-versions)"
for kp_api in \
  "apps/v1" \
  "networking.k8s.io/v1" \
  "admissionregistration.k8s.io/v1" \
  "storage.k8s.io/v1"; do
  grep -Fx "${kp_api}" <<<"${kp_api_versions}" >/dev/null || \
    kp_die "required Kubernetes API is unavailable: ${kp_api}"
done

kp_nodes_json="$(kp_kubectl get nodes -o json)"
kp_node_count="$(jq -r '.items | length' <<<"${kp_nodes_json}")"
(( kp_node_count > 0 )) || kp_die "the selected cluster has no nodes"

kp_unsupported_platform_count="$(jq -r \
  '[.items[] | select(.status.nodeInfo.operatingSystem != "linux" or (.status.nodeInfo.architecture != "amd64" and .status.nodeInfo.architecture != "arm64"))] | length' \
  <<<"${kp_nodes_json}")"
(( kp_unsupported_platform_count == 0 )) || \
  kp_die "selected nodes must use a supported linux/amd64 or linux/arm64 platform"

kp_not_ready_count="$(jq -r \
  '[.items[] | select(([.status.conditions[]? | select(.type == "Ready") | .status][0] // "False") != "True")] | length' \
  <<<"${kp_nodes_json}")"
(( kp_not_ready_count == 0 )) || \
  kp_die "one or more selected nodes are not Ready"

kp_platforms="$(jq -r \
  '[.items[] | "\(.status.nodeInfo.operatingSystem)/\(.status.nodeInfo.architecture)"] | group_by(.) | map("\(.[0])=\(length)") | join(",")' \
  <<<"${kp_nodes_json}")"

kp_storage_json="$(kp_kubectl get storageclasses.storage.k8s.io -o json)"
kp_default_storage_count="$(jq -r \
  '[.items[] | select(.metadata.annotations["storageclass.kubernetes.io/is-default-class"] == "true" or .metadata.annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true")] | length' \
  <<<"${kp_storage_json}")"
(( kp_default_storage_count == 1 )) || \
  kp_die "the selected cluster must expose exactly one default StorageClass"

kp_api_resources="$(kp_kubectl api-resources -o name)"
kp_workloads_json="$(kp_kubectl get deployments.apps,statefulsets.apps,daemonsets.apps,services \
  --all-namespaces -o json)"

kp_inventory_matches() {
  local kp_expression="${1:?jq expression required}"
  jq -e "[.items[] | select(${kp_expression})] | length > 0" \
    <<<"${kp_workloads_json}" >/dev/null
}

kp_traefik_status="absent"
kp_ingress_json="$(kp_kubectl get ingressclasses.networking.k8s.io -o json)"
if jq -e '[.items[] | select((.spec.controller // "") | test("traefik"; "i"))] | length > 0' \
    <<<"${kp_ingress_json}" >/dev/null || \
   kp_inventory_matches '((.metadata.name // "") | test("traefik"; "i")) or ((.metadata.labels["app.kubernetes.io/name"] // "") | test("^traefik$"; "i")) or (any((.spec.template.spec.containers // [])[]?; (.image // "") | test("(^|/)traefik([:@]|$)"; "i")))'; then
  kp_traefik_status="present"
fi

kp_argocd_status="absent"
if grep -Fx 'applications.argoproj.io' <<<"${kp_api_resources}" >/dev/null || \
   kp_inventory_matches '((.metadata.name // "") | test("(^|-)argocd($|-)"; "i")) or ((.metadata.labels["app.kubernetes.io/part-of"] // "") | test("^argocd$"; "i"))'; then
  kp_argocd_status="present"
fi

kp_cert_manager_status="absent"
if grep -Fx 'certificates.cert-manager.io' <<<"${kp_api_resources}" >/dev/null || \
   kp_inventory_matches '((.metadata.name // "") | test("cert-manager"; "i")) or ((.metadata.labels["app.kubernetes.io/name"] // "") | test("cert-manager"; "i"))'; then
  kp_cert_manager_status="present"
fi

kp_prometheus_status="absent"
if grep -Fx 'prometheuses.monitoring.coreos.com' <<<"${kp_api_resources}" >/dev/null || \
   kp_inventory_matches '((.metadata.name // "") | test("prometheus"; "i")) or ((.metadata.labels["app.kubernetes.io/name"] // "") | test("prometheus"; "i"))'; then
  kp_prometheus_status="present"
fi

kp_registry_status="absent"
if kp_inventory_matches '((.metadata.name // "") | test("(^|[-_.])(registry|distribution|harbor)([-_.]|$)"; "i")) or ((.metadata.labels["app.kubernetes.io/name"] // "") | test("^(registry|distribution|harbor)$"; "i")) or (any((.spec.template.spec.containers // [])[]?; (.image // "") | test("(^|/)(registry|distribution|harbor)([:@/]|$)"; "i")))'; then
  kp_registry_status="present"
fi

kp_k3s_helmchart_status="absent"
kp_k3s_packaged_count=0
kp_k3s_packaged_traefik_status="absent"
if grep -Fx 'helmcharts.helm.cattle.io' <<<"${kp_api_resources}" >/dev/null; then
  kp_k3s_helmchart_status="present"
  kp_k3s_helmcharts_json="$(kp_kubectl get helmcharts.helm.cattle.io \
    --all-namespaces -o json)"
  kp_k3s_packaged_count="$(jq -r '.items | length' \
    <<<"${kp_k3s_helmcharts_json}")"
  if jq -e '[.items[] | select(.metadata.name == "traefik" or .metadata.name == "traefik-crd")] | length > 0' \
      <<<"${kp_k3s_helmcharts_json}" >/dev/null; then
    kp_k3s_packaged_traefik_status="present"
    kp_traefik_status="present"
  fi
fi

kp_k3s_addon_status="absent"
kp_k3s_addon_count=0
if grep -Fx 'addons.k3s.cattle.io' <<<"${kp_api_resources}" >/dev/null; then
  kp_k3s_addon_status="present"
  kp_k3s_addons_json="$(kp_kubectl get addons.k3s.cattle.io \
    --all-namespaces -o json)"
  kp_k3s_addon_count="$(jq -r '.items | length' <<<"${kp_k3s_addons_json}")"
  if jq -e '[.items[] | select(.metadata.name == "traefik" or .metadata.name == "traefik-crd")] | length > 0' \
      <<<"${kp_k3s_addons_json}" >/dev/null; then
    kp_k3s_packaged_traefik_status="present"
    kp_traefik_status="present"
  fi
fi

printf 'Kubernetes integration preflight passed.\n'
printf 'kubernetes=%s nodes=%s platforms=%s default-storage-class=present\n' \
  "${kp_server_version}" "${kp_node_count}" "${kp_platforms}"
printf 'traefik=%s argocd=%s cert-manager=%s prometheus=%s registry=%s\n' \
  "${kp_traefik_status}" "${kp_argocd_status}" "${kp_cert_manager_status}" \
  "${kp_prometheus_status}" "${kp_registry_status}"
printf 'k3s-helmchart-api=%s k3s-packaged-components=%s k3s-packaged-traefik=%s\n' \
  "${kp_k3s_helmchart_status}" "${kp_k3s_packaged_count}" \
  "${kp_k3s_packaged_traefik_status}"
printf 'k3s-addon-api=%s k3s-addons=%s\n' \
  "${kp_k3s_addon_status}" "${kp_k3s_addon_count}"
