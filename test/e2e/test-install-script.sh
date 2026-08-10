#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-install-test.XXXXXX")"
kp_cleanup() {
  [[ -n "${kp_tmp:-}" && "${kp_tmp}" == *kuberploy-install-test.* ]] && rm -rf -- "${kp_tmp}"
}
trap kp_cleanup EXIT

mkdir -p "${kp_tmp}/bin"
touch "${kp_tmp}/kubeconfig"

cat >"${kp_tmp}/bin/helm" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "${1:-}" == version ]]; then
  printf '%s\n' 'v4.2.3'
  exit 0
fi
printf '%s\n' "$@" >>"${KUBERPLOY_INSTALL_CAPTURE:?}"
printf '%s\n' '__HELM_CALL__' >>"${KUBERPLOY_INSTALL_CAPTURE:?}"
EOF

cat >"${kp_tmp}/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' '{"web":["192.30.252.0/22","2a0a:a440::/29","192.30.252.0/22"]}'
EOF

cat >"${kp_tmp}/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
kp_args=" $* "
case "${kp_args}" in
  *' config get-contexts test-context -o name '*) printf '%s\n' 'test-context' ;;
  *' config view --minify --raw -o json '*) printf '%s\n' '{"clusters":[{"cluster":{"server":"https://192.0.2.10:6443"}}]}' ;;
  *' get --raw /version '*) printf '{"minor":"%s"}\n' "${KUBERPLOY_INSTALL_TEST_MINOR:-36}" ;;
  *' --namespace default get service kubernetes -o json '*) printf '%s\n' '{"spec":{"clusterIP":"10.43.0.1"}}' ;;
  *' --namespace default get endpointslice --selector kubernetes.io/service-name=kubernetes -o json '*)
    printf '%s\n' '{"items":[{"endpoints":[{"addresses":["10.72.252.250"],"conditions":{"ready":true}}]}]}'
    ;;
  *' get crd applications.argoproj.io applicationsets.argoproj.io appprojects.argoproj.io '*)
    [[ "${KUBERPLOY_INSTALL_TEST_CRDS_PRESENT:-false}" == true ]]
    ;;
  *' wait --for=condition=Established --timeout=5m crd applications.argoproj.io applicationsets.argoproj.io appprojects.argoproj.io '*) ;;
  *) printf 'unexpected kubectl invocation: %s\n' "$*" >&2; exit 1 ;;
esac
EOF

chmod +x "${kp_tmp}/bin/helm" "${kp_tmp}/bin/curl" "${kp_tmp}/bin/kubectl"

export KUBERPLOY_INSTALL_CAPTURE="${kp_tmp}/helm-args"
PATH="${kp_tmp}/bin:${PATH}" "${kp_root}/scripts/install.sh" \
  --version 0.1.0-rc.4 \
  --kubeconfig "${kp_tmp}/kubeconfig" \
  --context test-context \
  --yes >"${kp_tmp}/output"

rg -Fx 'oci://ghcr.io/kuberploy/charts/kuberploy-installer' "${kp_tmp}/helm-args" >/dev/null
rg -Fx '0.1.0-rc.4' "${kp_tmp}/helm-args" >/dev/null
rg -Fx 'source.targetRevision=v0.1.0-rc.4' "${kp_tmp}/helm-args" >/dev/null
rg -Fx 'bootstrap.controlPlaneToken.kubeAPIServerCIDRs=["10.43.0.1/32","10.72.252.250/32"]' "${kp_tmp}/helm-args" >/dev/null
rg -Fx 'argoCD.argoFoundation.networkPolicy.repositoryEgressCIDRs=["192.30.252.0/22","2a0a:a440::/29"]' "${kp_tmp}/helm-args" >/dev/null
rg -Fx 'components.controlPlane.enabled=true' "${kp_tmp}/helm-args" >/dev/null
rg -Fx 'components.postgresql.enabled=true' "${kp_tmp}/helm-args" >/dev/null
rg -Fx 'components.valkey.enabled=true' "${kp_tmp}/helm-args" >/dev/null
rg -F 'https://192.0.2.10:6443' "${kp_tmp}/output" >/dev/null
[[ "$(rg -c '^__HELM_CALL__$' "${kp_tmp}/helm-args")" == 2 ]] || {
  printf '%s\n' 'blank-cluster installation did not use exactly two Helm phases' >&2
  exit 1
}
rg -F 'Bootstrapping Argo CD and its CRDs before creating Applications' "${kp_tmp}/output" >/dev/null

: >"${kp_tmp}/helm-args"
KUBERPLOY_INSTALL_TEST_CRDS_PRESENT=true PATH="${kp_tmp}/bin:${PATH}" "${kp_root}/scripts/install.sh" \
  --version 0.1.0-rc.4 \
  --kubeconfig "${kp_tmp}/kubeconfig" \
  --context test-context \
  --yes >/dev/null
[[ "$(rg -c '^__HELM_CALL__$' "${kp_tmp}/helm-args")" == 1 ]] || {
  printf '%s\n' 'established Argo CRDs did not skip the bootstrap phase' >&2
  exit 1
}

if PATH="${kp_tmp}/bin:${PATH}" "${kp_root}/scripts/install.sh" \
  --version v0.1.0-rc.4 --kubeconfig "${kp_tmp}/kubeconfig" --context test-context --yes >/dev/null 2>&1; then
  printf '%s\n' 'installer accepted a v-prefixed package version' >&2
  exit 1
fi

if KUBERPLOY_INSTALL_TEST_MINOR=37 PATH="${kp_tmp}/bin:${PATH}" "${kp_root}/scripts/install.sh" \
  --version 0.1.0-rc.4 --kubeconfig "${kp_tmp}/kubeconfig" --context test-context --yes >/dev/null 2>&1; then
  printf '%s\n' 'installer accepted an unsupported Kubernetes version' >&2
  exit 1
fi

if PATH="${kp_tmp}/bin:${PATH}" "${kp_root}/scripts/install.sh" \
  --version 0.1.0-rc.4 --kubeconfig "${kp_tmp}/kubeconfig" --context test-context </dev/null >/dev/null 2>&1; then
  printf '%s\n' 'installer mutated a non-interactive cluster without --yes' >&2
  exit 1
fi

printf '%s\n' 'installer script checks passed'
