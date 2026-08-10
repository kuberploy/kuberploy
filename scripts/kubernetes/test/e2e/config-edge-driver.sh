#!/usr/bin/env bash

# Sourced by builtin-driver.sh. The caller owns strict mode and common helpers.
# This file deliberately emits proof only for observations made below. The
# hostname-only LoadBalancer and ExternalDNS provider lanes need dedicated
# alternate/provider scenarios and are not represented by success booleans.

kp_config_edge_request() {
  local kp_method="${1:?method}" kp_path="${2:?path}" kp_body="${3-}" kp_out="${4:?output}"
  local -a kp_curl_args
  shift 4
  kp_curl_args=(-sS -o "${kp_out}" -w '%{http_code}' -X "${kp_method}"
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")"
    --header "X-CSRF-Token: $(<"${KUBERPLOY_E2E_CSRF_TOKEN_FILE}")"
    -H 'Content-Type: application/json')
  if [[ -n "${kp_body}" ]]; then
    kp_curl_args+=(--data-binary "${kp_body}")
  fi
  curl "${kp_curl_args[@]}" "$@" "$(jq -r '.apiBaseURL' "${kp_scenario}")${kp_path}"
}

kp_config_edge_put() {
  local kp_path="${1:?path}" kp_body="${2:?body}" kp_preview="${3:?preview}" kp_key="${4:?key}" kp_out="${5:?output}"
  kp_config_edge_request PUT "${kp_path}" "${kp_body}" "${kp_out}" \
    --header "Preview-Token: ${kp_preview}" --header "Idempotency-Key: ${kp_key}"
}

kp_config_edge_variable_set() {
  local kp_scope="${1:?scope}" kp_yaml="${2:?yaml}" kp_dir="${3:?dir}" kp_environment="${4:?environment}"
  local kp_base kp_body kp_etag kp_preview kp_operation kp_actual
  kp_base="$(jq -r '.apiBaseURL' "${kp_scenario}")"
  curl -sS -o "${kp_dir}/${kp_scope}-before.json" -w '%{http_code}' \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    "${kp_base}/v1/environments/${kp_environment}/variable-sets" | grep -Fx 200 >/dev/null
  kp_etag="$(jq -r --arg scope "${kp_scope}" '.items[]|select(.scope==$scope)|(.etag // "")' "${kp_dir}/${kp_scope}-before.json")"
  kp_body="$(jq -cn --arg raw "${kp_yaml}" '{rawYaml:$raw}')"
  kp_actual="$(kp_config_edge_request POST "/v1/environments/${kp_environment}/variable-sets/${kp_scope}/preview" \
    "${kp_body}" "${kp_dir}/${kp_scope}-preview.json" --header "If-Match: ${kp_etag}")"
  [[ "${kp_actual}" == 200 ]]
  jq -e --arg scope "${kp_scope}" '.previewToken|type=="string" and length==43' "${kp_dir}/${kp_scope}-preview.json" >/dev/null
  kp_preview="$(jq -er '.previewToken' "${kp_dir}/${kp_scope}-preview.json")"
  kp_actual="$(kp_config_edge_put "/v1/environments/${kp_environment}/variable-sets/${kp_scope}" "${kp_body}" "${kp_preview}" \
    "qualification-${KUBERPLOY_E2E_RUN_ID}-variables-${kp_scope}" "${kp_dir}/${kp_scope}-operation.json")"
  [[ "${kp_actual}" == 202 ]]
  kp_operation="$(jq -er '.id' "${kp_dir}/${kp_scope}-operation.json")"
  kp_poll_operation "${kp_operation}" "${kp_dir}/${kp_scope}-terminal.json"
  jq -e --arg operation "${kp_operation}" '.id==$operation and .status=="succeeded" and (.gitRevision.commit|test("^[0-9a-f]{40}$"))' \
    "${kp_dir}/${kp_scope}-terminal.json" >/dev/null
}

kp_config_edge_rejection() {
  local kp_name="${1:?name}" kp_body="${2:?body}" kp_dir="${3:?dir}"
  local kp_actual
  kp_actual="$(kp_config_edge_request POST /v1/deployments "${kp_body}" "${kp_dir}/${kp_name}.json" \
    --header "Idempotency-Key: qualification-${KUBERPLOY_E2E_RUN_ID}-${kp_name}")"
  [[ "${kp_actual}" == 422 ]]
  jq -e '.code=="SchedulingProfileInvalid"' "${kp_dir}/${kp_name}.json" >/dev/null
}

kp_config_edge_wait_argo() {
  local kp_deployment="${1:?deployment}" kp_application="${2:?application}" kp_project="${3:?project}"
  local kp_environment="${4:?environment}" kp_revision="${5:?revision}" kp_out="${6:?output}"
  local kp_name="kp-d-${kp_deployment//-/}"
  for _ in {1..120}; do
    if "${KUBERPLOY_E2E_KUBECTL}" get application "${kp_name}" --namespace argocd -o json >"${kp_out}" 2>/dev/null &&
      jq -e --arg deployment "${kp_deployment}" --arg application "${kp_application}" --arg project "${kp_project}" \
        --arg environment "${kp_environment}" --arg revision "${kp_revision}" '
        .metadata.labels["app.kubernetes.io/managed-by"]=="kuberploy" and
        .metadata.labels["kuberploy.io/deployment-id"]==$deployment and
        .metadata.labels["kuberploy.io/application-id"]==$application and
        .metadata.labels["kuberploy.io/project-id"]==$project and
        .metadata.labels["kuberploy.io/environment-id"]==$environment and
        .status.sync.status=="Synced" and .status.health.status=="Healthy" and
        ([.status.sync.revision,.status.sync.revisions[]?]|index($revision)!=null) and
        any(.status.resources[]; .kind=="Deployment" and .status=="Synced" and .health.status=="Healthy")' \
        "${kp_out}" >/dev/null; then
      return 0
    fi
    sleep 5
  done
  kp_die "config-edge Argo Application did not converge to ${kp_revision}"
}

kp_config_edge_create_object() {
  local kp_object="${1:?object JSON}" kp_kind kp_resource kp_api kp_name kp_namespace kp_uid
  local -a kp_scope_args
  kp_kind="$(jq -er '.kind' <<<"${kp_object}")"
  kp_resource="$(tr '[:upper:]' '[:lower:]' <<<"${kp_kind}")"
  kp_api="$(jq -er '.apiVersion' <<<"${kp_object}")"
  kp_name="$(jq -er '.metadata.name' <<<"${kp_object}")"
  kp_namespace="$(jq -r '.metadata.namespace // ""' <<<"${kp_object}")"
  kp_scope_args=()
  [[ -z "${kp_namespace}" ]] || kp_scope_args=(--namespace "${kp_namespace}")
  if [[ -n "$("${KUBERPLOY_E2E_KUBECTL}" get "${kp_resource}" "${kp_name}" "${kp_scope_args[@]}" --ignore-not-found -o name)" ]]; then
    kp_die "config-edge owned ${kp_kind}/${kp_namespace}/${kp_name} already exists"
  fi
  kp_plan_create_inventory "${kp_api}" "${kp_kind}" "${kp_namespace}" "${kp_name}"
  "${KUBERPLOY_E2E_KUBECTL}" create -f - <<<"${kp_object}" >/dev/null
  kp_uid="$("${KUBERPLOY_E2E_KUBECTL}" get "${kp_resource}" "${kp_name}" "${kp_scope_args[@]}" -o json | jq -er '.metadata.uid')"
  kp_finalize_create_inventory "${kp_kind}" "${kp_namespace}" "${kp_name}" "${kp_uid}"
}

kp_config_edge_external_dns() {
  local kp_dir="${1:?evidence dir}" kp_state="${2:?state}" kp_project="${3:?project}" kp_environment="${4:?environment}"
  local kp_expected_address="${5:?expected DNS address}"
  local kp_namespace="${KUBERPLOY_E2E_EXTERNAL_DNS_NAMESPACE:?external DNS namespace required}"
  local kp_image="${KUBERPLOY_E2E_RFC2136_PROVIDER_IMAGE:?RFC2136 provider image required}"
  local kp_runtime_image
  local kp_secret kp_provider kp_config kp_egress kp_provider_policy kp_zone kp_nameserver kp_tsig_name kp_tsig_secret
  local kp_labels kp_object kp_body kp_actual kp_integration kp_app kp_deployment kp_operation kp_etag kp_preview kp_host kp_query_name kp_query_succeeded kp_attempt
  [[ "${kp_image}" =~ @sha256:[a-f0-9]{64}$ ]] || kp_die "RFC2136 provider image must be digest pinned"
  kp_runtime_image="$(jq -er '.workflow.directDeployment.image|select(test("@sha256:[a-f0-9]{64}$"))' "${kp_scenario}")"
  [[ "${KUBERPLOY_E2E_EXTERNAL_DNS_NAMESPACE_AUTHORITY:-}" == "pre-existing:${KUBERPLOY_E2E_RUN_ID}:${kp_namespace}" ]] ||
    kp_die "external DNS namespace authority must explicitly bind the pre-existing namespace to this run"
  "${KUBERPLOY_E2E_KUBECTL}" get namespace "${kp_namespace}" -o json >"${kp_dir}/external-dns-namespace.json"
  jq -e --arg ns "${kp_namespace}" '.metadata.name==$ns and (.metadata.uid|type=="string" and length>0)' "${kp_dir}/external-dns-namespace.json" >/dev/null
  kp_secret="kp-rfc2136-${KUBERPLOY_E2E_RUN_ID}"
  kp_provider="kp-rfc2136-${KUBERPLOY_E2E_RUN_ID}"
  kp_config="kp-rfc2136-config-${KUBERPLOY_E2E_RUN_ID}"
  kp_egress="kp-rfc2136-egress-${KUBERPLOY_E2E_RUN_ID}"
  kp_provider_policy="kp-rfc2136-ingress-${KUBERPLOY_E2E_RUN_ID}"
  kp_zone="qualification.test"
  kp_nameserver="ns.${kp_zone}"
  kp_tsig_name="kuberploy-${KUBERPLOY_E2E_RUN_ID}.${kp_zone}"
  kp_tsig_secret="$(<"${KUBERPLOY_E2E_RFC2136_TSIG_SECRET_FILE:?RFC2136 TSIG secret file required}")"
  [[ "$(wc -l <"${KUBERPLOY_E2E_RFC2136_TSIG_SECRET_FILE}" | tr -d ' ')" == 1 ]] || kp_die "RFC2136 TSIG secret file must contain exactly one line"
  [[ "${kp_tsig_secret}" =~ ^[A-Za-z0-9+/]+={0,2}$ ]] || kp_die "RFC2136 TSIG secret is not base64"
  local kp_tsig_bytes kp_tsig_canonical
  kp_tsig_bytes="$(printf '%s' "${kp_tsig_secret}" | openssl base64 -d -A | wc -c | tr -d ' ')"
  [[ "${kp_tsig_bytes}" =~ ^[0-9]+$ ]] && ((kp_tsig_bytes >= 16 && kp_tsig_bytes <= 64)) || kp_die "RFC2136 TSIG secret must decode to 16 through 64 bytes"
  kp_tsig_canonical="$(printf '%s' "${kp_tsig_secret}" | openssl base64 -d -A | openssl base64 -A)"
  [[ "${kp_tsig_canonical}" == "${kp_tsig_secret}" ]] || kp_die "RFC2136 TSIG secret must use canonical padded base64"
  kp_labels="$(jq -cn --arg run "${KUBERPLOY_E2E_RUN_ID}" --arg managed "${KUBERPLOY_E2E_MANAGED_BY_LABEL_VALUE}" \
    '{"kuberploy.io/test-run":$run,"app.kubernetes.io/managed-by":$managed,"kuberploy.io/qualification-component":"rfc2136"}')"

  kp_object="$(jq -cn --arg ns "${kp_namespace}" --arg name "${kp_secret}" --argjson labels "${kp_labels}" \
    --arg key "${kp_tsig_name}" --arg secret "${kp_tsig_secret}" '{apiVersion:"v1",kind:"Secret",metadata:{name:$name,namespace:$ns,labels:$labels},type:"Opaque",stringData:{EXTERNAL_DNS_RFC2136_TSIG_KEYNAME:$key,EXTERNAL_DNS_RFC2136_TSIG_SECRET:$secret,EXTERNAL_DNS_RFC2136_TSIG_SECRET_ALG:"hmac-sha256"}}')"
  kp_config_edge_create_object "${kp_object}"
  kp_object="$(jq -cn --arg ns "${kp_namespace}" --arg name "${kp_config}" --argjson labels "${kp_labels}" --arg provider "${kp_provider}" --arg zone "${kp_zone}" \
    '{apiVersion:"v1",kind:"ConfigMap",metadata:{name:$name,namespace:$ns,labels:$labels},data:{EXTERNAL_DNS_RFC2136_HOST:$provider,EXTERNAL_DNS_RFC2136_PORT:"53",EXTERNAL_DNS_RFC2136_ZONE:$zone}}')"
  kp_config_edge_create_object "${kp_object}"
  kp_object="$(jq -cn --arg ns "${kp_namespace}" --arg name "${kp_provider}" --argjson labels "${kp_labels}" \
    --arg run "${KUBERPLOY_E2E_RUN_ID}" '{apiVersion:"v1",kind:"Service",metadata:{name:$name,namespace:$ns,labels:$labels},spec:{selector:{"kuberploy.io/qualification-component":"rfc2136","kuberploy.io/test-run":$run},ports:[{name:"dns-udp",protocol:"UDP",port:53,targetPort:5353},{name:"dns-tcp",protocol:"TCP",port:53,targetPort:5353}]}}')"
  kp_config_edge_create_object "${kp_object}"
  kp_object="$(jq -cn --arg ns "${kp_namespace}" --arg name "${kp_provider}" --arg image "${kp_image}" --argjson labels "${kp_labels}" --arg secret "${kp_secret}" \
    --arg zone "${kp_zone}" --arg nameserver "${kp_nameserver}" --arg key "${kp_tsig_name}" \
    '{apiVersion:"v1",kind:"Pod",metadata:{name:$name,namespace:$ns,labels:$labels},spec:{automountServiceAccountToken:false,restartPolicy:"Never",securityContext:{runAsNonRoot:true,seccompProfile:{type:"RuntimeDefault"}},containers:[{name:"authority",image:$image,securityContext:{allowPrivilegeEscalation:false,readOnlyRootFilesystem:true,capabilities:{drop:["ALL"]}},ports:[{containerPort:5353,protocol:"UDP"},{containerPort:5353,protocol:"TCP"}],env:[{name:"KUBERPLOY_RFC2136_ZONE",value:$zone},{name:"KUBERPLOY_RFC2136_NAMESERVER",value:$nameserver},{name:"KUBERPLOY_RFC2136_TSIG_NAME",value:$key},{name:"KUBERPLOY_RFC2136_TSIG_SECRET",valueFrom:{secretKeyRef:{name:$secret,key:"EXTERNAL_DNS_RFC2136_TSIG_SECRET"}}}],resources:{requests:{cpu:"10m",memory:"32Mi"},limits:{cpu:"200m",memory:"128Mi"}}}]}}')"
  kp_config_edge_create_object "${kp_object}"
  "${KUBERPLOY_E2E_KUBECTL}" wait --namespace "${kp_namespace}" --for=condition=Ready "pod/${kp_provider}" --timeout=120s >/dev/null
  kp_body="$(jq -cn --arg run "${KUBERPLOY_E2E_RUN_ID}" --arg environment "${kp_environment}" --arg secret "${kp_secret}" --arg config "${kp_config}" --arg egress "${kp_egress}" --arg zone "${kp_zone}" \
    '{slug:("qualification-"+$run),name:"Qualification RFC2136",mode:"managed",providerKind:"rfc2136",txtOwnerId:("kuberploy."+$run),allowedDomainSuffixes:[$zone],syncPolicy:"upsert-only",credentialSecretRef:$secret,providerConfigRef:$config,egressConfigRef:$egress,environmentIds:[$environment]}')"
  kp_actual="$(kp_config_edge_request POST /v1/external-dns/integrations "${kp_body}" "${kp_dir}/external-dns-integration.json" --header "Idempotency-Key: qualification-${KUBERPLOY_E2E_RUN_ID}-external-dns")"
  [[ "${kp_actual}" == 201 ]]
  kp_integration="$(jq -er '.id' "${kp_dir}/external-dns-integration.json")"
  kp_object="$(jq -cn --arg ns "${kp_namespace}" --arg name "${kp_egress}" --argjson labels "${kp_labels}" --arg id "${kp_integration}" --arg run "${KUBERPLOY_E2E_RUN_ID}" --arg cidr "${KUBERPLOY_E2E_KUBE_API_CIDR:?Kubernetes API CIDR required}" \
    '{apiVersion:"networking.k8s.io/v1",kind:"NetworkPolicy",metadata:{name:$name,namespace:$ns,labels:$labels},spec:{podSelector:{matchLabels:{"kuberploy.io/dns-integration":$id}},policyTypes:["Egress"],egress:[{to:[{podSelector:{matchLabels:{"kuberploy.io/qualification-component":"rfc2136","kuberploy.io/test-run":$run}}}],ports:[{protocol:"UDP",port:5353},{protocol:"TCP",port:5353},{protocol:"UDP",port:53},{protocol:"TCP",port:53}]},{to:[{namespaceSelector:{matchLabels:{"kubernetes.io/metadata.name":"kube-system"}},podSelector:{matchLabels:{"k8s-app":"kube-dns"}}}],ports:[{protocol:"UDP",port:53},{protocol:"TCP",port:53}]},{to:[{ipBlock:{cidr:$cidr}}],ports:[{protocol:"TCP",port:443}]}]}}')"
  kp_config_edge_create_object "${kp_object}"
  kp_object="$(jq -cn --arg ns "${kp_namespace}" --arg name "${kp_provider_policy}" --argjson labels "${kp_labels}" --arg id "${kp_integration}" --arg run "${KUBERPLOY_E2E_RUN_ID}" \
    '{apiVersion:"networking.k8s.io/v1",kind:"NetworkPolicy",metadata:{name:$name,namespace:$ns,labels:$labels},spec:{podSelector:{matchLabels:{"kuberploy.io/qualification-component":"rfc2136","kuberploy.io/test-run":$run}},policyTypes:["Ingress"],ingress:[{from:[{podSelector:{matchLabels:{"kuberploy.io/dns-integration":$id}}}],ports:[{protocol:"UDP",port:5353},{protocol:"TCP",port:5353}]}]}}')"
  kp_config_edge_create_object "${kp_object}"
  for _ in {1..60}; do
    curl -sS -o "${kp_dir}/external-dns-integrations-ready.json" --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/external-dns/integrations"
    jq -e --arg id "${kp_integration}" 'any(.items[]; .id==$id and .protectedGitState=="materialized" and .runtimeRevision==1)' "${kp_dir}/external-dns-integrations-ready.json" >/dev/null && break
    sleep 2
  done
  jq -e --arg id "${kp_integration}" 'any(.items[]; .id==$id and .protectedGitState=="materialized" and .runtimeRevision==1)' "${kp_dir}/external-dns-integrations-ready.json" >/dev/null
  for _ in {1..60}; do
    curl -sS -o "${kp_dir}/external-dns-status.json" --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/external-dns/status"
    jq -e '.controllerReadiness=="ready" and .runtimeAvailable==true' "${kp_dir}/external-dns-status.json" >/dev/null && break
    sleep 2
  done
  jq -e '.controllerReadiness=="ready" and .runtimeAvailable==true' "${kp_dir}/external-dns-status.json" >/dev/null

  kp_human_post create-external-dns-app /v1/applications "$(jq -cn --arg p "${kp_project}" '{projectId:$p,name:"External DNS Edge",slug:"external-dns-edge"}')" 201 "${kp_dir}/external-dns-application.json"
  kp_app="$(jq -er '.id' "${kp_dir}/external-dns-application.json")"
  kp_body="$(jq -cn --arg e "${kp_environment}" --arg a "${kp_app}" --arg image "${kp_runtime_image}" '{environmentId:$e,applicationId:$a,image:$image,runtime:{replicas:1,ports:[{name:"http",containerPort:8080,protocol:"TCP"}],resources:{requests:{cpu:"25m",memory:"32Mi"}}}}')"
  kp_human_post create-external-dns-deployment /v1/deployments "${kp_body}" 202 "${kp_dir}/external-dns-deployment-operation.json"
  kp_operation="$(jq -er '.id' "${kp_dir}/external-dns-deployment-operation.json")"
  kp_deployment="$(jq -er '.targetId' "${kp_dir}/external-dns-deployment-operation.json")"
  kp_poll_operation "${kp_operation}" "${kp_dir}/external-dns-deployment-terminal.json"
  curl -sS -o "${kp_dir}/external-dns-config-before.json" --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment}/config"
  kp_etag="$(jq -er '.etag' "${kp_dir}/external-dns-config-before.json")"
  kp_host="config-edge-${KUBERPLOY_E2E_RUN_ID}.${kp_zone}"
  kp_body="$(jq -cn --arg host "${kp_host}" --arg ref "qualification-${KUBERPLOY_E2E_RUN_ID}" '{mode:"jsonPatch",patch:[{op:"add",path:"/spec/routes",value:[{id:"public",host:$host,path:"/",port:"http",dns:{mode:"externalDns",integrationRef:$ref},tls:{mode:"httpOnly"}}]}]}')"
  kp_actual="$(kp_config_edge_request POST "/v1/deployments/${kp_deployment}/config/preview" "${kp_body}" "${kp_dir}/external-dns-route-preview.json" --header "If-Match: ${kp_etag}")"
  [[ "${kp_actual}" == 200 ]]
  jq -e '.warnings|any(contains("freshly observed ready"))' "${kp_dir}/external-dns-route-preview.json" >/dev/null
  kp_preview="$(jq -er '.previewToken' "${kp_dir}/external-dns-route-preview.json")"
  kp_actual="$(kp_config_edge_put "/v1/deployments/${kp_deployment}/config" "${kp_body}" "${kp_preview}" "qualification-${KUBERPLOY_E2E_RUN_ID}-external-dns-route" "${kp_dir}/external-dns-route-operation.json")"
  [[ "${kp_actual}" == 202 ]]
  kp_operation="$(jq -er '.id' "${kp_dir}/external-dns-route-operation.json")"
  kp_poll_operation "${kp_operation}" "${kp_dir}/external-dns-route-terminal.json"
  local kp_route_revision
  kp_route_revision="$(jq -er '.gitRevision.commit|select(test("^[0-9a-f]{40}$"))' "${kp_dir}/external-dns-route-terminal.json")"
  kp_config_edge_wait_argo "${kp_deployment}" "${kp_app}" "${kp_project}" "${kp_environment}" "${kp_route_revision}" \
    "${kp_dir}/external-dns-route-argo.json"
  kp_query_succeeded=false
  for kp_attempt in {1..12}; do
    kp_query_name="kp-rfc2136-query-${KUBERPLOY_E2E_RUN_ID}-${kp_attempt}"
    kp_object="$(jq -cn --arg ns "${kp_namespace}" --arg name "${kp_query_name}" --arg run "${KUBERPLOY_E2E_RUN_ID}" --arg managed "${KUBERPLOY_E2E_MANAGED_BY_LABEL_VALUE}" --arg id "${kp_integration}" --arg provider "${kp_provider}" --arg host "${kp_host}" \
      '{apiVersion:"v1",kind:"Pod",metadata:{name:$name,namespace:$ns,labels:{"kuberploy.io/test-run":$run,"app.kubernetes.io/managed-by":$managed,"kuberploy.io/dns-integration":$id}},spec:{automountServiceAccountToken:false,restartPolicy:"Never",containers:[{name:"query",image:"docker.io/library/alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b",command:["nslookup",$host,$provider],securityContext:{allowPrivilegeEscalation:false,readOnlyRootFilesystem:true,capabilities:{drop:["ALL"]},runAsNonRoot:true,runAsUser:65532},resources:{requests:{cpu:"5m",memory:"16Mi"},limits:{cpu:"50m",memory:"32Mi"}}}]}}')"
    kp_config_edge_create_object "${kp_object}"
    if "${KUBERPLOY_E2E_KUBECTL}" wait --namespace "${kp_namespace}" --for=jsonpath='{.status.phase}'=Succeeded "pod/${kp_query_name}" --timeout=15s >/dev/null 2>&1; then
      kp_query_succeeded=true
      break
    fi
    sleep 2
  done
  [[ "${kp_query_succeeded}" == true ]]
  "${KUBERPLOY_E2E_KUBECTL}" logs --namespace "${kp_namespace}" "${kp_query_name}" >"${kp_dir}/external-dns-query.txt"
  grep -F "${kp_host}" "${kp_dir}/external-dns-query.txt" >/dev/null
  grep -F "${kp_expected_address}" "${kp_dir}/external-dns-query.txt" >/dev/null
  jq -n --arg integrationId "${kp_integration}" --arg applicationId "${kp_app}" --arg deploymentId "${kp_deployment}" --arg hostname "${kp_host}" \
    '{integrationId:$integrationId,externalDNSApplicationId:$applicationId,externalDNSDeploymentId:$deploymentId,externalDNSHostname:$hostname,providerKind:"rfc2136",protectedMaterialized:true,controllerReady:true,routeSaved:true,dnsReconciled:true}' >"${kp_dir}/external-dns-proof.json"
}

kp_run_config_edge_workflow() {
  local kp_dir="${KUBERPLOY_E2E_STAGE_DIR}/evidence" kp_state="${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json"
  local kp_project kp_environment kp_application kp_other_application kp_namespace
  local kp_profile kp_revision kp_spec_digest kp_assignments_digest kp_body kp_actual kp_operation kp_deployment kp_runtime_image
  local kp_hostname kp_ip kp_first_ip kp_config_etag kp_raw kp_candidate kp_preview kp_revision_fence kp_configmap
  mkdir -p "${kp_dir}"
  kp_project="$(jq -er '.projectId' "${kp_state}")"
  kp_environment="$(jq -er '.directEnvironmentId' "${kp_state}")"
  kp_namespace="$(jq -er '.directEnvironmentNamespace' "${kp_state}")"
  kp_runtime_image="$(jq -er '.workflow.directDeployment.image|select(test("@sha256:[a-f0-9]{64}$"))' "${kp_scenario}")"

  kp_config_edge_variable_set project $'apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  SHARED_REGION: "ap-southeast-1"\n  RELEASE_LANE: "project"\n' "${kp_dir}" "${kp_environment}"
  kp_config_edge_variable_set environment $'apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  RELEASE_LANE: "environment"\n  FEATURE_PROBES: "enabled"\n' "${kp_dir}" "${kp_environment}"
  kp_revision_fence="$(jq -er '.gitRevision.commit' "${kp_dir}/environment-terminal.json")"

  kp_body="$(jq -cn --arg environment "${kp_environment}" '{name:"qualification-scheduling",spec:{description:"qualification exact PodSpec",pod:{requiredNodeAffinity:[{key:"kubernetes.io/os",operator:"In",values:["linux"]}],preferredNodeAffinity:[{weight:70,requirements:[{key:"topology.kubernetes.io/zone",operator:"Exists"}]}],sameApplicationPodAntiAffinity:[{enforcement:"required",topologyKey:"kubernetes.io/hostname"}],topologySpread:[{maxSkew:1,topologyKey:"topology.kubernetes.io/zone",whenUnsatisfiable:"ScheduleAnyway"}],tolerations:[{key:"qualification.kuberploy.io/workload",operator:"Equal",value:"application",effect:"NoSchedule"}]}},assignments:[{scope:"environment",id:$environment}]}')"
  kp_human_post create-config-edge-scheduling /v1/platform/scheduling-profiles "${kp_body}" 201 "${kp_dir}/scheduling-profile.json"
  kp_profile="$(jq -er '.profile.id' "${kp_dir}/scheduling-profile.json")"
  kp_revision="$(jq -er '.revision.revision' "${kp_dir}/scheduling-profile.json")"
  kp_spec_digest="$(jq -er '.revision.specDigest' "${kp_dir}/scheduling-profile.json")"
  kp_assignments_digest="$(jq -er '.revision.assignmentsDigest' "${kp_dir}/scheduling-profile.json")"

  kp_human_post create-config-edge-app /v1/applications "$(jq -cn --arg p "${kp_project}" '{projectId:$p,name:"Config Edge",slug:"config-edge"}')" 201 "${kp_dir}/application.json"
  kp_application="$(jq -er '.id' "${kp_dir}/application.json")"
  kp_human_post create-config-edge-other-app /v1/applications "$(jq -cn --arg p "${kp_project}" '{projectId:$p,name:"Config Edge Other",slug:"config-edge-other"}')" 201 "${kp_dir}/other-application.json"
  kp_other_application="$(jq -er '.id' "${kp_dir}/other-application.json")"

  kp_actual="$(curl -sS -o "${kp_dir}/sslip-preview.json" -w '%{http_code}' --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/applications/${kp_application}/sslip-hostname?environmentId=${kp_environment}")"
  [[ "${kp_actual}" == 200 ]]
  kp_hostname="$(jq -er '.hostname|select(test("^[^.]+\\.[0-9]+-[0-9]+-[0-9]+-[0-9]+\\.sslip\\.io$"))' "${kp_dir}/sslip-preview.json")"
  kp_ip="$(cut -d. -f2 <<<"${kp_hostname}" | tr - '.')"
  "${KUBERPLOY_E2E_KUBECTL}" get services --namespace kuberploy-system -l app.kubernetes.io/name=traefik -o json >"${kp_dir}/edge-services.json"
  kp_first_ip="$(jq -er '
    def canonical_public_v4:
      . as $raw | split(".") | select(length==4) | map(tonumber)
      | select(all(.[]; .>=0 and .<=255))
      | select((map(tostring)|join("."))==$raw)
      | select(
          .[0]!=0 and .[0]!=10 and .[0]!=127 and .[0]<224 and
          ((.[0]==100 and .[1]>=64 and .[1]<=127)|not) and
          ((.[0]==169 and .[1]==254)|not) and
          ((.[0]==172 and .[1]>=16 and .[1]<=31)|not) and
          ((.[0]==192 and .[1]==0 and (.[2]==0 or .[2]==2))|not) and
          ((.[0]==192 and .[1]==88 and .[2]==99)|not) and
          ((.[0]==192 and .[1]==168)|not) and
          ((.[0]==198 and (.[1]==18 or .[1]==19))|not) and
          ((.[0]==198 and .[1]==51 and .[2]==100)|not) and
          ((.[0]==203 and .[1]==0 and .[2]==113)|not)) ;
    [.items[]|select(.spec.type=="LoadBalancer")|.status.loadBalancer.ingress[]?.ip
      | select(test("^[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+$")) | canonical_public_v4]
    | sort | first | map(tostring) | join(".")' "${kp_dir}/edge-services.json")"
  [[ "${kp_ip}" == "${kp_first_ip}" ]]

  kp_body="$(jq -cn --arg e "${kp_environment}" --arg a "${kp_application}" --arg image "${kp_runtime_image}" --arg p "${kp_profile}" --argjson r "${kp_revision}" --arg s "${kp_spec_digest}" --arg d "${kp_assignments_digest}" \
    '{environmentId:$e,applicationId:$a,image:$image,runtime:{replicas:1,ports:[{name:"http",containerPort:8080,protocol:"TCP"}],resources:{requests:{cpu:"50m",memory:"64Mi"},limits:{cpu:"250m",memory:"128Mi"}},probes:{readiness:{httpGet:{path:"/ready",port:8080},initialDelaySeconds:1,periodSeconds:5},liveness:{httpGet:{path:"/health",port:8080},initialDelaySeconds:5,periodSeconds:10}},schedulingProfile:{profileId:$p,revision:$r,specDigest:$s,assignmentsDigest:$d}},route:{dnsMode:"sslip",pathPrefix:"/",tlsMode:"httpOnly"}}')"
  kp_human_post config-edge-first-deployment /v1/deployments "${kp_body}" 202 "${kp_dir}/deployment-operation.json"
  kp_operation="$(jq -er '.id' "${kp_dir}/deployment-operation.json")"
  kp_deployment="$(jq -er '.targetId' "${kp_dir}/deployment-operation.json")"
  kp_poll_operation "${kp_operation}" "${kp_dir}/deployment-terminal.json"

  curl -sS -o "${kp_dir}/deployment.json" --header "$(<"${KUBERPLOY_E2E_API_AUTH_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment}"
  jq -e --arg host "${kp_hostname}" --arg profile "${kp_profile}" \
    '.route.hostname==$host and .route.dnsMode=="sslip" and .runtime.schedulingProfile.profileId==$profile and .runtime.resources.requests.cpu=="50m" and .runtime.probes.readiness.httpGet.path=="/ready"' \
    "${kp_dir}/deployment.json" >/dev/null

  # Workload callers cannot substitute either another application's selector
  # or provider-owned placement labels alongside an immutable profile ref.
  kp_config_edge_rejection cross-application-affinity "$(jq --arg other "${kp_other_application}" \
    '.runtime.affinity={podAntiAffinity:{requiredDuringSchedulingIgnoredDuringExecution:[{topologyKey:"kubernetes.io/hostname",labelSelector:{matchLabels:{"kuberploy.io/application":$other}}}]}}' <<<"${kp_body}")" "${kp_dir}"
  kp_config_edge_rejection provider-label-injection "$(jq '.runtime.nodeSelector={"karpenter.sh/capacity-type":"spot"}' <<<"${kp_body}")" "${kp_dir}"

  # One candidate is expressed through the guided JSON-patch model and through
  # the YAML document model. The server must resolve both to the same effective
  # variables; only the YAML candidate is granted preview/save authority.
  kp_actual="$(curl -sS -D "${kp_dir}/config-headers.txt" -o "${kp_dir}/config-before.json" -w '%{http_code}' \
    --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment}/config?atLeastRevision=${kp_revision_fence}&waitSeconds=10")"
  [[ "${kp_actual}" == 200 ]]
  kp_config_etag="$(jq -er '.etag' "${kp_dir}/config-before.json")"
  kp_raw="$(jq -er '.documents[]|select(.documentId=="app.yaml")|.rawYaml' "${kp_dir}/config-before.json")"
  [[ "$(grep -Ec '^    replicas: 1$' <<<"${kp_raw}")" == 1 ]] || kp_die "AppConfig YAML does not contain one exact guided replicas scalar"
  kp_candidate="$(sed 's/^    replicas: 1$/    replicas: 2/' <<<"${kp_raw}")"
  kp_body='{"mode":"jsonPatch","patch":[{"op":"replace","path":"/spec/runtime/replicas","value":2}]}'
  kp_actual="$(kp_config_edge_request POST "/v1/deployments/${kp_deployment}/config/validate" "${kp_body}" "${kp_dir}/guided-validate.json")"
  [[ "${kp_actual}" == 200 ]]
  jq -e '.valid==true and (.diagnostics|length)==0' "${kp_dir}/guided-validate.json" >/dev/null
  jq -e '(.effectiveVariables|sort_by(.name))==([{"name":"FEATURE_PROBES","value":"enabled","source":"environment"},{"name":"RELEASE_LANE","value":"environment","source":"environment","overrides":[{"scope":"project","value":"project"}]},{"name":"SHARED_REGION","value":"ap-southeast-1","source":"project"}]|sort_by(.name))' "${kp_dir}/guided-validate.json" >/dev/null
  kp_body="$(jq -cn --arg raw "${kp_candidate}" '{mode:"yaml",documents:[{documentId:"app.yaml",rawYaml:$raw}]}')"
  kp_actual="$(kp_config_edge_request POST "/v1/deployments/${kp_deployment}/config/preview" "${kp_body}" "${kp_dir}/yaml-preview.json" --header "If-Match: ${kp_config_etag}")"
  [[ "${kp_actual}" == 200 ]]
  jq -e '.previewToken|type=="string" and length==43' "${kp_dir}/yaml-preview.json" >/dev/null
  jq -e '.gitDiff|contains("replicas")' "${kp_dir}/yaml-preview.json" >/dev/null
  jq -e '.renderedDiff|type=="string" and length>0' "${kp_dir}/yaml-preview.json" >/dev/null
  jq -e '.renderIdentityDigest|test("^sha256:[0-9a-f]{64}$")' "${kp_dir}/yaml-preview.json" >/dev/null
  jq -e 'any(.semanticChanges[]; .pointer=="/spec/runtime/replicas" and .before==1 and .after==2)' "${kp_dir}/yaml-preview.json" >/dev/null
  jq -e '(.effectiveVariables|sort_by(.name))==([{"name":"FEATURE_PROBES","value":"enabled","source":"environment"},{"name":"RELEASE_LANE","value":"environment","source":"environment","overrides":[{"scope":"project","value":"project"}]},{"name":"SHARED_REGION","value":"ap-southeast-1","source":"project"}]|sort_by(.name))' "${kp_dir}/yaml-preview.json" >/dev/null
  jq -e '(.effectiveVariables|sort_by(.name))==([{"name":"FEATURE_PROBES","value":"enabled","source":"environment"},{"name":"RELEASE_LANE","value":"environment","source":"environment","overrides":[{"scope":"project","value":"project"}]},{"name":"SHARED_REGION","value":"ap-southeast-1","source":"project"}]|sort_by(.name))' "${kp_dir}/config-before.json" >/dev/null
  jq -e '(.variableDependencies|length)==2 and all(.variableDependencies[]; .present==true and (.blobId|type=="string" and length>0)) and
    ([.documents[]|select(.documentKind=="VariableSet")]|length)==2' "${kp_dir}/config-before.json" >/dev/null
  kp_preview="$(jq -er '.previewToken' "${kp_dir}/yaml-preview.json")"
  kp_actual="$(kp_config_edge_put "/v1/deployments/${kp_deployment}/config" "${kp_body}" "${kp_preview}" \
    "qualification-${KUBERPLOY_E2E_RUN_ID}-config-edge-save" "${kp_dir}/config-save-operation.json")"
  [[ "${kp_actual}" == 202 ]]
  kp_operation="$(jq -er '.id' "${kp_dir}/config-save-operation.json")"
  kp_poll_operation "${kp_operation}" "${kp_dir}/config-save-terminal.json"
  local kp_config_revision
  kp_config_revision="$(jq -er '.gitRevision.commit|select(test("^[0-9a-f]{40}$"))' "${kp_dir}/config-save-terminal.json")"
  kp_config_edge_wait_argo "${kp_deployment}" "${kp_application}" "${kp_project}" "${kp_environment}" "${kp_config_revision}" \
    "${kp_dir}/config-save-argo.json"

  for _ in {1..120}; do
    "${KUBERPLOY_E2E_KUBECTL}" get deployment --namespace "${kp_namespace}" --selector "kuberploy.io/application=${kp_application}" -o json >"${kp_dir}/live-podspec.json"
    if jq -e '.items|length==1 and .[0].spec.replicas==2 and .[0].status.observedGeneration==.[0].metadata.generation' \
      "${kp_dir}/live-podspec.json" >/dev/null; then break; fi
    sleep 5
  done
  jq -e --arg app "${kp_application}" --arg profile "${kp_profile}" \
    '.items|length==1 and .[0].spec.replicas==2 and .[0].spec.template.metadata.annotations["kuberploy.io/scheduling-profile"]==($profile+"@1") and
     .[0].spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0].labelSelector.matchLabels["kuberploy.io/application"]==$app and
     .[0].spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key=="kubernetes.io/os" and
     .[0].spec.template.spec.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].weight==70 and
     (.[0].spec.template.spec.topologySpreadConstraints|length)==1 and (.[0].spec.template.spec.tolerations|length)==1 and
     .[0].spec.template.spec.terminationGracePeriodSeconds==30 and
     .[0].spec.template.spec.automountServiceAccountToken==false and
     .[0].spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation==false and
     .[0].spec.template.spec.containers[0].resources.requests.cpu=="50m" and
     .[0].spec.template.spec.containers[0].readinessProbe.httpGet.path=="/ready" and
     .[0].spec.template.spec.containers[0].livenessProbe.httpGet.path=="/health"' "${kp_dir}/live-podspec.json" >/dev/null

  for _ in {1..120}; do
    "${KUBERPLOY_E2E_KUBECTL}" get configmaps --namespace "${kp_namespace}" --selector "kuberploy.io/application=${kp_application}" -o json >"${kp_dir}/live-configmaps.json"
    if jq -e '[.items[]|select(.immutable==true and .data.SHARED_REGION=="ap-southeast-1" and .data.RELEASE_LANE=="environment" and .data.FEATURE_PROBES=="enabled")]|length==1' \
      "${kp_dir}/live-configmaps.json" >/dev/null; then break; fi
    sleep 5
  done
  kp_configmap="$(jq -er '.items|map(select(.immutable==true and .data.SHARED_REGION=="ap-southeast-1" and .data.RELEASE_LANE=="environment" and .data.FEATURE_PROBES=="enabled"))|select(length==1)|.[0].metadata.name' "${kp_dir}/live-configmaps.json")"
  jq -e --arg configmap "${kp_configmap}" '[.items[0].spec.template.spec.containers[0].env[]|select(.name=="SHARED_REGION" or .name=="RELEASE_LANE" or .name=="FEATURE_PROBES")|.valueFrom.configMapKeyRef.name]|length==3 and all(.[]; .==$configmap)' "${kp_dir}/live-podspec.json" >/dev/null

  kp_config_edge_external_dns "${kp_dir}" "${kp_state}" "${kp_project}" "${kp_environment}" "${kp_ip}"
  jq -n --arg applicationId "${kp_application}" --arg deploymentId "${kp_deployment}" --arg hostname "${kp_hostname}" \
    --arg ingressIPv4 "${kp_ip}" --arg profileId "${kp_profile}" --arg configMap "${kp_configmap}" \
    '{mutation:"variables-appconfig-scheduling-sslip-external-dns",applicationId:$applicationId,deploymentId:$deploymentId,hostname:$hostname,selectedCanonicalPublicIPv4:$ingressIPv4,profileId:$profileId,configMap:$configMap,projectVariableSetSaved:true,environmentVariableSetSaved:true,overrideProvenanceVerified:true,immutableConfigMapRefsVerified:true,guidedYAMLSharedDraft:true,renderedManifestDiffVerified:true,defaultsProbesResourcesVerified:true,exactLivePodSpecVerified:true,crossApplicationDenied:true,providerLabelInjectionDenied:true} + $external[0]' --slurpfile external "${kp_dir}/external-dns-proof.json" >"${kp_dir}/config-edge-proof.json"
}

kp_cleanup_config_edge_workflow() {
  local kp_proof="${KUBERPLOY_E2E_STAGE_DIR}/evidence/config-edge-proof.json"
  local kp_id kp_deployment kp_application kp_project kp_environment kp_actual kp_out="${KUBERPLOY_E2E_STAGE_DIR}/evidence/external-dns-deactivate.json"
  local kp_body kp_etag kp_preview kp_operation kp_revision
  [[ -s "${kp_proof}" && ! -L "${kp_proof}" ]] || return 0
  kp_id="$(jq -er '.integrationId' "${kp_proof}")"
  kp_deployment="$(jq -er '.externalDNSDeploymentId' "${kp_proof}")"
  kp_application="$(jq -er '.externalDNSApplicationId' "${kp_proof}")"
  kp_project="$(jq -er '.projectId' "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json")"
  kp_environment="$(jq -er '.directEnvironmentId' "${KUBERPLOY_E2E_ARTIFACT_DIR}/workflow-state.json")"
  curl -sS -o "${KUBERPLOY_E2E_STAGE_DIR}/evidence/external-dns-cleanup-before.json" --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
    "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/deployments/${kp_deployment}/config"
  if jq -e --arg ref "qualification-${KUBERPLOY_E2E_RUN_ID}" 'tostring|contains("integrationRef") and contains($ref)' \
      "${KUBERPLOY_E2E_STAGE_DIR}/evidence/external-dns-cleanup-before.json" >/dev/null; then
    kp_etag="$(jq -er '.etag' "${KUBERPLOY_E2E_STAGE_DIR}/evidence/external-dns-cleanup-before.json")"
    kp_body='{"mode":"jsonPatch","patch":[{"op":"remove","path":"/spec/routes"}]}'
    kp_actual="$(kp_config_edge_request POST "/v1/deployments/${kp_deployment}/config/preview" "${kp_body}" \
      "${KUBERPLOY_E2E_STAGE_DIR}/evidence/external-dns-cleanup-preview.json" --header "If-Match: ${kp_etag}")"
    [[ "${kp_actual}" == 200 ]]
    kp_preview="$(jq -er '.previewToken' "${KUBERPLOY_E2E_STAGE_DIR}/evidence/external-dns-cleanup-preview.json")"
    kp_actual="$(kp_config_edge_put "/v1/deployments/${kp_deployment}/config" "${kp_body}" "${kp_preview}" \
      "qualification-${KUBERPLOY_E2E_RUN_ID}-external-dns-route-remove" "${KUBERPLOY_E2E_STAGE_DIR}/evidence/external-dns-cleanup-operation.json")"
    [[ "${kp_actual}" == 202 ]]
    kp_operation="$(jq -er '.id' "${KUBERPLOY_E2E_STAGE_DIR}/evidence/external-dns-cleanup-operation.json")"
    kp_poll_operation "${kp_operation}" "${KUBERPLOY_E2E_STAGE_DIR}/evidence/external-dns-cleanup-terminal.json"
    kp_revision="$(jq -er '.gitRevision.commit' "${KUBERPLOY_E2E_STAGE_DIR}/evidence/external-dns-cleanup-terminal.json")"
    kp_config_edge_wait_argo "${kp_deployment}" "${kp_application}" "${kp_project}" "${kp_environment}" "${kp_revision}" \
      "${KUBERPLOY_E2E_STAGE_DIR}/evidence/external-dns-cleanup-argo.json"
  fi
  kp_actual="$(kp_config_edge_request DELETE "/v1/external-dns/integrations/${kp_id}" "" "${kp_out}" \
    --header "Idempotency-Key: qualification-${KUBERPLOY_E2E_RUN_ID}-external-dns-deactivate")"
  [[ "${kp_actual}" == 200 ]]
  jq -e --arg id "${kp_id}" '.id==$id and .lifecycle=="deactivated" and .protectedGitState=="pending"' "${kp_out}" >/dev/null
  for _ in {1..60}; do
    curl -sS -o "${KUBERPLOY_E2E_STAGE_DIR}/evidence/external-dns-dematerialized.json" --header "$(<"${KUBERPLOY_E2E_HUMAN_COOKIE_HEADER_FILE}")" \
      "$(jq -r '.apiBaseURL' "${kp_scenario}")/v1/external-dns/integrations"
    jq -e --arg id "${kp_id}" 'any(.items[]; .id==$id and .lifecycle=="deactivated" and .protectedGitState=="dematerialized")' \
      "${KUBERPLOY_E2E_STAGE_DIR}/evidence/external-dns-dematerialized.json" >/dev/null && return 0
    sleep 2
  done
  return 1
}
