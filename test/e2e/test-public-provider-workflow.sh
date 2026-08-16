#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-public-provider-test.XXXXXX")"
trap 'find "${kp_tmp}" -type f -delete; rmdir -- "${kp_tmp}"' EXIT

kp_credential="${kp_tmp}/credential"
kp_inventory="${kp_tmp}/inventory.ndjson"
kp_evidence="${kp_tmp}/provider.json"
kp_cleanup="${kp_tmp}/cleanup.json"
kp_state="${kp_tmp}/cloudflare-record.json"
printf 'token-do-not-leak\n' >"${kp_credential}"
chmod 600 "${kp_credential}"

cat >"${kp_tmp}/curl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
method=GET output= url= body=
while (($#)); do
  case "$1" in
    --config|--retry|--request|--output|--write-out|--data-binary)
      case "$1" in
        --request) method="$2" ;;
        --output) output="$2" ;;
        --data-binary) body="${2#@}" ;;
      esac
      shift 2 ;;
    --silent|--show-error) shift ;;
    https://*) url="$1"; shift ;;
    *) shift ;;
  esac
done
response='{"success":true,"result":[]}' status=200
if [[ "${url}" == *"/zones?name=${KUBERPLOY_E2E_PUBLIC_PROVIDER_ZONE}"* ]]; then
  response="$(jq -cn --arg zone "${KUBERPLOY_E2E_PUBLIC_PROVIDER_ZONE}" '{success:true,result:[{id:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",name:$zone,status:"active"}]}')"
elif [[ "${method}" == GET && "${url}" == */dns_records\?* ]]; then
  [[ ! -f "${KP_PUBLIC_PROVIDER_FAKE_STATE}" ]] || response="$(jq -cn --slurpfile r "${KP_PUBLIC_PROVIDER_FAKE_STATE}" '{success:true,result:$r}')"
elif [[ "${method}" == POST && "${url}" == */dns_records ]]; then
  jq -n --slurpfile body "${body}" '{id:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",type:$body[0].type,name:$body[0].name,content:$body[0].content,proxied:$body[0].proxied,comment:$body[0].comment}' >"${KP_PUBLIC_PROVIDER_FAKE_STATE}"
  response="$(jq -cn --slurpfile r "${KP_PUBLIC_PROVIDER_FAKE_STATE}" '{success:true,result:$r[0]}')"
elif [[ "${method}" == GET && "${url}" == */dns_records/* ]]; then
  if [[ -f "${KP_PUBLIC_PROVIDER_FAKE_STATE}" ]]; then
    response="$(jq -cn --slurpfile r "${KP_PUBLIC_PROVIDER_FAKE_STATE}" '{success:true,result:$r[0]}')"
  else
    status=404
    response='{"success":false,"result":null}'
  fi
elif [[ "${method}" == DELETE && "${url}" == */dns_records/* ]]; then
  rm -f -- "${KP_PUBLIC_PROVIDER_FAKE_STATE}"
  response='{"success":true,"result":null}'
fi
if [[ -n "${output}" ]]; then printf '%s\n' "${response}" >"${output}"; fi
printf '%s' "${status}"
EOF
cat >"${kp_tmp}/dig" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ " $* " == *' NS '* ]]; then
  printf 'ns.fixture.\n'
elif [[ ! -f "${KP_PUBLIC_PROVIDER_FAKE_STATE}" ]]; then
  exit 0
else
  jq -r '.content' "${KP_PUBLIC_PROVIDER_FAKE_STATE}"
fi
EOF
chmod 755 "${kp_tmp}/curl" "${kp_tmp}/dig"
export PATH="${kp_tmp}:${PATH}"
export KP_PUBLIC_PROVIDER_FAKE_STATE="${kp_state}"
export KUBERPLOY_E2E_RUN_ID="pv1"
export KUBERPLOY_E2E_PUBLIC_PROVIDER_ZONE="example.test"
export KUBERPLOY_E2E_PUBLIC_HOSTNAME="kuberploy-pv1.example.test"
export KUBERPLOY_E2E_PUBLIC_EFFECTS_ACK="public-provider:pv1:kuberploy-pv1.example.test"
export KUBERPLOY_E2E_PUBLIC_DNS_TARGET="192.0.2.10"
export KUBERPLOY_E2E_PUBLIC_DNS_RECORD_TYPE="A"
export KUBERPLOY_E2E_DNS_PROVIDER_CREDENTIAL_FILE="${kp_credential}"
export KUBERPLOY_E2E_CLOUDFLARE_API_BASE_URL="https://api.cloudflare.test/client/v4"
export KUBERPLOY_E2E_PUBLIC_PROVIDER_INVENTORY_FILE="${kp_inventory}"
export KUBERPLOY_E2E_PUBLIC_PROVIDER_EVIDENCE_FILE="${kp_evidence}"
export KUBERPLOY_E2E_PUBLIC_PROVIDER_CLEANUP_RESULT_FILE="${kp_cleanup}"

source "${kp_root}/scripts/kubernetes/test/e2e/lib.sh"
source "${kp_root}/scripts/kubernetes/test/e2e/public-provider-workflow.sh"
: >"${kp_inventory}"

jq -n --arg host "${KUBERPLOY_E2E_PUBLIC_HOSTNAME}" '{id:"cccccccccccccccccccccccccccccccc",type:"A",name:$host,content:"198.51.100.10",proxied:false,comment:"unrelated"}' >"${kp_state}"
if (kp_public_provider_run) >/dev/null 2>&1; then
  printf 'pre-existing-record-adoption was not rejected\n' >&2
  exit 1
fi
jq -e '.content == "198.51.100.10" and .comment == "unrelated"' "${kp_state}" >/dev/null
rm -- "${kp_state}"

kp_public_provider_run
jq -e '.exactProviderRecord == true and .publicDNSObserved == true and .target == "192.0.2.10"' \
  "${kp_evidence}" >/dev/null
grep -Fqx 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' <(jq -r '.uid' "${kp_inventory}")
! grep -F 'token-do-not-leak' "${kp_evidence}" "${kp_inventory}"

jq '.content = "198.51.100.11"' "${kp_state}" >"${kp_state}.tmp"
mv -- "${kp_state}.tmp" "${kp_state}"
if (kp_public_provider_cleanup) >/dev/null 2>&1; then
  printf 'changed-record-identity was not rejected\n' >&2
  exit 1
fi
[[ -e "${kp_state}" ]]
jq '.content = "192.0.2.10"' "${kp_state}" >"${kp_state}.tmp"
mv -- "${kp_state}.tmp" "${kp_state}"

kp_public_provider_cleanup
jq -e '.status == "cleaned" and .verifiedAbsentOrRestored == true and .cleanedOrRestoredCount == 1' \
  "${kp_cleanup}" >/dev/null
[[ ! -e "${kp_state}" ]]

printf 'public-provider-workflow: ok\n'
