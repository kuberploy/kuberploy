#!/usr/bin/env bash

set -Eeuo pipefail

kp_version=""
kp_kubeconfig=""
kp_context=""
kp_yes=false

kp_usage() {
  cat <<'EOF'
Install Kuberploy from the public OCI installer chart.

Usage:
  scripts/install.sh \
    --version 0.1.0-rc.7 \
    --kubeconfig /absolute/path/to/kubeconfig \
    --context exact-context \
    [--yes]

The installer derives the in-cluster Kubernetes API CIDR and GitHub repository
egress CIDRs. It installs only the minimal managed control plane: Argo CD,
PostgreSQL, Valkey, the Kuberploy API, worker, and web UI. Optional provider
integrations remain disabled until configured by an administrator.
EOF
}

while (($#)); do
  case "$1" in
    --version)
      [[ $# -ge 2 ]] || { printf '%s\n' '--version requires a value' >&2; exit 2; }
      kp_version="$2"
      shift 2
      ;;
    --kubeconfig)
      [[ $# -ge 2 ]] || { printf '%s\n' '--kubeconfig requires a value' >&2; exit 2; }
      kp_kubeconfig="$2"
      shift 2
      ;;
    --context)
      [[ $# -ge 2 ]] || { printf '%s\n' '--context requires a value' >&2; exit 2; }
      kp_context="$2"
      shift 2
      ;;
    --yes)
      kp_yes=true
      shift
      ;;
    --help|-h)
      kp_usage
      exit 0
      ;;
    *)
      printf 'unknown argument: %s\n' "$1" >&2
      kp_usage >&2
      exit 2
      ;;
  esac
done

[[ "${kp_version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$ ]] || {
  printf '%s\n' '--version must be explicit semantic version text without a v prefix' >&2
  exit 2
}
[[ "${kp_kubeconfig}" == /* && -f "${kp_kubeconfig}" ]] || {
  printf '%s\n' '--kubeconfig must name an existing absolute file' >&2
  exit 2
}
[[ -n "${kp_context}" && "${kp_context}" != *$'\n'* && "${kp_context}" != *$'\r'* ]] || {
  printf '%s\n' '--context must be one non-empty line' >&2
  exit 2
}

for kp_tool in curl helm jq kubectl; do
  command -v "${kp_tool}" >/dev/null 2>&1 || {
    printf 'missing required tool: %s\n' "${kp_tool}" >&2
    exit 1
  }
done

[[ "$(helm version --template '{{.Version}}')" == "v4.2.3" ]] || {
  printf '%s\n' 'Kuberploy installation requires Helm 4.2.3' >&2
  exit 1
}

kp_context_exists="$(kubectl --kubeconfig "${kp_kubeconfig}" config get-contexts "${kp_context}" -o name)"
[[ "${kp_context_exists}" == "${kp_context}" ]] || {
  printf 'kubeconfig does not contain exact context %s\n' "${kp_context}" >&2
  exit 1
}

kp_server="$(kubectl --kubeconfig "${kp_kubeconfig}" --context "${kp_context}" config view --minify --raw -o json | jq -er '
  select((.clusters | length) == 1) |
  .clusters[0].cluster.server |
  select(type == "string" and startswith("https://") and (contains(" ") | not))
')"
kp_server_minor="$(kubectl --kubeconfig "${kp_kubeconfig}" --context "${kp_context}" get --raw /version | jq -er '.minor | capture("^(?<minor>[0-9]+)").minor | tonumber')"
((kp_server_minor >= 34 && kp_server_minor <= 36)) || {
  printf 'unsupported Kubernetes server minor version: %s (supported: 1.34-1.36)\n' "${kp_server_minor}" >&2
  exit 1
}

kp_service_ip="$(kubectl --kubeconfig "${kp_kubeconfig}" --context "${kp_context}" \
  --namespace default get service kubernetes -o json | jq -er '.spec.clusterIP | select(. != "" and . != "None")')"
if [[ "${kp_service_ip}" == *:* ]]; then
  kp_service_cidr="${kp_service_ip}/128"
elif [[ "${kp_service_ip}" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; then
  kp_service_cidr="${kp_service_ip}/32"
else
  printf 'could not derive an exact Kubernetes API CIDR from service IP: %s\n' "${kp_service_ip}" >&2
  exit 1
fi

# NetworkPolicy implementations may evaluate Service traffic before or after
# destination NAT. Bind egress to both the stable Service IP and the exact ready
# API endpoint addresses so a blank K3s cluster works without broad node CIDRs.
kp_api_endpoint_cidrs="$(kubectl --kubeconfig "${kp_kubeconfig}" --context "${kp_context}" \
  --namespace default get endpointslice \
  --selector kubernetes.io/service-name=kubernetes -o json | jq -ce '
    [.items[].endpoints[]? |
      select(.conditions.ready != false) |
      .addresses[]? |
      select(type == "string" and length > 0 and (contains(" ") | not)) |
      if contains(":") then . + "/128" else . + "/32" end] |
    unique |
    select(length > 0 and length <= 8)
  ')"
kp_api_cidrs_json="$(jq -cn \
  --arg service "${kp_service_cidr}" \
  --argjson endpoints "${kp_api_endpoint_cidrs}" \
  '([$service] + $endpoints) | unique')"

kp_repository_cidrs="$(curl --fail --silent --show-error --location \
  --proto '=https' --tlsv1.2 https://api.github.com/meta | jq -ce '
    .web |
    select(type == "array" and length > 0 and length <= 64) |
    map(select(type == "string" and . != "0.0.0.0/0" and . != "::/0" and (contains(" ") | not))) |
    unique |
    select(length > 0 and length <= 64)
  ')"

printf 'Kuberploy version: %s\n' "${kp_version}"
printf 'Kubernetes context: %s\n' "${kp_context}"
printf 'Kubernetes API server: %s\n' "${kp_server}"
printf 'Kubernetes API policy CIDRs: %s\n' "$(jq -r 'join(", ")' <<<"${kp_api_cidrs_json}")"
printf 'GitHub repository egress CIDRs: %s entries\n' "$(jq 'length' <<<"${kp_repository_cidrs}")"

if [[ "${kp_yes}" != true ]]; then
  [[ -t 0 ]] || {
    printf '%s\n' 'refusing a non-interactive install without --yes' >&2
    exit 1
  }
  read -r -p "Install into this cluster? [y/N] " kp_answer
  [[ "${kp_answer}" == "y" || "${kp_answer}" == "Y" ]] || {
    printf '%s\n' 'installation cancelled'
    exit 0
  }
fi

kp_helm_base=(
  upgrade --install kuberploy-installer
  oci://ghcr.io/kuberploy/charts/kuberploy-installer
  --version "${kp_version}"
  --namespace kuberploy-system
  --create-namespace
  --kubeconfig "${kp_kubeconfig}"
  --kube-context "${kp_context}"
  --set bootstrap.valkey.enabled=true
  --set bootstrap.argoCD.enabled=true
  --set bootstrap.argoCD.mode=managed
  --set bootstrap.argoCD.managedPrerequisitesConfirmed=true
  --set-json "argoCD.argoFoundation.networkPolicy.kubeAPIServerCIDRs=${kp_api_cidrs_json}"
  --set-json "argoCD.argoFoundation.networkPolicy.repositoryEgressCIDRs=${kp_repository_cidrs}"
  --wait
  --timeout 20m
)

kp_argo_crds=(
  applications.argoproj.io
  applicationsets.argoproj.io
  appprojects.argoproj.io
)

if ! kubectl --kubeconfig "${kp_kubeconfig}" --context "${kp_context}" \
  get crd "${kp_argo_crds[@]}" >/dev/null 2>&1; then
  printf '%s\n' 'Bootstrapping Argo CD and its CRDs before creating Applications...'
  helm "${kp_helm_base[@]}"
  kubectl --kubeconfig "${kp_kubeconfig}" --context "${kp_context}" \
    wait --for=condition=Established --timeout=5m crd "${kp_argo_crds[@]}"
fi

helm "${kp_helm_base[@]}" \
  --set bootstrap.controlPlaneToken.mode=generated \
  --set-json "bootstrap.controlPlaneToken.kubeAPIServerCIDRs=${kp_api_cidrs_json}" \
  --set-string source.repoURL=https://github.com/kuberploy/kuberploy.git \
  --set-string "source.targetRevision=v${kp_version}" \
  --set components.controlPlane.enabled=true \
  --set components.controlPlane.mode=managed \
  --set-string "components.controlPlane.expectedPackageVersion=${kp_version}" \
  --set components.postgresql.enabled=true \
  --set components.postgresql.mode=managed \
  --set-string "components.postgresql.expectedPackageVersion=${kp_version}" \
  --set-string 'components.postgresql.valueFiles[0]=../../examples/installer/postgresql.yaml' \
  --set components.valkey.enabled=true \
  --set components.valkey.mode=managed \
  --set-string "components.valkey.expectedPackageVersion=${kp_version}"

cat <<EOF

Bootstrap resources were accepted. Argo CD now reconciles the selected child
Applications from v${kp_version}. Wait for both Applications to become Synced
and Healthy:

  kubectl --kubeconfig ${kp_kubeconfig} --context ${kp_context} -n argocd get applications

After kuberploy-control-plane is Healthy, retrieve the one-time bootstrap token:

  kubectl --kubeconfig ${kp_kubeconfig} --context ${kp_context} -n kuberploy-system logs job/kuberploy-bootstrap-token | sed -nE 's/^KUBERPLOY_BOOTSTRAP_TOKEN=(kp_bootstrap_[A-Za-z0-9_-]{43})$/\1/p'

Open the UI locally:

  kubectl --kubeconfig ${kp_kubeconfig} --context ${kp_context} -n kuberploy-system port-forward service/kuberploy-web 18080:8080

Then visit http://127.0.0.1:18080/ and complete the bootstrap form.
EOF
