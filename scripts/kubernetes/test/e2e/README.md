# Full MVP conforming-cluster qualification

This directory contains the code-side orchestrator for the full MVP
qualification matrix. It does not contain a cluster identity, provider account,
credential, public hostname, or a claim that a live run passed.

`qualification.sh` always validates one explicit kubeconfig, context, and API
server. It runs the fixed stage order below and then invokes the built-in stage
implementation in reverse order to clean or restore every inventoried mutation. A failed run is
still a failed run when cleanup succeeds. A run whose cleanup cannot be proved
also fails.

| Stage | Required proof |
|---|---|
| `00-preflight` | Explicit target, supported version/APIs/nodes/storage, dependency inventory, and exact ready NetworkPolicy-provider identity |
| `10-one-chart-install` | One installer entrypoint, independent Applications, immutable source and package digests |
| `20-postgresql-valkey` | PostgreSQL restart durability plus exact managed-Valkey PVC dataset deletion, durable outbox reconstruction, exactly-once operation convergence, and Deployment restoration |
| `30-git-argo` | Direct projection, protected PR, and a new rollback intent |
| `40-source-build` | Signed/deduplicated delivery, safety-poll mode, isolated credential lanes, live build cancellation with exact Job deletion and successful retry, actual second-build cache hit, cache-only cold degradation, terminal push-only failure, durable auto-deploy receipt, immutable promotion, and approved Helm OCI |
| `50-runtime-edge` | Middleware attachment through AppConfig plus the exact HTTP hostname/path |
| `60-local-tls` | Custom certificate plus local ACME issue and renewal without a public ACME request |
| `70-registry-retention` | One execution that preserves protected entries and removes eligible entries |
| `80-observability` | Non-empty merged and exact-source logs, bounded follow/gap events, sanitized Kubernetes events, seven named metrics at service/namespace/global scope, managed-or-compatible monitoring identity, and scoped denials |
| `90-security` | RBAC/admission/ResourceQuota denial, enforced NetworkPolicy deny+allow, versioned secret non-disclosure, tenant isolation, and actor/resource/outcome audit timeline |
| `100-upgrade-rollback` | Source/release identity, successful target health, and post-upgrade rollback intent/result |

## Repository-owned execution boundary

`builtin-driver.sh` owns every substantive stage. Operator-provided executable
drivers are not supported. The operator instead supplies one strict declarative
JSON scenario. Assertion targets, paths, pointers, and proof schemas are
repository-owned templates; a scenario cannot substitute an unrelated
Kubernetes resource, API path, HTTP URL, or TLS/DNS hostname. It cannot contain
commands, shell fragments, Kubernetes arguments, curl options, or cleanup logic.

Each assertion ID is permanently bound in `lib.sh` to an exact
`helm-install`, `installer-proof`, `workflow-proof`, `http`, or `tls` contract.
The scenario must contain exactly the catalog assertions and exact contract;
missing, additional, swapped, or retargeted probes fail before artifacts or
mutation. Results also pass a closed per-stage proof schema.
The built-in driver permits only structured resource names, JSON pointers,
read-only GET API paths, expected HTTP statuses/JSON values, hostnames, ports, and
expected A records. It then performs the fixed Helm, kubectl, curl, OpenSSL, and
DNS interactions and writes the evidence itself.

Probe objects use only these fields:

| Probe | Declarative fields |
|---|---|
| `helm-install` | `probe` only; chart, release, namespace, wait, and timeout are repository-fixed |
| `installer-proof` | `probe` plus the exact assertion ID in `contract` |
| `workflow-proof` | `probe` plus the exact assertion ID in `contract` |
| `http` | absolute `url`, `expectedStatus` |
| `tls` | `hostname`, optional `port`, optional `minimumRemainingSeconds` |

The API base URL must be HTTPS. Declarative `api` assertion probes are read-only,
but the repository also owns fixed human mutation sequences. It creates a
project, direct and protected environments, an application, direct and
protected deployments, and a rollback intent; retries and promotes an exact
failed source build; previews and executes managed-registry retention; and
starts a verified platform upgrade. Paths, methods, Cookie/CSRF use,
idempotency keys, response schemas, terminal polling and direct-versus-PR
publication assertions are code-owned. The scenario supplies only closed body
fields and pre-existing external IDs such as the failed build and registry
target.

The security scenario also pins one exact ready CNI daemonset/container/image
identity and one exact environment ResourceQuota/resource/over-limit quantity.
The CNI image must be immutable by digest. Qualification fails during read-only
preflight if that identity or the NetworkPolicy API is absent; stage 90 then
proves actual default-deny and exact allow connectivity with run-owned Pods,
Service, and NetworkPolicies. Those objects are planned in inventory before
creation and are deleted only after exact UID and ownership validation.

Stage 10 creates the exact run namespace and the installer-required `argocd`
namespace using create-only semantics, records both observed UIDs, and invokes
the installer chart once in `argocd`. Any pre-existing `argocd` namespace fails
this deliberately disposable-cluster boundary instead of being adopted or
updated. Later stages create one owned marker before
their fixed interactions. Reverse cleanup rereads every exact identity and
verifies every UID plus both ownership labels before the first cleanup mutation.
Stage 10 uninstalls the exact Helm release and deletes both exact namespaces
last. Wildcard and
selector deletion remain prohibited by the kubectl wrapper. Each stage also
captures a UID-bearing snapshot of exact run-labeled resources. Installer
evidence includes the pre-mutation rendered manifest and the post-install exact
Helm-instance object identities and UIDs.

`result.json` has this interface:

```json
{
  "schemaVersion": 1,
  "runID": "run-id",
  "stage": "50-runtime-edge",
  "status": "passed",
  "assertions": [
    {
      "id": "http-route",
      "status": "passed",
      "evidenceFiles": ["evidence/http-route.json"]
    }
  ]
}
```

Every catalog assertion must appear exactly once and every evidence path must
resolve to a regular, non-symlink file inside the stage artifact directory.

Each line of `inventory.ndjson` has this interface:

```json
{
  "schemaVersion": 1,
  "runID": "run-id",
  "stage": "50-runtime-edge",
  "apiVersion": "apps/v1",
  "kind": "Deployment",
  "namespace": "kuberploy-e2e-run-id",
  "name": "route-probe",
  "uid": "observed-uid-after-create",
  "operation": "created",
  "cleanupPolicy": "delete",
  "ownership": {
    "runLabelKey": "kuberploy.io/test-run",
    "runLabelValue": "run-id",
    "managedBy": "kuberploy-e2e-harness"
  }
}
```

Updates use `operation: "updated"`, `cleanupPolicy: "restore"`, and an
`beforeStateEvidenceFile` under `evidence/`. Cleanup must resolve only those
exact identities, verify the observed UID and ownership labels before acting,
and never use wildcard, namespace-wide, or label-only deletion. It then writes:

```json
{
  "schemaVersion": 1,
  "runID": "run-id",
  "stage": "50-runtime-edge",
  "status": "cleaned",
  "cleanedOrRestoredCount": 1,
  "verifiedUIDAndOwnership": true,
  "verifiedAbsentOrRestored": true
}
```

## Required inputs

In addition to `KUBECONFIG`, `KUBERPLOY_TEST_CONTEXT`,
`KUBERPLOY_TEST_SERVER`, and `KUBERPLOY_E2E_RUN_ID`, a full local-provider run
requires:

- a new empty absolute `KUBERPLOY_E2E_ARTIFACT_DIR` named
  `kuberploy-qualification-<run-id>`;
- `KUBERPLOY_E2E_MUTATION_ACK=qualify:<run-id>:<exact-context>`;
- `KUBERPLOY_E2E_DISPOSABLE_CLUSTER_ACK=destroy-after-qualification:<run-id>:<exact-context>`;
- an absolute `KUBERPLOY_E2E_SCENARIO_FILE` using schema version 1 and an
  explicit HTTPS `apiBaseURL`;
- an absolute executable Chromium-compatible `KUBERPLOY_E2E_BROWSER_EXECUTABLE`.
  The repository-owned CDP runner exercises the installed UI; a hermetic-only
  seam cannot claim a real browser run;
- custom certificate PEM and custom private-key PEM inputs for write-only TLS
  tests. Human cookies, CSRF state, and scoped bearer credentials are derived
  after bootstrap into an auto-cleaned mode-0700 directory outside evidence;
- distinct mode-0600 `KUBERPLOY_E2E_RUNTIME_SECRET_INITIAL_VALUE_FILE` and
  `KUBERPLOY_E2E_RUNTIME_SECRET_ROTATED_VALUE_FILE` inputs, each containing one
  non-empty line. The driver ingests them through the write-only API, removes
  transient request bodies immediately, and rejects either value in evidence;
- mode-0600 one-line registry credential sources in
  `KUBERPLOY_E2E_REGISTRY_PUSH_USERNAME_FILE`,
  `KUBERPLOY_E2E_REGISTRY_PUSH_PASSWORD_FILE`,
  `KUBERPLOY_E2E_REGISTRY_CACHE_USERNAME_FILE`, and
  `KUBERPLOY_E2E_REGISTRY_CACHE_PASSWORD_FILE`, plus a distinct one-line
  `KUBERPLOY_E2E_REGISTRY_FAULT_PASSWORD_FILE`. Stage 40 streams them into two
  distinct run-owned Kubernetes Secrets, faults and restores one lane at a
  time, and never reads Secret data or stores any value in evidence. The
  scenario pins only their namespace and names under
  `workflow.sourceBuild.credentials`; its pre-existing registry target must
  reference those exact names;
- installer values that set
  `config.gitProjection.webhookWakeEnabled: false` and a bounded 15-60 second
  `pollIntervalSeconds` for this disposable run. Stage 40 attests the installed
  immutable ConfigMap before accepting suppressed-wake convergence evidence;
- the teardown authority public key. Its canonical DER SubjectPublicKeyInfo
  SHA-256 must equal `teardown.publicKeySHA256` in the scenario before any
  mutation starts;
- exact installer and upgrade-from values files;
- explicit HTTP, custom-TLS, and local-ACME hostnames and an HTTPS local ACME
  directory URL;
- exact external object IDs referenced by the
  declarative scenario. These are live-only inputs; the repository cannot know
  an operator's project/application/deployment IDs or credentials in advance.

`workflow.sourceBuild.push.deliveryId` must be a lowercase canonical UUID. The
GitHub webhook verifier enforces the provider delivery-ID format, so a readable
fixture label such as `qualification-delivery-1` is rejected before the
source-build workflow can run.

When `KUBERPLOY_E2E_PUBLIC_PROVIDER_TESTS=true`, the harness runs the
repository-owned Cloudflare workflow in `public-provider-workflow.sh`. It
creates only the exact run-scoped
`kuberploy-<run-id>.<configured-provider-zone>` A record, proves the provider
object and public DNS target, verifies the public TLS certificate and HTTP route, then
deletes the record by its observed provider ID and verifies absence. A
pre-existing record, changed provider identity, non-run-scoped hostname, or
missing credential fails closed. The credential is supplied by the mode-0600
`KUBERPLOY_E2E_DNS_PROVIDER_CREDENTIAL_FILE`; its contents never enter
evidence or the report. Public-provider runs still require the explicitly
acknowledged disposable cluster because the HTTPS route is product state.
Missing inputs fail before the first mutating stage. Secret file contents and
paths are never copied into the report.

The final report uses `qualified-teardown-required`, not `passed` or `cleaned`.
The harness removes its exact markers and Helm bootstrap release, but product
records, retained installer objects, and asynchronously Argo-created descendants
do not all have safe deletion APIs. The run is therefore valid only on the
explicitly acknowledged disposable cluster, which must be destroyed after
evidence collection. The report records that retained-state boundary rather
than falsely claiming per-object cleanup. Evidence remains local with mode-0700
directories and mode-0600 control files.

After the pinned infrastructure authority destroys the exact disposable
cluster, it signs a schema-v1 JSON receipt containing only the exact run,
context/server target, authority, infrastructure ID, qualified-report SHA-256,
`status: "destroyed"`, and destruction timestamp. Run
`finalize-teardown.sh` with the receipt, detached signature, and already pinned
public key. It verifies the SPKI digest, signature, report binding, destruction
time after qualification, and replay marker before transitioning the report to
`passed`. A new key, mismatched infrastructure, stale/future receipt, edited
report, or replay cannot finalize the run.
