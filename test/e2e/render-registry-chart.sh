#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
kp_chart="${kp_root}/charts/kuberploy-registry"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-registry-render.XXXXXX")"

kp_remove_tmp() {
  if [[ -n "${kp_tmp:-}" && "${kp_tmp}" == *"/kuberploy-registry-render."* ]]; then
    rm -rf -- "${kp_tmp}"
  fi
}
trap kp_remove_tmp EXIT

for kp_tool in helm jq rg yq; do
  command -v "${kp_tool}" >/dev/null 2>&1 || {
    printf 'missing tool: %s\n' "${kp_tool}" >&2
    exit 1
  }
done

kp_auth_values="${kp_chart}/testdata/authenticated-values.yaml"
kp_test_values="${kp_chart}/testdata/test-only-values.yaml"
kp_existing_values="${kp_chart}/testdata/existing-claim-values.yaml"
kp_load_balancer_values="${kp_chart}/testdata/loadbalancer-values.yaml"
kp_auth_render="${kp_tmp}/authenticated.yaml"
kp_test_render="${kp_tmp}/test-only.yaml"
kp_existing_render="${kp_tmp}/existing-claim.yaml"

helm lint "${kp_chart}"
helm lint "${kp_chart}" -f "${kp_auth_values}"
helm lint "${kp_chart}" -f "${kp_test_values}"

kp_disabled_render="$(helm template disabled "${kp_chart}")"
[[ -z "${kp_disabled_render}" ]] || {
  printf 'disabled registry chart rendered Kubernetes resources\n' >&2
  exit 1
}

helm template authenticated "${kp_chart}" \
  --namespace kuberploy-registry \
  -f "${kp_auth_values}" >"${kp_auth_render}"
yq eval-all 'true' "${kp_auth_render}" >/dev/null

kp_expected_image='docker.io/library/registry:3.1.1'
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.containers[0].image' "${kp_auth_render}")" == "${kp_expected_image}" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.replicas' "${kp_auth_render}")" == "1" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.strategy.type' "${kp_auth_render}")" == "Recreate" ]]
[[ "$(yq eval-all 'select(.kind == "Service") | .spec.type' "${kp_auth_render}")" == "ClusterIP" ]]
[[ "$(yq eval-all 'select(.kind == "Service") | .spec.ports[0].port' "${kp_auth_render}")" == "443" ]]
[[ "$(yq eval-all 'select(.kind == "Ingress") | .spec.ingressClassName' "${kp_auth_render}")" == "traefik" ]]
[[ "$(yq eval-all 'select(.kind == "Ingress") | .spec.rules[0].host' "${kp_auth_render}")" == "registry.example.com" ]]
[[ "$(yq eval-all 'select(.kind == "Ingress") | .spec.tls[0].secretName' "${kp_auth_render}")" == "registry-tls" ]]
[[ "$(yq eval-all 'select(.kind == "Ingress") | .metadata.annotations."external-dns.alpha.kubernetes.io/cloudflare-proxied"' "${kp_auth_render}")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Service" and .metadata.name == "authenticated") | .metadata.annotations."traefik.ingress.kubernetes.io/service.serversscheme"' "${kp_auth_render}")" == "https" ]]
[[ "$(yq eval-all 'select(.kind == "Service" and .metadata.name == "authenticated") | .metadata.annotations."traefik.ingress.kubernetes.io/service.serverstransport"' "${kp_auth_render}")" == "kuberploy-registry-authenticated-backend@kubernetescrd" ]]
[[ "$(yq eval-all 'select(.kind == "Ingress") | .metadata.annotations."traefik.ingress.kubernetes.io/service.serverstransport"' "${kp_auth_render}")" == "null" ]]
[[ "$(yq eval-all 'select(.kind == "ServersTransport") | .spec.serverName' "${kp_auth_render}")" == "registry.example.com" ]]
[[ "$(yq eval-all 'select(.kind == "ServersTransport") | .spec.insecureSkipVerify' "${kp_auth_render}")" == "null" ]]
[[ "$(yq eval-all 'select(.kind == "Ingress") | .spec.rules[0].http.paths[0].backend.service.port.name' "${kp_auth_render}")" == "registry" ]]
[[ "$(yq eval-all 'select(.kind == "Certificate") | .spec.secretName' "${kp_auth_render}")" == "registry-tls" ]]
[[ "$(yq eval-all 'select(.kind == "Certificate") | .spec.issuerRef.name' "${kp_auth_render}")" == "kuberploy-letsencrypt-production" ]]
[[ "$(yq eval-all 'select(.kind == "Certificate") | .spec.dnsNames[0]' "${kp_auth_render}")" == "registry.example.com" ]]
[[ "$(yq eval-all 'select(.kind == "PersistentVolumeClaim") | .spec.storageClassName' "${kp_auth_render}")" == "fixture-storage" ]]
[[ "$(yq eval-all 'select(.kind == "PersistentVolumeClaim") | .spec.resources.requests.storage' "${kp_auth_render}")" == "2Gi" ]]
[[ "$(yq eval-all 'select(.kind == "PersistentVolumeClaim") | .metadata.annotations."helm.sh/resource-policy"' "${kp_auth_render}")" == "keep" ]]

[[ "$(yq eval-all 'select(.kind == "ServiceAccount") | .automountServiceAccountToken' "${kp_auth_render}")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.automountServiceAccountToken' "${kp_auth_render}")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.securityContext.runAsNonRoot' "${kp_auth_render}")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.securityContext.runAsUser' "${kp_auth_render}")" == "10000" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem' "${kp_auth_render}")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation' "${kp_auth_render}")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.containers[0].securityContext.capabilities.drop[0]' "${kp_auth_render}")" == "ALL" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.containers[0].startupProbe.tcpSocket.port' "${kp_auth_render}")" == "registry" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.containers[0].env[] | select(.name == "REGISTRY_HTTP_SECRET") | .valueFrom.secretKeyRef.name' "${kp_auth_render}")" == "registry-auth" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.containers[0].env[] | select(.name == "REGISTRY_HTTP_SECRET") | .valueFrom.secretKeyRef.key' "${kp_auth_render}")" == "httpSecret" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.volumes[] | select(.name == "auth") | .secret.secretName' "${kp_auth_render}")" == "registry-auth" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.volumes[] | select(.name == "auth") | .secret.items[0].key' "${kp_auth_render}")" == "htpasswd" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.volumes[] | select(.name == "tls") | .secret.secretName' "${kp_auth_render}")" == "registry-tls" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.containers[0].volumeMounts[] | select(.name == "tls") | .mountPath' "${kp_auth_render}")" == "/tls" ]]

kp_config="$(yq eval-all 'select(.kind == "ConfigMap") | .data."config.yml"' "${kp_auth_render}")"
grep -F 'rootdirectory: /var/lib/registry' <<<"${kp_config}" >/dev/null
grep -F 'path: /auth/htpasswd' <<<"${kp_config}" >/dev/null
grep -A1 -F 'delete:' <<<"${kp_config}" | grep -F 'enabled: true' >/dev/null
grep -F 'addr: :5000' <<<"${kp_config}" >/dev/null
grep -F 'certificate: /tls/tls.crt' <<<"${kp_config}" >/dev/null
grep -F 'key: /tls/tls.key' <<<"${kp_config}" >/dev/null

[[ "$(yq eval-all '[select(.kind == "NetworkPolicy") | .spec.ingress[].from[].namespaceSelector.matchLabels."kubernetes.io/metadata.name"] | sort | join(",")' "${kp_auth_render}" | tail -1)" == "kuberploy,kuberploy-build" ]]
[[ "$(yq eval-all 'select(.kind == "NetworkPolicy") | .spec.egress | length' "${kp_auth_render}")" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "NetworkPolicy") | .spec.podSelector.matchLabels."app.kubernetes.io/component"' "${kp_auth_render}")" == "registry" ]]

if yq eval-all 'select(.kind == "Secret" or .kind == "Job" or .kind == "CronJob" or (.kind == "Service" and .spec.type != "ClusterIP")) | .kind' "${kp_auth_render}" | grep -q .; then
  printf 'registry chart rendered a forbidden Secret, direct exposure, or GC workload\n' >&2
  exit 1
fi
if rg -n 'password[[:space:]]*:|(^|[[:space:]])garbage-collect([[:space:]]|$)|NodePort|LoadBalancer' "${kp_auth_render}"; then
  printf 'registry render contains plaintext password, automatic GC, or public exposure\n' >&2
  exit 1
fi

if helm template invalid "${kp_chart}" \
    --namespace kuberploy-registry \
    --set enabled=true \
    --set 'networkPolicy.allowedNamespaces[0]=kuberploy' >/dev/null 2>&1; then
  printf 'enabled production render accepted an empty auth Secret reference\n' >&2
  exit 1
fi
if helm template invalid "${kp_chart}" \
    --namespace kuberploy-registry \
    -f "${kp_auth_values}" \
    --set exposure.secretName= >/dev/null 2>&1; then
  printf 'registry ingress accepted missing TLS Secret identity\n' >&2
  exit 1
fi
if helm template invalid "${kp_chart}" \
    --namespace kuberploy-registry \
    -f "${kp_test_values}" \
    --set exposure.mode=ingress \
    --set-string exposure.endpoint=registry.example.com \
    --set-string exposure.secretName=registry-tls \
    --set-string exposure.clusterIssuerName=kuberploy-letsencrypt-production >/dev/null 2>&1; then
  printf 'public registry ingress accepted unauthenticated test mode\n' >&2
  exit 1
fi

kp_load_balancer_render="${kp_tmp}/load-balancer.yaml"
helm template registry "${kp_chart}" \
  --namespace kuberploy-system \
  -f "${kp_auth_values}" \
  -f "${kp_load_balancer_values}" >"${kp_load_balancer_render}"
[[ "$(yq eval-all 'select(.kind == "Service" and (.metadata.name | test("-edge$"))) | .spec.type' "${kp_load_balancer_render}")" == "LoadBalancer" ]]
[[ "$(yq eval-all 'select(.kind == "Service" and (.metadata.name | test("-edge$"))) | .spec.allocateLoadBalancerNodePorts' "${kp_load_balancer_render}")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Service" and (.metadata.name | test("-edge$"))) | .spec.loadBalancerClass' "${kp_load_balancer_render}")" == "example.com/private" ]]
[[ "$(yq eval-all 'select(.kind == "Service" and (.metadata.name | test("-edge$"))) | .spec.loadBalancerIP' "${kp_load_balancer_render}")" == "10.20.30.40" ]]
[[ "$(yq eval-all 'select(.kind == "Service" and (.metadata.name | test("-edge$"))) | .spec.loadBalancerSourceRanges[0]' "${kp_load_balancer_render}")" == "10.20.0.0/16" ]]
[[ "$(yq eval-all 'select(.kind == "Service" and (.metadata.name | test("-edge$"))) | .metadata.annotations."example.com/internal-load-balancer"' "${kp_load_balancer_render}")" == "true" ]]
[[ "$(yq eval-all 'select(.kind == "Service" and (.metadata.name | test("-edge$"))) | .metadata.annotations."external-dns.alpha.kubernetes.io/cloudflare-proxied"' "${kp_load_balancer_render}")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Service" and (.metadata.name | test("-edge$"))) | .spec.selector."app.kubernetes.io/instance"' "${kp_load_balancer_render}")" == "edge-kuberploy-system" ]]
[[ "$(yq eval-all 'select(.kind == "Service" and (.metadata.name | test("-edge$"))) | .spec.ports[0].targetPort' "${kp_load_balancer_render}")" == "websecure" ]]

helm template registry "${kp_chart}" \
  --namespace kuberploy-system \
  -f "${kp_auth_values}" \
  -f "${kp_load_balancer_values}" \
  --set-string exposure.endpoint=10.20.30.40 >"${kp_tmp}/load-balancer-ip.yaml"
[[ "$(yq eval-all 'select(.kind == "Certificate") | .spec.ipAddresses[0]' "${kp_tmp}/load-balancer-ip.yaml")" == "10.20.30.40" ]]
[[ "$(yq eval-all 'select(.kind == "Ingress") | has(.spec.rules[0].host)' "${kp_tmp}/load-balancer-ip.yaml")" == "false" ]]
[[ "$(yq eval-all 'select(.kind == "Service" and (.metadata.name | test("-edge$"))) | has(.metadata.annotations."external-dns.alpha.kubernetes.io/hostname")' "${kp_tmp}/load-balancer-ip.yaml")" == "false" ]]

if helm template invalid "${kp_chart}" --namespace kuberploy-system -f "${kp_auth_values}" -f "${kp_load_balancer_values}" --set-json exposure.loadBalancer.sourceRanges='[]' >/dev/null 2>&1; then
  printf 'registry LoadBalancer accepted empty source ranges\n' >&2
  exit 1
fi
if helm template invalid "${kp_chart}" --namespace kuberploy-system -f "${kp_auth_values}" -f "${kp_load_balancer_values}" --set-string exposure.loadBalancer.annotations.external-dns\.alpha\.kubernetes\.io/cloudflare-proxied=true >/dev/null 2>&1; then
  printf 'registry LoadBalancer accepted caller-controlled Cloudflare proxy mode\n' >&2
  exit 1
fi
if helm template invalid "${kp_chart}" \
    --namespace kuberploy-registry \
    -f "${kp_auth_values}" \
    --set image.reference=registry:latest >/dev/null 2>&1; then
  printf 'registry chart accepted a floating image\n' >&2
  exit 1
fi
if helm template invalid "${kp_chart}" \
    --namespace kuberploy-registry \
    -f "${kp_test_values}" \
    --set auth.existingSecret=must-not-be-used >/dev/null 2>&1; then
  printf 'test-only unauthenticated mode accepted an auth Secret reference\n' >&2
  exit 1
fi
if helm template invalid "${kp_chart}" \
    --namespace kuberploy-registry \
    -f "${kp_test_values}" \
    --set service.type=NodePort >/dev/null 2>&1; then
  printf 'registry chart accepted a non-ClusterIP Service\n' >&2
  exit 1
fi
if helm template invalid "${kp_chart}" \
    --namespace kuberploy-registry \
    -f "${kp_auth_values}" \
    --set networkPolicy.enabled=false >/dev/null 2>&1; then
  printf 'registry chart accepted a disabled NetworkPolicy\n' >&2
  exit 1
fi
if helm template invalid "${kp_chart}" \
    --namespace kuberploy-registry \
    -f "${kp_auth_values}" \
    --set unsupportedField=true >/dev/null 2>&1; then
  printf 'registry chart values schema accepted an unknown field\n' >&2
  exit 1
fi

helm template test-only "${kp_chart}" \
  --namespace kuberploy-e2e-render \
  -f "${kp_test_values}" >"${kp_test_render}"
yq eval-all 'true' "${kp_test_render}" >/dev/null
[[ "$(yq eval-all 'select(.kind == "Service") | .spec.ports[0].port' "${kp_test_render}")" == "5000" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.metadata.annotations."kuberploy.io/security-warning"' "${kp_test_render}")" == "test-only-unauthenticated" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment") | .spec.template.spec.volumes[] | select(.name == "auth")] | length' "${kp_test_render}" | tail -1)" == "0" ]]
[[ "$(yq eval-all '[select(.kind == "Deployment") | .spec.template.spec.volumes[] | select(.name == "tls")] | length' "${kp_test_render}" | tail -1)" == "0" ]]
if grep -F 'auth:' <<<"$(yq eval-all 'select(.kind == "ConfigMap") | .data."config.yml"' "${kp_test_render}")" >/dev/null; then
  printf 'test-only registry unexpectedly rendered an auth provider\n' >&2
  exit 1
fi
helm install test-only "${kp_chart}" \
  --namespace kuberploy-e2e-render \
  --dry-run=client \
  -f "${kp_test_values}" | \
  grep -F 'WARNING: TEST-ONLY UNAUTHENTICATED REGISTRY MODE IS ENABLED.' >/dev/null

helm template existing "${kp_chart}" \
  --namespace kuberploy-registry \
  -f "${kp_auth_values}" \
  -f "${kp_existing_values}" >"${kp_existing_render}"
[[ "$(yq eval-all '[select(.kind == "PersistentVolumeClaim")] | length' "${kp_existing_render}" | tail -1)" == "0" ]]
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.template.spec.volumes[] | select(.name == "data") | .persistentVolumeClaim.claimName' "${kp_existing_render}")" == "retained-registry-data" ]]

printf 'Managed registry chart lint, fail-closed auth, storage, network, and security checks passed.\n'
