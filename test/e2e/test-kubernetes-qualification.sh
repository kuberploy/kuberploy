#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-qualification-test.XXXXXX")"
kp_tmp="$(cd "${kp_tmp}" && pwd -P)"

kp_remove_tmp() {
  [[ "${KP_KEEP_QUALIFICATION_TMP:-false}" != "true" ]] || return 0
  if [[ -n "${kp_tmp:-}" && "${kp_tmp}" == *"/kuberploy-qualification-test."* ]]; then
    rm -rf -- "${kp_tmp}"
  fi
}
trap kp_remove_tmp EXIT

kp_kubeconfig="${kp_tmp}/kubeconfig"
kp_installer_values="${kp_tmp}/installer-values.yaml"
kp_upgrade_values="${kp_tmp}/upgrade-values.yaml"
kp_custom_certificate="${kp_tmp}/custom-certificate.pem"
kp_custom_private_key="${kp_tmp}/custom-private-key.pem"
kp_runtime_secret_initial="${kp_tmp}/runtime-secret-initial"
kp_runtime_secret_rotated="${kp_tmp}/runtime-secret-rotated"
kp_rfc2136_tsig_secret="${kp_tmp}/rfc2136-tsig-secret"
kp_registry_push_username="${kp_tmp}/registry-push-username"
kp_registry_push_password="${kp_tmp}/registry-push-password"
kp_registry_cache_username="${kp_tmp}/registry-cache-username"
kp_registry_cache_password="${kp_tmp}/registry-cache-password"
kp_registry_fault_password="${kp_tmp}/registry-fault-password"
printf 'fixture kubeconfig\n' >"${kp_kubeconfig}"
printf 'fixture: installer\n' >"${kp_installer_values}"
printf 'fixture: upgrade-from\n' >"${kp_upgrade_values}"
printf '%s\n' '-----BEGIN CERTIFICATE-----' 'fixture' '-----END CERTIFICATE-----' >"${kp_custom_certificate}"
printf '%s\n' '-----BEGIN PRIVATE KEY-----' 'fixture' '-----END PRIVATE KEY-----' >"${kp_custom_private_key}"
printf 'fixture-initial-runtime-material\n' >"${kp_runtime_secret_initial}"
printf 'fixture-rotated-runtime-material\n' >"${kp_runtime_secret_rotated}"
printf 'MDEyMzQ1Njc4OWFiY2RlZg==\n' >"${kp_rfc2136_tsig_secret}"
printf 'push-user-fixture\n' >"${kp_registry_push_username}"
printf 'push-password-fixture\n' >"${kp_registry_push_password}"
printf 'cache-user-fixture\n' >"${kp_registry_cache_username}"
printf 'cache-password-fixture\n' >"${kp_registry_cache_password}"
printf 'invalid-password-fixture\n' >"${kp_registry_fault_password}"
chmod 600 "${kp_kubeconfig}" "${kp_custom_certificate}" "${kp_custom_private_key}" \
  "${kp_runtime_secret_initial}" "${kp_runtime_secret_rotated}" "${kp_rfc2136_tsig_secret}"
chmod 600 "${kp_registry_push_username}" "${kp_registry_push_password}" \
  "${kp_registry_cache_username}" "${kp_registry_cache_password}" "${kp_registry_fault_password}"

export KP_EXPECTED_KUBECONFIG="${kp_kubeconfig}"
export KP_EXPECTED_CONTEXT="qualification-fixture"
export KP_EXPECTED_SERVER="https://api.example.invalid:6443"
export KP_COMMAND_LOG="${kp_tmp}/commands.log"
export KP_DRIVER_LOG="${kp_tmp}/drivers.log"
export KUBERPLOY_E2E_BROWSER_EXECUTABLE="/usr/bin/true"
export KUBERPLOY_E2E_BROWSER_TEST_SEAM="true"
export KUBERPLOY_E2E_HERMETIC_TEST="true"
: >"${KP_COMMAND_LOG}"
: >"${KP_DRIVER_LOG}"

cat >"${kp_tmp}/kubectl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "${1:-}" == "--kubeconfig" && "${2:-}" == "${KP_EXPECTED_KUBECONFIG}" ]]
[[ "${3:-}" == "--context" && "${4:-}" == "${KP_EXPECTED_CONTEXT}" ]]
shift 4
printf 'kubectl|%s\n' "$*" >>"${KP_COMMAND_LOG}"
if [[ "${1:-}" == "config" && "${2:-}" == "get-contexts" ]]; then
  printf '%s\n' "${KP_EXPECTED_CONTEXT}"
elif [[ "${1:-}" == "config" && "${2:-}" == "view" ]]; then
  printf '%s' "${KP_EXPECTED_SERVER}"
elif [[ "${1:-}" == "version" ]]; then
  printf '%s\n' '{"serverVersion":{"gitVersion":"v1.35.3"}}'
elif [[ "${1:-}" == "api-versions" ]]; then
  printf '%s\n' apps/v1 networking.k8s.io/v1 admissionregistration.k8s.io/v1 storage.k8s.io/v1
elif [[ "${1:-}" == "api-resources" ]]; then
  printf '%s\n' deployments.apps services ingressclasses.networking.k8s.io networkpolicies.networking.k8s.io
elif [[ "${1:-}" == "get" && "${2:-}" == "nodes" ]]; then
  printf '%s\n' '{"items":[{"status":{"nodeInfo":{"operatingSystem":"linux","architecture":"amd64"},"conditions":[{"type":"Ready","status":"True"}]}}]}'
elif [[ "${1:-}" == "get" && "${2:-}" == "storageclasses.storage.k8s.io" ]]; then
  printf '%s\n' '{"items":[{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}]}'
elif [[ "${1:-}" == "get" && "${2:-}" == "ingressclasses.networking.k8s.io" ]]; then
  printf '%s\n' '{"items":[]}'
elif [[ "${1:-}" == "get" && "${2:-}" == "jobs.batch" && " $* " == *' app.kubernetes.io/component=bootstrap-token '* ]]; then
  printf '%s\n' '{"items":[{"metadata":{"name":"kuberploy-bootstrap-token","namespace":"kuberploy-system"}}]}'
elif [[ "${1:-}" == "get" && "${2:-}" == "jobs.batch" && " $* " == *'kuberploy.io/build-operation=66666666666646668666666666666666,'* ]]; then
  printf '%s\n' '{"items":[{"metadata":{"name":"build-66666666"},"spec":{"template":{"spec":{"hostNetwork":false,"hostPID":false,"hostIPC":false,"nodeSelector":{"kuberploy.io/builder-pool":"dind"},"volumes":[{"name":"registry-push-credentials","secret":{"secretName":"push-secret"}},{"name":"registry-cache-credentials","secret":{"secretName":"cache-secret"}}],"initContainers":[{"name":"checkout","volumeMounts":[]},{"name":"dind","securityContext":{"privileged":true},"volumeMounts":[]}],"containers":[{"name":"agent","volumeMounts":[{"name":"registry-push-credentials","mountPath":"/var/run/secrets/kuberploy/registry-push","readOnly":true},{"name":"registry-cache-credentials","mountPath":"/var/run/secrets/kuberploy/registry-cache","readOnly":true}]}]}}}}]}'
elif [[ "${1:-}" == "get" && "${2:-}" == "jobs.batch" && " $* " == *'kuberploy.io/build-operation=65656565656545658565656565656565,'* ]]; then
  if [[ -f "${KP_COMMAND_LOG}.build-cancelled" ]]; then
    printf '%s\n' '{"items":[]}'
  else
    printf '%s\n' '{"items":[{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"build-65656565","namespace":"kuberploy-build-dind","uid":"uid-build-65656565","labels":{"kuberploy.io/build-operation":"65656565656545658565656565656565","kuberploy.io/build-generation":"2"}},"status":{"active":1,"conditions":[]}}]}'
  fi
elif [[ "${1:-}" == "logs" && "${4:-}" == "job/kuberploy-bootstrap-token" ]]; then
  printf '%s\n' 'KUBERPLOY_BOOTSTRAP_TOKEN=kp_bootstrap_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'
elif [[ "${1:-}" == "get" && "${2:-}" == "deployments.apps,statefulsets.apps,daemonsets.apps,services" ]]; then
  printf '%s\n' '{"items":[]}'
elif [[ "${1:-}" == "create" && " $* " == *' --dry-run=server '* ]]; then
  kp_object="$(jq -c .)"
  if [[ "$(jq -r '.metadata.name' <<<"${kp_object}")" == "qualification-quota-denial" ]]; then
    printf 'pods "qualification-quota-denial" is forbidden: exceeded quota: direct-quota, requested: requests.cpu=100000\n' >&2
  else
    printf 'denied by fixture admission\n' >&2
  fi
  exit 1
elif [[ "${1:-}" == "create" && "${2:-}" == "-f" ]]; then
  kp_object="$(jq -c .)"
  if [[ "$(jq -r '.kind' <<<"${kp_object}")" == "Namespace" ]]; then
    : >"${KP_COMMAND_LOG}.namespace-$(jq -r '.metadata.name' <<<"${kp_object}")"
  elif [[ "$(jq -r '.kind' <<<"${kp_object}")" == "ConfigMap" ]]; then
    : >"${KP_COMMAND_LOG}.configmap-$(jq -r '.metadata.name' <<<"${kp_object}")"
  elif [[ "$(jq -r '.kind' <<<"${kp_object}")" == "Secret" ]]; then
    kp_secret_name="$(jq -r '.metadata.name' <<<"${kp_object}")"
    : >"${KP_COMMAND_LOG}.secret-${kp_secret_name}"
    if [[ " $* " == *' jsonpath='* ]]; then printf 'uid-%s' "${kp_secret_name}"; exit 0; fi
  elif [[ "$(jq -r '.kind' <<<"${kp_object}")" == "Certificate" ]]; then
    : >"${KP_COMMAND_LOG}.certificate-$(jq -r '.metadata.name' <<<"${kp_object}")"
  elif [[ "$(jq -r '.kind' <<<"${kp_object}")" == "Pod" ]]; then
    : >"${KP_COMMAND_LOG}.pod-$(jq -r '.metadata.name' <<<"${kp_object}")"
  elif [[ "$(jq -r '.kind' <<<"${kp_object}")" == "Service" ]]; then
    : >"${KP_COMMAND_LOG}.service-$(jq -r '.metadata.name' <<<"${kp_object}")"
  elif [[ "$(jq -r '.kind' <<<"${kp_object}")" == "NetworkPolicy" ]]; then
    : >"${KP_COMMAND_LOG}.networkpolicy-$(jq -r '.metadata.name' <<<"${kp_object}")"
  fi
  printf 'created\n'
elif [[ "${1:-}" == "get" && ( "${2:-}" == "namespace" || "${2:-}" == "namespaces" ) ]]; then
  if [[ " $* " == *' --ignore-not-found '* && ! -f "${KP_COMMAND_LOG}.namespace-${3}" ]]; then
    :
  else
    printf '{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"%s","uid":"uid-%s","labels":{"kuberploy.io/test-run":"%s","app.kubernetes.io/managed-by":"kuberploy-e2e-harness"}}}\n' "${3}" "${3}" "${KUBERPLOY_E2E_RUN_ID}"
  fi
elif [[ "${1:-}" == "get" && "${2:-}" == "services" && " $* " == *' --namespace kuberploy-system '* ]]; then
  printf '%s\n' '{"items":[{"spec":{"type":"LoadBalancer"},"status":{"loadBalancer":{"ingress":[{"ip":"1.1.1.1"},{"ip":"8.8.8.8"}]}}}]}'
elif [[ "${1:-}" == "get" && "${2:-}" == "configmaps" && " $* " == *'kuberploy.io/application=35353535-3535-4353-8353-353535353531'* ]]; then
  printf '%s\n' '{"items":[{"metadata":{"name":"vars-immutable-25252525"},"immutable":true,"data":{"SHARED_REGION":"ap-southeast-1","RELEASE_LANE":"environment","FEATURE_PROBES":"enabled"}}]}'
elif [[ "${1:-}" == "get" && "${2:-}" == "configmaps" && " $* " == *'app.kubernetes.io/name=kuberploy,kuberploy.io/test-run='* ]]; then
  printf '%s\n' '{"items":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"kuberploy-config-fixture","namespace":"kuberploy-system","uid":"uid-kuberploy-config"},"data":{"KUBERPLOY_GIT_PROJECTION_WEBHOOK_WAKE_ENABLED":"false","KUBERPLOY_GIT_PROJECTION_POLL_INTERVAL_SECONDS":"15"}}]}'
elif [[ "${1:-}" == "get" && ( "${2:-}" == "configmap" || "${2:-}" == "configmaps" ) ]]; then
  if [[ " $* " == *' --ignore-not-found '* && ! -f "${KP_COMMAND_LOG}.configmap-${3}" ]]; then
    :
  else
    printf '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"%s","uid":"uid-%s","labels":{"kuberploy.io/test-run":"%s","app.kubernetes.io/managed-by":"kuberploy-e2e-harness"}}}\n' "${3}" "${3}" "${KUBERPLOY_E2E_RUN_ID}"
  fi
elif [[ "${1:-}" == "get" && "${2:-}" == "certificates" ]]; then
  printf '%s\n' '{"items":[{"apiVersion":"cert-manager.io/v1","kind":"Certificate","metadata":{"name":"qualification-local-acme-route","uid":"uid-route-certificate","ownerReferences":[{"kind":"Ingress","name":"qualification-local-acme","uid":"uid-route-ingress","controller":true}]},"spec":{"secretName":"qualification-local-acme-route-tls","dnsNames":["acme.fixture.test"],"issuerRef":{"name":"local-acme","kind":"ClusterIssuer"}},"status":{"revision":1,"conditions":[{"type":"Ready","status":"True"}]}}]}'
elif [[ "${1:-}" == "get" && "${2:-}" == "secret" ]]; then
  if [[ "${3:-}" == "qualification-local-acme-route-tls" ]]; then
    printf '%s\n' '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"qualification-local-acme-route-tls","namespace":"kuberploy-direct","uid":"uid-route-secret","labels":{"controller.cert-manager.io/fao":"true"}},"type":"kubernetes.io/tls","data":{"tls.crt":"Zml4dHVyZS1jZXJ0aWZpY2F0ZQo=","tls.key":"bmV2ZXItcGVyc2lzdGVk"}}'
  elif [[ " $* " == *' --ignore-not-found '* && ! -f "${KP_COMMAND_LOG}.secret-${3}" ]]; then :; else
    printf '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"%s","uid":"uid-%s","labels":{"kuberploy.io/test-run":"%s","app.kubernetes.io/managed-by":"kuberploy-e2e-harness"}}}\n' "${3}" "${3}" "${KUBERPLOY_E2E_RUN_ID}"
  fi
elif [[ "${1:-}" == "get" && "${2:-}" == "certificate" ]]; then
  if [[ " $* " == *' --ignore-not-found '* && ! -f "${KP_COMMAND_LOG}.certificate-${3}" ]]; then
    :
  else
    kp_revision=1; [[ ! -f "${KP_COMMAND_LOG}.renew" ]] || kp_revision=2
    printf '{"apiVersion":"cert-manager.io/v1","kind":"Certificate","metadata":{"name":"%s","uid":"uid-route-certificate"},"status":{"revision":%s,"conditions":[{"type":"Ready","status":"True"}]}}\n' "${3}" "${kp_revision}"
  fi
elif [[ "${1:-}" == "get" && "${2:-}" == "clusterissuer" ]]; then
  printf '{"metadata":{"name":"%s"},"spec":{"acme":{"server":"%s"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}\n' "${3}" "${KUBERPLOY_E2E_LOCAL_ACME_DIRECTORY_URL}"
elif [[ "${1:-}" == "get" && "${2:-}" == "daemonset" && "${3:-}" == "cilium" ]]; then
  printf '%s\n' '{"metadata":{"name":"cilium","namespace":"kube-system","uid":"uid-cilium"},"spec":{"template":{"spec":{"containers":[{"name":"cilium-agent","image":"quay.io/cilium/cilium@sha256:1111111111111111111111111111111111111111111111111111111111111111"}]}}},"status":{"desiredNumberScheduled":1,"numberReady":1}}'
elif [[ "${1:-}" == "get" && "${2:-}" == "resourcequota" && "${3:-}" == "direct-quota" ]]; then
  printf '%s\n' '{"metadata":{"name":"direct-quota","namespace":"kuberploy-direct","uid":"uid-direct-quota"},"status":{"hard":{"requests.cpu":"2"},"used":{"requests.cpu":"100m"}}}'
elif [[ "${1:-}" == "get" && "${2:-}" == "pod" && "${3:-}" == qualification-network-* ]]; then
  if [[ " $* " == *' --ignore-not-found '* && ! -f "${KP_COMMAND_LOG}.pod-${3}" ]]; then :; else
    printf '{"apiVersion":"v1","kind":"Pod","metadata":{"name":"%s","namespace":"kuberploy-e2e-%s","uid":"uid-%s","labels":{"kuberploy.io/test-run":"%s","app.kubernetes.io/managed-by":"kuberploy-e2e-harness"}},"status":{"phase":"Succeeded"}}\n' "${3}" "${KUBERPLOY_E2E_RUN_ID}" "${3}" "${KUBERPLOY_E2E_RUN_ID}"
  fi
elif [[ "${1:-}" == "get" && "${2:-}" == "service" && "${3:-}" == "qualification-network" ]]; then
  if [[ " $* " == *' --ignore-not-found '* && ! -f "${KP_COMMAND_LOG}.service-${3}" ]]; then :; else
    printf '{"apiVersion":"v1","kind":"Service","metadata":{"name":"%s","namespace":"kuberploy-e2e-%s","uid":"uid-%s","labels":{"kuberploy.io/test-run":"%s","app.kubernetes.io/managed-by":"kuberploy-e2e-harness"}}}\n' "${3}" "${KUBERPLOY_E2E_RUN_ID}" "${3}" "${KUBERPLOY_E2E_RUN_ID}"
  fi
elif [[ "${1:-}" == "get" && "${2:-}" == "networkpolicy" && "${3:-}" == qualification-network-* ]]; then
  if [[ " $* " == *' --ignore-not-found '* && ! -f "${KP_COMMAND_LOG}.networkpolicy-${3}" ]]; then :; else
    printf '{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy","metadata":{"name":"%s","namespace":"kuberploy-e2e-%s","uid":"uid-%s","labels":{"kuberploy.io/test-run":"%s","app.kubernetes.io/managed-by":"kuberploy-e2e-harness"}}}\n' "${3}" "${KUBERPLOY_E2E_RUN_ID}" "${3}" "${KUBERPLOY_E2E_RUN_ID}"
  fi
elif [[ "${1:-}" == "get" && "${2:-}" == "pod" && "${3:-}" == kp-rfc2136-* ]]; then
  if [[ " $* " == *' --ignore-not-found '* && ! -f "${KP_COMMAND_LOG}.pod-${3}" ]]; then :; else
    printf '{"apiVersion":"v1","kind":"Pod","metadata":{"name":"%s","uid":"uid-%s","labels":{"kuberploy.io/test-run":"%s","app.kubernetes.io/managed-by":"kuberploy-e2e-harness"}},"status":{"phase":"Succeeded"}}\n' "${3}" "${3}" "${KUBERPLOY_E2E_RUN_ID}"
  fi
elif [[ "${1:-}" == "get" && "${2:-}" == "service" && "${3:-}" == kp-rfc2136-* ]]; then
  if [[ " $* " == *' --ignore-not-found '* && ! -f "${KP_COMMAND_LOG}.service-${3}" ]]; then :; else
    printf '{"apiVersion":"v1","kind":"Service","metadata":{"name":"%s","uid":"uid-%s","labels":{"kuberploy.io/test-run":"%s","app.kubernetes.io/managed-by":"kuberploy-e2e-harness"}}}\n' "${3}" "${3}" "${KUBERPLOY_E2E_RUN_ID}"
  fi
elif [[ "${1:-}" == "get" && "${2:-}" == "networkpolicy" && "${3:-}" == kp-rfc2136-* ]]; then
  if [[ " $* " == *' --ignore-not-found '* && ! -f "${KP_COMMAND_LOG}.networkpolicy-${3}" ]]; then :; else
    printf '{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy","metadata":{"name":"%s","uid":"uid-%s","labels":{"kuberploy.io/test-run":"%s","app.kubernetes.io/managed-by":"kuberploy-e2e-harness"}}}\n' "${3}" "${3}" "${KUBERPLOY_E2E_RUN_ID}"
  fi
elif [[ "${1:-}" == "get" && "${2:-}" == "pod" ]]; then
  kp_uid="uid-${3}-before"; [[ ! -f "${KP_COMMAND_LOG}.restart-${3}" ]] || kp_uid="uid-${3}-after"
  kp_controller="${3%-0}"; kp_component="${kp_controller#kuberploy-}"
  printf '{"apiVersion":"v1","kind":"Pod","metadata":{"uid":"%s","labels":{"app.kubernetes.io/name":"%s","app.kubernetes.io/instance":"%s"},"ownerReferences":[{"kind":"StatefulSet","name":"%s","uid":"uid-%s-controller","controller":true}]},"status":{"conditions":[{"type":"Ready","status":"True"}]}}\n' "${kp_uid}" "${kp_component}" "${kp_controller}" "${kp_controller}" "${kp_controller}"
elif [[ "${1:-}" == "get" && "${2:-}" == "statefulset" ]]; then
  printf '{"metadata":{"uid":"uid-%s-controller"}}\n' "${3}"
elif [[ "${1:-}" == "get" && "${2:-}" == "application" && "${3:-}" == kp-h-* ]]; then
  printf '%s\n' '{"metadata":{"name":"kp-h-33333333333343338333333333333333","labels":{"app.kubernetes.io/component":"approved-helm-application","kuberploy.io/application-id":"33333333-3333-4333-8333-333333333333","kuberploy.io/environment-id":"22222222-2222-4222-8222-222222222222"},"annotations":{"kuberploy.io/helm-release-revision":"41414141-4141-4141-8141-414141414141"}},"spec":{"source":{"targetRevision":"abababababababababababababababababababab"}},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"},"resources":[{"kind":"Deployment","status":"Synced","health":{"status":"Healthy"}}]}}'
elif [[ "${1:-}" == "get" && "${2:-}" == "application" ]]; then
  kp_deployment=99999999-9999-4999-8999-999999999999; kp_application=33333333-3333-4333-8333-333333333333; kp_revision=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  if [[ -f "${KP_COMMAND_LOG}.image-tag-deployed" && "${3:-}" == kp-d-99999999999949998999999999999999 ]]; then kp_revision=7878787878787878787878787878787878787878; fi
  if [[ "${3:-}" == kp-d-27272727272742728272272727272721 ]]; then kp_deployment=27272727-2727-4272-8272-272727272721; kp_application=35353535-3535-4353-8353-353535353531; kp_revision=2828282828282828282828282828282828282828
  elif [[ "${3:-}" == kp-d-27272727272742728272272727272722 ]]; then kp_deployment=27272727-2727-4272-8272-272727272722; kp_application=35353535-3535-4353-8353-353535353533; kp_revision=2828282828282828282828282828282828282828; fi
  printf '{"metadata":{"name":"%s","labels":{"app.kubernetes.io/managed-by":"kuberploy","kuberploy.io/deployment-id":"%s","kuberploy.io/application-id":"%s","kuberploy.io/project-id":"11111111-1111-4111-8111-111111111111","kuberploy.io/environment-id":"22222222-2222-4222-8222-222222222221"},"annotations":{"kuberploy.io/runtime-chart-digest":"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}},"status":{"sync":{"status":"Synced","revision":"%s"},"health":{"status":"Healthy"},"resources":[{"kind":"Deployment","status":"Synced","health":{"status":"Healthy"}}]}}\n' "${3}" "${kp_deployment}" "${kp_application}" "${kp_revision}"
elif [[ "${1:-}" == "get" && "${2:-}" == "deployment" && " $* " == *'kuberploy.io/application=35353535-3535-4353-8353-353535353531'* ]]; then
  printf '%s\n' '{"items":[{"metadata":{"name":"config-edge","generation":2},"spec":{"replicas":2,"template":{"metadata":{},"spec":{"automountServiceAccountToken":false,"terminationGracePeriodSeconds":30,"nodeSelector":{"kubernetes.io/os":"linux"},"affinity":{"podAntiAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":[{"topologyKey":"kubernetes.io/hostname","labelSelector":{"matchLabels":{"kuberploy.io/application":"35353535-3535-4353-8353-353535353531"}}}]},"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/os","operator":"In","values":["linux"]}]}]},"preferredDuringSchedulingIgnoredDuringExecution":[{"weight":70,"preference":{"matchExpressions":[{"key":"topology.kubernetes.io/zone","operator":"Exists"}]}}]}},"topologySpreadConstraints":[{"maxSkew":1,"topologyKey":"topology.kubernetes.io/zone","whenUnsatisfiable":"ScheduleAnyway","labelSelector":{"matchLabels":{"kuberploy.io/application":"35353535-3535-4353-8353-353535353531"}}}],"tolerations":[{"key":"qualification.kuberploy.io/workload","operator":"Equal","value":"application","effect":"NoSchedule"}],"containers":[{"securityContext":{"allowPrivilegeEscalation":false},"resources":{"requests":{"cpu":"50m"}},"readinessProbe":{"httpGet":{"path":"/ready"}},"livenessProbe":{"httpGet":{"path":"/health"}},"env":[{"name":"SHARED_REGION","valueFrom":{"configMapKeyRef":{"name":"vars-immutable-25252525"}}},{"name":"RELEASE_LANE","valueFrom":{"configMapKeyRef":{"name":"vars-immutable-25252525"}}},{"name":"FEATURE_PROBES","valueFrom":{"configMapKeyRef":{"name":"vars-immutable-25252525"}}}]}]}}},"status":{"observedGeneration":2}}]}'
elif [[ "${1:-}" == "get" && "${2:-}" == "deployments" && " $* " == *'kuberploy.io/deployment-id=99999999-9999-4999-8999-999999999999 '* ]]; then
  printf '%s\n' '{"items":[{"metadata":{"name":"qualification-app","generation":2,"labels":{"kuberploy.io/application-id":"33333333-3333-4333-8333-333333333333","kuberploy.io/deployment-id":"99999999-9999-4999-8999-999999999999"}},"spec":{"template":{"spec":{"containers":[{"image":"registry.fixture.test/probe@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}}},"status":{"observedGeneration":2,"availableReplicas":1}}]}'
elif [[ "${1:-}" == "get" && "${2:-}" == "sealedsecrets.bitnami.com" ]]; then
  if [[ " $* " == *'kuberploy.io/secret-version=2 '* ]]; then
    kp_version=2; kp_uid=uid-runtime-secret-v2; kp_name=qualification-runtime-v2
  else
    kp_version=1; kp_uid=uid-runtime-secret-v1; kp_name=qualification-runtime-v1
  fi
  printf '{"items":[{"apiVersion":"bitnami.com/v1alpha1","kind":"SealedSecret","metadata":{"name":"%s","namespace":"kuberploy-direct","uid":"%s","labels":{"kuberploy.io/secret-binding":"91919191-9191-4191-8191-919191919191","kuberploy.io/secret-version":"%s"}},"spec":{"encryptedData":{"password":"ciphertext-never-persisted"},"template":{"metadata":{"name":"%s"},"type":"Opaque","immutable":true}}}]}' "${kp_name}" "${kp_uid}" "${kp_version}" "${kp_name}"
elif [[ "${1:-}" == "get" && "${2:-}" == "deployments" && " $* " == *' kuberploy.io/application-id=33333333-3333-4333-8333-333333333333 '* ]]; then
  kp_name=qualification-runtime-v1; [[ ! -f "${KP_COMMAND_LOG}.runtime-secret-rotated" ]] || kp_name=qualification-runtime-v2
  printf '{"items":[{"metadata":{"name":"qualification-app","generation":2},"spec":{"template":{"spec":{"containers":[{"env":[{"name":"QUALIFICATION_PASSWORD","valueFrom":{"secretKeyRef":{"name":"%s","key":"password"}}}]}]}}},"status":{"observedGeneration":2,"availableReplicas":1}}]}' "${kp_name}"
elif [[ "${1:-}" == "patch" && "${2:-}" == "certificate" ]]; then
  : >"${KP_COMMAND_LOG}.renew"
  printf 'patched\n'
elif [[ "${1:-}" == "patch" && "${2:-}" == "secret" ]]; then
  kp_patch="$(jq -c .)"
  jq -e --arg uid "uid-${3}" --arg run "${KUBERPLOY_E2E_RUN_ID}" '
    any(.[];.op=="test" and .path=="/metadata/uid" and .value==$uid) and
    any(.[];.op=="test" and .path=="/metadata/labels/kuberploy.io~1test-run" and .value==$run) and
    any(.[];.op=="replace" and .path=="/data/username") and
    any(.[];.op=="replace" and .path=="/data/password")
  ' <<<"${kp_patch}" >/dev/null
  printf 'secret/%s\n' "${3}"
elif [[ "${1:-}" == "auth" && "${2:-}" == "can-i" ]]; then
  printf 'no\n'
  exit 1
elif [[ "${1:-}" == "logs" && "${4:-}" == "qualification-network-allowed" ]]; then
  printf 'kuberploy-network-policy-ok\n'
elif [[ "${1:-}" == "logs" && "${4:-}" == "qualification-network-denied" ]]; then
  :
elif [[ "${1:-}" == "logs" && "${4:-}" == kp-rfc2136-query-* ]]; then
  printf 'Name: config-edge-%s.qualification.test\nAddress: 1.1.1.1\n' "${KUBERPLOY_E2E_RUN_ID}"
elif [[ "${1:-}" == "delete" ]]; then
  if [[ "${2:-}" == "pod" ]]; then : >"${KP_COMMAND_LOG}.restart-${3}"; fi
  printf 'deleted\n'
elif [[ "${1:-}" == "get" && "${2:-}" == *,* ]]; then
  if [[ " $* " == *' app.kubernetes.io/instance=kuberploy-qualification '* ]]; then
    printf '%s\n' '{"items":[{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"namespace":"argocd","name":"argocd-server","uid":"uid-installer-server","resourceVersion":"1","labels":{"app.kubernetes.io/instance":"kuberploy-qualification"}}}]}'
  else
    printf '%s\n' '{"items":[]}'
  fi
else
  printf 'fixture-ok\n'
fi
EOF

cat >"${kp_tmp}/helm" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "${1:-}" == "--kubeconfig" && "${2:-}" == "${KP_EXPECTED_KUBECONFIG}" ]]
[[ "${3:-}" == "--kube-context" && "${4:-}" == "${KP_EXPECTED_CONTEXT}" ]]
shift 4
printf 'helm|%s\n' "$*" >>"${KP_COMMAND_LOG}"
if [[ " $* " == *' template '* ]]; then
  for kp_name in control-plane edge; do
    printf '%s\n' '---' 'apiVersion: argoproj.io/v1alpha1' 'kind: Application' \
      "metadata:" "  name: kuberploy-${kp_name}" \
      '  annotations:' \
      '    kuberploy.io/expected-package-version: "0.1.0-rc.290"' \
      'spec:' '  source:' \
      '    targetRevision: "0123456789abcdef0123456789abcdef01234567"'
  done
elif [[ " ${1:-} " == ' upgrade ' ]]; then
  if [[ " $* " == *' --install '* ]]; then
    printf '1\n' >"${KP_COMMAND_LOG}.helm-revision"
    rm -f -- "${KP_COMMAND_LOG}.upgraded"
  else
    printf '2\n' >"${KP_COMMAND_LOG}.helm-revision"
    : >"${KP_COMMAND_LOG}.upgraded"
  fi
  printf 'fixture-ok\n'
elif [[ " ${1:-} " == ' rollback ' ]]; then
  printf '3\n' >"${KP_COMMAND_LOG}.helm-revision"
  rm -f -- "${KP_COMMAND_LOG}.upgraded"
  printf 'fixture-ok\n'
elif [[ " ${1:-} " == ' history ' ]]; then
  kp_revision=1
  [[ ! -f "${KP_COMMAND_LOG}.helm-revision" ]] || kp_revision="$(<"${KP_COMMAND_LOG}.helm-revision")"
  printf '[{"revision":%s,"status":"deployed"}]\n' "${kp_revision}"
elif [[ " $* " == *' status '* ]]; then
  printf '%s\n' '{"info":{"status":"deployed"}}'
else
  printf 'fixture-ok\n'
fi
EOF
cat >"${kp_tmp}/curl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
kp_output="" kp_method="GET" kp_url="" kp_body="" kp_denied="false" kp_revoked="false" kp_invalid_signature="false" kp_dump_header="" kp_cookie_jar="" kp_delivery_header=""
kp_cookie_header="false" kp_csrf_header="false" kp_idempotency_header="false"
while (($#)); do
  case "$1" in
    --output) kp_output="$2"; shift 2 ;;
    -o) kp_output="$2"; shift 2 ;;
    --request) kp_method="$2"; shift 2 ;;
    -X) kp_method="$2"; shift 2 ;;
    --dump-header) kp_dump_header="$2"; shift 2 ;;
    -D) kp_dump_header="$2"; shift 2 ;;
    -c) kp_cookie_jar="$2"; shift 2 ;;
    --header|-H)
      [[ "$2" != *denied* ]] || kp_denied="true"
      [[ "$2" != *BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB* ]] || kp_revoked="true"
      [[ "$2" != *CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC* ]] || kp_denied="true"
      [[ "$2" != 'X-Hub-Signature-256: sha256=0000000000000000000000000000000000000000000000000000000000000000' ]] || kp_invalid_signature="true"
      [[ "$2" != X-GitHub-Delivery:* ]] || kp_delivery_header="${2#X-GitHub-Delivery: }"
      [[ "$2" != Cookie:* ]] || kp_cookie_header="true"
      [[ "$2" != 'X-CSRF-Token: fixture-csrf' ]] || kp_csrf_header="true"
      [[ "$2" != 'Idempotency-Key: qualification-'*'-40-source-build-cancel-live-build' ]] || kp_idempotency_header="true"
      shift 2 ;;
    --write-out) shift 2 ;;
    --data-binary)
      [[ "${kp_method}" != GET ]] || kp_method=POST
      if [[ "$2" == @* ]]; then kp_body="$(<"${2#@}")"; else kp_body="$2"; fi
      shift 2 ;;
    -w) shift 2 ;;
    --silent|--show-error|-sS|-fsS) shift ;;
    http*) kp_url="$1"; shift ;;
    *) shift ;;
  esac
done
if [[ -n "${kp_cookie_jar}" ]]; then
  printf '.fixture.test\tTRUE\t/\tTRUE\t0\tkuberploy_session\tfixture-session\n.fixture.test\tTRUE\t/\tTRUE\t0\tkuberploy_csrf\tfixture-csrf\n' >"${kp_cookie_jar}"
fi
if [[ -n "${kp_dump_header}" ]]; then
  # HTTP/2 clients commonly lowercase response header names. Keep the fixture
  # honest so the qualification driver proves portable CSRF extraction.
  printf 'HTTP/1.1 200 OK\r\nx-csrf-token: fixture-csrf\r\nX-Kuberploy-Qualification: passed\r\n\r\n' >"${kp_dump_header}"
fi
printf 'curl|%s|%s\n' "${kp_method}" "${kp_url}" >>"${KP_COMMAND_LOG}"
if [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/auth/bootstrap ]]; then
  jq -e 'has("email") and (.email | test("^qualification-admin-[a-z0-9-]+@example\\.test$")) and
    has("displayName") and has("password")' <<<"${kp_body}" >/dev/null || {
    printf 'bootstrap fixture received stale non-email identity payload\n' >&2
    exit 1
  }
  : >"${KP_COMMAND_LOG}.email-bootstrap"
  printf '%s\n' '{"id":"10101010-1010-4010-8010-101010101010"}' >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/auth/logout ]]; then : >"${KP_COMMAND_LOG}.logged-out"; : >"${kp_output}"; printf '204'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/me && -f "${KP_COMMAND_LOG}.logged-out" ]]; then printf '{}' >"${kp_output}"; printf '401'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/me ]]; then printf '{"id":"10101010-1010-4010-8010-101010101010","role":"platform-admin","authentication":{"kind":"session"}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/auth/login ]]; then
  jq -e 'has("email") and (.email | test("^qualification-(admin|developer)-[a-z0-9-]+@example\\.test$")) and
    has("password")' <<<"${kp_body}" >/dev/null || {
    printf 'login fixture received stale non-email identity payload\n' >&2
    exit 1
  }
  : >"${KP_COMMAND_LOG}.email-login"
  rm -f -- "${KP_COMMAND_LOG}.logged-out"
  printf '{"id":"10101010-1010-4010-8010-101010101010"}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/users/invitations ]]; then
  jq -e 'has("email") and (.email | test("^qualification-developer-[a-z0-9-]+@example\\.test$")) and
    (keys | . == ["email"])' <<<"${kp_body}" >/dev/null || {
    printf 'invitation fixture received stale display-name identity payload\n' >&2
    exit 1
  }
  : >"${KP_COMMAND_LOG}.email-invitation"
  printf '{"token":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}' >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/auth/invitations/accept ]]; then printf '{"id":"20202020-2020-4020-8020-202020202020"}' >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/teams ]]; then printf '{"id":"30303030-3030-4030-8030-303030303030"}' >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/teams/*/members ]]; then
  jq -e '(.userId | test("^[0-9a-f-]{36}$")) and (.role == "member" or .role == "owner")' <<<"${kp_body}" >/dev/null || {
    printf 'team member fixture received invalid role payload\n' >&2
    exit 1
  }
  jq -c --arg team '30303030-3030-4030-8030-303030303030' \
    '{teamId:$team,userId:.userId,role:.role}' <<<"${kp_body}" >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "DELETE" && "${kp_url}" == */v1/teams/*/members/20202020-2020-4020-8020-202020202020 ]]; then printf '{}' >"${kp_output}"; printf '204'
elif [[ "${kp_method}" == "DELETE" && "${kp_url}" == */v1/teams/*/members/* ]]; then printf '{}' >"${kp_output}"; printf '409'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/github/installations ]]; then printf '{"id":"40404040-4040-4040-8040-404040404040"}' >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/github/installations ]]; then printf '{"items":[{"id":"50505050-5050-4050-8050-505050505050","githubInstallationId":12345}]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/github/installations/50505050-*/repositories ]]; then printf '{"items":[{"id":"51515151-5151-4151-8151-515151515151","installationId":"50505050-5050-4050-8050-505050505050","githubRepositoryId":67890,"lifecycle":"active"}]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "PATCH" && "${kp_url}" == */sharing ]]; then printf '{}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && ( "${kp_url}" == */openapi.json || "${kp_url}" == */openapi.yaml ) ]]; then printf '{"openapi":"3.2.0","paths":{"/v1/auth/login":{"post":{"operationId":"loginWithLocalPassword"}}}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */openapi-agent.json ]]; then printf '{"operations":[]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */arazzo.yaml ]]; then printf 'sourceDescription: fixture\n' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */docs/ ]]; then printf 'url: "/openapi.yaml"\n' >"${kp_output}"; printf '200'
elif [[ "${kp_revoked}" == "true" && "${kp_method}" == "GET" ]]; then
  printf '%s\n' '{"status":401,"code":"Unauthenticated"}' >"${kp_output}"; printf '401'
elif [[ "${kp_denied}" == "true" && "${kp_method}" == "GET" ]]; then
  printf '%s\n' '{"status":404,"code":"NotFound","detail":"resource is not visible"}' >"${kp_output}"; printf '404'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/workloads/*/logs* ]]; then
  printf '%s\n' '{"items":[{"message":"fixture log"}]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/workloads/*/events* ]]; then
  printf '%s\n' '{"items":[{"reason":"Ready"}]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/metrics/query-range* ]]; then
  printf '%s\n' '{"series":[{"values":[[1,1]]}]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/capabilities ]]; then
  kp_git=true; [[ "${KP_DISABLE_GIT_CAPABILITY:-false}" != "true" ]] || kp_git=false
  printf '{"features":{"git":%s,"gitops":true,"argo":true,"argoCD":true,"deploymentRollbacks":true,"builder":true,"builds":true,"autoDeploy":true,"githubAppSetup":true,"helmDeployments":true,"edge":true,"traefik":true,"sslip":true,"externalDNS":true,"externalDNSConfiguration":true,"variableSets":true,"traefikMiddlewares":true,"middlewareProfiles":true,"certManager":true,"customCertificates":true,"certificateIssuerCatalog":true,"registry":true,"managedRegistry":true,"imageTagResolution":true,"logs":true,"monitoring":true,"metrics":true,"secretBindings":true},"actions":["projects:create","environments:create","applications:create","deployments:create","deployments:update","operations:read","builds:read","builds:cancel","builds:retry","build-definitions:create","helm.read","helm.deploy","deployment-config:read","deployment-config:preview","deployment-config:write","certificate-bindings:read","certificate-bindings:bind","certificate-bindings:create","registry:read","registry-cleanup:preview","registry-cleanup:execute","logs:read","metrics:read","secret-bindings:read","secret-bindings:bind","secret-bindings:create","secret-bindings:rotate","platform-releases:read"],"capabilities":[],"limits":{}}\n' "${kp_git}" >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/audit-events\?targetType=deployment* ]]; then
  printf '%s\n' '{"items":[{"id":"71717171-7171-4171-8171-717171717171","actorId":"10101010-1010-4010-8010-101010101010","action":"deployment.config.accepted","targetType":"deployment","targetId":"99999999-9999-4999-8999-999999999999","outcome":"accepted","requestId":"qualification-1","createdAt":"2026-08-09T10:02:00Z"},{"id":"72727272-7272-4272-8272-727272727272","actorId":"10101010-1010-4010-8010-101010101010","action":"deployment.config.accepted","targetType":"deployment","targetId":"99999999-9999-4999-8999-999999999999","outcome":"accepted","requestId":"qualification-2","createdAt":"2026-08-09T10:00:00Z"}]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/meta ]]; then
  if [[ -f "${KP_COMMAND_LOG}.upgraded" ]]; then kp_version=0.2.0; else kp_version=0.1.0; fi
  printf '{"version":"%s"}\n' "${kp_version}" >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/platform/releases/latest ]]; then
  printf 'HTTP/1.1 200 OK\r\nETag: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"\r\n\r\n' >"${kp_dump_header}"
  printf '%s\n' '{"currentVersion":"0.1.0","updateAvailable":true,"compatibility":{"status":"compatible"},"release":{"version":"0.2.0","manifestDigest":"sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/projects ]]; then
  kp_count_file="${KP_COMMAND_LOG}.projects"; kp_count=0; [[ ! -f "${kp_count_file}" ]] || kp_count="$(<"${kp_count_file}")"
  kp_count=$((kp_count+1)); printf '%s' "${kp_count}" >"${kp_count_file}"
  if [[ "${kp_count}" -eq 1 ]]; then kp_id=11111111-1111-4111-8111-111111111111; else kp_id=11111111-1111-4111-8111-111111111112; fi
  printf '{"id":"%s"}\n' "${kp_id}" >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/projects/11111111-*/grants ]]; then
  printf '%s\n' '{"id":"61616161-6161-4161-8161-616161616161"}' >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/projects/11111111-*/service-accounts ]]; then
  if [[ "${kp_url}" == */11111111-1111-4111-8111-111111111112/* ]]; then kp_id=62626262-6262-4262-8262-626262626263; else kp_id=62626262-6262-4262-8262-626262626262; fi
  printf '{"id":"%s","name":"qualification-workflow"}\n' "${kp_id}" >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/service-accounts/62626262-6262-4262-8262-626262626263/tokens ]]; then
  printf '%s\n' '{"token":"kp_sa_CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","tokenRecord":{"id":"63636363-6363-4363-8363-636363636363"}}' >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/service-accounts/62626262-*/tokens ]]; then
  kp_token_count_file="${KP_COMMAND_LOG}.service-account-tokens"; kp_token_count=0
  [[ ! -f "${kp_token_count_file}" ]] || kp_token_count="$(<"${kp_token_count_file}")"
  kp_token_count=$((kp_token_count+1)); printf '%s' "${kp_token_count}" >"${kp_token_count_file}"
  if [[ "${kp_token_count}" -eq 1 ]]; then kp_token='kp_sa_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'; kp_token_id=63636363-6363-4363-8363-636363636361
  else kp_token='kp_sa_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'; kp_token_id=63636363-6363-4363-8363-636363636362; fi
  printf '{"token":"%s","tokenRecord":{"id":"%s"}}\n' "${kp_token}" "${kp_token_id}" >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "DELETE" && "${kp_url}" == */v1/service-accounts/62626262-*/tokens/63636363-* ]]; then
  : >"${kp_output}"; printf '204'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/projects/11111111-* ]]; then
  printf '%s\n' '{"id":"11111111-1111-4111-8111-111111111111","name":"Qualification"}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/environments ]]; then
  kp_count_file="${KP_COMMAND_LOG}.environments"; kp_count=0; [[ ! -f "${kp_count_file}" ]] || kp_count="$(<"${kp_count_file}")"
  kp_count=$((kp_count+1)); printf '%s' "${kp_count}" >"${kp_count_file}"
  if [[ "${kp_count}" -eq 1 ]]; then kp_id=22222222-2222-4222-8222-222222222221; kp_namespace=kuberploy-direct; else kp_id=22222222-2222-4222-8222-222222222222; kp_namespace=kuberploy-protected; fi
  printf '{"id":"%s","namespace":"%s"}\n' "${kp_id}" "${kp_namespace}" >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/environments/*/variable-sets ]]; then
  printf '%s\n' '{"items":[{"scope":"project","etag":"","present":false},{"scope":"environment","etag":"","present":false}]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */variable-sets/*/preview ]]; then
  printf '%s\n' '{"previewToken":"VVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVV"}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "PUT" && "${kp_url}" == */variable-sets/project ]]; then
  printf '%s\n' '{"id":"24242424-2424-4242-8242-242424242421","status":"queued"}' >"${kp_output}"; printf '202'
elif [[ "${kp_method}" == "PUT" && "${kp_url}" == */variable-sets/environment ]]; then
  printf '%s\n' '{"id":"24242424-2424-4242-8242-242424242422","status":"queued"}' >"${kp_output}"; printf '202'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/applications ]]; then
  if [[ "${kp_body}" == *'"slug":"config-edge"'* ]]; then kp_id=35353535-3535-4353-8353-353535353531
  elif [[ "${kp_body}" == *'"slug":"config-edge-other"'* ]]; then kp_id=35353535-3535-4353-8353-353535353532
  elif [[ "${kp_body}" == *'"slug":"external-dns-edge"'* ]]; then kp_id=35353535-3535-4353-8353-353535353533
  else kp_id=33333333-3333-4333-8333-333333333333; fi
  printf '{"id":"%s"}\n' "${kp_id}" >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/applications/35353535-3535-4353-8353-353535353531/sslip-hostname* ]]; then
  printf '%s\n' '{"mode":"sslip","hostname":"config-edge.1-1-1-1.sslip.io","source":"service-ip","observedAt":"2026-08-10T00:00:00Z"}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/deployments/image-resolution-preview ]]; then
  printf '%s\n' '{"requestedImage":"registry.fixture.test/probe:candidate-66666666666646668666666666666666-g1-ffffffffffff","immutableImage":"registry.fixture.test/probe@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","resolved":true}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/deployments ]]; then
  if [[ "${kp_body}" == *'35353535-3535-4353-8353-353535353532'* ]]; then
    printf '%s\n' '{"status":422,"code":"ValidationFailed","fields":[{"pointer":"/runtime/affinity/podAntiAffinity/requiredDuringSchedulingIgnoredDuringExecution/0/labelSelector","code":"ApplicationSelectorRequired"}]}' >"${kp_output}"; printf '422'; exit 0
  elif [[ "${kp_body}" == *'"kuberploy.io/builder"'* ]]; then
    printf '%s\n' '{"status":422,"code":"ValidationFailed","fields":[{"pointer":"/runtime/nodeSelector/kuberploy.io~1builder","code":"ReservedSchedulingKey"}]}' >"${kp_output}"; printf '422'; exit 0
  elif [[ "${kp_body}" == *'"applicationId":"35353535-3535-4353-8353-353535353531"'* ]]; then
    printf '%s\n' '{"id":"26262626-2626-4262-8262-262626262621","targetId":"27272727-2727-4272-8272-272727272721","status":"queued"}' >"${kp_output}"; printf '202'; exit 0
  elif [[ "${kp_body}" == *'"applicationId":"35353535-3535-4353-8353-353535353533"'* ]]; then
    printf '%s\n' '{"id":"26262626-2626-4262-8262-262626262622","targetId":"27272727-2727-4272-8272-272727272722","status":"queued"}' >"${kp_output}"; printf '202'; exit 0
  elif [[ "${kp_body}" == *'"expectedImmutableImage"'* ]]; then
    : >"${KP_COMMAND_LOG}.image-tag-deployed"
    printf '%s\n' '{"id":"78787878-7878-4878-8878-787878787878","targetId":"99999999-9999-4999-8999-999999999999","status":"queued"}' >"${kp_output}"; printf '202'; exit 0
  fi
  kp_count_file="${KP_COMMAND_LOG}.deployments"; kp_count=0; [[ ! -f "${kp_count_file}" ]] || kp_count="$(<"${kp_count_file}")"
  kp_count=$((kp_count+1)); printf '%s' "${kp_count}" >"${kp_count_file}"
  if [[ "${kp_count}" -eq 1 ]]; then
    kp_id=44444444-4444-4444-8444-444444444444; kp_target=99999999-9999-4999-8999-999999999999
  elif [[ "${kp_count}" -eq 2 ]]; then
    kp_id=55555555-5555-4555-8555-555555555555; kp_target=98989898-9898-4989-8989-989898989898
  else
    kp_id=12121212-1212-4212-8212-121212121212; kp_target=99999999-9999-4999-8999-999999999999
  fi
  printf '{"id":"%s","targetId":"%s","status":"queued"}\n' "${kp_id}" "${kp_target}" >"${kp_output}"; printf '202'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/deployments/*/rollback ]]; then
  kp_count_file="${KP_COMMAND_LOG}.rollbacks"; kp_count=0; [[ ! -f "${kp_count_file}" ]] || kp_count="$(<"${kp_count_file}")"
  kp_count=$((kp_count+1)); printf '%s' "${kp_count}" >"${kp_count_file}"
  if [[ "${kp_count}" -eq 1 ]]; then kp_id=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa; else kp_id=acacacac-acac-4cac-8cac-acacacacacac; fi
  printf '{"id":"%s","status":"queued"}\n' "${kp_id}" >"${kp_output}"; printf '202'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/applications/*/build-definitions ]]; then
  printf '%s\n' '{"id":"52525252-5252-4252-8252-525252525252","applicationId":"33333333-3333-4333-8333-333333333333","installationId":"50505050-5050-4050-8050-505050505050","repositoryId":"51515151-5151-4151-8151-515151515151"}' >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/webhooks/github && "${kp_invalid_signature}" == "true" ]]; then
  printf '%s\n' '{"status":401,"code":"Unauthenticated"}' >"${kp_output}"; printf '401'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/webhooks/github ]]; then
  kp_delivery_file="${KP_COMMAND_LOG}.github-deliveries"
  kp_delivery_record="${kp_delivery_header}|$(printf '%s' "${kp_body}" | jq -r '.after')"
  touch "${kp_delivery_file}"
  if ! grep -Fxq "${kp_delivery_record}" "${kp_delivery_file}"; then
    printf '%s\n' "${kp_delivery_record}" >>"${kp_delivery_file}"
  fi
  printf '%s\n' '{"accepted":true}' >"${kp_output}"; printf '202'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/applications/*/builds ]]; then
  printf '%s' '{"items":[' >"${kp_output}"
  printf '%s' '{"id":"66666666-6666-4666-8666-666666666666","definitionId":"52525252-5252-4252-8252-525252525252","commitSha":"ffffffffffffffffffffffffffffffffffffffff","generation":1,"state":"running"}' >>"${kp_output}"
  if grep -Fq '|eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee' "${KP_COMMAND_LOG}.github-deliveries" 2>/dev/null; then
    printf '%s' ',{"id":"65656565-6565-4565-8565-656565656565","definitionId":"52525252-5252-4252-8252-525252525252","commitSha":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","generation":1,"state":"running"}' >>"${kp_output}"
  fi
  if [[ -f "${KP_COMMAND_LOG}.build-retry" ]]; then
    printf '%s' ',{"id":"67676767-6767-4767-8767-676767676767","definitionId":"52525252-5252-4252-8252-525252525252","commitSha":"ffffffffffffffffffffffffffffffffffffffff","generation":2,"state":"succeeded"}' >>"${kp_output}"
  fi
  printf '%s\n' ']}' >>"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/builds/66666666-* ]]; then
  printf '%s\n' '{"id":"66666666-6666-4666-8666-666666666666","definitionId":"52525252-5252-4252-8252-525252525252","commitSha":"ffffffffffffffffffffffffffffffffffffffff","generation":1,"state":"succeeded","image":{"reference":"registry.fixture.test/probe@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/applications/*/auto-deploy-policies ]]; then
  printf '%s\n' '{"id":"73737373-7373-4373-8373-737373737373","buildDefinitionId":"52525252-5252-4252-8252-525252525252","projectId":"11111111-1111-4111-8111-111111111111","applicationId":"33333333-3333-4333-8333-333333333333","environmentId":"22222222-2222-4222-8222-222222222221","currentRevision":1,"current":{"enabled":true}}' >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/builds/66666666-*/retry ]]; then
  printf '%s\n' '{"id":"65656565-6565-4565-8565-656565656565","generation":2,"state":"queued"}' >"${kp_output}"; printf '202'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/builds/65656565-* ]]; then
  if [[ -f "${KP_COMMAND_LOG}.build-cancelled" ]]; then kp_state=cancelled; else kp_state=running; fi
  printf '{"id":"65656565-6565-4565-8565-656565656565","definitionId":"52525252-5252-4252-8252-525252525252","generation":2,"state":"%s"}\n' "${kp_state}" >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/builds/65656565-*/cancel ]]; then
  if [[ "${kp_cookie_header}" != "true" || "${kp_csrf_header}" != "true" || "${kp_idempotency_header}" != "true" ]]; then
    printf '%s\n' '{"status":403,"code":"CancellationHeadersMissing"}' >"${kp_output}"; printf '403'
  else
    : >"${KP_COMMAND_LOG}.build-cancelled"
    printf '%s\n' '{"id":"65656565-6565-4565-8565-656565656565","definitionId":"52525252-5252-4252-8252-525252525252","generation":2,"state":"cancelling"}' >"${kp_output}"; printf '202'
  fi
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/builds/65656565-*/retry ]]; then
  kp_retry_count_file="${KP_COMMAND_LOG}.build-retry-count"; kp_retry_count=0
  [[ ! -f "${kp_retry_count_file}" ]] || kp_retry_count="$(<"${kp_retry_count_file}")"
  kp_retry_count=$((kp_retry_count+1)); printf '%s' "${kp_retry_count}" >"${kp_retry_count_file}"
  : >"${KP_COMMAND_LOG}.build-retry"
  case "${kp_retry_count}" in
    1) printf '%s\n' '{"id":"67676767-6767-4767-8767-676767676767","state":"queued"}' >"${kp_output}" ;;
    2) printf '%s\n' '{"id":"68686868-6868-4868-8868-686868686868","state":"queued"}' >"${kp_output}" ;;
    3) printf '%s\n' '{"id":"69696969-6969-4969-8969-696969696969","state":"queued"}' >"${kp_output}" ;;
    *) printf '%s\n' '{"status":409,"code":"RetryLimit"}' >"${kp_output}"; printf '409'; exit 0 ;;
  esac
  printf '202'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/builds/67676767-*/logs* ]]; then
  printf '%s\n' '{"source":{"id":"build_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ready":false,"previous":false},"lines":[{"type":"line","source":{"id":"build_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ready":false,"previous":false},"message":"safe lifecycle event","truncated":false}],"bytes":20,"truncated":false,"observedAt":"2026-08-10T00:00:00Z"}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/builds/67676767-* ]]; then
  printf '%s\n' '{"id":"67676767-6767-4767-8767-676767676767","state":"succeeded","cacheReuse":"hit","cacheReference":"registry.fixture.test/cache:generation-2","warnings":[],"image":{"reference":"registry.fixture.test/probe@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/builds/68686868-* ]]; then
  printf '%s\n' '{"id":"68686868-6868-4868-8868-686868686868","state":"succeeded","warnings":["ColdBuild","CacheDegraded"],"image":{"reference":"registry.fixture.test/probe@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/builds/69696969-* ]]; then
  printf '%s\n' '{"id":"69696969-6969-4969-8969-696969696969","state":"failed","failureCode":"builder-job-failed"}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/auto-deploy-policies/73737373-*/runs* ]]; then
  printf '%s\n' '{"items":[{"attemptId":"67676767-6767-4767-8767-676767676767","state":"submitted","operationId":"74747474-7474-4474-8474-747474747474","deploymentId":"75757575-7575-4575-8575-757575757575"}]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/builds/66666666-*/promote ]]; then
  printf '%s\n' '{"id":"77777777-7777-4777-8777-777777777777","status":"queued"}' >"${kp_output}"; printf '202'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */helm/values-preview ]]; then
  printf '%s\n' '{"approval":{"id":"40404040-4040-4040-8040-404040404040","revision":1},"normalizedValuesYaml":"replicaCount: 1\n","valuesDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","effectiveValues":{"replicaCount":1},"changedPaths":["/replicaCount"]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "PUT" && "${kp_url}" == */helm/release ]]; then
  printf '%s\n' '{"id":"41414141-4141-4141-8141-414141414141","generation":1,"releaseName":"qualification","action":"initial","desiredEnabled":true,"approval":{"id":"40404040-4040-4040-8040-404040404040","revision":1},"valuesDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","intentDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","requestId":"qualification","createdAt":"2026-08-09T00:00:00Z"}' >"${kp_output}"; printf '202'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */helm/release ]]; then
  printf '%s\n' '{"revision":{"id":"41414141-4141-4141-8141-414141414141","generation":1},"phase":"published","renderState":"succeeded","payloadState":"verified","payloadRevision":"cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd","applicationState":"verified","applicationRevision":"abababababababababababababababababababab"}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */helm/rendered-preview ]]; then
  printf '%s\n' '{"releaseRevisionId":"41414141-4141-4141-8141-414141414141","generation":1,"manifestDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","inventoryDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","resourceCount":1,"previewBytes":64,"resources":[{"apiVersion":"apps/v1","kind":"Deployment","namespace":"kuberploy-protected","name":"qualification","sanitizedYaml":"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: qualification\n"}]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */registry/cleanup-previews ]]; then
  printf '%s\n' '{"id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","state":"preview","items":[{"disposition":"protect","action":"none"},{"disposition":"delete","action":"delete-manifest"}]}' >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/registry-cleanup-plans/*/executions ]]; then
  printf '%s\n' '{"id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","state":"succeeded","summary":{"cacheQuotaSatisfied":true},"items":[{"state":"protected"},{"state":"deleted"}]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */readyz ]]; then
  printf '%s\n' '{"status":"ready"}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/middlewares ]]; then
  printf '%s\n' '{"profile":{"id":"edededed-eded-4ded-8ded-edededededed"},"revision":{"profileId":"edededed-eded-4ded-8ded-edededededed","revision":1,"spec":{"headers":{"customResponseHeaders":{"X-Kuberploy-Qualification":"passed"}}},"specDigest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","assignmentsDigest":"sha256:2222222222222222222222222222222222222222222222222222222222222222"}}' >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/applications/*/secret-bindings ]]; then
  printf '%s\n' '{"id":"91919191-9191-4191-8191-919191919191","applicationId":"33333333-3333-4333-8333-333333333333","environmentId":"22222222-2222-4222-8222-222222222221","name":"qualification-runtime","provider":"sealed-secrets","state":"provisioning","createdBy":"11111111-1111-4111-8111-111111111111","createdAt":"2026-08-09T10:00:00Z","updatedAt":"2026-08-09T10:00:00Z","versions":[{"id":"92929292-9292-4292-8292-929292929291","number":1,"state":"awaiting-readiness","deliveries":[{"sourceKey":"password","kind":"environment","environmentName":"QUALIFICATION_PASSWORD"}],"createdAt":"2026-08-09T10:00:00Z","updatedAt":"2026-08-09T10:00:00Z"}]}' >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/secret-bindings/*/versions ]]; then
  : >"${KP_COMMAND_LOG}.runtime-secret-rotated"
  printf '%s\n' '{"id":"91919191-9191-4191-8191-919191919191","applicationId":"33333333-3333-4333-8333-333333333333","environmentId":"22222222-2222-4222-8222-222222222221","name":"qualification-runtime","provider":"sealed-secrets","state":"provisioning","activeVersion":1,"createdBy":"11111111-1111-4111-8111-111111111111","createdAt":"2026-08-09T10:00:00Z","updatedAt":"2026-08-09T10:01:00Z","versions":[{"id":"92929292-9292-4292-8292-929292929292","number":2,"state":"awaiting-readiness","deliveries":[{"sourceKey":"password","kind":"environment","environmentName":"QUALIFICATION_PASSWORD"}],"createdAt":"2026-08-09T10:01:00Z","updatedAt":"2026-08-09T10:01:00Z"}]}' >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/secret-bindings/91919191-* ]]; then
  if [[ -f "${KP_COMMAND_LOG}.runtime-secret-rotated" ]]; then
    printf '%s\n' '{"id":"91919191-9191-4191-8191-919191919191","applicationId":"33333333-3333-4333-8333-333333333333","environmentId":"22222222-2222-4222-8222-222222222221","name":"qualification-runtime","provider":"sealed-secrets","state":"ready","activeVersion":2,"createdBy":"11111111-1111-4111-8111-111111111111","createdAt":"2026-08-09T10:00:00Z","updatedAt":"2026-08-09T10:02:00Z","versions":[{"id":"92929292-9292-4292-8292-929292929291","number":1,"state":"retained","deliveries":[{"sourceKey":"password","kind":"environment","environmentName":"QUALIFICATION_PASSWORD"}],"readinessObservedAt":"2026-08-09T10:00:10Z","activatedAt":"2026-08-09T10:00:10Z","retainedAt":"2026-08-09T10:02:00Z","createdAt":"2026-08-09T10:00:00Z","updatedAt":"2026-08-09T10:02:00Z"},{"id":"92929292-9292-4292-8292-929292929292","number":2,"state":"active","deliveries":[{"sourceKey":"password","kind":"environment","environmentName":"QUALIFICATION_PASSWORD"}],"readinessObservedAt":"2026-08-09T10:02:00Z","activatedAt":"2026-08-09T10:02:00Z","createdAt":"2026-08-09T10:01:00Z","updatedAt":"2026-08-09T10:02:00Z"}]}' >"${kp_output}"
  else
    printf '%s\n' '{"id":"91919191-9191-4191-8191-919191919191","applicationId":"33333333-3333-4333-8333-333333333333","environmentId":"22222222-2222-4222-8222-222222222221","name":"qualification-runtime","provider":"sealed-secrets","state":"ready","activeVersion":1,"createdBy":"11111111-1111-4111-8111-111111111111","createdAt":"2026-08-09T10:00:00Z","updatedAt":"2026-08-09T10:00:10Z","versions":[{"id":"92929292-9292-4292-8292-929292929291","number":1,"state":"active","deliveries":[{"sourceKey":"password","kind":"environment","environmentName":"QUALIFICATION_PASSWORD"}],"readinessObservedAt":"2026-08-09T10:00:10Z","activatedAt":"2026-08-09T10:00:10Z","createdAt":"2026-08-09T10:00:00Z","updatedAt":"2026-08-09T10:00:10Z"}]}' >"${kp_output}"
  fi
  printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/deployments/27272727-2727-4272-8272-272727272721 ]]; then
  printf '%s\n' '{"route":{"hostname":"config-edge.1-1-1-1.sslip.io","dnsMode":"sslip"},"runtime":{"nodeSelector":{"kubernetes.io/os":"linux"},"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/os","operator":"In","values":["linux"]}]}]},"preferredDuringSchedulingIgnoredDuringExecution":[{"weight":70,"preference":{"matchExpressions":[{"key":"topology.kubernetes.io/zone","operator":"Exists"}]}}]},"podAntiAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":[{"topologyKey":"kubernetes.io/hostname","labelSelector":{"matchLabels":{"kuberploy.io/application":"35353535-3535-4353-8353-353535353531"}}}]}},"topologySpreadConstraints":[{"maxSkew":1,"topologyKey":"topology.kubernetes.io/zone","whenUnsatisfiable":"ScheduleAnyway","labelSelector":{"matchLabels":{"kuberploy.io/application":"35353535-3535-4353-8353-353535353531"}}}],"tolerations":[{"key":"qualification.kuberploy.io/workload","operator":"Equal","value":"application","effect":"NoSchedule"}],"resources":{"requests":{"cpu":"50m"}},"probes":{"readiness":{"httpGet":{"path":"/ready"}}}}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/deployments/27272727-2727-4272-8272-272727272721/config\?atLeastRevision=* ]]; then
  kp_app_yaml=$'apiVersion: config.kuberploy.io/v1alpha1\nkind: AppConfig\nspec:\n  runtime:\n    replicas: 1\n    nodeSelector:\n      kubernetes.io/os: linux\n    affinity:\n      nodeAffinity:\n        requiredDuringSchedulingIgnoredDuringExecution:\n          nodeSelectorTerms:\n            - matchExpressions:\n                - key: kubernetes.io/os\n                  operator: In\n                  values: [linux]\n        preferredDuringSchedulingIgnoredDuringExecution:\n          - weight: 70\n            preference:\n              matchExpressions:\n                - key: topology.kubernetes.io/zone\n                  operator: Exists\n      podAntiAffinity:\n        requiredDuringSchedulingIgnoredDuringExecution:\n          - topologyKey: kubernetes.io/hostname\n            labelSelector:\n              matchLabels:\n                kuberploy.io/application: 35353535-3535-4353-8353-353535353531\n    topologySpreadConstraints:\n      - maxSkew: 1\n        topologyKey: topology.kubernetes.io/zone\n        whenUnsatisfiable: ScheduleAnyway\n        labelSelector:\n          matchLabels:\n            kuberploy.io/application: 35353535-3535-4353-8353-353535353531\n    tolerations:\n      - key: qualification.kuberploy.io/workload\n        operator: Equal\n        value: application\n        effect: NoSchedule\n'
  jq -n --arg raw "${kp_app_yaml}" '{etag:"cfg-edge",documents:[{documentId:"app.yaml",documentKind:"AppConfig",rawYaml:$raw},{documentId:"project-variables.yaml",documentKind:"VariableSet",rawYaml:"kind: VariableSet"},{documentId:"environment-variables.yaml",documentKind:"VariableSet",rawYaml:"kind: VariableSet"}],variableDependencies:[{scope:"project",present:true,blobId:"blob-project"},{scope:"environment",present:true,blobId:"blob-environment"}],effectiveVariables:[{name:"FEATURE_PROBES",value:"enabled",source:"environment"},{name:"RELEASE_LANE",value:"environment",source:"environment",overrides:[{scope:"project",value:"project"}]},{name:"SHARED_REGION",value:"ap-southeast-1",source:"project"}]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/deployments/27272727-2727-4272-8272-272727272721/config/validate ]]; then
  printf '%s\n' '{"valid":true,"diagnostics":[],"effectiveVariables":[{"name":"FEATURE_PROBES","value":"enabled","source":"environment"},{"name":"RELEASE_LANE","value":"environment","source":"environment","overrides":[{"scope":"project","value":"project"}]},{"name":"SHARED_REGION","value":"ap-southeast-1","source":"project"}]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/deployments/27272727-2727-4272-8272-272727272721/config/preview ]]; then
  printf '%s\n' '{"previewToken":"YYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYY","gitDiff":"replicas: 2","renderedDiff":"Deployment replicas: 2","renderIdentityDigest":"sha256:abababababababababababababababababababababababababababababababab","semanticChanges":[{"pointer":"/spec/runtime/replicas","before":1,"after":2}],"effectiveVariables":[{"name":"FEATURE_PROBES","value":"enabled","source":"environment"},{"name":"RELEASE_LANE","value":"environment","source":"environment","overrides":[{"scope":"project","value":"project"}]},{"name":"SHARED_REGION","value":"ap-southeast-1","source":"project"}]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "PUT" && "${kp_url}" == */v1/deployments/27272727-2727-4272-8272-272727272721/config ]]; then
  printf '%s\n' '{"id":"28282828-2828-4282-8282-282828282821","status":"queued"}' >"${kp_output}"; printf '202'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/external-dns/integrations ]]; then
  printf '%s\n' '{"id":"29292929-2929-4292-8292-292929292929"}' >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/external-dns/integrations ]]; then
  if [[ -f "${KP_COMMAND_LOG}.external-dns-deactivated" ]]; then kp_lifecycle=deactivated; kp_git=dematerialized; else kp_lifecycle=active; kp_git=materialized; fi
  printf '{"items":[{"id":"29292929-2929-4292-8292-292929292929","lifecycle":"%s","protectedGitState":"%s","runtimeRevision":1}]}\n' "${kp_lifecycle}" "${kp_git}" >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/external-dns/status ]]; then
  printf '%s\n' '{"controllerReadiness":"ready","runtimeAvailable":true}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/deployments/27272727-2727-4272-8272-272727272722/config ]]; then
  printf '%s\n' '{"etag":"cfg-dns"}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/deployments/27272727-2727-4272-8272-272727272722/config/preview ]]; then
  printf '%s\n' '{"previewToken":"ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ","warnings":["integration freshly observed ready"]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "PUT" && "${kp_url}" == */v1/deployments/27272727-2727-4272-8272-272727272722/config ]]; then
  if [[ "${kp_body}" == *'"remove"'* ]]; then kp_id=28282828-2828-4282-8282-282828282823; else kp_id=28282828-2828-4282-8282-282828282822; fi
  printf '{"id":"%s","status":"queued"}\n' "${kp_id}" >"${kp_output}"; printf '202'
elif [[ "${kp_method}" == "DELETE" && "${kp_url}" == */v1/external-dns/integrations/29292929-* ]]; then
  : >"${KP_COMMAND_LOG}.external-dns-deactivated"; printf '%s\n' '{"id":"29292929-2929-4292-8292-292929292929","lifecycle":"deactivated","protectedGitState":"pending"}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/deployments/*/config\?atLeastRevision=* ]]; then
  kp_revision="${kp_url#*atLeastRevision=}"; kp_revision="${kp_revision%%&*}"
  printf '{"freshness":"fresh","indexedRevision":"%s"}\n' "${kp_revision}" >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/deployments/99999999-9999-4999-8999-999999999999 ]]; then
  printf '%s\n' '{"id":"99999999-9999-4999-8999-999999999999","environmentId":"22222222-2222-4222-8222-222222222221","applicationId":"33333333-3333-4333-8333-333333333333","image":"registry.fixture.test/probe@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","runtime":{"replicas":1,"containers":[{"name":"app","ports":[{"name":"http","containerPort":8080}]}]},"generation":4,"state":"active"}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/deployments/99999999-*/config ]]; then
  printf '%s\n' '{"kind":"ConfigBundle","etag":"\"cfg-sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"","documents":[{"document":{"spec":{"routes":[{"host":"http.fixture.test"}]}}}]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */v1/deployments/99999999-*/config/preview ]]; then
  printf '%s\n' '{"previewToken":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","renderIdentityDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","semanticChanges":[{"path":"/spec/runtime/env"}],"gitDiff":"qualification-security-headers custom.fixture.test acme.fixture.test 91919191-9191-4191-8191-919191919191 version 1 2","renderedDiff":"qualification-security-headers custom.fixture.test acme.fixture.test 91919191-9191-4191-8191-919191919191 version 1 2"}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "PUT" && "${kp_url}" == */v1/deployments/99999999-*/config ]]; then
  kp_count_file="${KP_COMMAND_LOG}.config-saves"; kp_count=0; [[ ! -f "${kp_count_file}" ]] || kp_count="$(<"${kp_count_file}")"
  kp_count=$((kp_count+1)); printf '%s' "${kp_count}" >"${kp_count_file}"
  if [[ "${kp_count}" -eq 1 ]]; then kp_id=fefefefe-fefe-4efe-8efe-fefefefefefe
  elif [[ "${kp_count}" -eq 2 ]]; then kp_id=dfdfdfdf-dfdf-4fdf-8fdf-dfdfdfdfdfdf
  elif [[ "${kp_count}" -eq 3 ]]; then kp_id=93939393-9393-4393-8393-939393939391
  else kp_id=93939393-9393-4393-8393-939393939392; fi
  printf '{"id":"%s","status":"queued"}\n' "${kp_id}" >"${kp_output}"; printf '202'
elif [[ "${kp_method}" == "POST" && "${kp_url}" == */certificate-bindings ]]; then
  printf '%s\n' '{"id":"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee","state":"provisioning"}' >"${kp_output}"; printf '201'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/certificate-bindings/eeeeeeee-* ]]; then
  printf '%s\n' '{"id":"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee","name":"qualification-custom","state":"ready","activeVersion":1,"versions":[{"number":1,"leafFingerprint":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","dnsNames":["custom.fixture.test"]}]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */certificate-issuers* ]]; then
  printf '%s\n' '{"items":[{"name":"local-acme","source":"bootstrap","solverTypes":["http01"]}]}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/operations/44444444-* ]]; then
  printf '%s\n' '{"id":"44444444-4444-4444-8444-444444444444","status":"succeeded","generation":1,"gitRevision":{"commit":"0123456789abcdef0123456789abcdef01234567"}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/operations/24242424-2424-4242-8242-24242424242* ]]; then
  kp_id="${kp_url##*/}"; printf '{"id":"%s","status":"succeeded","gitRevision":{"commit":"2424242424242424242424242424242424242424"}}\n' "${kp_id}" >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/operations/26262626-2626-4262-8262-26262626262* ]]; then
  kp_id="${kp_url##*/}"; printf '{"id":"%s","status":"succeeded","gitRevision":{"commit":"2626262626262626262626262626262626262626"}}\n' "${kp_id}" >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/operations/28282828-2828-4282-8282-28282828282* ]]; then
  kp_id="${kp_url##*/}"; printf '{"id":"%s","status":"succeeded","gitRevision":{"commit":"2828282828282828282828282828282828282828"}}\n' "${kp_id}" >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/operations/55555555-* ]]; then
  printf '%s\n' '{"id":"55555555-5555-4555-8555-555555555555","status":"succeeded","pullRequest":{"url":"https://git.example.test/pull/1"}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/operations/77777777-* ]]; then
  printf '%s\n' '{"id":"77777777-7777-4777-8777-777777777777","status":"succeeded","pullRequest":{"url":"https://git.example.test/pull/2"}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/operations/74747474-* ]]; then
  printf '%s\n' '{"id":"74747474-7474-4474-8474-747474747474","status":"succeeded","generation":4,"gitRevision":{"commit":"7474747474747474747474747474747474747474"}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/operations/78787878-* ]]; then
  printf '%s\n' '{"id":"78787878-7878-4878-8878-787878787878","status":"succeeded","generation":5,"gitRevision":{"commit":"7878787878787878787878787878787878787878"}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/operations/aaaaaaaa-* ]]; then
  printf '%s\n' '{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","status":"succeeded","generation":3,"gitRevision":{"commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/operations/12121212-* ]]; then
  printf '%s\n' '{"id":"12121212-1212-4212-8212-121212121212","status":"succeeded","generation":2,"gitRevision":{"commit":"123456789abcdef0123456789abcdef012345678"}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/operations/bbbbbbbb-* ]]; then
  printf '%s\n' '{"id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","status":"succeeded"}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/operations/acacacac-* ]]; then
  printf '%s\n' '{"id":"acacacac-acac-4cac-8cac-acacacacacac","status":"succeeded","generation":8,"gitRevision":{"commit":"cccccccccccccccccccccccccccccccccccccccc"}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/operations/93939393-9393-4393-8393-939393939391 ]]; then
  printf '%s\n' '{"id":"93939393-9393-4393-8393-939393939391","status":"succeeded","generation":6,"gitRevision":{"commit":"dddddddddddddddddddddddddddddddddddddddd"}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/operations/93939393-9393-4393-8393-939393939392 ]]; then
  printf '%s\n' '{"id":"93939393-9393-4393-8393-939393939392","status":"succeeded","generation":7,"gitRevision":{"commit":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/operations/fefefefe-* ]]; then
  printf '%s\n' '{"id":"fefefefe-fefe-4efe-8efe-fefefefefefe","status":"succeeded","generation":4,"gitRevision":{"commit":"dddddddddddddddddddddddddddddddddddddddd"}}' >"${kp_output}"; printf '200'
elif [[ "${kp_method}" == "GET" && "${kp_url}" == */v1/operations/dfdfdfdf-* ]]; then
  printf '%s\n' '{"id":"dfdfdfdf-dfdf-4fdf-8fdf-dfdfdfdfdfdf","status":"succeeded","generation":5,"gitRevision":{"commit":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}}' >"${kp_output}"; printf '200'
else
  printf '%s\n' '{"status":true}' >"${kp_output}"; printf '200'
fi
EOF
cat >"${kp_tmp}/dig" <<'EOF'
#!/usr/bin/env bash
printf '192.0.2.10\n'
EOF
cat >"${kp_tmp}/openssl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "${1:-}" == "s_client" ]]; then
  printf 'fixture-certificate\n'
elif [[ "${1:-}" == "x509" ]]; then
  kp_out="" kp_in="" kp_fingerprint="false"
  while (($#)); do
    if [[ "$1" == "-out" ]]; then kp_out="$2"; shift 2
    elif [[ "$1" == "-in" ]]; then kp_in="$2"; shift 2
    elif [[ "$1" == "-fingerprint" ]]; then kp_fingerprint="true"; shift
    else shift; fi
  done
  if [[ -n "${kp_out}" ]]; then printf 'fixture-certificate\n' >"${kp_out}"
  elif [[ "${kp_fingerprint}" == "true" ]]; then
    if [[ -z "${kp_in}" ]]; then while IFS= read -r _; do :; done; fi
    printf 'sha256 Fingerprint=AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA\n'
  else :; fi
else
  exec /usr/bin/openssl "$@"
fi
EOF
chmod 755 "${kp_tmp}/kubectl" "${kp_tmp}/helm" "${kp_tmp}/curl" \
  "${kp_tmp}/dig" "${kp_tmp}/openssl"
export PATH="${kp_tmp}:${PATH}"

export KUBECONFIG="${kp_kubeconfig}"
export KUBERPLOY_TEST_CONTEXT="${KP_EXPECTED_CONTEXT}"
export KUBERPLOY_TEST_SERVER="${KP_EXPECTED_SERVER}"
export KUBERPLOY_E2E_INSTALLER_VALUES_FILE="${kp_installer_values}"
export KUBERPLOY_E2E_UPGRADE_FROM_VALUES_FILE="${kp_upgrade_values}"
export KUBERPLOY_E2E_CUSTOM_CERTIFICATE_PEM_FILE="${kp_custom_certificate}"
export KUBERPLOY_E2E_CUSTOM_PRIVATE_KEY_PEM_FILE="${kp_custom_private_key}"
export KUBERPLOY_E2E_RUNTIME_SECRET_INITIAL_VALUE_FILE="${kp_runtime_secret_initial}"
export KUBERPLOY_E2E_RUNTIME_SECRET_ROTATED_VALUE_FILE="${kp_runtime_secret_rotated}"
export KUBERPLOY_E2E_RFC2136_TSIG_SECRET_FILE="${kp_rfc2136_tsig_secret}"
export KUBERPLOY_E2E_REGISTRY_PUSH_USERNAME_FILE="${kp_registry_push_username}"
export KUBERPLOY_E2E_REGISTRY_PUSH_PASSWORD_FILE="${kp_registry_push_password}"
export KUBERPLOY_E2E_REGISTRY_CACHE_USERNAME_FILE="${kp_registry_cache_username}"
export KUBERPLOY_E2E_REGISTRY_CACHE_PASSWORD_FILE="${kp_registry_cache_password}"
export KUBERPLOY_E2E_REGISTRY_FAULT_PASSWORD_FILE="${kp_registry_fault_password}"
export KUBERPLOY_E2E_RFC2136_PROVIDER_IMAGE="registry.fixture.test/kuberploy/rfc2136-test-provider@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
export KUBERPLOY_E2E_EXTERNAL_DNS_NAMESPACE="kuberploy-system"
export KUBERPLOY_E2E_KUBE_API_CIDR="10.43.0.1/32"
kp_github_webhook_secret="${kp_tmp}/github-webhook-secret"
printf 'qualification-webhook-secret\n' >"${kp_github_webhook_secret}"
chmod 600 "${kp_github_webhook_secret}"
export KUBERPLOY_E2E_GITHUB_WEBHOOK_SECRET_FILE="${kp_github_webhook_secret}"
export KUBERPLOY_E2E_HTTP_HOSTNAME="http.fixture.test"
export KUBERPLOY_E2E_CUSTOM_TLS_HOSTNAME="custom.fixture.test"
export KUBERPLOY_E2E_LOCAL_ACME_HOSTNAME="acme.fixture.test"
export KUBERPLOY_E2E_LOCAL_ACME_DIRECTORY_URL="https://acme.fixture.test/directory"
export KUBERPLOY_E2E_PUBLIC_PROVIDER_TESTS="false"
export KUBERPLOY_E2E_BROWSER_EXECUTABLE="/usr/bin/true"
export KUBERPLOY_E2E_BROWSER_TEST_SEAM="true"
export KUBERPLOY_E2E_HERMETIC_TEST="true"
kp_auth_header="${kp_tmp}/api-auth-header"
printf 'Authorization: Bearer fixture\n' >"${kp_auth_header}"
chmod 600 "${kp_auth_header}"
export KUBERPLOY_E2E_API_AUTH_HEADER_FILE="${kp_auth_header}"
kp_denied_auth_header="${kp_tmp}/denied-auth-header"
printf 'Authorization: Bearer denied-fixture\n' >"${kp_denied_auth_header}"
chmod 600 "${kp_denied_auth_header}"
export KUBERPLOY_E2E_DENIED_AUTH_HEADER_FILE="${kp_denied_auth_header}"
kp_cookie_header="${kp_tmp}/human-cookie-header"
kp_csrf_token="${kp_tmp}/csrf-token"
printf 'Cookie: kuberploy_session=fixture; kuberploy_csrf=fixture-csrf\n' >"${kp_cookie_header}"
printf 'fixture-csrf\n' >"${kp_csrf_token}"
chmod 600 "${kp_cookie_header}" "${kp_csrf_token}"
export KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE="${kp_cookie_header}"
export KUBERPLOY_E2E_CSRF_TOKEN_FILE="${kp_csrf_token}"
kp_teardown_private="${kp_tmp}/teardown-private.pem"
kp_teardown_public="${kp_tmp}/teardown-public.pem"
/usr/bin/openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out "${kp_teardown_private}" >/dev/null 2>&1
/usr/bin/openssl pkey -in "${kp_teardown_private}" -pubout \
  -out "${kp_teardown_public}" >/dev/null 2>&1
kp_teardown_key_digest="$(/usr/bin/openssl pkey -pubin -in "${kp_teardown_public}" -outform DER | \
  /usr/bin/openssl dgst -sha256 -r | awk '{print $1}')"
export KUBERPLOY_E2E_TEARDOWN_PUBLIC_KEY_FILE="${kp_teardown_public}"
export KUBERPLOY_E2E_SCENARIO_FILE="${kp_tmp}/scenario.json"

source "${kp_root}/scripts/kubernetes/test/e2e/lib.sh"
kp_scenario='{"schemaVersion":1,"apiBaseURL":"https://api.fixture.test","teardown":{"authority":"fixture-iac","infrastructureId":"fixture-cluster-1","publicKeySHA256":"placeholder"},"workflow":{"project":{"name":"Qualification","slug":"qualification"},"directEnvironment":{"name":"Direct","slug":"direct"},"protectedEnvironment":{"name":"Protected","slug":"protected"},"application":{"name":"Probe","slug":"probe"},"directDeployment":{"image":"registry.fixture.test/probe@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","runtime":{"replicas":1}},"directDeploymentUpdate":{"image":"registry.fixture.test/probe@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","runtime":{"replicas":2}},"protectedDeployment":{"image":"registry.fixture.test/probe@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","runtime":{"replicas":1}},"sourceBuild":{"builderPool":{"nodeSelector":{"kuberploy.io/builder-pool":"dind"}},"github":{"installationId":"50505050-5050-4050-8050-505050505050","repositoryId":"51515151-5151-4151-8151-515151515151","githubInstallationId":12345,"githubRepositoryId":67890,"ownerId":23456,"ownerLogin":"kuberploy","repositoryName":"qualification","senderId":34567,"senderLogin":"qualification-user"},"definition":{"installationId":"50505050-5050-4050-8050-505050505050","repositoryId":"51515151-5151-4151-8151-515151515151","registryTargetId":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","triggerRef":"refs/heads/main","contextPath":".","dockerfilePath":"Dockerfile","platforms":["linux/amd64"],"cacheTrustLane":"qualification","cacheImports":1,"profile":{"resource":"small","timeoutSeconds":900,"egress":"internet"},"maxAttempts":2},"push":{"deliveryId":"9f000000-0000-4000-8000-000000000001","afterCommit":"ffffffffffffffffffffffffffffffffffffffff"},"cancellationPush":{"deliveryId":"9f000000-0000-4000-8000-000000000002","afterCommit":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},"promotion":{"runtime":{"replicas":1}}},"registryCleanup":{"targetId":"dddddddd-dddd-4ddd-8ddd-dddddddddddd"},"upgrade":{"sourceVersion":"0.1.0","targetVersion":"0.2.0"}},"stages":{}}'
kp_scenario="$(jq -c --arg digest "${kp_teardown_key_digest}" \
  '.teardown.publicKeySHA256=$digest |
   .workflow.directDeployment.route={hostname:"http.fixture.test",dnsMode:"manual",pathPrefix:"/",tlsMode:"httpOnly"} |
   .workflow.directDeploymentUpdate.route={hostname:"http.fixture.test",dnsMode:"manual",pathPrefix:"/",tlsMode:"httpOnly"} |
   .workflow.tls={customCertificateName:"qualification-custom",localACMEIssuerName:"local-acme"} |
   .workflow.recovery={postgresql:{namespace:"kuberploy-system",podName:"kuberploy-postgresql-0",controllerName:"kuberploy-postgresql"},valkey:{namespace:"kuberploy-system",controllerName:"kuberploy-valkey",persistentVolumeClaimName:"kuberploy-valkey"},worker:{namespace:"kuberploy-system",controllerName:"kuberploy-worker"}} |
   .workflow.observability={workloadId:"abababab-abab-4bab-8bab-abababababab",from:"2026-08-09T10:00:00Z",to:"2026-08-09T10:05:00Z"} |
   .workflow.runtimeSecret={name:"qualification-runtime",key:"password",environmentName:"QUALIFICATION_PASSWORD"} |
   .workflow.sourceBuild.credentials={namespace:"kuberploy-build-dind",pushSecretName:"push-secret",cacheSecretName:"cache-secret"} |
   .workflow.security={
     networkPolicyProvider:{namespace:"kube-system",daemonSet:"cilium",container:"cilium-agent",
       image:"quay.io/cilium/cilium@sha256:1111111111111111111111111111111111111111111111111111111111111111"},
     resourceQuota:{name:"direct-quota",resource:"requests.cpu",exceededValue:"100000"}} |
   .workflow.helm={approvalId:"40404040-4040-4040-8040-404040404040",approvalRevision:1,valuesYaml:"replicaCount: 1\n"}' \
  <<<"${kp_scenario}")"
while IFS='|' read -r kp_stage kp_mutating kp_assertions; do
  IFS=',' read -r -a kp_assertion_list <<<"${kp_assertions}"
  for kp_assertion in "${kp_assertion_list[@]}"; do
    kp_probe="$(kp_qualification_expected_probe "${kp_assertion}")"
    case "${kp_probe}" in
      helm-install) kp_spec='{"probe":"helm-install"}' ;;
      installer-proof|workflow-proof|browser-proof)
        kp_spec="$(jq -cn --arg p "${kp_probe}" --arg a "${kp_assertion}" '{probe:$p,contract:$a}')" ;;
      http) kp_spec='{"probe":"http","url":"http://http.fixture.test/","expectedStatus":200}' ;;
      tls)
        kp_spec='{"probe":"tls","hostname":"custom.fixture.test","port":443,"minimumRemainingSeconds":300}' ;;
      dns) kp_spec='{"probe":"dns","hostname":"public.fixture.test","expectedAddress":"192.0.2.10"}' ;;
    esac
    kp_scenario="$(jq -c --arg s "${kp_stage}" --arg a "${kp_assertion}" --argjson spec "${kp_spec}" '.stages[$s].assertions[$a]=$spec' <<<"${kp_scenario}")"
  done
done < <(kp_qualification_stage_catalog)
printf '%s\n' "${kp_scenario}" >"${KUBERPLOY_E2E_SCENARIO_FILE}"

kp_run_qualification() {
  local kp_run_id="${1:?run ID required}"
  export KUBERPLOY_E2E_RUN_ID="${kp_run_id}"
  export KUBERPLOY_E2E_EXTERNAL_DNS_NAMESPACE_AUTHORITY="pre-existing:${kp_run_id}:${KUBERPLOY_E2E_EXTERNAL_DNS_NAMESPACE}"
  export KUBERPLOY_E2E_MUTATION_ACK="qualify:${kp_run_id}:${KUBERPLOY_TEST_CONTEXT}"
  export KUBERPLOY_E2E_DISPOSABLE_CLUSTER_ACK="destroy-after-qualification:${kp_run_id}:${KUBERPLOY_TEST_CONTEXT}"
  export KUBERPLOY_E2E_ARTIFACT_DIR="${kp_tmp}/kuberploy-qualification-${kp_run_id}"
  "${kp_root}/scripts/kubernetes/test/e2e/qualification.sh"
}

kp_expect_failure() {
  local kp_name="${1:?test name required}"
  shift
  if "$@" >/dev/null 2>&1; then
    printf 'expected failure: %s\n' "${kp_name}" >&2
    exit 1
  fi
}

# Success proves fixed execution order, reverse cleanup, wrapper selectors and
# a report that becomes passed only after cleanup verification.
export KUBERPLOY_E2E_RUN_ID="noack1"
export KUBERPLOY_E2E_MUTATION_ACK="qualify:noack1:${KUBERPLOY_TEST_CONTEXT}"
export KUBERPLOY_E2E_ARTIFACT_DIR="${kp_tmp}/kuberploy-qualification-noack1"
unset KUBERPLOY_E2E_DISPOSABLE_CLUSTER_ACK || true
kp_expect_failure missing-disposable-cluster-ack \
  "${kp_root}/scripts/kubernetes/test/e2e/qualification.sh"
[[ ! -e "${KUBERPLOY_E2E_ARTIFACT_DIR}" ]]

kp_run_qualification success1 >/dev/null
kp_success_report="${kp_tmp}/kuberploy-qualification-success1/qualification-report.json"
jq -e '.status == "qualified-teardown-required" and
  .disposableCluster.acknowledged == true and .disposableCluster.teardownRequired == true and
  .disposableCluster.retainedState == "product records, retained installer objects, and Argo-created descendants" and
  .disposableCluster.authority == "fixture-iac" and
  .disposableCluster.infrastructureId == "fixture-cluster-1" and
  (.disposableCluster.publicKeySHA256 | test("^[a-f0-9]{64}$")) and
  (.stages | length) == 12 and
  (.stages | map(.stage)) == [
    "00-preflight","10-one-chart-install","20-postgresql-valkey","25-config-edge","30-git-argo",
    "40-source-build","50-runtime-edge","60-local-tls","70-registry-retention",
    "80-observability","90-security","100-upgrade-rollback"
  ] and
  (all(.stages[1:][]; .events[-1] == {phase:"cleanup",status:"passed"}))' \
  "${kp_success_report}" >/dev/null
kp_secret_evidence="${kp_tmp}/kuberploy-qualification-success1/60-local-tls/evidence/workflow-local-acme-route-secret-metadata.json"
jq -e '.type == "kubernetes.io/tls" and (.metadata.uid | length > 0) and
  (has("data") | not)' "${kp_secret_evidence}" >/dev/null
! rg -n 'tls\.key|bmV2ZXItcGVyc2lzdGVk' \
  "${kp_tmp}/kuberploy-qualification-success1/60-local-tls/evidence"
jq -e '.directGeneration == 7' \
  "${kp_tmp}/kuberploy-qualification-success1/workflow-state.json" >/dev/null

# GitHub delivery IDs are canonical lowercase UUIDs. Reject a fixture or
# operator scenario that would pass declarative validation but fail webhook
# verification after the first mutating stage starts.
cp "${KUBERPLOY_E2E_SCENARIO_FILE}" "${kp_tmp}/scenario-valid-delivery.json"
jq '.workflow.sourceBuild.push.deliveryId="qualification-delivery-1"' \
  "${kp_tmp}/scenario-valid-delivery.json" >"${KUBERPLOY_E2E_SCENARIO_FILE}"
kp_expect_failure malformed-webhook-delivery-id \
  kp_run_qualification malformeddelivery1
[[ ! -e "${kp_tmp}/kuberploy-qualification-malformeddelivery1" ]]
cp "${kp_tmp}/scenario-valid-delivery.json" "${KUBERPLOY_E2E_SCENARIO_FILE}"
export KUBERPLOY_E2E_RUN_ID="success1"
export KUBERPLOY_E2E_ARTIFACT_DIR="${kp_tmp}/kuberploy-qualification-success1"

grep -F "helm|upgrade --install kuberploy-qualification ${kp_root}/charts/kuberploy-installer --namespace kuberploy-system --values ${KUBERPLOY_E2E_UPGRADE_FROM_VALUES_FILE} --server-side=false" \
  "${KP_COMMAND_LOG}" >/dev/null
grep -F "helm|upgrade kuberploy-qualification ${kp_root}/charts/kuberploy-installer --namespace kuberploy-system --values ${KUBERPLOY_E2E_INSTALLER_VALUES_FILE} --server-side=false --wait --wait-for-jobs --timeout 20m" \
  "${KP_COMMAND_LOG}" >/dev/null
grep -F 'helm|rollback kuberploy-qualification 1 --namespace kuberploy-system --wait --wait-for-jobs --timeout 20m' \
  "${KP_COMMAND_LOG}" >/dev/null
for kp_required_mutation in \
  'curl|POST|https://api.fixture.test/v1/projects' \
  'curl|POST|https://api.fixture.test/v1/environments' \
  'curl|POST|https://api.fixture.test/v1/applications' \
  'curl|POST|https://api.fixture.test/v1/deployments' \
  'curl|POST|https://api.fixture.test/v1/deployments/99999999-9999-4999-8999-999999999999/rollback' \
  'curl|POST|https://api.fixture.test/v1/applications/33333333-3333-4333-8333-333333333333/build-definitions' \
  'curl|POST|https://api.fixture.test/v1/webhooks/github' \
  'curl|POST|https://api.fixture.test/v1/builds/65656565-6565-4565-8565-656565656565/cancel' \
  'curl|POST|https://api.fixture.test/v1/builds/65656565-6565-4565-8565-656565656565/retry' \
  'curl|POST|https://api.fixture.test/v1/builds/66666666-6666-4666-8666-666666666666/promote' \
  'curl|POST|https://api.fixture.test/v1/applications/33333333-3333-4333-8333-333333333333/registry/cleanup-previews' \
  'curl|POST|https://api.fixture.test/v1/registry-cleanup-plans/cccccccc-cccc-4ccc-8ccc-cccccccccccc/executions'; do
  grep -F "${kp_required_mutation}" "${KP_COMMAND_LOG}" >/dev/null
done
[[ "$(grep -Fxc 'curl|POST|https://api.fixture.test/v1/projects' "${KP_COMMAND_LOG}")" -eq 2 ]]
[[ "$(grep -Fxc 'curl|POST|https://api.fixture.test/v1/deployments/99999999-9999-4999-8999-999999999999/rollback' "${KP_COMMAND_LOG}")" -eq 1 ]]
[[ "$(grep -Fxc 'curl|POST|https://api.fixture.test/v1/builds/65656565-6565-4565-8565-656565656565/cancel' "${KP_COMMAND_LOG}")" -eq 1 ]]
[[ "$(grep -Fc 'kubectl|get jobs.batch --all-namespaces --selector kuberploy.io/build-operation=65656565656545658565656565656565,kuberploy.io/build-generation=2 -o json' "${KP_COMMAND_LOG}")" -ge 2 ]]
jq -e '.mutation == "github-webhook-build-cancel-cache-fault-auto-deploy-promotion-and-approved-helm" and
  .buildCancellationAccepted == true and .buildCancellationJobDeleted == true and
  .buildCancellationRetrySucceeded == true and
  .cancelledBuildId == "65656565-6565-4565-8565-656565656565" and
  .cancelRetryBuildId == .cacheHitBuildId' \
  "${kp_tmp}/kuberploy-qualification-success1/40-source-build/evidence/workflow-proof.json" >/dev/null
jq -e '.state == "cancelled" and .generation == 2 and (.image == null)' \
  "${kp_tmp}/kuberploy-qualification-success1/40-source-build/evidence/workflow-cancel-terminal.json" >/dev/null
jq -e '.remainingJobs == 0 and .deleted == true' \
  "${kp_tmp}/kuberploy-qualification-success1/40-source-build/evidence/workflow-cancel-job-deleted.json" >/dev/null
grep -F 'kubectl|patch certificate qualification-local-acme' "${KP_COMMAND_LOG}" >/dev/null
grep -F 'kubectl|auth can-i get secrets' "${KP_COMMAND_LOG}" >/dev/null
grep -F 'kubectl|create --dry-run=server -f -' "${KP_COMMAND_LOG}" >/dev/null
# Both owned namespaces are read and validated before the first uninstall or
# delete mutation; the installer never targets the run namespace.
kp_first_cleanup_mutation="$(grep -nE 'helm\|uninstall|kubectl\|delete' "${KP_COMMAND_LOG}" | head -1 | cut -d: -f1)"
kp_argocd_validation="$(grep -n 'kubectl|get namespace argocd -o json' "${KP_COMMAND_LOG}" | tail -1 | cut -d: -f1)"
[[ "${kp_argocd_validation}" -lt "${kp_first_cleanup_mutation}" ]]

# Only a signed, exact infrastructure-destruction receipt can transition the
# qualified retained-state report to passed. Mismatch and replay fail closed.
kp_report_digest="$(/usr/bin/openssl dgst -sha256 -r "${kp_success_report}" | awk '{print $1}')"
kp_receipt="${kp_tmp}/teardown-receipt.json"
kp_signature="${kp_tmp}/teardown-receipt.sig"
jq -n --arg digest "${kp_report_digest}" --arg run success1 \
  --arg context "${KUBERPLOY_TEST_CONTEXT}" --arg server "${KUBERPLOY_TEST_SERVER}" \
  '{schemaVersion:1,runID:$run,target:{context:$context,server:$server},status:"destroyed",
    authority:"fixture-iac",infrastructureId:"wrong-cluster",
    qualificationReportSHA256:$digest,destroyedAt:(now|todateiso8601)}' >"${kp_receipt}"
/usr/bin/openssl dgst -sha256 -sign "${kp_teardown_private}" -out "${kp_signature}" "${kp_receipt}"
export KUBERPLOY_E2E_TEARDOWN_RECEIPT_FILE="${kp_receipt}"
export KUBERPLOY_E2E_TEARDOWN_SIGNATURE_FILE="${kp_signature}"
export KUBERPLOY_E2E_TEARDOWN_PUBLIC_KEY_FILE="${kp_teardown_public}"
kp_expect_failure mismatched-teardown-receipt \
  "${kp_root}/scripts/kubernetes/test/e2e/finalize-teardown.sh"
jq '.infrastructureId="fixture-cluster-1"' "${kp_receipt}" >"${kp_receipt}.tmp"
mv "${kp_receipt}.tmp" "${kp_receipt}"
# A perfectly valid receipt signed by a newly generated attacker key still
# fails because the verifier SPKI digest was pinned before qualification.
kp_attacker_private="${kp_tmp}/attacker-private.pem"
kp_attacker_public="${kp_tmp}/attacker-public.pem"
/usr/bin/openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "${kp_attacker_private}" >/dev/null 2>&1
/usr/bin/openssl pkey -in "${kp_attacker_private}" -pubout -out "${kp_attacker_public}" >/dev/null 2>&1
/usr/bin/openssl dgst -sha256 -sign "${kp_attacker_private}" -out "${kp_signature}" "${kp_receipt}"
export KUBERPLOY_E2E_TEARDOWN_PUBLIC_KEY_FILE="${kp_attacker_public}"
kp_expect_failure unpinned-teardown-key \
  "${kp_root}/scripts/kubernetes/test/e2e/finalize-teardown.sh"
export KUBERPLOY_E2E_TEARDOWN_PUBLIC_KEY_FILE="${kp_teardown_public}"
/usr/bin/openssl dgst -sha256 -sign "${kp_teardown_private}" -out "${kp_signature}" "${kp_receipt}"
"${kp_root}/scripts/kubernetes/test/e2e/finalize-teardown.sh" >/dev/null
jq -e '.status == "passed" and .disposableCluster.teardownRequired == false and
  .disposableCluster.teardownVerified == true and
  (.disposableCluster.receiptSHA256 | test("^[a-f0-9]{64}$"))' "${kp_success_report}" >/dev/null
kp_expect_failure replayed-teardown-receipt \
  "${kp_root}/scripts/kubernetes/test/e2e/finalize-teardown.sh"
kp_delete_order="$(grep '^kubectl|delete configmap probe-' "${KP_COMMAND_LOG}" | sed 's/.*probe-//' | cut -d' ' -f1)"
[[ "$(head -1 <<<"${kp_delete_order}")" == "100-upgrade-rollback" ]]
[[ "$(tail -1 <<<"${kp_delete_order}")" == "20-postgresql-valkey" ]]

kp_expect_failure ambient-context-change \
  "${kp_root}/scripts/kubernetes/test/e2e/kubectl.sh" config use-context other
kp_expect_failure broad-delete \
  "${kp_root}/scripts/kubernetes/test/e2e/kubectl.sh" delete pods --all
kp_expect_failure selector-delete \
  "${kp_root}/scripts/kubernetes/test/e2e/kubectl.sh" delete pods --selector app=probe

kp_tls_proof="${kp_tmp}/kuberploy-qualification-success1/60-local-tls/evidence/workflow-proof.json"
cp "${kp_tls_proof}" "${kp_tls_proof}.good"
jq '.acmeDirectoryURL="https://attacker.fixture.test/directory"' \
  "${kp_tls_proof}.good" >"${kp_tls_proof}"
kp_expect_failure substituted-acme-directory \
  bash -c 'source "$1"; kp_qualification_validate_semantic_proof 60-local-tls "$2"' _ \
  "${kp_root}/scripts/kubernetes/test/e2e/lib.sh" \
  "${kp_tmp}/kuberploy-qualification-success1/60-local-tls"
mv "${kp_tls_proof}.good" "${kp_tls_proof}"

kp_build_proof="${kp_tmp}/kuberploy-qualification-success1/40-source-build/evidence/workflow-proof.json"
cp "${kp_build_proof}" "${kp_build_proof}.good"
jq '.buildCancellationJobDeleted=false' "${kp_build_proof}.good" >"${kp_build_proof}"
kp_expect_failure forged-build-cancellation-deletion \
  bash -c 'source "$1"; kp_qualification_validate_semantic_proof 40-source-build "$2"' _ \
  "${kp_root}/scripts/kubernetes/test/e2e/lib.sh" \
  "${kp_tmp}/kuberploy-qualification-success1/40-source-build"
mv "${kp_build_proof}.good" "${kp_build_proof}"

# Missing declarative proof and any attempt to weaken a required probe fail
# before mutation. The repository assertion-to-probe binding is exact.
cp "${KUBERPLOY_E2E_SCENARIO_FILE}" "${kp_tmp}/scenario-good.json"
jq '.workflow.security.networkPolicyProvider.image="quay.io/cilium/cilium@sha256:2222222222222222222222222222222222222222222222222222222222222222"' \
  "${kp_tmp}/scenario-good.json" >"${KUBERPLOY_E2E_SCENARIO_FILE}"
kp_expect_failure mismatched-network-policy-provider kp_run_qualification cnifail1
[[ ! -e "${kp_tmp}/kuberploy-qualification-cnifail1/10-one-chart-install" ]]
jq 'del(.stages["50-runtime-edge"].assertions["http-route"])' \
  "${kp_tmp}/scenario-good.json" >"${KUBERPLOY_E2E_SCENARIO_FILE}"
kp_expect_failure missing-assertion kp_run_qualification missing1
[[ ! -e "${kp_tmp}/kuberploy-qualification-missing1" ]]
jq '.stages["90-security"].assertions["cross-tenant-deny"].probe="http"' \
  "${kp_tmp}/scenario-good.json" >"${KUBERPLOY_E2E_SCENARIO_FILE}"
kp_expect_failure weakened-probe kp_run_qualification weakened1
[[ ! -e "${kp_tmp}/kuberploy-qualification-weakened1" ]]
jq '.stages["90-security"].assertions["cross-tenant-deny"]={probe:"api",method:"GET",path:"/qualification",expectedStatus:200,jsonPointer:"/status",expected:true}' \
  "${kp_tmp}/scenario-good.json" >"${KUBERPLOY_E2E_SCENARIO_FILE}"
kp_expect_failure unrelated-api-target kp_run_qualification apiwrong1
[[ ! -e "${kp_tmp}/kuberploy-qualification-apiwrong1" ]]
jq '.stages["50-runtime-edge"].assertions.middleware={probe:"kubernetes",resource:"configmaps",namespace:"kuberploy-e2e-success1",name:"probe-50-runtime-edge",jsonPointer:"/kind",expected:"ConfigMap"}' \
  "${kp_tmp}/scenario-good.json" >"${KUBERPLOY_E2E_SCENARIO_FILE}"
kp_expect_failure marker-configmap-substitution kp_run_qualification marker1
[[ ! -e "${kp_tmp}/kuberploy-qualification-marker1" ]]
jq '.stages["50-runtime-edge"].assertions["http-route"].url="http://unrelated.fixture.test/"' \
  "${kp_tmp}/scenario-good.json" >"${KUBERPLOY_E2E_SCENARIO_FILE}"
kp_expect_failure unrelated-http-host kp_run_qualification httpwrong1
[[ ! -e "${kp_tmp}/kuberploy-qualification-httpwrong1" ]]
jq '.stages["60-local-tls"].assertions["custom-certificate"].hostname="acme.fixture.test"' \
  "${kp_tmp}/scenario-good.json" >"${KUBERPLOY_E2E_SCENARIO_FILE}"
kp_expect_failure unrelated-tls-host kp_run_qualification tlswrong1
[[ ! -e "${kp_tmp}/kuberploy-qualification-tlswrong1" ]]
jq '.stages["30-git-argo"].assertions["git-direct-projection"].contract="git-protected-pr"' \
  "${kp_tmp}/scenario-good.json" >"${KUBERPLOY_E2E_SCENARIO_FILE}"
kp_expect_failure swapped-proof-contract kp_run_qualification swapped1
[[ ! -e "${kp_tmp}/kuberploy-qualification-swapped1" ]]
cp "${kp_tmp}/scenario-good.json" "${KUBERPLOY_E2E_SCENARIO_FILE}"
if rg -n 'KUBERPLOY_E2E_DRIVER_' "${kp_root}/scripts/kubernetes/test/e2e"; then
  printf 'qualification harness still accepts operator executable drivers\n' >&2
  exit 1
fi

# Simulate a lost create response: the pre-mutation planned record remains but
# the exact labeled object exists. Cleanup must recover by identity+ownership,
# delete only that object, and report success without requiring a forged UID.
export KUBERPLOY_E2E_RUN_ID="responseloss1"
export KUBERPLOY_E2E_STAGE_ID="90-security"
export KUBERPLOY_E2E_STAGE_ASSERTIONS="namespace-rbac"
export KUBERPLOY_E2E_KUBECTL="${kp_root}/scripts/kubernetes/test/e2e/kubectl.sh"
export KUBERPLOY_E2E_HELM="${kp_root}/scripts/kubernetes/test/e2e/helm.sh"
export KUBERPLOY_E2E_RUN_LABEL_KEY="kuberploy.io/test-run"
export KUBERPLOY_E2E_MANAGED_BY_LABEL_KEY="app.kubernetes.io/managed-by"
export KUBERPLOY_E2E_MANAGED_BY_LABEL_VALUE="kuberploy-e2e-harness"
kp_response_loss_dir="${kp_tmp}/response-loss-stage"
mkdir -p "${kp_response_loss_dir}/evidence"
export KUBERPLOY_E2E_STAGE_DIR="${kp_response_loss_dir}"
export KUBERPLOY_E2E_STAGE_INVENTORY_FILE="${kp_response_loss_dir}/inventory.ndjson"
export KUBERPLOY_E2E_STAGE_CLEANUP_RESULT_FILE="${kp_response_loss_dir}/cleanup-result.json"
: >"${KP_COMMAND_LOG}.configmap-response-loss-marker"
jq -cn --arg run "${KUBERPLOY_E2E_RUN_ID}" '
  {schemaVersion:1,runID:$run,stage:"90-security",apiVersion:"v1",kind:"ConfigMap",
   namespace:("kuberploy-e2e-"+$run),name:"response-loss-marker",uid:null,
   operation:"planned-create",absentBefore:true,cleanupPolicy:"delete",
   ownership:{runLabelKey:"kuberploy.io/test-run",runLabelValue:$run,
     managedBy:"kuberploy-e2e-harness"}}
' >"${KUBERPLOY_E2E_STAGE_INVENTORY_FILE}"
"${kp_root}/scripts/kubernetes/test/e2e/builtin-driver.sh" cleanup >/dev/null
jq -e '.status == "cleaned" and .cleanedOrRestoredCount == 1 and
  .verifiedUIDAndOwnership == true' "${KUBERPLOY_E2E_STAGE_CLEANUP_RESULT_FILE}" >/dev/null
grep -F 'kubectl|delete configmap response-loss-marker' "${KP_COMMAND_LOG}" >/dev/null

# Public effects require a separate, run-and-host-bound acknowledgement and
# credential input before any artifact directory or mutation is created.
export KUBERPLOY_E2E_RUN_ID="public1"
export KUBERPLOY_E2E_MUTATION_ACK="qualify:public1:${KUBERPLOY_TEST_CONTEXT}"
export KUBERPLOY_E2E_DISPOSABLE_CLUSTER_ACK="destroy-after-qualification:public1:${KUBERPLOY_TEST_CONTEXT}"
export KUBERPLOY_E2E_ARTIFACT_DIR="${kp_tmp}/kuberploy-qualification-public1"
export KUBERPLOY_E2E_PUBLIC_PROVIDER_TESTS="true"
export KUBERPLOY_E2E_PUBLIC_HOSTNAME="public.fixture.test"
export KUBERPLOY_E2E_ACME_EMAIL="operator@example.test"
export KUBERPLOY_E2E_PUBLIC_EFFECTS_ACK="public-provider:public1:public.fixture.test"
kp_expect_failure unsupported-public-provider \
  "${kp_root}/scripts/kubernetes/test/e2e/qualification.sh"
[[ ! -e "${KUBERPLOY_E2E_ARTIFACT_DIR}" ]]

if rg -n 'kubectl[[:space:]]+config[[:space:]]+use-context|delete[^\n]*(--all|\*)' \
    "${kp_root}/scripts/kubernetes/test/e2e"; then
  printf 'qualification harness contains ambient-context mutation or broad deletion\n' >&2
  exit 1
fi

printf 'Kubernetes qualification order, selector, ownership, failure, cleanup, and report tests passed.\n'
