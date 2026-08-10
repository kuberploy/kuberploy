#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-edge-render.XXXXXX")"

kp_cleanup() {
  if [[ -n "${kp_tmp:-}" && "${kp_tmp}" == *"/kuberploy-edge-render."* ]]; then
    rm -rf -- "${kp_tmp}"
  fi
}
trap kp_cleanup EXIT

for kp_tool in curl diff helm python3 rg shasum yq; do
  command -v "${kp_tool}" >/dev/null 2>&1 || {
    printf 'missing tool: %s\n' "${kp_tool}" >&2
    exit 1
  }
done

kp_stage_chart() {
  local kp_name="$1"
  local kp_source="${kp_root}/charts/${kp_name}"
  local kp_target="${kp_tmp}/${kp_name}"
  local kp_lock="${kp_source}/testdata/upstream-artifacts.lock"
  local kp_checksum kp_filename kp_url kp_actual kp_lines

  cp -R "${kp_source}" "${kp_target}"
  mkdir -p "${kp_target}/charts"
  kp_lines="$(awk 'NF && $1 !~ /^#/ {count++} END {print count+0}' "${kp_lock}")"
  [[ "${kp_lines}" == "1" ]] || {
    printf '%s must lock exactly one independently owned upstream chart\n' "${kp_name}" >&2
    exit 1
  }
  while read -r kp_checksum kp_filename kp_url; do
    [[ -n "${kp_checksum}" && -n "${kp_filename}" && "${kp_url}" == https://* ]] || {
      printf 'malformed upstream lock for %s\n' "${kp_name}" >&2
      exit 1
    }
    rg -F "${kp_checksum}" "${kp_root}/DEPENDENCIES.md" >/dev/null || {
      printf '%s checksum is not present in DEPENDENCIES.md\n' "${kp_name}" >&2
      exit 1
    }
    curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
      "${kp_url}" -o "${kp_target}/charts/${kp_filename}"
    kp_actual="$(shasum -a 256 "${kp_target}/charts/${kp_filename}" | awk '{print $1}')"
    [[ "${kp_actual}" == "${kp_checksum}" ]] || {
      printf '%s checksum mismatch: expected %s, got %s\n' "${kp_filename}" "${kp_checksum}" "${kp_actual}" >&2
      exit 1
    }
  done <"${kp_lock}"
  helm dependency list "${kp_target}" | rg -F 'ok' >/dev/null
}

kp_expect_reject() {
  local kp_reason="$1"
  local kp_chart="$2"
  local kp_namespace="$3"
  local kp_values="$4"
  shift 4
  if helm template invalid "${kp_chart}" --namespace "${kp_namespace}" -f "${kp_values}" "$@" \
      >"${kp_tmp}/rejected.stdout" 2>"${kp_tmp}/rejected.stderr"; then
    printf 'unsafe render was accepted: %s\n' "${kp_reason}" >&2
    exit 1
  fi
}

kp_count_kind() {
  local kp_kind="$1"
  local kp_render="$2"
  yq eval-all "[select(.kind == \"${kp_kind}\")] | length" "${kp_render}" | tail -1
}

kp_edge="${kp_tmp}/kuberploy-edge"
kp_cert="${kp_tmp}/kuberploy-cert-manager"
kp_dns="${kp_tmp}/kuberploy-external-dns"
kp_edge_values="${kp_root}/charts/kuberploy-edge/testdata/managed-values.yaml"
kp_edge_adopted="${kp_root}/charts/kuberploy-edge/testdata/adopted-values.yaml"
kp_cert_values="${kp_root}/charts/kuberploy-cert-manager/testdata/managed-values.yaml"
kp_cert_adopted="${kp_root}/charts/kuberploy-cert-manager/testdata/adopted-values.yaml"
kp_cert_dns01="${kp_root}/charts/kuberploy-cert-manager/testdata/dns01-values.yaml"
kp_dns_values="${kp_root}/charts/kuberploy-external-dns/testdata/managed-values.yaml"
kp_dns_adopted="${kp_root}/charts/kuberploy-external-dns/testdata/adopted-values.yaml"

kp_stage_chart kuberploy-edge
kp_stage_chart kuberploy-cert-manager
kp_stage_chart kuberploy-external-dns

python3 -m json.tool "${kp_edge}/values.schema.json" >/dev/null
python3 -m json.tool "${kp_cert}/values.schema.json" >/dev/null
python3 -m json.tool "${kp_dns}/values.schema.json" >/dev/null

[[ "$(yq '.annotations."kuberploy.io/traefik-chart-sha256"' "${kp_edge}/Chart.yaml")" == '42cf5c2a30a3630adb7cefa1ec5b84dfef0105599cd217c7574bd77c6ad369ee' ]]
[[ "$(yq '.annotations."kuberploy.io/cert-manager-chart-sha256"' "${kp_cert}/Chart.yaml")" == 'c27101f3f3e2349fb4a9e704316105bf7b52ad73b8c8257d3498ef7f2f6a4adc' ]]
[[ "$(yq '.annotations."kuberploy.io/external-dns-chart-sha256"' "${kp_dns}/Chart.yaml")" == '5dd033a4b872bf641860695705ee460031d0bc695f114bf8926fee6736814e19' ]]
[[ "$(yq '.dependencies | length' "${kp_edge}/Chart.yaml")" == "1" ]]
[[ "$(yq '.dependencies | length' "${kp_cert}/Chart.yaml")" == "1" ]]
[[ "$(yq '.dependencies | length' "${kp_dns}/Chart.yaml")" == "1" ]]

helm lint "${kp_edge}" --namespace kuberploy-system -f "${kp_edge_values}"
helm lint "${kp_edge}" --namespace kuberploy-system -f "${kp_edge_adopted}"
helm lint "${kp_cert}" --namespace cert-manager
helm lint "${kp_cert}" --namespace cert-manager -f "${kp_cert_values}"
helm lint "${kp_cert}" --namespace cert-manager -f "${kp_cert_adopted}"
helm lint "${kp_cert}" --namespace cert-manager -f "${kp_cert_dns01}"
helm lint "${kp_dns}" --namespace kuberploy-system
helm lint "${kp_dns}" --namespace kuberploy-system -f "${kp_dns_values}"
helm lint "${kp_dns}" --namespace kuberploy-system -f "${kp_dns_adopted}"

helm template edge "${kp_edge}" --namespace kuberploy-system -f "${kp_edge_values}" >"${kp_tmp}/edge.yaml"
helm template edge "${kp_edge}" --namespace kuberploy-system -f "${kp_edge_values}" >"${kp_tmp}/edge-again.yaml"
diff -u "${kp_tmp}/edge.yaml" "${kp_tmp}/edge-again.yaml"
yq eval-all 'true' "${kp_tmp}/edge.yaml" >/dev/null

kp_traefik_image='docker.io/library/traefik:v3.7.10'
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.containers[0].image' "${kp_tmp}/edge.yaml")" == "${kp_traefik_image}" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.replicas' "${kp_tmp}/edge.yaml")" == "2" ]]
[[ "$(yq eval-all 'select(.kind == "Service" and .spec.type == "LoadBalancer") | .spec.type' "${kp_tmp}/edge.yaml")" == "LoadBalancer" ]]
[[ "$(yq eval-all 'select(.kind == "Service" and .spec.type == "LoadBalancer") | [.spec.ports[].port] | sort | join(",")' "${kp_tmp}/edge.yaml")" == "80,443" ]]
[[ "$(yq eval-all 'select(.kind == "Namespace") | .metadata.labels."pod-security.kubernetes.io/enforce"' "${kp_tmp}/edge.yaml")" == "restricted" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "edge-edge-profile") | .data.httpRoutesSupported' "${kp_tmp}/edge.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "edge-edge-profile") | .data.customTLSSecretRoutesSupported' "${kp_tmp}/edge.yaml")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "edge-edge-profile") | .data.letsEncryptRoutesRequireApprovedIssuer' "${kp_tmp}/edge.yaml")" == "true" ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "ConfigMap" and .metadata.name == "edge-edge-profile") | .data' "${kp_tmp}/edge.yaml" | jq -cS .)" == '{"customTLSSecretRoutesSupported":"true","httpRoutesSupported":"true","ingressClassName":"traefik","letsEncryptRoutesRequireApprovedIssuer":"true","management":"managed","runtimeNamespaceSelector":"kuberploy.io/runtime-namespace=true","sslipMode":"auto-first-ip","sslipStaticPublicIPv4":""}' ]]
[[ "$(kp_count_kind TLSStore "${kp_tmp}/edge.yaml")" == "0" ]]
[[ "$(kp_count_kind Secret "${kp_tmp}/edge.yaml")" == "0" ]]
[[ "$(kp_count_kind Certificate "${kp_tmp}/edge.yaml")" == "0" ]]
[[ "$(kp_count_kind ClusterIssuer "${kp_tmp}/edge.yaml")" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy") | .spec.egress[].to[] | select(.namespaceSelector.matchLabels."kuberploy.io/runtime-namespace" == "true")] | length' "${kp_tmp}/edge.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all -o=json '.' "${kp_tmp}/edge.yaml" | jq -s '[.[] | select(.kind == "NetworkPolicy") | .spec.egress[].to[] | select(.namespaceSelector != null and (.namespaceSelector | length) == 0)] | length')" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy") | .spec.egress[].to[] | select(.ipBlock.cidr == "0.0.0.0/0" or .ipBlock.cidr == "::/0")] | length' "${kp_tmp}/edge.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy") | .spec.egress[].to[].ipBlock.cidr | select(. == "10.43.0.1/32")] | length' "${kp_tmp}/edge.yaml" | tail -1)" == "1" ]]
if rg -n 'hostPort:|kind: TLSStore|cert-manager|external-dns|:latest' "${kp_tmp}/edge.yaml"; then
  printf 'Traefik release rendered cross-owned resources or a forbidden capability\n' >&2
  exit 1
fi

helm template edge-adopted "${kp_edge}" --namespace kuberploy-system -f "${kp_edge_adopted}" >"${kp_tmp}/edge-adopted.yaml"
[[ "$(kp_count_kind Deployment "${kp_tmp}/edge-adopted.yaml")" == "0" ]]
[[ "$(kp_count_kind Namespace "${kp_tmp}/edge-adopted.yaml")" == "0" ]]
[[ "$(kp_count_kind ClusterRole "${kp_tmp}/edge-adopted.yaml")" == "0" ]]
[[ "$(kp_count_kind NetworkPolicy "${kp_tmp}/edge-adopted.yaml")" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.management' "${kp_tmp}/edge-adopted.yaml")" == "adopted" ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "ConfigMap") | .data' "${kp_tmp}/edge-adopted.yaml" | jq -cS .)" == '{"customTLSSecretRoutesSupported":"true","httpRoutesSupported":"true","ingressClassName":"adopted-traefik","letsEncryptRoutesRequireApprovedIssuer":"true","management":"adopted","runtimeNamespaceSelector":"kuberploy.io/runtime-namespace=true"}' ]]

helm template edge-static "${kp_edge}" --namespace kuberploy-system -f "${kp_edge_values}" \
  --set-string edge.traefik.sslip.mode=verified-static-ip \
  --set-string edge.traefik.sslip.staticPublicIPv4=8.8.8.8 >"${kp_tmp}/edge-static.yaml"
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "edge-static-edge-profile") | .data.sslipMode' "${kp_tmp}/edge-static.yaml")" == "verified-static-ip" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "edge-static-edge-profile") | .data.sslipStaticPublicIPv4' "${kp_tmp}/edge-static.yaml")" == "8.8.8.8" ]]

if helm template invalid "${kp_edge}" --namespace kuberploy-system >/dev/null 2>&1; then
  printf 'managed Traefik accepted missing API-server CIDRs\n' >&2
  exit 1
fi
kp_expect_reject 'all-address Traefik API egress' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string 'edge.networkPolicy.kubeAPIServerCIDRs[0]=0.0.0.0/0'
kp_expect_reject 'mutable runtime namespace selector' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string 'edge.networkPolicy.runtimeNamespaceSelector.attacker=value'
kp_expect_reject 'global custom certificate fallback' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string 'traefik.tlsStore.default.defaultCertificate.secretName=global-tls'
kp_expect_reject 'cross-namespace Middleware references' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set traefik.providers.kubernetesCRD.allowCrossNamespace=true
kp_expect_reject 'insecure Traefik dashboard' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set traefik.api.insecure=true
kp_expect_reject 'Traefik identity label bypass' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string traefik.nameOverride=unconfined-traefik
kp_expect_reject 'Traefik pod annotation injection' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string traefik.deployment.podAnnotations.sidecar=enabled
kp_expect_reject 'Traefik cloud identity annotation' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string traefik.serviceAccountAnnotations.cloud=identity
kp_expect_reject 'Traefik namespace escape' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string traefik.namespaceOverride=other-namespace
kp_expect_reject 'additional public Traefik Service' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set traefik.service.additionalServices.admin.enabled=true
kp_expect_reject 'global HTTP-to-HTTPS redirect' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string traefik.ports.web.http.redirections.entryPoint.to=websecure
kp_expect_reject 'public Traefik admin port' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set 'traefik.ports.admin.port=9000' --set 'traefik.ports.admin.expose.default=true'
kp_expect_reject 'access-log header capture' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string traefik.accessLog.fields.headers.defaultMode=keep
kp_expect_reject 'unbounded Traefik file provider' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set traefik.providers.file.enabled=true
kp_expect_reject 'Traefik plugin execution' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string traefik.experimental.plugins.attacker.moduleName=example.invalid/plugin
kp_expect_reject 'floating Traefik image' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string traefik.image.digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
kp_expect_reject 'disabled edge NetworkPolicy' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set edge.networkPolicy.enabled=false
kp_expect_reject 'wrong Traefik namespace' "${kp_edge}" default "${kp_edge_values}"
kp_expect_reject 'unknown sslip selection mode' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string edge.traefik.sslip.mode=caller-ip
kp_expect_reject 'dormant static IP in automatic sslip mode' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string edge.traefik.sslip.staticPublicIPv4=8.8.8.8
kp_expect_reject 'verified sslip mode without its static IP' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string edge.traefik.sslip.mode=verified-static-ip
kp_expect_reject 'private verified sslip address' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string edge.traefik.sslip.mode=verified-static-ip --set-string edge.traefik.sslip.staticPublicIPv4=10.0.0.1
kp_expect_reject 'documentation-range verified sslip address' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string edge.traefik.sslip.mode=verified-static-ip --set-string edge.traefik.sslip.staticPublicIPv4=203.0.113.10
kp_expect_reject 'non-canonical verified sslip address' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string edge.traefik.sslip.mode=verified-static-ip --set-string edge.traefik.sslip.staticPublicIPv4=008.008.008.008
kp_expect_reject 'unknown sslip profile field' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set-string edge.traefik.sslip.callerAddress=8.8.8.8
kp_expect_reject 'null dormant sslip profile' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --set edge.traefik.sslip=null

# The standalone profile stays closed even if values-schema validation is
# explicitly bypassed: the ConfigMap can contain only the exact runtime data.
kp_expect_reject 'schema-bypassed auto static IP' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --skip-schema-validation --set-string edge.traefik.sslip.staticPublicIPv4=8.8.8.8
kp_expect_reject 'schema-bypassed missing static IP' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --skip-schema-validation --set-string edge.traefik.sslip.mode=verified-static-ip
kp_expect_reject 'schema-bypassed private static IP' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --skip-schema-validation --set-string edge.traefik.sslip.mode=verified-static-ip --set-string edge.traefik.sslip.staticPublicIPv4=192.168.1.10
kp_expect_reject 'schema-bypassed unknown field' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --skip-schema-validation --set-string edge.traefik.sslip.callerAddress=8.8.8.8
kp_expect_reject 'schema-bypassed null profile' "${kp_edge}" kuberploy-edge "${kp_edge_values}" --skip-schema-validation --set edge.traefik.sslip=null

helm template cert "${kp_cert}" --namespace cert-manager --include-crds -f "${kp_cert_values}" >"${kp_tmp}/cert.yaml"
helm template cert "${kp_cert}" --namespace cert-manager --include-crds -f "${kp_cert_values}" >"${kp_tmp}/cert-again.yaml"
diff -u "${kp_tmp}/cert.yaml" "${kp_tmp}/cert-again.yaml"
yq eval-all 'true' "${kp_tmp}/cert.yaml" >/dev/null
[[ "$(kp_count_kind Deployment "${kp_tmp}/cert.yaml")" == "3" ]]
[[ "$(kp_count_kind ClusterIssuer "${kp_tmp}/cert.yaml")" == "2" ]]
[[ "$(kp_count_kind CustomResourceDefinition "${kp_tmp}/cert.yaml")" -gt "1" ]]
[[ "$(kp_count_kind TLSStore "${kp_tmp}/cert.yaml")" == "0" ]]
[[ "$(kp_count_kind Secret "${kp_tmp}/cert.yaml")" == "0" ]]
[[ "$(kp_count_kind Certificate "${kp_tmp}/cert.yaml")" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy")] | length' "${kp_tmp}/cert.yaml" | tail -1)" == "3" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and .metadata.name == "cert-controller") | .spec.egress[].to[].ipBlock.cidr | select(. == "0.0.0.0/0")] | length' "${kp_tmp}/cert.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy" and (.metadata.name == "cert-webhook" or .metadata.name == "cert-cainjector")) | .spec.egress[].to[].ipBlock.cidr | select(. == "0.0.0.0/0" or . == "::/0")] | length' "${kp_tmp}/cert.yaml" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "ClusterIssuer") | .spec.acme.server] | sort | join(",")' "${kp_tmp}/cert.yaml" | tail -1)" == 'https://acme-staging-v02.api.letsencrypt.org/directory,https://acme-v02.api.letsencrypt.org/directory' ]]
[[ "$(yq eval-all '[select(.kind == "ClusterIssuer") | .spec.acme.solvers[0].http01.ingress.ingressClassName] | unique | join(",")' "${kp_tmp}/cert.yaml" | tail -1)" == "traefik" ]]
[[ "$(yq eval-all '[select(.kind == "ClusterIssuer") | .spec.acme.solvers[0].http01.ingress.ingressTemplate.metadata.annotations."external-dns.alpha.kubernetes.io/ingress-hostname-source"] | unique | join(",")' "${kp_tmp}/cert.yaml" | tail -1)" == "annotation-only" ]]
[[ "$(yq eval-all -o=json -I=0 'select(.kind == "ConfigMap" and .metadata.name == "cert-certificate-profile") | .data' "${kp_tmp}/cert.yaml" | jq -cS .)" == '{"ingressClassName":"traefik","management":"managed","productionDNS01Profiles":"","productionIssuer":"kuberploy-letsencrypt-production","productionServerClass":"letsencrypt-production","productionSolverTypes":"http01","stagingDNS01Profiles":"","stagingIssuer":"kuberploy-letsencrypt-staging","stagingServerClass":"letsencrypt-staging","stagingSolverTypes":"http01"}' ]]
if yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.containers[].image' "${kp_tmp}/cert.yaml" | rg -v '^(---|quay\.io/jetstack/cert-manager-(controller|webhook|cainjector):v1\.21\.1)$'; then
  printf 'cert-manager rendered an unexpected image\n' >&2
  exit 1
fi
if rg -n 'kind: Ingress$|kind: TLSStore|external-dns\.alpha\.kubernetes\.io/exclude|library/traefik|:latest' "${kp_tmp}/cert.yaml"; then
  printf 'cert-manager release rendered a route, custom certificate store, or cross-owned workload\n' >&2
  exit 1
fi

helm template cert-dns01 "${kp_cert}" --namespace cert-manager -f "${kp_cert_dns01}" >"${kp_tmp}/cert-dns01.yaml"
[[ "$(kp_count_kind ClusterIssuer "${kp_tmp}/cert-dns01.yaml")" == "1" ]]
[[ "$(kp_count_kind Secret "${kp_tmp}/cert-dns01.yaml")" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "ClusterIssuer") | .spec.acme.solvers | length' "${kp_tmp}/cert-dns01.yaml")" == "3" ]]
[[ "$(yq eval-all 'select(.kind == "ClusterIssuer") | [.spec.acme.solvers[0:2][].dns01.cloudflare.apiTokenSecretRef.name] | join(",")' "${kp_tmp}/cert-dns01.yaml")" == "cloudflare-secondary-dns01,cloudflare-primary-dns01" ]]
[[ "$(yq eval-all 'select(.kind == "ClusterIssuer") | [.spec.acme.solvers[0:2][].selector.dnsZones[]] | join(",")' "${kp_tmp}/cert-dns01.yaml")" == "secondary.example.test,example.test" ]]
[[ "$(yq eval-all 'select(.kind == "ClusterIssuer") | .spec.acme.solvers[2].http01.ingress.ingressClassName' "${kp_tmp}/cert-dns01.yaml")" == "traefik" ]]
[[ "$(yq eval-all 'select(.kind == "ClusterIssuer") | .spec.acme.solvers[2].http01.ingress.ingressTemplate.metadata.annotations."external-dns.alpha.kubernetes.io/ingress-hostname-source"' "${kp_tmp}/cert-dns01.yaml")" == "annotation-only" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.productionSolverTypes' "${kp_tmp}/cert-dns01.yaml")" == "dns01,http01" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap") | .data.productionDNS01Profiles' "${kp_tmp}/cert-dns01.yaml")" == "cloudflare-primary,cloudflare-secondary" ]]
if rg -n 'api-token:|apiToken:|external-dns\.alpha\.kubernetes\.io/exclude|webhook:' "${kp_tmp}/cert-dns01.yaml"; then
  printf 'DNS-01 profile rendered credential material, an unsupported exclusion, or arbitrary webhook provider\n' >&2
  exit 1
fi

helm template cert-adopted "${kp_cert}" --namespace cert-manager -f "${kp_cert_adopted}" >"${kp_tmp}/cert-adopted.yaml"
[[ "$(kp_count_kind Deployment "${kp_tmp}/cert-adopted.yaml")" == "0" ]]
[[ "$(kp_count_kind Namespace "${kp_tmp}/cert-adopted.yaml")" == "0" ]]
[[ "$(kp_count_kind ClusterRole "${kp_tmp}/cert-adopted.yaml")" == "0" ]]
[[ "$(kp_count_kind NetworkPolicy "${kp_tmp}/cert-adopted.yaml")" == "0" ]]
[[ "$(kp_count_kind ClusterIssuer "${kp_tmp}/cert-adopted.yaml")" == "1" ]]
kp_cert_disabled="$(helm template cert-disabled "${kp_cert}" --namespace cert-manager)"
[[ -z "${kp_cert_disabled}" ]] || { printf 'disabled cert-manager chart rendered resources\n' >&2; exit 1; }

kp_expect_reject 'cert-manager without API-server CIDRs' "${kp_cert}" cert-manager "${kp_cert_values}" --set 'foundation.networkPolicy.kubeAPIServerCIDRs={}'
kp_expect_reject 'all-address cert-manager API egress' "${kp_cert}" cert-manager "${kp_cert_values}" --set-string 'foundation.networkPolicy.kubeAPIServerCIDRs[0]=::/0'
kp_expect_reject 'global route TLS mode in cert-manager release' "${kp_cert}" cert-manager "${kp_cert_values}" --set-string foundation.tls.mode=custom
kp_expect_reject 'issuer without email' "${kp_cert}" cert-manager "${kp_cert_values}" --set-string foundation.issuers.production.email=
kp_expect_reject 'mutable production ACME directory' "${kp_cert}" cert-manager "${kp_cert_values}" --set-string foundation.issuers.production.server=https://acme.attacker.invalid/directory
kp_expect_reject 'unknown DNS-01 provider' "${kp_cert}" cert-manager "${kp_cert_dns01}" --set-string foundation.issuers.production.dns01Profiles[0].provider=webhook
kp_expect_reject 'empty DNS-01 zone selector' "${kp_cert}" cert-manager "${kp_cert_dns01}" --set 'foundation.issuers.production.dns01Profiles[0].dnsZones={}'
kp_expect_reject 'overlapping DNS-01 zone selector' "${kp_cert}" cert-manager "${kp_cert_dns01}" --set-string foundation.issuers.production.dns01Profiles[0].dnsZones[0]=example.test
kp_expect_reject 'duplicate DNS-01 profile name' "${kp_cert}" cert-manager "${kp_cert_dns01}" --set-string foundation.issuers.production.dns01Profiles[0].name=cloudflare-primary
kp_expect_reject 'inline DNS-01 credential' "${kp_cert}" cert-manager "${kp_cert_dns01}" --set-string foundation.issuers.production.dns01Profiles[0].cloudflare.apiToken=attacker
kp_expect_reject 'dormant DNS-01 profile' "${kp_cert}" cert-manager "${kp_cert_dns01}" --set foundation.issuers.production.enabled=false
kp_expect_reject 'schema-bypassed DNS-01 provider' "${kp_cert}" cert-manager "${kp_cert_dns01}" --skip-schema-validation --set-string foundation.issuers.production.dns01Profiles[0].provider=webhook
kp_expect_reject 'schema-bypassed DNS-01 arbitrary field' "${kp_cert}" cert-manager "${kp_cert_dns01}" --skip-schema-validation --set-string foundation.issuers.production.dns01Profiles[0].solver.raw=attacker
kp_expect_reject 'schema-bypassed null DNS-01 profiles' "${kp_cert}" cert-manager "${kp_cert_values}" --skip-schema-validation --set foundation.issuers.production.dns01Profiles=null
kp_expect_reject 'cert-manager without CRDs' "${kp_cert}" cert-manager "${kp_cert_values}" --set certmanager.crds.enabled=false
kp_expect_reject 'floating cert-manager image' "${kp_cert}" cert-manager "${kp_cert_values}" --set-string certmanager.image.tag=latest
kp_expect_reject 'upstream broad cert-manager policy' "${kp_cert}" cert-manager "${kp_cert_values}" --set certmanager.networkPolicy.enabled=true
kp_expect_reject 'cert-manager namespace escape' "${kp_cert}" cert-manager "${kp_cert_values}" --set-string certmanager.namespace=other-namespace
kp_expect_reject 'cert-manager controller argument injection' "${kp_cert}" cert-manager "${kp_cert_values}" --set-string 'certManager.extraArgs[0]=--feature-gates=attacker=true'
kp_expect_reject 'cert-manager pod annotation injection' "${kp_cert}" cert-manager "${kp_cert_values}" --set-string certmanager.podAnnotations.sidecar=enabled
kp_expect_reject 'cert-manager cloud identity annotation' "${kp_cert}" cert-manager "${kp_cert_values}" --set-string certmanager.serviceAccount.annotations.cloud=identity
kp_expect_reject 'cert-manager webhook host network' "${kp_cert}" cert-manager "${kp_cert_values}" --set certmanager.webhook.hostNetwork=true
kp_expect_reject 'cert-manager startup hook' "${kp_cert}" cert-manager "${kp_cert_values}" --set certmanager.startupapicheck.enabled=true
kp_expect_reject 'wrong cert-manager namespace' "${kp_cert}" kuberploy-edge "${kp_cert_values}"

helm template dns "${kp_dns}" --namespace kuberploy-system -f "${kp_dns_values}" >"${kp_tmp}/dns.yaml"
helm template dns "${kp_dns}" --namespace kuberploy-system -f "${kp_dns_values}" >"${kp_tmp}/dns-again.yaml"
diff -u "${kp_tmp}/dns.yaml" "${kp_tmp}/dns-again.yaml"
yq eval-all 'true' "${kp_tmp}/dns.yaml" >/dev/null
[[ "$(kp_count_kind Deployment "${kp_tmp}/dns.yaml")" == "1" ]]
[[ "$(kp_count_kind Namespace "${kp_tmp}/dns.yaml")" == "0" ]]
[[ "$(kp_count_kind Secret "${kp_tmp}/dns.yaml")" == "0" ]]
[[ "$(kp_count_kind NetworkPolicy "${kp_tmp}/dns.yaml")" == "1" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.containers[0].image' "${kp_tmp}/dns.yaml")" == 'registry.k8s.io/external-dns/external-dns:v0.21.0' ]]
kp_dns_args="$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.containers[0].args[]' "${kp_tmp}/dns.yaml")"
for kp_arg in \
  '--source=ingress' \
  '--policy=upsert-only' \
  '--registry=txt' \
  '--label-filter=kuberploy.io/dns-integration=cloudflare-primary' \
  '--annotation-filter=external-dns.alpha.kubernetes.io/hostname' \
  '--domain-filter=example.test' \
  '--txt-owner-id=kuberploy-cloudflare-primary-fixture'; do
  grep -Fx -- "${kp_arg}" <<<"${kp_dns_args}" >/dev/null || {
    printf 'external-dns render lacks %s\n' "${kp_arg}" >&2
    exit 1
  }
done
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.containers[0].env[0].valueFrom.secretKeyRef.name' "${kp_tmp}/dns.yaml")" == "external-dns-cloudflare-primary" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "dns-dns-profile") | .data.providerKind' "${kp_tmp}/dns.yaml")" == "cloudflare" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "dns-dns-profile") | .data.credentialSecretRef' "${kp_tmp}/dns.yaml")" == "external-dns-cloudflare-primary" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "dns-dns-profile") | .data.providerConfigRef' "${kp_tmp}/dns.yaml")" == "cloudflare-provider" ]]
[[ "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "dns-dns-profile") | .data.egressConfigRef' "${kp_tmp}/dns.yaml")" == "cloudflare-egress" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy") | .spec.egress[].to[].ipBlock.cidr | select(. == "10.43.0.1/32")] | length' "${kp_tmp}/dns.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all '[select(.kind == "NetworkPolicy") | .spec.egress[].to[].ipBlock.cidr | select(. == "192.0.2.10/32")] | length' "${kp_tmp}/dns.yaml" | tail -1)" == "1" ]]
[[ "$(yq eval-all -o=json '.' "${kp_tmp}/dns.yaml" | jq -s '[.[] | select(.kind == "NetworkPolicy") | .spec.egress[].to[] | select(.namespaceSelector != null and (.namespaceSelector | length) == 0)] | length')" == "0" ]]
if rg -n 'kind: (Ingress|ClusterIssuer|TLSStore)$|library/traefik|cert-manager|:latest' "${kp_tmp}/dns.yaml"; then
  printf 'external-dns release rendered a route, certificate, or cross-owned workload\n' >&2
  exit 1
fi

helm template dns-adopted "${kp_dns}" --namespace kuberploy-system -f "${kp_dns_adopted}" >"${kp_tmp}/dns-adopted.yaml"
[[ "$(kp_count_kind Deployment "${kp_tmp}/dns-adopted.yaml")" == "0" ]]
[[ "$(kp_count_kind ClusterRole "${kp_tmp}/dns-adopted.yaml")" == "0" ]]
[[ "$(kp_count_kind NetworkPolicy "${kp_tmp}/dns-adopted.yaml")" == "0" ]]
kp_dns_disabled="$(helm template dns-disabled "${kp_dns}" --namespace kuberploy-system)"
[[ -z "${kp_dns_disabled}" ]] || { printf 'disabled external-dns chart rendered resources\n' >&2; exit 1; }

kp_expect_reject 'external-dns without API CIDRs' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set 'foundation.networkPolicy.kubeAPIServerCIDRs={}'
kp_expect_reject 'external-dns without provider CIDRs' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set 'foundation.networkPolicy.providerEgressCIDRs={}'
kp_expect_reject 'all-address external-dns API egress' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set-string 'foundation.networkPolicy.kubeAPIServerCIDRs[0]=0.0.0.0/0'
kp_expect_reject 'DNS integration without label filter' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set-string externaldns.labelFilter=
kp_expect_reject 'DNS integration watching manual routes' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set-string externaldns.annotationFilter=
kp_expect_reject 'DNS integration watching Services' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set 'externalDns.sources[0]=service'
kp_expect_reject 'DNS integration without domain filters' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set 'externalDns.domainFilters={}'
kp_expect_reject 'DNS integration without TXT identity' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set-string externaldns.txtOwnerId=
kp_expect_reject 'destructive DNS sync without acknowledgement' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set externaldns.policy=sync
kp_expect_reject 'plaintext provider credential' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set-string 'externalDns.env[0].value=plaintext'
kp_expect_reject 'provider runtime identity substitution' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --skip-schema-validation --set-string foundation.externalDNS.identity.providerKind=aws
kp_expect_reject 'credential runtime identity substitution' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --skip-schema-validation --set-string foundation.externalDNS.identity.credentialSecretRef=other-credentials
kp_expect_reject 'provider-config runtime identity omission' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --skip-schema-validation --set-string foundation.externalDNS.identity.providerConfigRef=
kp_expect_reject 'adopted managed-reference injection' "${kp_dns}" kuberploy-dns "${kp_dns_adopted}" --skip-schema-validation --set-string foundation.externalDNS.identity.egressConfigRef=managed-egress
kp_expect_reject 'chart-rendered provider Secret' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set externaldns.secretConfiguration.enabled=true
kp_expect_reject 'privileged external-dns' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set externaldns.securityContext.privileged=true
kp_expect_reject 'ambient external-dns workload identity' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set-string externaldns.serviceAccount.annotations.cloud=identity
kp_expect_reject 'external-dns NetworkPolicy identity bypass' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set-string externaldns.nameOverride=unconfined-dns
kp_expect_reject 'cert-manager dependency alias name bypass' "${kp_cert}" cert-manager "${kp_cert_values}" --set-string certmanager.nameOverride=unconfined-cert-manager
kp_expect_reject 'external-dns namespace escape' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set-string externaldns.namespaceOverride=other-namespace
kp_expect_reject 'external-dns pod annotation injection' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set-string externaldns.podAnnotations.sidecar=enabled
kp_expect_reject 'floating external-dns image' "${kp_dns}" kuberploy-dns "${kp_dns_values}" --set-string externaldns.image.tag=latest
kp_expect_reject 'wrong external-dns namespace' "${kp_dns}" cert-manager "${kp_dns_values}"

helm template dns-sync "${kp_dns}" --namespace kuberploy-system -f "${kp_dns_values}" \
  --set externaldns.policy=sync --set foundation.allowDestructiveSync=true >"${kp_tmp}/dns-sync.yaml"
rg -F -- '--policy=sync' "${kp_tmp}/dns-sync.yaml" >/dev/null

for kp_kube_version in 1.34.10 1.35.7 1.36.3; do
  helm template edge-lane "${kp_edge}" --namespace kuberploy-system --kube-version "${kp_kube_version}" -f "${kp_edge_values}" >/dev/null
  helm template cert-lane "${kp_cert}" --namespace cert-manager --kube-version "${kp_kube_version}" -f "${kp_cert_values}" >/dev/null
  helm template dns-lane "${kp_dns}" --namespace kuberploy-system --kube-version "${kp_kube_version}" -f "${kp_dns_values}" >/dev/null
done

for kp_render in "${kp_tmp}/edge.yaml" "${kp_tmp}/cert.yaml" "${kp_tmp}/dns.yaml"; do
  if yq eval-all 'select(.kind == "ClusterRoleBinding") | .subjects[]? | select(.kind == "ServiceAccount" and .name == "default") | .name' "${kp_render}" | grep -q .; then
    printf 'managed release bound cluster RBAC to the default ServiceAccount: %s\n' "${kp_render}" >&2
    exit 1
  fi
done

printf 'Independent Traefik, cert-manager, and external-dns chart locks, modes, ownership, network, RBAC, and mutation checks passed.\n'
