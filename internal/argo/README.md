# Argo reconciliation core

The renderer generates deterministic, environment-scoped `AppProject`,
`ApplicationSet`, and `Application` manifests. Every environment owns one
distinct AppProject named from its server-derived namespace, so multiple
environments in a project never emit the same Argo resource. Project, destination cluster,
destination namespace, protected Git path, and Argo project are derived from
server-owned project/environment identity. ApplicationSet list elements are
trusted generated values, never fields read from tenant YAML.

Every managed Application has exactly two sources:

- a generic OCI source whose `repoURL` is the exact runtime chart repository,
  whose `path` is `.`, and whose `targetRevision` is the locked
  `sha256:...` chart digest; and
- a `$values` Git source pinned to the exact provider-verified active
  `indexedRevision`, never the binding's mutable branch.

The OCI source has three server-derived `valueFiles` entries in exact
project-VariableSet, environment-VariableSet, application-`app.yaml` order.
The two parent VariableSets are optional. The application document is
mandatory: operator-owned Helm parameters carry the exact expected project,
environment, and application IDs, and the runtime chart's schema/identity
guard rejects missing, default, or substituted `app.yaml` content even though
Argo is configured to ignore missing parent value files. A `chart` field,
semantic-version target, singular `source`, caller-controlled Helm parameters
or values, pass-credentials switches, UI overrides, namespace creation, and
cluster-scoped application resources are absent. The closed-shape tests
protect this contract; digest annotations are metadata, not the enforcement
boundary.

## Protected desired-state runtime

The stable schema and desired-state worker provide an immutable per-environment
command log, lease epochs, retry saturation, exact runtime readiness identity,
and crash recovery through the hardened Git projection mirror/token broker.
The command persists the exact environment revision, projection generation,
catalog digest, chart/renderer identities, protected manifest bytes, and
content digest. Planning and claiming require trusted validation receipts for
AppConfig/dependencies and resolved secret/registry references. A new mutation
is allowed only while that exact projection tuple is active.

The writer never calls the Argo sync API. It accepts only a server-derived
protected platform GitHub App binding and path. After a provider-verified
platform commit it performs one metadata-only hard-refresh patch on the exact
installer-owned root Application; narrow RBAC and a fail-closed admission
policy reject a spec, label, owner, finalizer, unrelated-annotation, namespace,
or name change. The durable command is completed only after that refresh is
acknowledged, so crash recovery safely repeats it. After an
ambiguous push it locates the exact operation-trailer commit, verifies that it
is an ancestor of the provider head, verifies the current protected path's
exact bytes, and records the operation commit rather than a later descendant.
Leases are heartbeated during provider and Git I/O and loss cancels the
operation. A command already present in Git remains recoverable when a newer
environment generation or later protected commit exists; a changed protected
path fails closed.

The immutable planned base remains the command's authorization snapshot. Under
the fenced claim, the writer proves that the current provider head is its
descendant and that this environment's path is still absent or has the exact
previous content ETag, then persists a once-only write-base revision and
provider-observation receipt before pushing. The commit uses that receipt as
its direct parent. This lets commands for different environments that were
planned from the same head serialize onto later safe descendants without
re-planning, while preserving deterministic before-push and after-push crash
recovery. A receipt is never rebased or rewritten.

The status model is safe for API reads and excludes manifest bytes,
credentials, and lease ownership. It exposes the pinned environment revision
and generation, catalog/content digests, runtime artifact identities, exact
operation commit, retry summary, and timestamps.

Registry pull eligibility is now a closed planning prerequisite. `Plan` clears
the projection approval's caller-supplied registry boolean before it can be
used and requires a `DesiredStateRegistryEligibilityResolver` result. The
PostgreSQL resolver opens a serializable transaction, locks the exact ready
environment binding and active indexed generation, requires the approved
application catalog to match every indexed application AppConfig, reparses the
raw bytes into `projectionpolicy.AppConfigPolicyDocument`, and calls
`RegistryPullArtifactEligibleTx` for each document. Public AppConfigs need no
artifact. A private AppConfig remains unresolved when its exact
environment/namespace/target/profile-revision artifact is absent, awaiting,
inactive, stale, or mismatched; no desired-state command is returned.

The isolated production core now implements the strict prerequisite chain:

- the platform-admin workflow supplies one server-derived platform GitHub App
  binding backed by verified installation/repository catalog rows;
- `PostgreSQLRuntimeBindingCatalog` re-reads every platform/environment
  binding against exact App ID, provider installation/repository identity,
  lifecycle, permissions, and fresh catalog timestamps;
- `RepositoryCredentialController` derives deterministic per-binding Argo
  repository Secret names and canonical GitHub HTTPS remotes, reads the
  operator private-key projection into a short-lived buffer, and applies only
  the closed Argo GitHub App Secret shape. It has no Secret get/list/watch
  surface. Explicit catalog revocation deletes only that deterministic name;
  readiness resumes only after an exact NotFound acknowledgement. A merely
  stale catalog blocks readiness without destructively deleting credentials;
- `InClusterProductionClient` observes the fixed root Application by exact
  name, UID, closed spec, provider-verified synced revision, and
  Synced/Healthy status. It exposes no Argo mutation or sync operation;
- `PostgreSQLDesiredStateProjectionGate` reconstructs the exact active indexed
  generation in a serializable transaction and invokes the same composite
  AppConfig policy used by projection activation. This revalidates edge,
  custom-certificate, external-DNS, runtime-secret, and registry-pull
  prerequisites at claim time. A durable post-push command may recover from
  its immutable receipt without being stranded by later mutable policy state;
  and
- `ProductionDesiredStateRuntime` is the only intended writer of the durable
  readiness lease. It requires the production claim-gate marker, observes the
  complete prerequisite proof before acquisition and every heartbeat, adopts
  one exact root UID/spec for the process lifetime, materializes commands, and
  runs the lease-fenced protected writer. `ProductionDesiredStateReadinessProbe`
  is the single API-facing readiness seam.

The production wiring now includes the exact GitHub branch/ruleset protection
observer, fixed recursive root Application, deterministic repository
credentials, exact-name Argo RBAC plus fail-closed credential admission, and
default-off API/worker construction using the production readiness probe. Argo
and dependent capabilities remain false when any configured identity,
protection proof, root observation, credential observation, policy receipt, or
worker lease is absent, stale, or mismatched; they can become true only for the
fully configured and freshly observed production path.

Ordinary rollback remains a separate Git command. It selects an eligible prior
immutable deployment input and submits a new desired-state change through the
same authorization, environment publication policy, and Argo readiness path.
Argo observations themselves remain read-only and never issue an imperative
sync, patch, or rollback.
