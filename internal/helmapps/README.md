# Approved external Helm application core

This package is the closed P0 contract for deploying a platform-approved Helm
chart from OCI, a classic HTTPS Helm repository, or a public HTTPS Git
repository. The stable schema stores immutable approvals, captured chart
packages, render work,
desired-release history, and the two fenced Git publication phases.
The package intentionally exposes no HTTP route or user-visible capability by
itself: production must still prove the renderer, protected publisher, Argo,
credential, and root-application observations ready before advertising Helm.

## Immutable input

An administrator approval pins all of the following as one immutable revision:

- exactly one source: canonical lowercase `oci://`, a classic HTTPS Helm
  repository plus chart name, or public HTTPS Git plus exact commit and chart
  path;
- one exact SemVer chart version and immutable source revision;
- the resolved source identity, fetched chart-package digest, and
  `values.schema.json` digest, all `sha256`;
- Helm `4.2.3`, policy `external-helm-p0.v1`, and the logical renderer
  reference `docker.io/alpine/helm:4.2.3`. Kubernetes renderer Jobs execute
  the verified immutable multi-architecture digest for that reference;
- a digest of that complete approval identity.

The server constructs `app.yaml` from the approval plus durable project,
environment, application, release, and namespace identities. There is no
caller decoder for that descriptor. The caller supplies only one bounded
`values.yaml` mapping. The stored input digest covers the exact approval,
descriptor, normalized values, and limits contract.

`values.yaml` rejects duplicate keys, aliases, anchors, explicit tags, merge
keys, multiple documents, non-JSON values, credential-like leaves, renderer
controls, arbitrary namespace controls, dependency/CRD switches,
post-renderers, schema skipping, and pass-credentials. Secret values are not an
input format. An `existingSecret`-style value may be accepted as chart
configuration only when it is a DNS-label reference, but rendered external
workloads may not read or mount arbitrary Kubernetes Secrets; application
secrets use the platform's separate, application-bound secret contract.

## Chart admission and render plan

Admission resolves the selected source once and captures its exact package in
PostgreSQL. OCI resolution is digest-pinned; classic repositories select one
exact index version; Git fetches one exact 40-character commit in an isolated
checkout and packages the declared chart path. The package must match the
resolved approval identities.
The package inspector then requires one v2 application chart whose root name
and exact version match the approval. It requires `values.yaml` and a
closed, offline `values.schema.json`, validates defaults and merged overrides,
and rejects:

- `dependencies`, packaged subcharts, `crds/`, remote schema references, and
  open object schemas;
- archive traversal, links, duplicate files, oversized files/archives, and
  ambiguous YAML/JSON;
- `lookup`, DNS lookup, time, UUID, and random template functions.

`UniversalChartPackageSource` is the admission-time dispatch boundary.
`OCIHTTPPackageSource` sends only
HTTPS `GET` requests to exact, sorted registry/auth host allowlists, requests
the approved manifest by digest (never a tag), rejects redirects, and accepts
only one OCI manifest containing one Helm chart layer with the exact approved
digest and size. The response's body digest and `Docker-Content-Digest` must
both match the approval before the layer is read. Optional Basic or Bearer
identity credentials come only from an operator-owned provider and are used solely for the bounded
Bearer-token exchange, and are cleared immediately; they are neither caller
input nor renderer input. The bounded process-local cache keys immutable
manifest/package digests, returns isolated byte copies, and clears evicted
chart bytes. The classic-repository source similarly permits only the exact
repository host, rejects redirects, and verifies the package selected by the
exact chart/version index entry. The Git source permits public HTTPS only,
disables ambient Git configuration and credentials, rejects redirects and
symlinks, checks the exact fetched commit, and packages with the pinned Helm
binary. SSH/private Git chart approval requires a future explicit credential
binding and is rejected rather than silently using ambient credentials.

After admission, `PostgresApprovedPackageSource` is the only production worker
package source. Render workers read and rehash the captured immutable package;
they never contact OCI, Helm, or Git providers and receive no source
credentials.

Production private access uses a strict, default-off registry-host profile
map. The API and worker resolve profiles only from their closed environment
contract and read either `username`/`password` or `token` beneath the fixed
projected root `/var/run/secrets/kuberploy/helm-oci/<profile>`. Rooted file
opens, ownership/mode checks, and byte bounds reject escapes, broad files, and
oversized material. Every credential is bound to one exact auth host; neither
Basic nor Bearer identity can follow a redirect or cross to another allowed
host. The renderer receives no projection. Profile identity, mode, hosts, and
the chart-derived Secret projection digest participate in the canonical
operator config digest; credential bytes do not. Before startup and before
each renderer-readiness lease, every configured profile must be readable and
valid. A registry with no profile retains the public anonymous behavior.

The only Helm argument vector is:

```text
template <server-release> /input/chart.tgz
  --namespace <server-namespace>
  --values /input/values.yaml
  --kube-version 1.36.3
```

There is no field through which a caller or adapter can add credentials,
kubeconfig, dependency URLs, hooks, a post-renderer, CRDs,
`--skip-schema-validation`, or `--pass-credentials`.

`KubernetesRenderExecutor` implements the process/Job seam through a narrow
Kubernetes REST client. Chart, schema, values, and descriptor inputs are
immutable digest-bound ConfigMap chunks; an init container reconstructs and
verifies them before Helm starts. Each render pass has a distinct fenced Job
identity, a deny-all ingress/egress NetworkPolicy, bounded output, deadline and
temporary storage, and UID/GID 65532. The executor may create/get/delete only
its ConfigMaps, Job, and NetworkPolicy, list the Job-selected Pod, and read the
fixed renderer container log. It has no Secret, exec, or ambient credential
operation.

The renderer contract is an
uncredentialed, non-root, read-only-root-filesystem worker with a read-only
input, no service-account token, no privilege escalation, all capabilities
dropped, RuntimeDefault seccomp, and no network. Input, expansion, values,
schema, output, resource count, total render time, attempts, and lease duration
are bounded. Merely having the Job objects or tables does not make the renderer
ready; the exact leased runtime identity must also match.

Each command renders twice inside one 30-second budget. The raw outputs must be
byte-identical. The result parser accepts only this namespaced allowlist:

- ConfigMap, ClusterIP Service, token-disabled ServiceAccount, and PVC;
- Deployment and StatefulSet;
- Job and CronJob;
- Ingress and NetworkPolicy;
- autoscaling/v2 HPA and policy/v1 PDB.

Every object must have the exact server-derived namespace and identity labels.
Secrets, RBAC, Namespaces, CRDs/custom resources, cluster-scoped kinds, Helm
hooks, generated names, owner references, finalizers, status, external/NodePort
Services, duplicate identities, and out-of-scope resources fail closed.

## Durable execution

`Store` has memory and PostgreSQL implementations. Approvals and submissions
use exact idempotent replay. Commands are claimed in due order with exclusive
row locks, bounded attempts, an expiring lease, a monotonically increasing
epoch, and exact renderer/policy/limits identity. Heartbeat, retry, failure, and
completion writes are owner-and-epoch fenced. Expired work can be reclaimed;
stale workers cannot publish. A result is immutable and content-addressed.
Readiness is also leased and must match every pinned runtime field exactly.

PostgreSQL is authoritative. The memory implementation mirrors the state
machine for tests and local development and is not a production capability.

## Desired releases and protected Git publication

Every application/environment pair has one immutable release history and one
monotonic head. Initial deploy, update, retry, disable, and rollback all create
a new revision. Retry and disable copy the exact parent input; rollback copies
one exact prior enabled revision. A caller cannot rewrite history, pick a
release name/namespace, or advance the head while publication of its current
stable Application is ambiguous. Request replay is actor-and-idempotency-key
bound to the complete desired input.

Approval documents expose the exact offline JSON schema and chart defaults.
`PreviewApprovalValues` validates defaults plus the single override document,
normalizes line endings, returns its content digest and effective JSON, and
computes a sorted bounded JSON-pointer diff from current desired values. The
PostgreSQL release service performs this validation synchronously before it
enqueues render work; renderer validation remains the independent execution
boundary.

Publication uses exactly two Git phases through `ProtectedPublicationStore`:

1. Commit `release.yaml` (or a server-derived disabled receipt) at the unique
   release revision path under `clusters/<cluster>/helm-manifests/...`. This
   path is outside Argo's recursive root and cannot affect the cluster yet.
2. Only after phase one is provider-verified, create/update/delete the one
   stable Application under `clusters/<cluster>/argocd/helm-applications/...`.
   A published Application pins `targetRevision` to the exact phase-one commit,
   uses directory mode with `recurse: false` and `include: release.yaml`, and
   contains no Helm, plugin, Kustomize, exclude, or mutable-ref source.

Both phases are exact-idempotent, due-ordered, platform-binding serialized,
lease/epoch fenced, and retain immutable write-base, commit, provider, path
digest, and trailer receipts. Never-attempted work fails closed if its active
projection snapshot becomes stale. Previously attempted work remains
recoverable because it might already have reached Git. Phase two also requires
the phase-one commit to be an ancestor of its claim-time provider head. The
typed mutation handoff permits only the two server-owned path families and
upsert/delete actions with absence or strong-ETag preconditions; it is designed
for the single hardened Git writer, not a second Helm-specific transport.

`PublicationPlanner` is the production bridge from durable render results to
those intents. Its PostgreSQL candidate queries select only the current head,
require a terminal successful render before publishing an enabled release,
and promote a provider-verified payload before starting more phase-one work.
The planner receives only durable identifiers; its binding snapshot comes
from the trusted active-projection resolver and is rechecked in the protected
store's serializable transaction. No HTTP request can provide that snapshot.
Admission records the exact current environment projection and foundation plus
the latest verified command that owns the environment AppProject. That command
may precede unrelated commits on the shared branch when Argo proves the rendered
desired-state bytes did not change. The publisher still requires its immutable
commit, the foundation commit, and the payload commit to be ancestors of the
fresh provider write base before making the Application reachable.

`RuntimeConfig` and `NewRuntime` compose the PostgreSQL release/render stores,
digest-only OCI source/cache, narrow Kubernetes renderer, planner, and the
protected Git publisher over one supplied gitprojection binding store,
provider verifier, credentialed mirror manager, and bounded publication lease.
The runtime independently runs renderer and publisher readiness, rendering,
planning, payload publication, and Application publication so one bounded
operation cannot starve another lease. Disabled configuration constructs none
of those dependencies and always reports both Helm capabilities false. When
enabled, every duration, identity, namespace, exact host allowlist, cache
bound, renderer policy, Argo namespace, Git dependency, and protected
publisher identity must validate together. Deploy and rollback capability
becomes true only while the exact renderer lease, exact protected-publisher
lease, and protected Argo readiness are all fresh; an error or stale
observation in any one dependency fails the whole gate closed.

The protected publisher config digest is derived from the active Git
projection runtime digest plus the complete enabled Helm runtime policy,
excluding only its own self-referential digest field. It covers renderer
placement, every poll/lease/timeout, OCI registry and authentication host
allowlists, the package-cache bound, and the Argo Application runtime. API and
worker use the same derivation, so a fresh publisher or Argo receipt from an
old rolling configuration cannot satisfy the new capability gate.

The stable schema persists that same operator digest on every queued command,
worker lease, immutable result, and renderer-readiness row. Claim, heartbeat,
completion, retry, readiness, and capability evaluation all compare it
exactly, so overlapping old and new worker Pods can process only commands
created under their own complete operator policy.

`ProductionProtectedArgoReadiness` is the concrete bridge to the sole
`argo.ProductionDesiredStateReadinessProbe`. Construction independently
cross-checks the platform binding, cluster, Argo namespace, deterministic
repository credential, production root Application name, runtime chart, and
native OCI digest enforcement against the Helm runtime and publisher identity.
Its compatibility digest fixes the recursive
`clusters/<cluster>/argocd` root and the protected `helm-applications`
directory. Each capability evaluation snapshots its requested timestamp into
the production probe, so a divergent caller clock, stale lease, changed probe,
publisher substitution, or root-identity mismatch cannot advertise Helm.

## Boundaries that remain outside this package

OCI authentication belongs only to the credentialed fetch boundary. The
render Job receives the already-fetched, digest-verified chart blob; registry
credentials must never be mounted into it. NetworkPolicy, Pod security, resource policy,
namespace ownership, allowed hosts, secret materialization, and the final
namespaced/cluster-scoped allowlist must also be enforced at Kubernetes/Argo
admission. Static Helm inspection cannot prove every behavior of arbitrary Go
templates or every Kubernetes admission/defaulting result.

In particular, server-side admission must remain the final barrier against
unsafe workload pod specs and races after rendering. Argo CD 3.5.0 embeds Helm
4.2.1 while this renderer pins Helm 4.2.3; promotion must test their output
equivalence and reject renderer/policy drift rather than assuming it.

`PostgresProtectedBindingResolver` derives `ProtectedBindingSnapshot` in one
serializable, read-only transaction. It requires the exact configured platform
binding and cluster, exactly one ready GitHub-App environment binding for the
durable project/environment scope, one matching active projection generation,
no invalid projected document, the application's exact project membership,
and a bounded immutable approval catalog whose identities and document bytes
all rehash correctly. Stale target/index heads, generation/parser
substitution, ambiguous or unready bindings, legacy credentials, catalog
tampering, and application-scope escape all fail closed. HTTP input never
supplies this snapshot.

`ProtectedGitPublisher` is the narrow adapter to gitprojection's existing
hardened mirror/token/mutation transport. It proves planned and payload
ancestry, the exact before-image or absence, both operation trailers, and the
exact postimage or match-delete before recording a provider-verified terminal
result. It does not publish readiness or enable the public capability by
itself; production runtime wiring and the combined Argo readiness proof remain
mandatory.

`ApprovalAdmissionService` is the only supported approval ingestion seam. It
accepts an authenticated platform actor, idempotency key, and one closed chart
source variant; resolves it through the configured admission boundary;
rehashes and inspects the package; extracts `values.schema.json` and
`values.yaml`; and atomically persists the approval, package, and immutable
documents.
Callers cannot submit document bytes or credentials. Its bounded catalog is
available independently of renderer and Argo readiness.

`PostgresRenderedManifestPreviewService.Preview` resolves only the exact
environment/application release head and its successful command/result in one
repeatable-read snapshot. It revalidates descriptor, manifest bytes, digests,
inventory, and resource count, then returns API version, kind, namespace, name,
and a deterministic bounded YAML projection for each resource. The projection
keeps only declarative identity/spec fields; it removes status and renderer
metadata and redacts Secret/ConfigMap payloads, annotations, literal environment
values, all commands and arguments, and sensitive leaves. Oversized resources
remain visible in inventory with an explicit omission marker. Raw manifests,
Kubernetes UIDs, Git identity, and renderer Job details never cross the seam.
