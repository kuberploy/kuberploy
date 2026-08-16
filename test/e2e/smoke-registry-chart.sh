#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../../scripts/kubernetes/lib.sh"

kp_initialize
kp_require_tools helm htpasswd openssl

kp_root="$(kp_repo_root)"
kp_namespace="$(kp_run_namespace)"
kp_namespace_created="false"

kp_registry_cleanup() {
  local kp_status=$?
  trap - EXIT INT TERM
  if [[ "${kp_namespace_created}" == "true" ]]; then
    if ! "${kp_root}/scripts/kubernetes/cleanup-run.sh"; then
      kp_status=1
    fi
  fi
  exit "${kp_status}"
}
trap kp_registry_cleanup EXIT INT TERM

kp_existing_namespace="$(kp_kubectl get namespace "${kp_namespace}" \
  --ignore-not-found -o name)"
[[ -z "${kp_existing_namespace}" ]] || \
  kp_die "refusing registry smoke because the run namespace already exists"

kp_kubectl create -f - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${kp_namespace}
  labels:
    ${KP_RUN_LABEL_KEY}: ${KUBERPLOY_E2E_RUN_ID}
    ${KP_MANAGED_BY_LABEL_KEY}: ${KP_MANAGED_BY_LABEL_VALUE}
    kuberploy.io/test-purpose: managed-registry-smoke
EOF
kp_namespace_created="true"

kp_registry_user="smoke"
kp_registry_password="$(openssl rand -hex 24)"
kp_registry_htpasswd="$(htpasswd -nbB \
  "${kp_registry_user}" "${kp_registry_password}")"
kp_registry_http_secret="$(openssl rand -hex 32)"

# Synthetic credentials exist only in this disposable namespace. The chart
# consumes an existing Secret and never renders or prints these values.
kp_kubectl --namespace "${kp_namespace}" create secret generic registry-smoke-auth \
  --from-literal=htpasswd="${kp_registry_htpasswd}" \
  --from-literal=httpSecret="${kp_registry_http_secret}" \
  --from-literal=username="${kp_registry_user}" \
  --from-literal=password="${kp_registry_password}" >/dev/null
kp_kubectl --namespace "${kp_namespace}" label secret registry-smoke-auth \
  "${KP_RUN_LABEL_KEY}=${KUBERPLOY_E2E_RUN_ID}" \
  "${KP_MANAGED_BY_LABEL_KEY}=${KP_MANAGED_BY_LABEL_VALUE}" >/dev/null

kp_helm install registry-smoke "${kp_root}/charts/kuberploy-registry" \
  --namespace "${kp_namespace}" \
  --set enabled=true \
  --set "global.testRun=${KUBERPLOY_E2E_RUN_ID}" \
  --set auth.existingSecret=registry-smoke-auth \
  --set auth.secretRevision=smoke-v1 \
  --set persistence.size=128Mi \
  --set persistence.retainOnDelete=false \
  --set "networkPolicy.allowedNamespaces[0]=${kp_namespace}" >/dev/null

kp_kubectl --namespace "${kp_namespace}" wait \
  --for=condition=Available deployment/registry-smoke \
  --timeout=180s >/dev/null
kp_pvc_phase="$(kp_kubectl --namespace "${kp_namespace}" get \
  persistentvolumeclaim/registry-smoke -o jsonpath='{.status.phase}')"
[[ "${kp_pvc_phase}" == "Bound" ]] || \
  kp_die "managed registry smoke PVC did not become Bound"

kp_kubectl create -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: registry-probe
  namespace: ${kp_namespace}
  labels:
    app.kubernetes.io/name: registry-probe
    ${KP_RUN_LABEL_KEY}: ${KUBERPLOY_E2E_RUN_ID}
    ${KP_MANAGED_BY_LABEL_KEY}: ${KP_MANAGED_BY_LABEL_VALUE}
spec:
  automountServiceAccountToken: false
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: probe
      image: docker.io/library/alpine:3.24.1
      imagePullPolicy: IfNotPresent
      command:
        - /bin/sh
        - -ec
        - |
          auth="\$(printf '%s:%s' "\${REGISTRY_USER}" "\${REGISTRY_PASSWORD}" | base64 | tr -d '\\n')"
          for attempt in \$(seq 1 30); do
            if ! wget -qO- http://registry-smoke:5000/v2/ >/dev/null 2>&1; then
              if wget -qO- --header "Authorization: Basic \${auth}" http://registry-smoke:5000/v2/ >/dev/null 2>&1; then
                printf 'authenticated-registry-ok\\n'
                exit 0
              fi
            else
              # A successful anonymous request would mean the registry auth
              # boundary is missing; keep retrying only for Service startup
              # races, never accept an unauthenticated response.
              exit 1
            fi
            sleep 1
          done
          exit 1
      env:
        - name: REGISTRY_USER
          valueFrom:
            secretKeyRef:
              name: registry-smoke-auth
              key: username
        - name: REGISTRY_PASSWORD
          valueFrom:
            secretKeyRef:
              name: registry-smoke-auth
              key: password
      resources:
        requests:
          cpu: 10m
          memory: 16Mi
        limits:
          cpu: 100m
          memory: 64Mi
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop:
            - ALL
        readOnlyRootFilesystem: true
EOF

kp_kubectl --namespace "${kp_namespace}" wait \
  --for=jsonpath='{.status.phase}'=Succeeded pod/registry-probe \
  --timeout=180s >/dev/null
kp_probe_log="$(kp_kubectl --namespace "${kp_namespace}" logs registry-probe)"
[[ "${kp_probe_log}" == "authenticated-registry-ok" ]] || \
  kp_die "managed registry authentication probe did not return the expected marker"

printf 'Authenticated ClusterIP registry /v2/ and PVC smoke passed.\n'
