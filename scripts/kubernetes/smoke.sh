#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_root="$(kp_repo_root)"
kp_created_namespace="false"

kp_cleanup_on_exit() {
  local kp_status=$?
  trap - EXIT
  if [[ "${kp_created_namespace}" == "true" ]]; then
    if ! "${kp_root}/scripts/kubernetes/cleanup-run.sh"; then
      printf 'error: automatic cleanup failed; rerun scripts/kubernetes/cleanup-run.sh with the same explicit environment and run ID\n' >&2
      kp_status=1
    fi
  fi
  exit "${kp_status}"
}
trap kp_cleanup_on_exit EXIT

"${kp_root}/scripts/kubernetes/preflight.sh"
kp_namespace="$(kp_run_namespace)"

kp_existing_namespace="$(kp_kubectl get namespace "${kp_namespace}" \
  --ignore-not-found -o name)"
[[ -z "${kp_existing_namespace}" ]] || \
  kp_die "refusing smoke test because the run namespace already exists"

kp_kubectl create -f - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${kp_namespace}
  labels:
    ${KP_RUN_LABEL_KEY}: ${KUBERPLOY_E2E_RUN_ID}
    ${KP_MANAGED_BY_LABEL_KEY}: ${KP_MANAGED_BY_LABEL_VALUE}
    kuberploy.io/test-purpose: cluster-smoke
EOF
kp_created_namespace="true"

readonly KP_SERVER_IMAGE="registry.k8s.io/e2e-test-images/agnhost:2.53"
readonly KP_CLIENT_IMAGE="docker.io/library/alpine:3.24.1"

kp_kubectl create -f - >/dev/null <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: smoke-data
  namespace: ${kp_namespace}
  labels:
    ${KP_RUN_LABEL_KEY}: ${KUBERPLOY_E2E_RUN_ID}
    ${KP_MANAGED_BY_LABEL_KEY}: ${KP_MANAGED_BY_LABEL_VALUE}
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 16Mi
---
apiVersion: v1
kind: Pod
metadata:
  name: smoke-server
  namespace: ${kp_namespace}
  labels:
    app.kubernetes.io/name: smoke-server
    ${KP_RUN_LABEL_KEY}: ${KUBERPLOY_E2E_RUN_ID}
    ${KP_MANAGED_BY_LABEL_KEY}: ${KP_MANAGED_BY_LABEL_VALUE}
spec:
  automountServiceAccountToken: false
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    fsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  initContainers:
    - name: initialize-pvc
      image: ${KP_CLIENT_IMAGE}
      imagePullPolicy: IfNotPresent
      command:
        - /bin/sh
        - -ec
        - printf 'kuberploy-pvc-ok\\n' > /data/pvc-marker
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
      volumeMounts:
        - name: data
          mountPath: /data
  containers:
    - name: server
      image: ${KP_SERVER_IMAGE}
      imagePullPolicy: IfNotPresent
      command:
        - /agnhost
      args:
        - netexec
        - --http-port=8080
        - --udp-port=-1
      ports:
        - name: http
          containerPort: 8080
      readinessProbe:
        httpGet:
          path: /
          port: http
        initialDelaySeconds: 1
        periodSeconds: 1
        timeoutSeconds: 1
        failureThreshold: 30
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
      volumeMounts:
        - name: data
          mountPath: /data
          readOnly: true
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: smoke-data
---
apiVersion: v1
kind: Service
metadata:
  name: smoke
  namespace: ${kp_namespace}
  labels:
    ${KP_RUN_LABEL_KEY}: ${KUBERPLOY_E2E_RUN_ID}
    ${KP_MANAGED_BY_LABEL_KEY}: ${KP_MANAGED_BY_LABEL_VALUE}
spec:
  type: ClusterIP
  selector:
    app.kubernetes.io/name: smoke-server
  ports:
    - name: http
      port: 8080
      targetPort: http
EOF

kp_kubectl wait --namespace "${kp_namespace}" \
  --for=condition=Ready pod/smoke-server --timeout=180s >/dev/null

kp_pvc_phase="$(kp_kubectl get --namespace "${kp_namespace}" \
  persistentvolumeclaim/smoke-data -o jsonpath='{.status.phase}')"
[[ "${kp_pvc_phase}" == "Bound" ]] || \
  kp_die "smoke PersistentVolumeClaim did not become Bound"

kp_kubectl create -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: smoke-probe
  namespace: ${kp_namespace}
  labels:
    app.kubernetes.io/name: smoke-probe
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
      image: ${KP_CLIENT_IMAGE}
      imagePullPolicy: IfNotPresent
      command:
        - /bin/sh
        - -ec
        - grep -Fxq kuberploy-pvc-ok /data/pvc-marker; nslookup smoke.${kp_namespace}.svc.cluster.local >/dev/null; for kp_attempt in \$(seq 1 30); do wget -qO- 'http://smoke:8080/echo?msg=kuberploy-smoke-ok' | grep -Fx kuberploy-smoke-ok && exit 0; sleep 1; done; exit 1
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
      volumeMounts:
        - name: data
          mountPath: /data
          readOnly: true
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: smoke-data
EOF

kp_kubectl wait --namespace "${kp_namespace}" \
  --for=jsonpath='{.status.phase}'=Succeeded pod/smoke-probe \
  --timeout=180s >/dev/null

kp_probe_log="$(kp_kubectl logs --namespace "${kp_namespace}" smoke-probe)"
kp_probe_last_line="$(tail -n 1 <<<"${kp_probe_log}")"
[[ "${kp_probe_last_line}" == "kuberploy-smoke-ok" ]] || \
  kp_die "smoke DNS/service response did not match the expected marker"

kp_kubectl create -f - >/dev/null <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: smoke-server-default-deny
  namespace: ${kp_namespace}
  labels:
    ${KP_RUN_LABEL_KEY}: ${KUBERPLOY_E2E_RUN_ID}
    ${KP_MANAGED_BY_LABEL_KEY}: ${KP_MANAGED_BY_LABEL_VALUE}
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: smoke-server
  policyTypes:
    - Ingress
---
apiVersion: v1
kind: Pod
metadata:
  name: smoke-denied
  namespace: ${kp_namespace}
  labels:
    app.kubernetes.io/name: smoke-denied
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
      image: ${KP_CLIENT_IMAGE}
      imagePullPolicy: IfNotPresent
      command:
        - /bin/sh
        - -ec
        - if wget -T 5 -qO- 'http://smoke:8080/echo?msg=unexpected'; then echo ingress-not-denied >&2; exit 1; fi
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

kp_kubectl wait --namespace "${kp_namespace}" \
  --for=jsonpath='{.status.phase}'=Succeeded pod/smoke-denied \
  --timeout=90s >/dev/null || \
  kp_die "the selected cluster did not enforce the default-deny ingress NetworkPolicy"

kp_kubectl create -f - >/dev/null <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: smoke-server-allow-probe
  namespace: ${kp_namespace}
  labels:
    ${KP_RUN_LABEL_KEY}: ${KUBERPLOY_E2E_RUN_ID}
    ${KP_MANAGED_BY_LABEL_KEY}: ${KP_MANAGED_BY_LABEL_VALUE}
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: smoke-server
  policyTypes:
    - Ingress
  ingress:
    - from:
        - podSelector:
            matchLabels:
              kuberploy.io/smoke-access: allowed
      ports:
        - protocol: TCP
          port: 8080
---
apiVersion: v1
kind: Pod
metadata:
  name: smoke-allowed
  namespace: ${kp_namespace}
  labels:
    app.kubernetes.io/name: smoke-allowed
    kuberploy.io/smoke-access: allowed
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
      image: ${KP_CLIENT_IMAGE}
      imagePullPolicy: IfNotPresent
      command:
        - /bin/sh
        - -ec
        - wget -T 5 -qO- 'http://smoke:8080/echo?msg=kuberploy-network-policy-ok' | grep -Fx kuberploy-network-policy-ok
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

kp_kubectl wait --namespace "${kp_namespace}" \
  --for=jsonpath='{.status.phase}'=Succeeded pod/smoke-allowed \
  --timeout=90s >/dev/null || \
  kp_die "the selected cluster did not enforce the exact NetworkPolicy allow rule"

kp_allowed_log="$(kp_kubectl logs --namespace "${kp_namespace}" smoke-allowed)"
[[ "${kp_allowed_log}" == "kuberploy-network-policy-ok" ]] || \
  kp_die "NetworkPolicy allow-path response did not match the expected marker"

printf 'Run-scoped Pod, Service, PVC, cluster-DNS, and NetworkPolicy enforcement smoke test passed.\n'
