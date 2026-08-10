#!/usr/bin/env bash

set -Eeuo pipefail

# The qualification harness deliberately reuses the generic target selectors.
# Sourcing this file never performs cluster I/O.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)/lib.sh"

readonly KP_QUALIFICATION_SCHEMA_VERSION="1"
readonly KP_QUALIFICATION_ARTIFACT_PREFIX="kuberploy-qualification-"

kp_qualification_stage_catalog() {
  cat <<'EOF'
10-one-chart-install|true|installer-single-entrypoint,independent-applications-created,immutable-source-revision,package-digests-attested,bootstrap-job-auth,recurring-login,invitation-login,sole-owner-denial,github-installation-sharing,contract-identities
20-postgresql-valkey|true|postgresql-durable,reset-recovery,multi-scope-grants,service-account-token-lifecycle
25-config-edge|true|variables-inherited-configmap,guided-yaml-rendered-diff,scheduling-profile-podspec,sslip-canonical-public-ip,external-dns-rfc2136
30-git-argo|true|git-direct-projection,git-protected-pr,rollback-new-intent
40-source-build|true|github-webhook-delivery,webhook-safety-poll,build-job-isolation,build-cancel-job-deleted,second-build-cache-hit,cache-cold-degrade,push-failure-terminal,auto-deploy-receipt,source-build-digest-promotion,approved-helm-oci
50-runtime-edge|true|middleware,http-route
60-local-tls|true|custom-certificate,local-acme-certificate,local-acme-renewal,no-public-acme
70-registry-retention|true|existing-image-tag-resolution,retention-removes-only-eligible
80-observability|true|logs-authorized,events-authorized,prometheus-query,tenant-filtered,browser-ui-workflow
90-security|true|namespace-rbac,admission-deny,resource-quota,network-isolation,secret-nondisclosure,cross-tenant-deny,audit-timeline
100-upgrade-rollback|true|ordered-upgrade,health-gate,rollback-intent,rollback-result
EOF
}

kp_qualification_public_stage_catalog() {
  # Public provider mutation and cleanup are not yet implemented by the
  # repository driver. Do not advertise assertions that only probe unrelated
  # pre-existing DNS or TLS state.
  return 0
}

kp_qualification_validate_safe_file() {
  local kp_name="${1:?input name required}"
  local kp_path="${2:-}"
  local kp_secret="${3:-false}"
  local kp_mode

  [[ -n "${kp_path}" ]] || kp_die "${kp_name} is required"
  [[ "${kp_path}" == /* ]] || kp_die "${kp_name} must be one absolute path"
  [[ -f "${kp_path}" && ! -L "${kp_path}" ]] || \
    kp_die "${kp_name} must identify one regular, non-symlink file"
  if [[ "${kp_secret}" == "true" ]]; then
    kp_mode="$(kp_file_mode "${kp_path}")"
    [[ "${kp_mode}" =~ ^[0-7]{3,4}$ ]] || \
      kp_die "could not validate ${kp_name} permissions"
    (( (8#${kp_mode} & 8#077) == 0 )) || \
      kp_die "${kp_name} must not be group/world accessible; use mode 0600"
  fi
}

kp_qualification_file_identity() {
  local kp_path="${1:?path required}"
  if stat -f '%d:%i' "${kp_path}" >/dev/null 2>&1; then
    stat -f '%d:%i' "${kp_path}"
  else
    stat -c '%d:%i' "${kp_path}"
  fi
}

kp_qualification_validate_hostname() {
  local kp_name="${1:?input name required}"
  local kp_value="${2:-}"
  local kp_label
  local -a kp_labels
  [[ ${#kp_value} -le 253 && "${kp_value}" == *.* ]] || \
    kp_die "${kp_name} must be one lowercase DNS hostname"
  IFS='.' read -r -a kp_labels <<<"${kp_value}"
  (( ${#kp_labels[@]} >= 2 )) || \
    kp_die "${kp_name} must be one lowercase DNS hostname"
  for kp_label in "${kp_labels[@]}"; do
    [[ "${kp_label}" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]] || \
      kp_die "${kp_name} contains an invalid DNS label"
  done
}

kp_qualification_validate_common_inputs() {
  : "${KUBERPLOY_E2E_ARTIFACT_DIR:?KUBERPLOY_E2E_ARTIFACT_DIR is required}"
  : "${KUBERPLOY_E2E_MUTATION_ACK:?KUBERPLOY_E2E_MUTATION_ACK is required}"
  : "${KUBERPLOY_E2E_LOCAL_ACME_DIRECTORY_URL:?KUBERPLOY_E2E_LOCAL_ACME_DIRECTORY_URL is required}"
  [[ -n "${KUBERPLOY_E2E_DISPOSABLE_CLUSTER_ACK:-}" ]] || \
    kp_die "KUBERPLOY_E2E_DISPOSABLE_CLUSTER_ACK is required"

  [[ "${KUBERPLOY_E2E_MUTATION_ACK}" == \
     "qualify:${KUBERPLOY_E2E_RUN_ID}:${KUBERPLOY_TEST_CONTEXT}" ]] || \
    kp_die "KUBERPLOY_E2E_MUTATION_ACK must equal qualify:<run-id>:<exact-context>"
  [[ "${KUBERPLOY_E2E_DISPOSABLE_CLUSTER_ACK}" == \
     "destroy-after-qualification:${KUBERPLOY_E2E_RUN_ID}:${KUBERPLOY_TEST_CONTEXT}" ]] || \
    kp_die "KUBERPLOY_E2E_DISPOSABLE_CLUSTER_ACK must bind cluster destruction to this run and context"
  [[ "${KUBERPLOY_E2E_LOCAL_ACME_DIRECTORY_URL}" =~ ^https://[^[:space:]@]+$ && \
     "${KUBERPLOY_E2E_LOCAL_ACME_DIRECTORY_URL}" != *'?'* && \
     "${KUBERPLOY_E2E_LOCAL_ACME_DIRECTORY_URL}" != *'#'* ]] || \
    kp_die "KUBERPLOY_E2E_LOCAL_ACME_DIRECTORY_URL must be an explicit HTTPS URL"

  kp_qualification_validate_safe_file KUBERPLOY_E2E_INSTALLER_VALUES_FILE \
    "${KUBERPLOY_E2E_INSTALLER_VALUES_FILE:-}" false
  kp_qualification_validate_safe_file KUBERPLOY_E2E_UPGRADE_FROM_VALUES_FILE \
    "${KUBERPLOY_E2E_UPGRADE_FROM_VALUES_FILE:-}" false
  kp_qualification_validate_safe_file KUBERPLOY_E2E_CUSTOM_CERTIFICATE_PEM_FILE \
    "${KUBERPLOY_E2E_CUSTOM_CERTIFICATE_PEM_FILE:-}" true
  kp_qualification_validate_safe_file KUBERPLOY_E2E_CUSTOM_PRIVATE_KEY_PEM_FILE \
    "${KUBERPLOY_E2E_CUSTOM_PRIVATE_KEY_PEM_FILE:-}" true
  kp_qualification_validate_safe_file KUBERPLOY_E2E_RUNTIME_SECRET_INITIAL_VALUE_FILE \
    "${KUBERPLOY_E2E_RUNTIME_SECRET_INITIAL_VALUE_FILE:-}" true
  kp_qualification_validate_safe_file KUBERPLOY_E2E_RUNTIME_SECRET_ROTATED_VALUE_FILE \
    "${KUBERPLOY_E2E_RUNTIME_SECRET_ROTATED_VALUE_FILE:-}" true
  kp_qualification_validate_safe_file KUBERPLOY_E2E_GITHUB_WEBHOOK_SECRET_FILE \
    "${KUBERPLOY_E2E_GITHUB_WEBHOOK_SECRET_FILE:-}" true
  [[ "$(wc -l <"${KUBERPLOY_E2E_GITHUB_WEBHOOK_SECRET_FILE}" | tr -d ' ')" == "1" &&
     -s "${KUBERPLOY_E2E_GITHUB_WEBHOOK_SECRET_FILE}" ]] ||
    kp_die "GitHub webhook secret file must contain exactly one non-empty line"
  [[ "$(wc -l <"${KUBERPLOY_E2E_RUNTIME_SECRET_INITIAL_VALUE_FILE}" | tr -d ' ')" == "1" &&
     "$(wc -l <"${KUBERPLOY_E2E_RUNTIME_SECRET_ROTATED_VALUE_FILE}" | tr -d ' ')" == "1" &&
     -s "${KUBERPLOY_E2E_RUNTIME_SECRET_INITIAL_VALUE_FILE}" &&
     -s "${KUBERPLOY_E2E_RUNTIME_SECRET_ROTATED_VALUE_FILE}" ]] || \
    kp_die "runtime-secret value files must each contain exactly one non-empty line"
  ! cmp -s "${KUBERPLOY_E2E_RUNTIME_SECRET_INITIAL_VALUE_FILE}" \
    "${KUBERPLOY_E2E_RUNTIME_SECRET_ROTATED_VALUE_FILE}" || \
    kp_die "runtime-secret initial and rotated values must be distinct"
  local kp_registry_file kp_registry_identity_count
  for kp_registry_file in \
    KUBERPLOY_E2E_REGISTRY_PUSH_USERNAME_FILE KUBERPLOY_E2E_REGISTRY_PUSH_PASSWORD_FILE \
    KUBERPLOY_E2E_REGISTRY_CACHE_USERNAME_FILE KUBERPLOY_E2E_REGISTRY_CACHE_PASSWORD_FILE \
    KUBERPLOY_E2E_REGISTRY_FAULT_PASSWORD_FILE; do
    kp_qualification_validate_safe_file "${kp_registry_file}" "${!kp_registry_file:-}" true
    [[ "$(wc -l <"${!kp_registry_file}" | tr -d ' ')" == "1" && -s "${!kp_registry_file}" ]] ||
      kp_die "${kp_registry_file} must contain exactly one non-empty line"
    LC_ALL=C grep -Eq '^[^[:cntrl:]]+$' "${!kp_registry_file}" ||
      kp_die "${kp_registry_file} contains a forbidden control character"
    awk 'length($0) >= 8 && length($0) <= 1024 {ok=1} END {exit !ok}' "${!kp_registry_file}" ||
      kp_die "${kp_registry_file} must contain 8 through 1024 characters"
  done
  kp_registry_identity_count="$(for kp_registry_file in \
    "${KUBERPLOY_E2E_REGISTRY_PUSH_USERNAME_FILE}" "${KUBERPLOY_E2E_REGISTRY_PUSH_PASSWORD_FILE}" \
    "${KUBERPLOY_E2E_REGISTRY_CACHE_USERNAME_FILE}" "${KUBERPLOY_E2E_REGISTRY_CACHE_PASSWORD_FILE}" \
    "${KUBERPLOY_E2E_REGISTRY_FAULT_PASSWORD_FILE}"; do
      kp_qualification_file_identity "${kp_registry_file}"
    done | sort -u | wc -l | tr -d ' ')"
  [[ "${kp_registry_identity_count}" == "5" ]] ||
    kp_die "registry credential inputs must be five distinct regular files"
  ! cmp -s "${KUBERPLOY_E2E_REGISTRY_FAULT_PASSWORD_FILE}" "${KUBERPLOY_E2E_REGISTRY_PUSH_PASSWORD_FILE}" &&
    ! cmp -s "${KUBERPLOY_E2E_REGISTRY_FAULT_PASSWORD_FILE}" "${KUBERPLOY_E2E_REGISTRY_CACHE_PASSWORD_FILE}" ||
    kp_die "registry fault password must differ from both valid lane passwords"
  kp_qualification_validate_safe_file KUBERPLOY_E2E_RFC2136_TSIG_SECRET_FILE \
    "${KUBERPLOY_E2E_RFC2136_TSIG_SECRET_FILE:-}" true
  local kp_rfc2136_secret kp_rfc2136_bytes kp_rfc2136_canonical
  kp_rfc2136_secret="$(<"${KUBERPLOY_E2E_RFC2136_TSIG_SECRET_FILE}")"
  [[ "$(wc -l <"${KUBERPLOY_E2E_RFC2136_TSIG_SECRET_FILE}" | tr -d ' ')" == 1 && \
     "${kp_rfc2136_secret}" =~ ^[A-Za-z0-9+/]+={0,2}$ ]] || \
    kp_die "RFC2136 TSIG secret file must contain exactly one base64 line"
  kp_rfc2136_bytes="$(printf '%s' "${kp_rfc2136_secret}" | openssl base64 -d -A | wc -c | tr -d ' ')"
  [[ "${kp_rfc2136_bytes}" =~ ^[0-9]+$ ]] && ((kp_rfc2136_bytes >= 16 && kp_rfc2136_bytes <= 64)) || \
    kp_die "RFC2136 TSIG secret must decode to 16 through 64 bytes"
  kp_rfc2136_canonical="$(printf '%s' "${kp_rfc2136_secret}" | openssl base64 -d -A | openssl base64 -A)"
  [[ "${kp_rfc2136_canonical}" == "${kp_rfc2136_secret}" ]] || \
    kp_die "RFC2136 TSIG secret must use canonical padded base64"
  [[ "${KUBERPLOY_E2E_RFC2136_PROVIDER_IMAGE:-}" =~ ^[A-Za-z0-9][A-Za-z0-9._:/-]*@sha256:[a-f0-9]{64}$ ]] || \
    kp_die "KUBERPLOY_E2E_RFC2136_PROVIDER_IMAGE must be digest pinned"
  [[ "${KUBERPLOY_E2E_EXTERNAL_DNS_NAMESPACE:-}" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]] || \
    kp_die "KUBERPLOY_E2E_EXTERNAL_DNS_NAMESPACE must be one DNS label"
  [[ "${KUBERPLOY_E2E_EXTERNAL_DNS_NAMESPACE_AUTHORITY:-}" == \
     "pre-existing:${KUBERPLOY_E2E_RUN_ID}:${KUBERPLOY_E2E_EXTERNAL_DNS_NAMESPACE}" ]] || \
    kp_die "external DNS namespace authority must bind this run and namespace"
  node - "${KUBERPLOY_E2E_KUBE_API_CIDR:-}" <<'JS' ||
const net = require("node:net");
const raw = process.argv[2] || "";
const slash = raw.lastIndexOf("/");
const address = raw.slice(0, slash);
const prefix = raw.slice(slash + 1);
const family = net.isIP(address);
let canonical = "";
if (family === 4 && prefix === "32") canonical = address.split(".").map(Number).join(".");
if (family === 6 && prefix === "128") canonical = new URL(`http://[${address}]/`).hostname.slice(1, -1).toLowerCase();
if (!canonical || canonical !== address.toLowerCase()) process.exit(1);
JS
    kp_die "KUBERPLOY_E2E_KUBE_API_CIDR must be one canonical IPv4 /32 or IPv6 /128"
  kp_qualification_validate_safe_file KUBERPLOY_E2E_SCENARIO_FILE \
    "${KUBERPLOY_E2E_SCENARIO_FILE:-}" false
  [[ "${KUBERPLOY_E2E_BROWSER_EXECUTABLE:-}" == /* && -x "${KUBERPLOY_E2E_BROWSER_EXECUTABLE}" ]] || \
    kp_die "KUBERPLOY_E2E_BROWSER_EXECUTABLE must be one absolute executable browser"
  kp_qualification_validate_safe_file KUBERPLOY_E2E_TEARDOWN_PUBLIC_KEY_FILE \
    "${KUBERPLOY_E2E_TEARDOWN_PUBLIC_KEY_FILE:-}" false
  kp_qualification_validate_hostname KUBERPLOY_E2E_HTTP_HOSTNAME \
    "${KUBERPLOY_E2E_HTTP_HOSTNAME:-}"
  kp_qualification_validate_hostname KUBERPLOY_E2E_CUSTOM_TLS_HOSTNAME \
    "${KUBERPLOY_E2E_CUSTOM_TLS_HOSTNAME:-}"
  kp_qualification_validate_hostname KUBERPLOY_E2E_LOCAL_ACME_HOSTNAME \
    "${KUBERPLOY_E2E_LOCAL_ACME_HOSTNAME:-}"

  case "${KUBERPLOY_E2E_PUBLIC_PROVIDER_TESTS:-false}" in
    false) ;;
    true)
      [[ -n "${KUBERPLOY_E2E_PUBLIC_HOSTNAME:-}" ]] || \
        kp_die "KUBERPLOY_E2E_PUBLIC_HOSTNAME is required when public provider tests are enabled"
      [[ -n "${KUBERPLOY_E2E_ACME_EMAIL:-}" ]] || \
        kp_die "KUBERPLOY_E2E_ACME_EMAIL is required when public provider tests are enabled"
      [[ -n "${KUBERPLOY_E2E_PUBLIC_EFFECTS_ACK:-}" ]] || \
        kp_die "KUBERPLOY_E2E_PUBLIC_EFFECTS_ACK is required when public provider tests are enabled"
      kp_qualification_validate_hostname KUBERPLOY_E2E_PUBLIC_HOSTNAME \
        "${KUBERPLOY_E2E_PUBLIC_HOSTNAME}"
      [[ "${KUBERPLOY_E2E_ACME_EMAIL}" =~ ^[^[:space:]@]+@[^[:space:]@]+$ ]] || \
        kp_die "KUBERPLOY_E2E_ACME_EMAIL must be one email address"
      [[ "${KUBERPLOY_E2E_PUBLIC_EFFECTS_ACK}" == \
         "public-provider:${KUBERPLOY_E2E_RUN_ID}:${KUBERPLOY_E2E_PUBLIC_HOSTNAME}" ]] || \
        kp_die "KUBERPLOY_E2E_PUBLIC_EFFECTS_ACK does not match this run and hostname"
      kp_qualification_validate_safe_file KUBERPLOY_E2E_DNS_PROVIDER_CREDENTIAL_FILE \
        "${KUBERPLOY_E2E_DNS_PROVIDER_CREDENTIAL_FILE:-}" true
      ;;
    *) kp_die "KUBERPLOY_E2E_PUBLIC_PROVIDER_TESTS must be true or false" ;;
  esac
}

kp_qualification_expected_probe() {
  case "${1:?assertion required}" in
    installer-single-entrypoint) printf '%s\n' helm-install ;;
    browser-ui-workflow) printf '%s\n' browser-proof ;;
    independent-applications-created|immutable-source-revision|package-digests-attested|bootstrap-job-auth|recurring-login|invitation-login|sole-owner-denial|github-installation-sharing|contract-identities) printf '%s\n' installer-proof ;;
    postgresql-durable|reset-recovery|multi-scope-grants|service-account-token-lifecycle|variables-inherited-configmap|guided-yaml-rendered-diff|scheduling-profile-podspec|sslip-canonical-public-ip|external-dns-rfc2136|git-direct-projection|git-protected-pr|argo-synced-healthy|rollback-new-intent|rollback-immutable-input|github-webhook-delivery|webhook-safety-poll|build-job-isolation|build-cancel-job-deleted|second-build-cache-hit|cache-cold-degrade|push-failure-terminal|auto-deploy-receipt|source-build-digest-promotion|approved-helm-oci|runtime-chart|traefik-route|middleware|local-acme-certificate|local-acme-renewal|no-public-acme|current-protected|rollback-set-protected|existing-image-tag-resolution|retention-removes-only-eligible|logs-authorized|events-authorized|prometheus-query|tenant-filtered|namespace-rbac|admission-deny|resource-quota|network-isolation|secret-nondisclosure|cross-tenant-deny|audit-timeline|ordered-upgrade|health-gate|rollback-intent|rollback-result) printf '%s\n' workflow-proof ;;
    http-route) printf '%s\n' http ;;
    custom-certificate|public-acme) printf '%s\n' tls ;;
    public-dns|public-hostname-resolves) printf '%s\n' dns ;;
    *) kp_die "no repository probe binding for qualification assertion $1" ;;
  esac
}

kp_qualification_validate_assertion_spec() {
  local kp_stage="${1:?stage required}" kp_assertion="${2:?assertion required}"
  local kp_probe="${3:?probe required}"
  case "${kp_probe}" in
    helm-install)
      jq -e --arg s "${kp_stage}" --arg a "${kp_assertion}" '
        .stages[$s].assertions[$a] == {probe:"helm-install"}
      ' "${KUBERPLOY_E2E_SCENARIO_FILE}" >/dev/null ;;
    installer-proof|workflow-proof|browser-proof)
      jq -e --arg s "${kp_stage}" --arg a "${kp_assertion}" --arg p "${kp_probe}" '
        .stages[$s].assertions[$a] == {probe:$p,contract:$a}
      ' "${KUBERPLOY_E2E_SCENARIO_FILE}" >/dev/null ;;
    http)
      jq -e --arg s "${kp_stage}" --arg a "${kp_assertion}" \
        --arg url "http://${KUBERPLOY_E2E_HTTP_HOSTNAME}/" '
        .stages[$s].assertions[$a] == {probe:"http",url:$url,expectedStatus:200}
      ' "${KUBERPLOY_E2E_SCENARIO_FILE}" >/dev/null ;;
    tls)
      local kp_hostname="${KUBERPLOY_E2E_CUSTOM_TLS_HOSTNAME}"
      [[ "${kp_assertion}" != "public-acme" ]] || \
        kp_hostname="${KUBERPLOY_E2E_PUBLIC_HOSTNAME:-}"
      jq -e --arg s "${kp_stage}" --arg a "${kp_assertion}" --arg host "${kp_hostname}" '
        .stages[$s].assertions[$a] ==
          {probe:"tls",hostname:$host,port:443,minimumRemainingSeconds:300}
      ' "${KUBERPLOY_E2E_SCENARIO_FILE}" >/dev/null ;;
    dns)
      jq -e --arg s "${kp_stage}" --arg a "${kp_assertion}" \
        --arg host "${KUBERPLOY_E2E_PUBLIC_HOSTNAME:-}" '
        .stages[$s].assertions[$a].probe == "dns" and
        .stages[$s].assertions[$a].hostname == $host and
        ((.stages[$s].assertions[$a] | keys | sort) ==
          ["expectedAddress","hostname","probe"])
      ' "${KUBERPLOY_E2E_SCENARIO_FILE}" >/dev/null ;;
    *) return 1 ;;
  esac
}

kp_qualification_validate_scenario() {
  local kp_catalog="${1:?catalog required}"
  local kp_stage kp_mutating kp_assertions kp_assertion kp_probe
  jq -e '
    .schemaVersion == 1 and
    (.apiBaseURL | type == "string" and test("^https://[^[:space:]@]+$") and
      (contains("?") | not) and (contains("#") | not) and (contains("..") | not)) and
    (.stages | type == "object") and
    (.workflow | type == "object") and
    (.workflow.project | keys - ["name","slug","teamId"] | length == 0) and
    (.workflow.project.name | type == "string" and length > 0 and length <= 100) and
    (.workflow.project.slug | type == "string" and test("^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$")) and
    (.workflow.directEnvironment | keys - ["name","slug"] | length == 0) and
    (.workflow.protectedEnvironment | keys - ["name","slug"] | length == 0) and
    (.workflow.application | keys - ["name","slug"] | length == 0) and
    (.workflow.directDeployment | type == "object") and
    (.workflow.directDeploymentUpdate | type == "object") and
    (.workflow.protectedDeployment | type == "object")
    and ((.workflow.sourceBuild | keys | sort) == ["builderPool","credentials","definition","github","promotion","push"])
    and ((.workflow.sourceBuild.credentials | keys | sort) == ["cacheSecretName","namespace","pushSecretName"])
    and all(.workflow.sourceBuild.credentials.namespace,.workflow.sourceBuild.credentials.pushSecretName,
      .workflow.sourceBuild.credentials.cacheSecretName;
      type == "string" and test("^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$"))
    and (.workflow.sourceBuild.credentials.pushSecretName != .workflow.sourceBuild.credentials.cacheSecretName)
    and ((.workflow.sourceBuild.github | keys | sort) == ["githubInstallationId","githubRepositoryId","installationId","ownerId","ownerLogin","repositoryId","repositoryName","senderId","senderLogin"])
    and all(.workflow.sourceBuild.github.installationId,.workflow.sourceBuild.github.repositoryId;
      type == "string" and test("^[a-f0-9-]{36}$"))
    and all(.workflow.sourceBuild.github.githubInstallationId,.workflow.sourceBuild.github.githubRepositoryId,
      .workflow.sourceBuild.github.ownerId,.workflow.sourceBuild.github.senderId;
      type == "number" and floor == . and . >= 1)
    and all(.workflow.sourceBuild.github.ownerLogin,.workflow.sourceBuild.github.repositoryName,
      .workflow.sourceBuild.github.senderLogin; type == "string" and test("^[A-Za-z0-9_.-]{1,100}$"))
    and ((.workflow.sourceBuild.push | keys | sort) == ["afterCommit","deliveryId"])
    and (.workflow.sourceBuild.push.afterCommit | type == "string" and test("^[a-f0-9]{40}$"))
    and (.workflow.sourceBuild.push.deliveryId | type == "string" and test("^[A-Za-z0-9._:-]{1,100}$"))
    and ((.workflow.sourceBuild.builderPool | keys) == ["nodeSelector"])
    and (.workflow.sourceBuild.builderPool.nodeSelector | type == "object" and length > 0)
    and (.workflow.sourceBuild.definition | type == "object")
    and (.workflow.sourceBuild.promotion | type == "object")
    and ((.workflow.helm | keys | sort) == ["approvalId","approvalRevision","valuesYaml"])
    and (.workflow.helm.approvalId | type == "string" and test("^[a-f0-9-]{36}$"))
    and (.workflow.helm.approvalRevision | type == "number" and floor == . and . >= 1)
    and (.workflow.helm.valuesYaml | type == "string" and length > 0 and length <= 262144)
    and (.workflow.registryCleanup.targetId | type == "string" and test("^[a-f0-9-]{36}$"))
    and ((.workflow.upgrade | keys | sort) == ["manifestDigest","sourceVersion","targetVersion"])
    and (.workflow.upgrade.sourceVersion | type == "string" and test("^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$"))
    and (.workflow.upgrade.targetVersion | type == "string" and test("^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$"))
    and (.workflow.upgrade.manifestDigest | type == "string" and test("^sha256:[a-f0-9]{64}$"))
    and ((.workflow.tls | keys | sort) == ["customCertificateName","localACMEIssuerName"])
    and (.workflow.tls.customCertificateName | type == "string" and test("^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$"))
    and (.workflow.tls.localACMEIssuerName | type == "string" and test("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"))
    and (.workflow.recovery == {
      postgresql:{namespace:"kuberploy-system",podName:"kuberploy-postgresql-0",
        controllerName:"kuberploy-postgresql"},
      valkey:{namespace:"kuberploy-system",controllerName:"kuberploy-valkey",
        persistentVolumeClaimName:"kuberploy-valkey"},
      worker:{namespace:"kuberploy-system",controllerName:"kuberploy-worker"}
    })
    and ((.workflow.observability | keys | sort) == ["from","to","workloadId"])
    and (.workflow.observability.workloadId | type == "string" and test("^[a-f0-9-]{36}$"))
    and ((.workflow.observability.from | fromdateiso8601) <
      (.workflow.observability.to | fromdateiso8601))
    and ((.workflow.runtimeSecret | keys | sort) == ["environmentName","key","name"])
    and (.workflow.runtimeSecret.name | type == "string" and test("^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$"))
    and (.workflow.runtimeSecret.key | type == "string" and test("^[A-Za-z0-9._-]{1,253}$"))
    and (.workflow.runtimeSecret.environmentName | type == "string" and test("^[A-Za-z_][A-Za-z0-9_]*$"))
    and ((.workflow.security | keys | sort) == ["networkPolicyProvider","resourceQuota"])
    and ((.workflow.security.networkPolicyProvider | keys | sort) == ["container","daemonSet","image","namespace"])
    and (.workflow.security.networkPolicyProvider.namespace | type == "string" and test("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"))
    and (.workflow.security.networkPolicyProvider.daemonSet | type == "string" and test("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"))
    and (.workflow.security.networkPolicyProvider.container | type == "string" and test("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"))
    and (.workflow.security.networkPolicyProvider.image | type == "string" and
      test("^[A-Za-z0-9][A-Za-z0-9._:/-]*@sha256:[a-f0-9]{64}$"))
    and ((.workflow.security.resourceQuota | keys | sort) == ["exceededValue","name","resource"])
    and (.workflow.security.resourceQuota.name | type == "string" and test("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"))
    and (.workflow.security.resourceQuota.resource | IN("requests.cpu","requests.memory","limits.cpu","limits.memory"))
    and (.workflow.security.resourceQuota.exceededValue | type == "string" and test("^[1-9][0-9]*(m|Mi|Gi)?$"))
    and ((.teardown | keys | sort) == ["authority","infrastructureId","publicKeySHA256"])
    and (.teardown.authority | type == "string" and test("^[A-Za-z0-9._:-]{3,128}$"))
    and (.teardown.infrastructureId | type == "string" and test("^[A-Za-z0-9._:/-]{3,255}$"))
    and (.teardown.publicKeySHA256 | type == "string" and test("^[a-f0-9]{64}$"))
  ' "${KUBERPLOY_E2E_SCENARIO_FILE}" >/dev/null || \
    kp_die "KUBERPLOY_E2E_SCENARIO_FILE has an invalid top-level contract"
  local kp_pinned_key_digest kp_actual_key_digest
  kp_pinned_key_digest="$(jq -r '.teardown.publicKeySHA256' "${KUBERPLOY_E2E_SCENARIO_FILE}")"
  kp_actual_key_digest="$(openssl pkey -pubin -in "${KUBERPLOY_E2E_TEARDOWN_PUBLIC_KEY_FILE}" \
    -outform DER 2>/dev/null | openssl dgst -sha256 -r | awk '{print $1}')"
  [[ "${kp_actual_key_digest}" == "${kp_pinned_key_digest}" ]] || \
    kp_die "teardown public key does not match the scenario-pinned SPKI digest"
  jq -e --arg http "${KUBERPLOY_E2E_HTTP_HOSTNAME}" '
      .workflow.directDeployment.route.hostname == $http and
      .workflow.directDeployment.route.tlsMode == "httpOnly" and
      .workflow.directDeploymentUpdate.route.hostname == $http and
      .workflow.directDeploymentUpdate.route.tlsMode == "httpOnly"
  ' "${KUBERPLOY_E2E_SCENARIO_FILE}" >/dev/null || \
    kp_die "workflow routes must exactly cover the declared HTTP, local ACME, and custom TLS hostnames"
  while IFS='|' read -r kp_stage kp_mutating kp_assertions; do
    [[ -n "${kp_stage}" ]] || continue
    IFS=',' read -r -a kp_assertion_list <<<"${kp_assertions}"
    for kp_assertion in "${kp_assertion_list[@]}"; do
      kp_probe="$(kp_qualification_expected_probe "${kp_assertion}")"
      kp_qualification_validate_assertion_spec "${kp_stage}" "${kp_assertion}" "${kp_probe}" || \
        kp_die "scenario assertion ${kp_stage}/${kp_assertion} does not match repository semantic contract ${kp_probe}"
    done
    jq -e --arg s "${kp_stage}" --arg csv "${kp_assertions}" '
      (.stages[$s].assertions | keys | sort) == ($csv | split(",") | sort)
    ' "${KUBERPLOY_E2E_SCENARIO_FILE}" >/dev/null || \
      kp_die "scenario stage ${kp_stage} must contain exactly the catalog assertions"
  done <<<"${kp_catalog}"
}

kp_qualification_prepare_artifacts() {
  local kp_path="${KUBERPLOY_E2E_ARTIFACT_DIR}"
  local kp_parent
  local kp_parent_physical

  [[ "${kp_path}" == /* ]] || \
    kp_die "KUBERPLOY_E2E_ARTIFACT_DIR must be one absolute path"
  [[ "${kp_path}" != "/" && "${kp_path}" != "${HOME:-/nonexistent}" ]] || \
    kp_die "KUBERPLOY_E2E_ARTIFACT_DIR is too broad"
  [[ "$(basename "${kp_path}")" == \
     "${KP_QUALIFICATION_ARTIFACT_PREFIX}${KUBERPLOY_E2E_RUN_ID}" ]] || \
    kp_die "artifact directory basename must be kuberploy-qualification-<run-id>"
  kp_parent="$(dirname "${kp_path}")"
  [[ -d "${kp_parent}" && ! -L "${kp_parent}" ]] || \
    kp_die "artifact directory parent must be an existing non-symlink directory"
  kp_parent_physical="$(cd "${kp_parent}" && pwd -P)"
  [[ "${kp_parent}" == "${kp_parent_physical}" ]] || \
    kp_die "artifact directory parent must be a canonical path without symlink traversal"
  if [[ -e "${kp_path}" ]]; then
    [[ -d "${kp_path}" && ! -L "${kp_path}" ]] || \
      kp_die "artifact path must be a non-symlink directory"
    [[ -z "$(find "${kp_path}" -mindepth 1 -maxdepth 1 -print -quit)" ]] || \
      kp_die "artifact directory must be empty"
  else
    mkdir -- "${kp_path}"
  fi
  chmod 700 "${kp_path}"
}

kp_qualification_validate_semantic_proof() {
  local kp_stage="${1:?stage required}" kp_stage_dir="${2:?stage directory required}"
  local kp_proof="${kp_stage_dir}/evidence/workflow-proof.json"
  local kp_uuid='^[a-f0-9]{8}-[a-f0-9]{4}-[1-5][a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$'
  if [[ "${kp_stage}" == "00-preflight" ]]; then
    :
  elif [[ "${kp_stage}" == "10-one-chart-install" ]]; then
    local kp_installer="${kp_stage_dir}/evidence/installer-proof.json"
    [[ -f "${kp_installer}" && ! -L "${kp_installer}" ]] || \
      kp_die "stage ${kp_stage} is missing repository installer proof"
    jq -e '
      (keys | sort) == ["adminRecurringLogin","adminUserId","applicationCount",
        "bootstrapTokenJobConsumed","contractsExact","developerRecurringLogin","developerUserId",
        "githubMetadataTeamShared","immutableSourceRevisions","independentApplicationsCreated",
        "installationId","invitationAccepted","logoutInvalidatedSession","mutation",
        "packageDigestsAttested","secretsExcludedFromEvidence","soleOwnerDenied","teamId"] and
      .mutation == "installer-render-and-release" and
      (.applicationCount | type == "number" and . >= 2 and floor == .) and
      .independentApplicationsCreated == true and
      .immutableSourceRevisions == true and
      .packageDigestsAttested == true and .bootstrapTokenJobConsumed == true and
      .logoutInvalidatedSession == true and .adminRecurringLogin == true and
      .invitationAccepted == true and .developerRecurringLogin == true and
      .soleOwnerDenied == true and .githubMetadataTeamShared == true and
      .contractsExact == true and .secretsExcludedFromEvidence == true and
      all(.adminUserId,.developerUserId,.teamId,.installationId; test("^[a-f0-9-]{36}$"))
    ' "${kp_installer}" >/dev/null || kp_die "stage ${kp_stage} installer proof is invalid"
    return 0
  fi
  [[ -f "${kp_proof}" && ! -L "${kp_proof}" ]] || \
    kp_die "stage ${kp_stage} is missing repository workflow proof"
  case "${kp_stage}" in
    20-postgresql-valkey)
      jq -e --arg uuid "${kp_uuid}" '
        (keys | sort) == ["acceptedOperationCompletedAfterRestarts","exactlyOnceConverged",
          "expiringTokenIssued","multiScopeGrantCount","mutation","outboxPublishedBeforeLoss",
          "postRestartRead","postgresOutboxReplayed","postgresqlUIDChanged","projectId",
          "revokedTokenDenied","serviceAccountAuthenticated","serviceAccountId",
          "valkeyDatasetDeleted","valkeyDeploymentRestored","workerDeploymentRestored"] and
        .mutation == "durable-operation-and-valkey-dataset-loss" and
        (.projectId | test($uuid)) and .postgresqlUIDChanged == true and
        .valkeyDatasetDeleted == true and .outboxPublishedBeforeLoss == true and
        .postgresOutboxReplayed == true and .exactlyOnceConverged == true and
        .valkeyDeploymentRestored == true and .workerDeploymentRestored == true and
        .postRestartRead == true and
        .acceptedOperationCompletedAfterRestarts == true and
        .multiScopeGrantCount == 3 and (.serviceAccountId | test($uuid)) and
        .expiringTokenIssued == true and .serviceAccountAuthenticated == true and
        .revokedTokenDenied == true
      ' "${kp_proof}" >/dev/null ;;
    25-config-edge)
      jq -e --arg uuid "${kp_uuid}" '
        (keys|sort)==["applicationId","configMap","controllerReady","crossApplicationDenied",
          "defaultsProbesResourcesVerified","deploymentId","dnsReconciled","environmentVariableSetSaved",
          "exactLivePodSpecVerified","externalDNSApplicationId","externalDNSDeploymentId","externalDNSHostname",
          "guidedYAMLSharedDraft","hostname","immutableConfigMapRefsVerified","integrationId","mutation",
          "overrideProvenanceVerified","profileId","projectVariableSetSaved","protectedMaterialized",
          "providerKind","providerLabelInjectionDenied","renderedManifestDiffVerified","routeSaved",
          "selectedCanonicalPublicIPv4"] and
        .mutation=="variables-appconfig-scheduling-sslip-external-dns" and
        all(.applicationId,.deploymentId,.profileId,.integrationId,.externalDNSApplicationId,.externalDNSDeploymentId; test($uuid)) and
        (.hostname|test("^[^.]+\\.[0-9]+-[0-9]+-[0-9]+-[0-9]+\\.sslip\\.io$")) and
        (.selectedCanonicalPublicIPv4|test("^[0-9]+(\\.[0-9]+){3}$")) and
        (.externalDNSHostname|test("^config-edge-[a-z0-9-]+\\.qualification\\.test$")) and
        .providerKind=="rfc2136" and (.configMap|type=="string" and length>0) and
        all(.projectVariableSetSaved,.environmentVariableSetSaved,.overrideProvenanceVerified,
          .immutableConfigMapRefsVerified,.guidedYAMLSharedDraft,.renderedManifestDiffVerified,
          .defaultsProbesResourcesVerified,.exactLivePodSpecVerified,.crossApplicationDenied,
          .providerLabelInjectionDenied,.protectedMaterialized,.controllerReady,.routeSaved,.dnsReconciled; .==true)
      ' "${kp_proof}" >/dev/null ;;
    30-git-argo)
      jq -e --arg uuid "${kp_uuid}" '
        (keys | sort) == ["argoApplication","argoRevision","argoSyncedHealthy",
          "directOperationId","directUpdateOperationId","mutation","protectedOperationId",
          "rollbackOperationId","runtimeDigestVerified"] and
        .mutation == "direct-protected-update-rollback" and
        all(.directOperationId,.directUpdateOperationId,.protectedOperationId,
          .rollbackOperationId; test($uuid)) and
        (.argoApplication | test("^kp-d-[a-f0-9]{32}$")) and
        (.argoRevision | test("^[a-f0-9]{40}$")) and
        .argoSyncedHealthy == true and .runtimeDigestVerified == true
      ' "${kp_proof}" >/dev/null ;;
    40-source-build)
      jq -e --arg uuid "${kp_uuid}" '
        (keys | sort) == ["autoDeployDeploymentId","autoDeployOperationId","autoDeployPolicyId",
          "autoDeployReceiptSubmitted","buildCancellationAccepted","buildCancellationJobDeleted",
          "buildCancellationRetrySucceeded","buildDefinitionId","buildPromotionOperationId",
          "cacheColdDegradedPushSucceeded","cacheDegradedBuildId","cacheHitBuildId","cancelRetryBuildId",
          "cancelledBuildId","credentialValuesExcluded","duplicateDeliveryDeduplicated",
          "durableDeliveryPollingConverged","helmApplicationRevision","helmArgoSyncedHealthy",
          "helmReleaseId","helmRenderedPreviewSanitized","invalidWebhookRejected",
          "liveBuildJobCredentialSplit","mutation","pushFailureBuildId",
          "pushFailureTerminal","safetyPollRetained","secondBuildCacheHit","signedWebhookAccepted",
          "successfulBuildId","webhookWakeDisabled"] and
        .mutation == "github-webhook-build-cancel-cache-fault-auto-deploy-promotion-and-approved-helm" and
        all(.successfulBuildId,.buildDefinitionId,.buildPromotionOperationId,.helmReleaseId,
          .autoDeployPolicyId,.autoDeployOperationId,.autoDeployDeploymentId,.cacheHitBuildId,.cacheDegradedBuildId,
          .pushFailureBuildId,.cancelledBuildId,.cancelRetryBuildId; test($uuid)) and
        .signedWebhookAccepted == true and .invalidWebhookRejected == true and
        .duplicateDeliveryDeduplicated == true and .liveBuildJobCredentialSplit == true and
        .webhookWakeDisabled == true and .safetyPollRetained == true and
        .durableDeliveryPollingConverged == true and .secondBuildCacheHit == true and
        .cacheColdDegradedPushSucceeded == true and .pushFailureTerminal == true and
        .autoDeployReceiptSubmitted == true and .credentialValuesExcluded == true and
        .buildCancellationAccepted == true and .buildCancellationJobDeleted == true and
        .buildCancellationRetrySucceeded == true and .cancelRetryBuildId == .cacheHitBuildId and
        (.helmApplicationRevision | test("^[a-f0-9]{40}$")) and
        .helmRenderedPreviewSanitized == true and .helmArgoSyncedHealthy == true
      ' "${kp_proof}" >/dev/null ;;
    50-runtime-edge)
      jq -e --arg uuid "${kp_uuid}" --arg hostname "${KUBERPLOY_E2E_HTTP_HOSTNAME}" '
        (keys | sort) == ["hostname","middlewareId","mutation",
          "profileAttachedThroughConfigSave","responseHeaderVerified"] and
        .mutation == "http-route-and-middleware" and .hostname == $hostname and
        (.middlewareId | test($uuid)) and .profileAttachedThroughConfigSave == true and
        .responseHeaderVerified == true
      ' "${kp_proof}" >/dev/null ;;
    60-local-tls)
      jq -e --arg uuid "${kp_uuid}" --arg custom "${KUBERPLOY_E2E_CUSTOM_TLS_HOSTNAME}" \
        --arg local "${KUBERPLOY_E2E_LOCAL_ACME_HOSTNAME}" \
        --arg directory "${KUBERPLOY_E2E_LOCAL_ACME_DIRECTORY_URL}" '
        (keys | sort) == ["acmeDirectoryURL","attachedCertificateReissued","attachedHostsTLSVerified","configuredDirectoryVerified",
          "customCertificateBindingId","customHostname","customRouteAttached","hostname",
          "issuerCatalogAuthorized","localACMERouteAttached","mutation"] and
        .mutation == "attached-custom-certificate-and-local-acme-routes" and
        .customHostname == $custom and .hostname == $local and .acmeDirectoryURL == $directory and
        (.customCertificateBindingId | test($uuid)) and .customRouteAttached == true and
        .localACMERouteAttached == true and .configuredDirectoryVerified == true and
        .issuerCatalogAuthorized == true and .attachedHostsTLSVerified == true and
        .attachedCertificateReissued == true
      ' "${kp_proof}" >/dev/null ;;
    70-registry-retention)
      jq -e --arg uuid "${kp_uuid}" '
        (keys | sort) == ["deleted","imageTagResolution","mutation","planId","protected","state"] and
        .mutation == "registry-preview-execution-and-existing-image" and (.planId | test($uuid)) and
        .state == "succeeded" and .protected >= 1 and .deleted >= 1 and
        (.imageTagResolution | keys | sort) == ["argoSyncedHealthy","gitRevision","immutableImage",
          "operationId","persistedDigestOnly","previewResolved","requestedTag","runtimeDigestOnly"] and
        (.imageTagResolution.operationId | test($uuid)) and
        (.imageTagResolution.gitRevision | test("^[a-f0-9]{40}$")) and
        (.imageTagResolution.requestedTag | test(":")) and
        (.imageTagResolution.immutableImage | test("@sha256:[a-f0-9]{64}$")) and
        .imageTagResolution.previewResolved == true and .imageTagResolution.persistedDigestOnly == true and
        .imageTagResolution.argoSyncedHealthy == true and .imageTagResolution.runtimeDigestOnly == true
      ' "${kp_proof}" >/dev/null ;;
    80-observability)
      jq -e --arg uuid "${kp_uuid}" '
        (keys | sort) == ["denials","metrics","monitoring","mutation","runtime","safeEvidence","schemaVersion"] and
        .mutation == "authorized-observability-and-cross-tenant-denial" and
        .schemaVersion == 1 and
        (.monitoring|keys|sort)==["available","identityAttestation","mode"] and
        (.monitoring.mode=="managed" or .monitoring.mode=="existing") and
        .monitoring.available==true and
        (if .monitoring.mode=="managed" then
          .monitoring.identityAttestation=="managed-exact-release-and-rules"
        else .monitoring.identityAttestation=="existing-compatible-catalog" end) and
        (.runtime|keys|sort)==["container","exactSourceNonEmpty","followNonEmpty","gapSemantics",
          "mergedSnapshotNonEmpty","namespace","pod","sanitizedEventNonEmpty","workloadId"] and
        (.runtime.workloadId|test($uuid)) and
        (.runtime.namespace|test("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")) and
        (.runtime.pod|type=="string" and length>0) and (.runtime.container|type=="string" and length>0) and
        .runtime.mergedSnapshotNonEmpty==true and .runtime.exactSourceNonEmpty==true and
        .runtime.followNonEmpty==true and .runtime.gapSemantics=="explicit-bounded-events" and
        .runtime.sanitizedEventNonEmpty==true and
        (.metrics|keys|sort)==["catalog","from","liveSeriesMetric","scopes","to"] and
        .metrics.catalog==["cpu-usage","memory-working-set","replicas-ready","container-restarts",
          "http-request-rate","http-error-ratio","http-latency-p95"] and
        .metrics.scopes==["service","namespace","global"] and
        .metrics.liveSeriesMetric=="replicas-ready" and
        ((.metrics.from|fromdateiso8601)<(.metrics.to|fromdateiso8601)) and
        .denials=={crossTenantLogs:404,crossTenantEvents:404,crossTenantServiceMetric:404,
          crossTenantNamespaceMetric:404,nonAdminGlobalMetric:403} and .safeEvidence==true
      ' "${kp_proof}" >/dev/null
      jq -e --argjson hermetic "${KUBERPLOY_E2E_HERMETIC_TEST:-false}" '
        (keys|sort)==["browserCommandInvoked","configPreview","hermeticSeam","logs","metrics","realBrowser","rollback","sourceChooser"] and
        .browserCommandInvoked==true and .sourceChooser==true and .configPreview==true and .logs==true and .metrics==true and .rollback==true and
        (if $hermetic then .hermeticSeam==true and .realBrowser==false else .hermeticSeam==false and .realBrowser==true end)' \
        "${kp_stage_dir}/evidence/browser-proof.json" >/dev/null ;;
    90-security)
      jq -e --arg uuid "${kp_uuid}" '
        (keys | sort) == ["auditActorResourceOutcome","auditEventIds","crossTenantDenied",
          "initialSealedSecretUID","initialVersionActivated","initialVersionAttached","mutation",
          "networkAllowed","networkDenied","networkProviderIdentityAttested","networkProviderUID",
          "priorVersionRetained","privilegedAdmissionDenied","quotaRejected","rbacDenied",
          "resourceQuotaUID","rolloutsReady","rotatedSealedSecretUID","rotatedVersionActivated",
          "rotatedVersionAttached","secretBindingId","secretNondisclosure"] and
        .mutation == "security-controls-and-runtime-secret-lifecycle" and
        .rbacDenied == true and .privilegedAdmissionDenied == true and
        .crossTenantDenied == true and .secretNondisclosure == true and
        .quotaRejected == true and .networkProviderIdentityAttested == true and
        .networkDenied == true and .networkAllowed == true and
        .auditActorResourceOutcome == true and
        (.networkProviderUID | type == "string" and length > 0) and
        (.resourceQuotaUID | type == "string" and length > 0) and
        (.auditEventIds | type == "array" and length >= 2) and
        ((.auditEventIds | unique | length) == (.auditEventIds | length)) and
        all(.auditEventIds[]; test($uuid)) and
        (.secretBindingId | test($uuid)) and
        (.initialSealedSecretUID | type == "string" and length > 0) and
        (.rotatedSealedSecretUID | type == "string" and length > 0) and
        .rotatedSealedSecretUID != .initialSealedSecretUID and
        .initialVersionActivated == true and .initialVersionAttached == true and
        .rotatedVersionActivated == true and .priorVersionRetained == true and
        .rotatedVersionAttached == true and .rolloutsReady == true
      ' "${kp_proof}" >/dev/null ;;
    100-upgrade-rollback)
      jq -e --arg uuid "${kp_uuid}" '
        (keys | sort) == ["mutation","postUpgradeRollbackSucceeded","releaseManifestVerified",
          "sourceIdentityVerified","targetIdentityReady","upgradeId","upgradeOperationId",
          "upgradeSucceeded"] and .mutation == "platform-upgrade-and-post-upgrade-rollback" and
        (.upgradeOperationId | test($uuid)) and (.upgradeId | test($uuid)) and
        .sourceIdentityVerified == true and .releaseManifestVerified == true and
        .upgradeSucceeded == true and .targetIdentityReady == true and
        .postUpgradeRollbackSucceeded == true
      ' "${kp_proof}" >/dev/null ;;
    *) kp_die "stage ${kp_stage} has no repository semantic proof schema" ;;
  esac || kp_die "stage ${kp_stage} workflow proof does not satisfy its semantic schema"
}

kp_qualification_validate_result() {
  local kp_stage="${1:?stage required}"
  local kp_assertions_csv="${2:?assertions required}"
  local kp_stage_dir="${3:?stage directory required}"
  local kp_result="${kp_stage_dir}/result.json"
  local kp_evidence

  [[ -f "${kp_result}" && ! -L "${kp_result}" ]] || \
    kp_die "stage ${kp_stage} did not produce result.json"
  jq -e --arg kp_run "${KUBERPLOY_E2E_RUN_ID}" \
    --arg kp_stage "${kp_stage}" --arg kp_expected "${kp_assertions_csv}" '
      .schemaVersion == 1 and .runID == $kp_run and .stage == $kp_stage and
      .status == "passed" and
      (.assertions | type == "array") and
      ((.assertions | map(.id) | sort) == ($kp_expected | split(",") | sort)) and
      (all(.assertions[];
        (.id | type == "string") and .status == "passed" and
        (.evidenceFiles | type == "array" and length > 0) and
        all(.evidenceFiles[]; type == "string" and length > 0)))
    ' "${kp_result}" >/dev/null || \
    kp_die "stage ${kp_stage} result does not satisfy its exact assertion contract"

  while IFS= read -r kp_evidence; do
    [[ "${kp_evidence}" =~ ^evidence/[A-Za-z0-9._/-]+$ && \
       "${kp_evidence}" != *".."* ]] || \
      kp_die "stage ${kp_stage} reported an unsafe evidence path"
    [[ -f "${kp_stage_dir}/${kp_evidence}" && \
       ! -L "${kp_stage_dir}/${kp_evidence}" ]] || \
      kp_die "stage ${kp_stage} evidence is missing or is a symlink: ${kp_evidence}"
  done < <(jq -r '.assertions[].evidenceFiles[]' "${kp_result}")
  if [[ "${kp_stage}" == "00-preflight" ]]; then
    :
  elif [[ "${kp_stage}" == "10-one-chart-install" ]]; then
    jq -e '
      all(.assertions[] | select(.id != "installer-single-entrypoint");
        .evidenceFiles | any(. == "evidence/installer-proof.json" or
          test("^evidence/[A-Za-z0-9-]+\\.evidence$")))
    ' "${kp_result}" >/dev/null || kp_die "stage ${kp_stage} assertions are not bound to installer proof"
  else
    jq -e 'all(.assertions[]; .evidenceFiles | any(. == "evidence/workflow-proof.json"))' \
      "${kp_result}" >/dev/null || kp_die "stage ${kp_stage} assertions are not bound to workflow proof"
  fi
  [[ "${kp_stage}" == "00-preflight" ]] || \
    kp_qualification_validate_semantic_proof "${kp_stage}" "${kp_stage_dir}"
  chmod 600 "${kp_result}"
}

kp_qualification_validate_inventory() {
  local kp_stage="${1:?stage required}"
  local kp_inventory="${2:?inventory required}"

  [[ -s "${kp_inventory}" && ! -L "${kp_inventory}" ]] || \
    kp_die "mutating stage ${kp_stage} must emit a non-empty inventory.ndjson"
  jq -s -e --arg kp_run "${KUBERPLOY_E2E_RUN_ID}" \
    --arg kp_stage "${kp_stage}" --arg kp_label "${KP_RUN_LABEL_KEY}" \
    --arg kp_managed "${KP_MANAGED_BY_LABEL_VALUE}" '
      length > 0 and
      (map([.apiVersion,.kind,.namespace,.name] | join("|")) | unique | length) == length and
      all(.[];
        .schemaVersion == 1 and .runID == $kp_run and .stage == $kp_stage and
        (.apiVersion | type == "string" and length > 0) and
        (.kind | type == "string" and length > 0) and
        (.namespace | type == "string") and
        (.name | type == "string" and test("^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$")) and
        (.uid | type == "string" and test("^[^[:space:]]+$")) and
        (.operation == "created" or .operation == "updated") and
        ((.operation == "created" and .cleanupPolicy == "delete") or
         (.operation == "updated" and .cleanupPolicy == "restore")) and
        .ownership.runLabelKey == $kp_label and
        .ownership.runLabelValue == $kp_run and
        .ownership.managedBy == $kp_managed and
        (if .operation == "updated" then
           (.beforeStateEvidenceFile | type == "string" and
            test("^evidence/[A-Za-z0-9._/-]+$") and (contains("..") | not))
         else true end))
    ' "${kp_inventory}" >/dev/null || \
    kp_die "stage ${kp_stage} inventory violates exact ownership or cleanup policy"
  chmod 600 "${kp_inventory}"

  while IFS= read -r kp_before_state; do
    [[ -f "$(dirname "${kp_inventory}")/${kp_before_state}" && \
       ! -L "$(dirname "${kp_inventory}")/${kp_before_state}" ]] || \
      kp_die "stage ${kp_stage} is missing an exact pre-update evidence file"
  done < <(jq -rs '.[] | select(.operation == "updated") | .beforeStateEvidenceFile' \
    "${kp_inventory}")
}

kp_qualification_validate_cleanup_result() {
  local kp_stage="${1:?stage required}"
  local kp_inventory="${2:?inventory required}"
  local kp_cleanup="${3:?cleanup result required}"
  local kp_count

  [[ -f "${kp_cleanup}" && ! -L "${kp_cleanup}" ]] || \
    kp_die "cleanup for ${kp_stage} did not produce cleanup-result.json"
  kp_count="$(wc -l <"${kp_inventory}" | tr -d ' ')"
  jq -e --arg kp_run "${KUBERPLOY_E2E_RUN_ID}" \
    --arg kp_stage "${kp_stage}" --argjson kp_count "${kp_count}" '
      .schemaVersion == 1 and .runID == $kp_run and .stage == $kp_stage and
      .status == "cleaned" and .verifiedAbsentOrRestored == true and
      .verifiedUIDAndOwnership == true and
      .cleanedOrRestoredCount == $kp_count
    ' "${kp_cleanup}" >/dev/null || \
    kp_die "cleanup for ${kp_stage} did not verify every inventoried mutation"
  chmod 600 "${kp_cleanup}"
}
