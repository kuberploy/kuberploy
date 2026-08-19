#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-outbox-relay-test.XXXXXX")"
kp_cleanup() {
  find "${kp_tmp}" -type f -delete
  rmdir "${kp_tmp}"
}
trap kp_cleanup EXIT

source "${kp_root}/scripts/kubernetes/test/e2e/outbox-relay-job.sh"

kp_worker_file="${kp_tmp}/worker.json"
kp_job_file="${kp_tmp}/relay-job.json"
jq -n '
  {metadata:{labels:{"app.kubernetes.io/instance":"kuberploy"}},
   spec:{template:{spec:{serviceAccountName:"kuberploy-worker",securityContext:{runAsNonRoot:true},containers:[{
     name:"worker",image:("ghcr.io/kuberploy/kuberploy-worker@sha256:" + ("a"*64)),
     imagePullPolicy:"IfNotPresent",resources:{requests:{cpu:"50m",memory:"64Mi"},limits:{cpu:"1",memory:"512Mi"}},
     securityContext:{allowPrivilegeEscalation:false,readOnlyRootFilesystem:true,capabilities:{drop:["ALL"]}},
     env:[
       {name:"KUBERPLOY_DATABASE_URL",valueFrom:{secretKeyRef:{name:"database",key:"url"}}},
       {name:"KUBERPLOY_VALKEY_ADDRESSES",valueFrom:{secretKeyRef:{name:"valkey",key:"addresses"}}},
       {name:"KUBERPLOY_VALKEY_PUBLISHER_USERNAME",valueFrom:{secretKeyRef:{name:"valkey",key:"publisher-username"}}},
       {name:"KUBERPLOY_VALKEY_PUBLISHER_PASSWORD",valueFrom:{secretKeyRef:{name:"valkey",key:"publisher-password"}}},
       {name:"UNRELATED_RUNTIME_SETTING",value:"must-not-copy"}
     ]}]}}}}' >"${kp_worker_file}"

kp_render_outbox_relay_job "${kp_worker_file}" relay-probe kuberploy-system relay-test kuberploy "${kp_job_file}"
jq -e '
  .kind == "Job" and
  .spec.template.spec.containers[0].command == ["/kuberploy-worker","outbox-relay-once"] and
  .spec.template.spec.containers[0].resources.requests == {cpu:"50m",memory:"64Mi"} and
  .spec.template.spec.containers[0].resources.limits == {cpu:"1",memory:"512Mi"} and
  ([.spec.template.spec.containers[0].env[].name] | sort) ==
    ["KUBERPLOY_DATABASE_URL","KUBERPLOY_VALKEY_ADDRESSES","KUBERPLOY_VALKEY_PUBLISHER_PASSWORD","KUBERPLOY_VALKEY_PUBLISHER_USERNAME"] and
  .spec.template.spec.automountServiceAccountToken == false and
  .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true
' "${kp_job_file}" >/dev/null
printf '%s\n' 'outbox relay Job render passed: digest, bounded env, quota resources, and least-privilege context'
