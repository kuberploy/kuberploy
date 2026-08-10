#!/usr/bin/env bash

# Stage-90 helpers. This file is sourced by builtin-driver.sh after its
# inventory and HTTP helpers are defined; sourcing it performs no I/O.

readonly KP_QUALIFICATION_NETWORK_SERVER_IMAGE="registry.k8s.io/e2e-test-images/agnhost:2.53@sha256:99c6b4bb4a1e1df3f0b3752168c89358794d02258ebebc26bf21c29399011a85"
readonly KP_QUALIFICATION_NETWORK_CLIENT_IMAGE="docker.io/library/alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b"

kp_security_resource_for_kind() {
  case "${1:?kind required}" in
    Pod) printf '%s\n' pod ;;
    Service) printf '%s\n' service ;;
    NetworkPolicy) printf '%s\n' networkpolicy ;;
    Secret) printf '%s\n' secret ;;
    *) kp_die "unsupported stage-90 inventory kind: $1" ;;
  esac
}

kp_security_create_object() {
  local kp_api_version="${1:?apiVersion required}" kp_kind="${2:?kind required}"
  local kp_ns="${3:?namespace required}" kp_name="${4:?name required}"
  local kp_json="${5:?JSON required}" kp_resource kp_current kp_uid
  kp_resource="$(kp_security_resource_for_kind "${kp_kind}")"
  [[ -z "$("${KUBERPLOY_E2E_KUBECTL}" get "${kp_resource}" "${kp_name}" \
    --namespace "${kp_ns}" --ignore-not-found -o name)" ]] || \
    kp_die "stage-90 resource ${kp_kind}/${kp_ns}/${kp_name} already exists"
  kp_plan_create_inventory "${kp_api_version}" "${kp_kind}" "${kp_ns}" "${kp_name}"
  printf '%s\n' "${kp_json}" | "${KUBERPLOY_E2E_KUBECTL}" create -f - >/dev/null
  kp_current="$("${KUBERPLOY_E2E_KUBECTL}" get "${kp_resource}" "${kp_name}" \
    --namespace "${kp_ns}" -o json)"
  jq -e --arg run "${KUBERPLOY_E2E_RUN_ID}" --arg managed "${KUBERPLOY_E2E_MANAGED_BY_LABEL_VALUE}" \
    '.metadata.labels["kuberploy.io/test-run"] == $run and
     .metadata.labels["app.kubernetes.io/managed-by"] == $managed and
     (.metadata.uid | type == "string" and length > 0)' <<<"${kp_current}" >/dev/null
  kp_uid="$(jq -er '.metadata.uid' <<<"${kp_current}")"
  kp_finalize_create_inventory "${kp_kind}" "${kp_ns}" "${kp_name}" "${kp_uid}"
}

kp_security_attest_network_provider() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_provider kp_ns kp_name kp_container kp_image
  kp_provider="$(jq -c '.workflow.security.networkPolicyProvider' "${KUBERPLOY_E2E_SCENARIO_FILE}")"
  kp_ns="$(jq -r '.namespace' <<<"${kp_provider}")"
  kp_name="$(jq -r '.daemonSet' <<<"${kp_provider}")"
  kp_container="$(jq -r '.container' <<<"${kp_provider}")"
  kp_image="$(jq -r '.image' <<<"${kp_provider}")"
  "${KUBERPLOY_E2E_KUBECTL}" api-resources -o name | \
    grep -Fx 'networkpolicies.networking.k8s.io' >/dev/null || \
    kp_die "the selected cluster does not expose NetworkPolicy resources"
  "${KUBERPLOY_E2E_KUBECTL}" get daemonset "${kp_name}" --namespace "${kp_ns}" -o json | \
    jq -e --arg ns "${kp_ns}" --arg name "${kp_name}" --arg container "${kp_container}" \
      --arg image "${kp_image}" '
        select(.metadata.namespace == $ns and .metadata.name == $name) |
        select(.metadata.uid | type == "string" and length > 0) |
        select(.status.desiredNumberScheduled | type == "number" and . > 0) |
        select(.status.numberReady == .status.desiredNumberScheduled) |
        select(any(.spec.template.spec.containers[]; .name == $container and .image == $image)) |
        {namespace:$ns,daemonSet:$name,uid:.metadata.uid,container:$container,image:$image,
         desiredNumberScheduled:.status.desiredNumberScheduled,numberReady:.status.numberReady,
         identityAttested:true}
      ' >"${kp_dir}/workflow-network-provider.json" || \
    kp_die "the scenario-pinned NetworkPolicy provider identity is absent or not Ready"
}

kp_security_resource_quota_rejection() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_state kp_ns kp_name kp_key kp_value
  local kp_quota kp_resources kp_status
  kp_state="${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"
  kp_ns="$(jq -r '.directEnvironmentNamespace' "${kp_state}")"
  kp_name="$(jq -r '.workflow.security.resourceQuota.name' "${kp_scenario}")"
  kp_key="$(jq -r '.workflow.security.resourceQuota.resource' "${kp_scenario}")"
  kp_value="$(jq -r '.workflow.security.resourceQuota.exceededValue' "${kp_scenario}")"
  kp_quota="$("${KUBERPLOY_E2E_KUBECTL}" get resourcequota "${kp_name}" --namespace "${kp_ns}" -o json)"
  jq -e --arg ns "${kp_ns}" --arg name "${kp_name}" --arg key "${kp_key}" '
    select(.metadata.namespace == $ns and .metadata.name == $name) |
    select(.metadata.uid | type == "string" and length > 0) |
    select(.status.hard[$key] | type == "string" and length > 0) |
    {namespace:$ns,name:$name,uid:.metadata.uid,resource:$key,
     hard:.status.hard[$key],used:(.status.used[$key] // "0")}
  ' <<<"${kp_quota}" >"${kp_dir}/workflow-resource-quota.json" || \
    kp_die "the exact scenario ResourceQuota and hard resource are unavailable"
  case "${kp_key}" in
    requests.cpu) kp_resources="$(jq -cn --arg value "${kp_value}" '{requests:{cpu:$value}}')" ;;
    requests.memory) kp_resources="$(jq -cn --arg value "${kp_value}" '{requests:{memory:$value}}')" ;;
    limits.cpu) kp_resources="$(jq -cn --arg value "${kp_value}" '{limits:{cpu:$value}}')" ;;
    limits.memory) kp_resources="$(jq -cn --arg value "${kp_value}" '{limits:{memory:$value}}')" ;;
    *) kp_die "unsupported ResourceQuota qualification key ${kp_key}" ;;
  esac
  if jq -n --arg ns "${kp_ns}" --argjson resources "${kp_resources}" '
      {apiVersion:"v1",kind:"Pod",metadata:{name:"qualification-quota-denial",namespace:$ns},
       spec:{automountServiceAccountToken:false,restartPolicy:"Never",securityContext:{runAsNonRoot:true,
         runAsUser:65532,runAsGroup:65532,seccompProfile:{type:"RuntimeDefault"}},containers:[{
           name:"probe",image:"registry.k8s.io/pause:3.10",resources:$resources,
           securityContext:{allowPrivilegeEscalation:false,readOnlyRootFilesystem:true,
             capabilities:{drop:["ALL"]}}}]}}
    ' | "${KUBERPLOY_E2E_KUBECTL}" create --dry-run=server -f - \
      >"${kp_dir}/workflow-resource-quota-deny.stdout" \
      2>"${kp_dir}/workflow-resource-quota-deny.stderr"; then
    kp_die "ResourceQuota admission accepted the deliberately over-limit Pod"
  else
    kp_status=$?
  fi
  [[ "${kp_status}" -ne 0 ]] && \
    grep -F "${kp_name}" "${kp_dir}/workflow-resource-quota-deny.stderr" >/dev/null && \
    grep -Eiq 'exceeded quota|exceeds.*quota' "${kp_dir}/workflow-resource-quota-deny.stderr" || \
    kp_die "over-limit Pod was not rejected by the exact ResourceQuota"
}

kp_security_network_policy_enforcement() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_labels kp_server kp_service kp_deny kp_allow
  local kp_denied kp_allowed
  kp_security_attest_network_provider
  kp_labels="$(jq -cn --arg run "${KUBERPLOY_E2E_RUN_ID}" --arg managed "${KUBERPLOY_E2E_MANAGED_BY_LABEL_VALUE}" \
    '{"kuberploy.io/test-run":$run,"app.kubernetes.io/managed-by":$managed}')"
  kp_server="$(jq -cn --arg ns "${kp_namespace}" --arg image "${KP_QUALIFICATION_NETWORK_SERVER_IMAGE}" \
    --argjson labels "${kp_labels}" '{apiVersion:"v1",kind:"Pod",metadata:{name:"qualification-network-server",
      namespace:$ns,labels:($labels+{"kuberploy.io/network-role":"server"})},spec:{automountServiceAccountToken:false,
      securityContext:{runAsNonRoot:true,runAsUser:65532,runAsGroup:65532,seccompProfile:{type:"RuntimeDefault"}},
      containers:[{name:"server",image:$image,command:["/agnhost"],args:["netexec","--http-port=8080","--udp-port=-1"],
      ports:[{name:"http",containerPort:8080}],readinessProbe:{httpGet:{path:"/",port:"http"},periodSeconds:1},
      resources:{requests:{cpu:"10m",memory:"16Mi"},limits:{cpu:"100m",memory:"64Mi"}},securityContext:{
      allowPrivilegeEscalation:false,readOnlyRootFilesystem:true,capabilities:{drop:["ALL"]}}}]}}')"
  kp_service="$(jq -cn --arg ns "${kp_namespace}" --argjson labels "${kp_labels}" '{apiVersion:"v1",kind:"Service",
    metadata:{name:"qualification-network",namespace:$ns,labels:$labels},spec:{selector:{"kuberploy.io/network-role":"server"},
    ports:[{name:"http",port:8080,targetPort:"http"}]}}')"
  kp_deny="$(jq -cn --arg ns "${kp_namespace}" --argjson labels "${kp_labels}" '{apiVersion:"networking.k8s.io/v1",
    kind:"NetworkPolicy",metadata:{name:"qualification-network-deny",namespace:$ns,labels:$labels},spec:{
    podSelector:{matchLabels:{"kuberploy.io/network-role":"server"}},policyTypes:["Ingress"]}}')"
  kp_allow="$(jq -cn --arg ns "${kp_namespace}" --argjson labels "${kp_labels}" '{apiVersion:"networking.k8s.io/v1",
    kind:"NetworkPolicy",metadata:{name:"qualification-network-allow",namespace:$ns,labels:$labels},spec:{
    podSelector:{matchLabels:{"kuberploy.io/network-role":"server"}},policyTypes:["Ingress"],ingress:[{
    from:[{podSelector:{matchLabels:{"kuberploy.io/network-access":"allowed"}}}],ports:[{protocol:"TCP",port:8080}]}]}}')"
  kp_denied="$(jq -cn --arg ns "${kp_namespace}" --arg image "${KP_QUALIFICATION_NETWORK_CLIENT_IMAGE}" \
    --argjson labels "${kp_labels}" '{apiVersion:"v1",kind:"Pod",metadata:{name:"qualification-network-denied",namespace:$ns,
    labels:($labels+{"kuberploy.io/network-access":"denied"})},spec:{automountServiceAccountToken:false,restartPolicy:"Never",
    securityContext:{runAsNonRoot:true,runAsUser:65532,runAsGroup:65532,seccompProfile:{type:"RuntimeDefault"}},containers:[{
    name:"probe",image:$image,command:["/bin/sh","-ec"],args:["if wget -T 5 -qO- http://qualification-network:8080/echo?msg=unexpected; then exit 1; fi"],
    resources:{requests:{cpu:"10m",memory:"16Mi"},limits:{cpu:"100m",memory:"64Mi"}},securityContext:{
    allowPrivilegeEscalation:false,readOnlyRootFilesystem:true,capabilities:{drop:["ALL"]}}}]}}')"
  kp_allowed="$(jq -cn --arg ns "${kp_namespace}" --arg image "${KP_QUALIFICATION_NETWORK_CLIENT_IMAGE}" \
    --argjson labels "${kp_labels}" '{apiVersion:"v1",kind:"Pod",metadata:{name:"qualification-network-allowed",namespace:$ns,
    labels:($labels+{"kuberploy.io/network-access":"allowed"})},spec:{automountServiceAccountToken:false,restartPolicy:"Never",
    securityContext:{runAsNonRoot:true,runAsUser:65532,runAsGroup:65532,seccompProfile:{type:"RuntimeDefault"}},containers:[{
    name:"probe",image:$image,command:["/bin/sh","-ec"],args:["for attempt in $(seq 1 12); do wget -T 5 -qO- http://qualification-network:8080/echo?msg=kuberploy-network-policy-ok | grep -Fx kuberploy-network-policy-ok && exit 0; sleep 2; done; exit 1"],
    resources:{requests:{cpu:"10m",memory:"16Mi"},limits:{cpu:"100m",memory:"64Mi"}},securityContext:{
    allowPrivilegeEscalation:false,readOnlyRootFilesystem:true,capabilities:{drop:["ALL"]}}}]}}')"
  kp_security_create_object v1 Pod "${kp_namespace}" qualification-network-server "${kp_server}"
  kp_security_create_object v1 Service "${kp_namespace}" qualification-network "${kp_service}"
  "${KUBERPLOY_E2E_KUBECTL}" wait --namespace "${kp_namespace}" --for=condition=Ready \
    pod/qualification-network-server --timeout=180s >/dev/null
  kp_security_create_object networking.k8s.io/v1 NetworkPolicy "${kp_namespace}" qualification-network-deny "${kp_deny}"
  kp_security_create_object v1 Pod "${kp_namespace}" qualification-network-denied "${kp_denied}"
  "${KUBERPLOY_E2E_KUBECTL}" wait --namespace "${kp_namespace}" --for=jsonpath='{.status.phase}'=Succeeded \
    pod/qualification-network-denied --timeout=90s >/dev/null || \
    kp_die "the scenario-pinned CNI did not enforce default-deny ingress"
  "${KUBERPLOY_E2E_KUBECTL}" logs --namespace "${kp_namespace}" qualification-network-denied \
    >"${kp_dir}/workflow-network-denied.log"
  [[ ! -s "${kp_dir}/workflow-network-denied.log" ]] || kp_die "denied network probe emitted unexpected output"
  kp_security_create_object networking.k8s.io/v1 NetworkPolicy "${kp_namespace}" qualification-network-allow "${kp_allow}"
  kp_security_create_object v1 Pod "${kp_namespace}" qualification-network-allowed "${kp_allowed}"
  "${KUBERPLOY_E2E_KUBECTL}" wait --namespace "${kp_namespace}" --for=jsonpath='{.status.phase}'=Succeeded \
    pod/qualification-network-allowed --timeout=90s >/dev/null || \
    kp_die "the scenario-pinned CNI did not enforce the exact allow rule"
  "${KUBERPLOY_E2E_KUBECTL}" logs --namespace "${kp_namespace}" qualification-network-allowed \
    >"${kp_dir}/workflow-network-allowed.log"
  [[ "$(tr -d '\r' <"${kp_dir}/workflow-network-allowed.log")" == "kuberploy-network-policy-ok" ]] || \
    kp_die "allowed network probe response did not match the exact marker"
}

kp_security_audit_timeline() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_deployment kp_actor kp_query
  kp_deployment="$(jq -er '.directDeploymentId' "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json")"
  kp_fixed_get "/v1/me" "${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}" 200 "${kp_dir}/workflow-audit-actor.json"
  kp_actor="$(jq -er '.id | select(test("^[a-f0-9-]{36}$"))' "${kp_dir}/workflow-audit-actor.json")"
  kp_query="/v1/audit-events?targetType=deployment&targetId=${kp_deployment}&action=deployment.config.accepted&limit=20"
  kp_fixed_get "${kp_query}" "${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}" 200 \
    "${kp_dir}/workflow-audit-timeline.json"
  jq -e --arg actor "${kp_actor}" --arg target "${kp_deployment}" '
    (.items | type == "array" and length >= 2 and length <= 20) and
    all(.items[]; (.id | test("^[a-f0-9-]{36}$")) and .actorId == $actor and
      .action == "deployment.config.accepted" and .targetType == "deployment" and
      .targetId == $target and .outcome == "accepted" and
      ((keys - ["id","actorId","action","targetType","targetId","outcome","requestId","createdAt"]) | length == 0))
  ' "${kp_dir}/workflow-audit-timeline.json" >/dev/null || \
    kp_die "audit timeline did not bind the session actor, exact deployment, action and accepted outcome"
}
