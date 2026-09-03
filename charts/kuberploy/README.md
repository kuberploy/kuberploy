# Kuberploy control-plane chart

## First-administrator bootstrap token

For a first install, set `config.bootstrapSecret.generate=true`. If optional
NetworkPolicy enforcement is enabled, also provide the Kubernetes API endpoint
CIDR in `networkPolicy.kubeAPIServerCIDRs`. A
pre-install hook runs the API image's fixed `/kuberploy-bootstrap-token`
command. It creates only the configured Opaque Secret and prints one strict
`KUBERPLOY_BOOTSTRAP_TOKEN=kp_bootstrap_...` assignment in the hook Job log;
the token is never a Helm value or rendered manifest field. Extract only the
token value before the Job's 24-hour TTL expires:

`config.bootstrapSecret.activeDeadlineSeconds` defaults to 1800 seconds and
includes both a cold-node API image pull and generator execution.

```sh
kubectl -n <release-namespace> logs job/<release-name>-bootstrap-token \
  | sed -nE 's/^KUBERPLOY_BOOTSTRAP_TOKEN=(kp_bootstrap_[A-Za-z0-9_-]{43})$/\1/p'
```

If the Secret already exists, the hook succeeds without reading or disclosing
its value. Argo CD can recreate the hook Job on a later sync, so that newer Job
may report the existing Secret after the original disclosure log is gone. A
cluster administrator can recover the same value directly without placing it
in Helm values or command arguments:

```sh
kubectl -n <release-namespace> get secret <bootstrap-secret-name> \
  -o jsonpath='{.data.token}' | base64 --decode
```

Set `generate=false` when an operator provisions the Secret by a different
protected mechanism. The temporary hook ServiceAccount can only create
Secrets, and its Role and RoleBinding are removed after success. When
NetworkPolicy is enabled, it permits only the supplied API CIDRs. Bootstrap
remains single-use at the PostgreSQL authority even if the Kubernetes Secret is
later replayed.

The first administrator setup asks for an email address, display name, and
password. The email is the local sign-in identifier; the display name is shown
in the UI independently. Invitation links bind the invited
email, while the invitee supplies a display name and password. No email-sending
provider is required, and SSO is reserved for a future provider integration.

This chart owns only the API, worker, web UI and namespaced control-plane
support resources. It never templates an Argo `Application`, tenant Namespace,
or tenant workload, so an in-place Helm upgrade cannot prune application state.

Source defaults use the explicit `0.1.0-rc.434` release-candidate tags. Stable
release packaging must inject immutable `image@sha256` references
for all five deployed release images (API, worker, web, migration, and builder-agent) and set
`global.requireImageDigest=true`; rendering then
fails closed if any component is not digest pinned.

Published packages embed the `kuberploy-builder` boundary as the disabled
`builder` subchart and pin its builder-agent image by digest. Enabling it uses
privileged DinD. The default `builder.nodeIsolation.enabled=false` works on one
schedulable node; enabling node isolation additionally requires the exact
dedicated builder label and taint. The source chart does not auto-enable
privileged builder resources.

GitHub source-build recovery and reconciliation are also disabled by default.
Enabling `config.githubApp` requires the hardened builder boundary, an immutable
builder-agent image in published packages, and a controller ServiceAccount that
is exactly this release's worker in the release namespace. The operator supplies
the GitHub App ID, client ID, App slug, Secret object name, and four selected
source-key names as values. The chart projects exactly the private-key source
into the worker at
`/var/run/secrets/kuberploy/github-app/runtime/private-key.pem` with mode `0440`;
the API separately receives the private key, webhook HMAC secret, state-signing
secret, and OAuth client secret below the same fixed read-only root. The web
container receives none. Credential bytes never appear in values, a ConfigMap,
or environment variables.

The GitHub App must use `https://<config.publicURL-host>/v1/github/installations/setup`
as its Setup URL and `https://<config.publicURL-host>/v1/github/installations/callback`
as its user-authorization Callback URL. Leave GitHub's **Request user
authorization (OAuth) during installation** option disabled: GitHub otherwise
does not send the documented Setup URL return that carries `installation_id`.
The chart routes both exact browser callback paths directly to the API, along
with `/v1/webhooks/github`; no broader API prefix is exposed through that
special-case ingress rule. The primary human session remains `SameSite=Strict`;
setup authorization creates a 15-minute HttpOnly, host-only, `SameSite=Lax`
copy scoped first to the exact Setup URL path and then rotated to the exact
OAuth callback path for these cross-site top-level returns. Successful OAuth
completion clears it, stores the verified random handoff only in a second
HttpOnly, host-only, `SameSite=Strict` cookie scoped to the exact link endpoint,
and redirects to `/github/setup/complete`. That same-origin UI page completes
the flow with an empty, CSRF-protected `POST .../link`; handoff bytes are never
returned to JavaScript.

This switch activates the authenticated setup/callback API, the exact raw-body
GitHub webhook ingress path, App-source/history commands, durable worker
receipt/build reconciliation, and post-build registry release/cache projection.
Empty builder source and registry CIDR lists permit dual-stack public egress on
only HTTPS and the verified registry port, with configured Kubernetes API CIDRs
excluded. Optional `builder.networkPolicy.sourceEgressCIDRs` accepts canonical
bounded provider ranges and `registryEgressCIDRs` accepts exact hosts for
infrastructure-managed narrowing. The normalized lists are snapshotted on every
accepted build attempt. `builder` and `builds` remain false in `/v1/capabilities` until a
worker with the exact matching App ID, namespace, agent digest, explicit
`builder.buildKitImage` reference, and runtime
profile has proved its projected App key, configured all three required worker
loops, and reported a fresh durable heartbeat. `builder.buildKitImage` accepts
the pinned `v0.32.2` tag or a sha256 mirror; `builder.dindImage` accepts a
semantic-version tag or sha256 mirror and is included in the immutable runtime
identity.

`builder.nodeIsolation.enabled=false` is the revision-zero single-VM default
and emits no node selector or toleration. After bootstrap, **Settings → Source
builders** controls node isolation, queue concurrency, and per-container
requests and limits for new attempts. `true` binds runtime readiness, immutable build
definitions, generated Jobs, and admission to
`kuberploy.io/node-class=dind-builder` plus
`kuberploy.io/dind-builder=true:NoSchedule`. In both modes DinD remains
privileged and never mounts the host Docker socket.

Optional `builder.buildSecret` and `builder.sshSecret` values reference
pre-created Secrets in the builder namespace. Their bounded `profiles` entries
publish only safe IDs, labels, Secret data keys, and an explicit application-ID
allowlist; the UI/API accepts those opaque IDs and resolves fixed BuildKit
secret/SSH mount paths server-side. Secret values and arbitrary file references
never enter the API contract.

`config.buildLogs.enabled` is a separate default-off, read-only API boundary
for source-build logs. It requires GitHub builds and the builder subchart;
NetworkPolicy remains optional infrastructure hardening, while the privileged
builder's Job admission boundary is mandatory whenever the builder is enabled.
The API receives a Role only in `builder.namespace.name`: exact Job
`get`, Pod `get/list`, and `pods/log` `get`. It receives no watch, mutation,
Secret, exec, attach, proxy, or port-forward permission. Requests name only an
opaque build-attempt UUID; the API derives the immutable Job identity and
selects the single UID/owner-fenced Pod and fixed `agent` container. Snapshot
and SSE follow reads are bounded, audited, redacted, no-store, and reconnect
with opaque cursors. `buildLogs` is advertised independently from deployment
`logs` and only while this exact TLS-verified Kubernetes boundary is ready.

`config.gitProjection.enabled` independently opts the worker into durable,
lease-fenced safety polling and shadow indexing of environment/platform Git
bindings. It requires the GitHub App boundary so every target head is resolved
through the exact stored installation, immutable repository ID, and full ref;
the worker reuses the same single projected private-key file and does not mount
webhook or state-signing keys. The disposable mirror/worktree cache is mounted
only when enabled at `/var/lib/kuberploy/git-projection`; `cacheMaxBytes`
(64 MiB through 2 GiB) bounds each checked mirror/expanded tree in the worker
and the whole Pod cache through `emptyDir.sizeLimit`.
`pollIntervalSeconds` is bounded from 15 seconds through 24 hours. These
polls remain the correctness/repair path when the default-true
`webhookWakeEnabled` acceleration is disabled. Disabling webhook wake does not
disable or lengthen the safety poll, and the setting is included in the exact
Git projection runtime identity. These non-secret settings participate in the
immutable ConfigMap name. Enabling the
runtime also requires `chartVersion` to be an explicit semantic version of the
AppConfig renderer chart and a bounded `policyVersion`; both are
included in every strong Git bundle ETag and in the exact API/worker runtime
readiness identity. Protected environments additionally require a fresh exact
GitHub branch/ruleset attestation in the API before submission and again in the
worker before candidate publication. The App needs `metadata:read`,
`contents:write`, `pull_requests:write`, and `administration:read`; Kuberploy
never requests `administration:write`. Development direct publication does not
depend on the protection-read path.

`config.runtimeSecrets.enabled` opts into the Sealed Secrets runtime and
requires Git projection plus at least one destination namespace. Namespace
lists are normalized and deduplicated. External Secrets is not a production
runtime option until a concrete audited `RemoteMaterialWriter` exists. Disabled
configuration ignores dormant settings.

The operator supplies two existing Secrets in the control-plane namespace. The
API alone receives exactly the HMAC data key at
`/var/run/secrets/kuberploy-system/runtime-secret-fingerprint.key`; the worker
never mounts or reads that private key. Both API and worker receive only the
Sealed Secrets public certificate at the fixed
`/var/run/secrets/kuberploy-system/sealed-secrets/tls.crt` path. Both projected
files use mode `0440` and the Pod's exact non-root `fsGroup`; their bytes never
variables, API responses, or logs. The API proves both files, while the worker
proves the public certificate and the exact HMAC key metadata/key ID in the
shared readiness digest.

For every configured destination namespace, the chart renders two namespace-
local Roles: API `get/create/delete` and worker `get` on only
`bitnami.com/sealedsecrets`. It renders no runtime-secret ClusterRole and grants
no Kubernetes Secret read/list/watch/update/patch permission. A fail-closed
`ValidatingAdmissionPolicy` and deny binding restrict SealedSecret create/delete
in those namespaces to the exact API ServiceAccount and to the immutable,
strict-scope Kuberploy labels, digests, target name, and same-name Secret
template. Ordinary bindings allow bounded `Opaque` data; TLS-certificate
bindings allow only `kubernetes.io/tls` with exactly `tls.crt` and `tls.key`.
Other identities or malformed objects are denied before persistence.

`config.certificateObservation.enabled` opts into continuous, read-only
readiness for active custom TLS certificate versions. It is false by default
and ignores dormant settings. Enabling it requires runtime secrets and Git
projection. It inherits the normalized `runtimeSecrets.namespaces` allowlist;
there is no second namespace setting.
The API and worker share an exact metadata-only observation identity, including
all bounded timings, while the enclosing Git policy digest also binds the
runtime-secret public sealing-certificate projection. The observer reads only
the named `bitnami.com/v1alpha1` SealedSecret described by the immutable
artifact receipt. It cannot list/watch resources, read Kubernetes Secrets, or
mutate, proxy, exec, attach, or port-forward. A custom-certificate reference is
eligible only while both the exact observation and the matching observer worker
heartbeat are fresh.

`config.certificateIssuerObserver.enabled` separately enables the admin-managed
cert-manager `ClusterIssuer` catalog. It is false by default and ignores
dormant binding and timing values. Enabling it requires Git
projection, protected Argo desired state, environment foundation, and the
cert-manager edge profile. Its platform-binding UUID must equal the Argo and
foundation identity. The chart derives the observer namespace from the
Helm release namespace and the observer identity from this release's worker
ServiceAccount; neither is operator-selectable.

The worker publishes each active issuer revision only through the protected
platform Git binding. That bundle contains the `ClusterIssuer` plus one
deterministic ClusterRole/ClusterRoleBinding granting this exact worker
ServiceAccount only `get` on that exact issuer name. The control-plane chart
therefore renders no broad cert-manager RBAC: no list/watch, Secret access,
mutation, proxy, exec, attach, or port-forward permission is added. API and
worker share the same bounded observer identity and durable freshness lease;
admin mutation readiness remains closed until materialization and exact live
observation both match the current catalog.

`secretBindings` is advertised only while the API backend, fresh matching
runtime worker, exact Git projection policy digest, namespace allowlist, and
transactional reference resolver are all ready. AppConfig preview is read-only;
create/save re-resolve exact active binding versions and `secrets.bind` grants
inside the PostgreSQL Git-write transaction. Direct Git indexing applies the
same policy before a commit can become deployable.

`config.runtimeRegistryPulls.enabled` separately enables revisioned private
registry pull credentials for both managed and external registry targets. It is
false by default and requires Git projection. Exact namespace lists and managed
Environment namespace prefixes are normalized; at least one is required.
Conflicting identities fail. Disabled configuration ignores dormant settings.

Each profile binds one durable registry target ID and registry server to an
opaque pull-credential reference, a positive revision, and one existing source
Secret data key in the control-plane namespace. The source value must be a
strict Docker `config.json` containing credentials for exactly that profile's
registry server. Kubernetes Secret base64 is an encoding rather than
encryption; operators should enable Kubernetes encryption at rest and restrict
access to these source Secrets. Rotate credentials by creating or updating the
operator source, advancing the profile revision, and rolling out the resulting
immutable ConfigMap/worker configuration.

Credential bytes are projected only into the worker, read-only with mode
`0440`, below
`/var/run/secrets/kuberploy/registry-pulls/<profile>/dockerconfigjson`. They
never enter chart values, the generated ConfigMap, API or web Pods, Git,
responses, or logs. Git AppConfig stores only the server-derived
`targetId`/`profileName`/`profileRevision` tuple. The workload chart derives the
namespace-local Secret name from that tuple; an
API caller cannot choose an `imagePullSecret` name.

For exact destination namespaces the chart renders namespaced Roles. For a
managed namespace prefix it renders a ClusterRole whose `get` permission is
restricted to the finite profile-derived Secret names. Kubernetes cannot scope
`create` by `resourceNames`, so the worker's closed Secret client and a
fail-closed `ValidatingAdmissionPolicy` enforce the remaining boundary. The
prefix policy selects only foundation-labeled managed Environment namespaces
and also forbids the worker from creating any non-reserved Secret. Reserved
creation requires this release's exact worker ServiceAccount, exact
name/profile/credential tuple, five strict labels, one credential-reference
annotation, and one bounded immutable `kubernetes.io/dockerconfigjson` data
key. Reserved updates and deletes are denied. No pull permission grants `list`,
`watch`, `update`, `patch`, or `delete`.
The configured Argo namespace and, when enabled, the configured builder
namespace are exempt from this runtime-pull policy because their separate
fail-closed admission policies already constrain the same worker's exact
repository-credential and ephemeral source-credential Secret lifecycles.

The worker validates source material locally and creates or exactly adopts the
immutable namespace-local Secret through the Kubernetes API; kubelets, not the
worker, connect to registries to pull workload images. Runtime readiness is
fenced by the complete profile digest and a fresh worker heartbeat, while each
Git projection additionally requires a fresh exact artifact observation before
it can become deployable. External registry lifecycle and retention remain the
external operator's responsibility; enabling pull projection does not grant
Kuberploy permission to delete external images.

`config.edgeRuntime.enabled` opts into a separate, read-only observer for the
operator-approved edge infrastructure. It is false by default, and disabled
configuration ignores dormant profiles and timing overrides. An enabled runtime
requires at least one Traefik, cert-manager, or external-dns profile. Profiles
carry an increasing revision, management mode, exact controller version,
namespace, object names and spec digests, and the exact immutable profile
ConfigMap name rendered by the corresponding standalone edge chart. Traefik and
cert-manager profiles additionally carry the complete fixed CRD set;
cert-manager names only explicitly approved ready ClusterIssuers. External-dns
profiles bind one integration UUID, TXT owner, policy, filters, and a normalized
domain allowlist.

The optional Traefik `sslip` profile is absent by default. `auto-first-ip`
accepts only a literal, public IPv4 reported by the LoadBalancer Service;
`verified-static-ip` additionally requires one operator-owned
`staticPublicIPv4`, which the observer re-verifies against the Service address
or current IPv4 answers on every poll. Tenant requests never provide an IP or
arbitrary sslip.io hostname. ALBs and other hostname-based LoadBalancers with
rotating addresses remain unavailable in automatic mode; static NLB/EIP
designs may use verified-static mode after the operator accepts the single-IP
availability tradeoff. sslip.io remains a test/convenience facility—use an
owned domain for production availability.

Only the worker reads Kubernetes objects. For each configured namespace, the
chart grants `get` on the exact named Deployments and profile ConfigMap; the
Traefik profile additionally grants `get` on its exact Service. A release-scoped
ClusterRole is rendered only when required and grants `get` on the exact named
IngressClass, CRDs, and approved ClusterIssuers. Edge Roles grant no Secret,
credential, list, watch, proxy, subresource, or mutation access, and the closed
runtime client admits only those six read paths. The worker's NetworkPolicy
permits TCP 443/6443 to the optional Kubernetes API CIDRs; an empty list uses a
port-scoped dual-stack public fallback. Enabling edge observation does not grant
API Pods Kubernetes API egress. Because this runtime
never creates or updates Kubernetes objects, a ValidatingAdmissionPolicy or
server-side dry-run boundary is not applicable.

The complete profile and timing contract participates in the immutable
ConfigMap name and is projected identically into API and worker. Multi-worker
leases, epochs, exact configuration and target digests, Kubernetes UIDs/specs,
and bounded freshness are rechecked before readiness is recorded. `edge`,
`traefik`, and `certManager` become true only after the API observes a fresh
worker matching that exact contract. `externalDNS` and `traefikMiddlewares`
remain false until the production Argo desired-state runtime can also prove that
their generated objects were materialized; `externalDNSConfiguration` remains
the management-only API flag.

`config.environmentFoundation.enabled` and `config.argoDesiredState.enabled`
are paired, default-off production authorities. Foundation configuration pins
the singleton platform-binding UUID, a supported Pod Security version,
and a bounded poll interval. The worker publishes the server-owned Namespace,
quota, limits, default-deny/DNS policies, and observer RBAC beneath the exact
root path before Argo can advertise desired-state readiness. The API derives
the current authoritative environment count on every readiness probe; a new
environment therefore closes readiness until its exact foundation intent is
published.

Fresh installs use two explicit stages. First,
`config.platformGitBinding.enabled` exposes the admin-only platform Git binding
workflow with one operator-owned binding UUID while foundation and protected
Argo remain disabled; only the API receives that bootstrap identity. After the
workflow creates the singleton binding, the operator enables foundation and
protected Argo with that same binding UUID. The worker receives that binding
identity only in this second stage, when a configured runtime needs it.

Protected Argo Git materialization can be enabled only with the foundation,
GitHub App, Git projection, and Argo observation runtimes enabled; Argo observation,
desired state, and `rbac.argoNamespace` must name the same namespace. The
operator also supplies one canonical platform-binding UUID, one canonical
OCI chart repository, semantic chart version, exact `sha256:` chart digest,
and bounded poll/catalog ages. The desired-state chart
digest must equal the Git projection renderer digest. All values participate in
the immutable ConfigMap name; NetworkPolicy hardening is optional.

The root Application name is fixed in the binary as
`kuberploy-platform-root`; it is not a chart value or environment variable.
Repository credential names are likewise derived only from catalog binding
UUIDs as `kuberploy-repo-<UUID-without-hyphens>`. The renderer identity is
always the chart's exact worker image and cannot be configured independently.
The API and worker receive the same closed production identity, while only the
worker receives Kubernetes mutation authority.

Protected publication uses Kubernetes metadata refreshes, not an Argo API
account or token. The worker hard-refreshes only the fixed platform root, then
requests one reconciliation of the exact `kp-e-<environment UUID>`
ApplicationSet and waits for its controller-owned refresh annotation to be
removed. Namespaced RBAC is paired with fail-closed admission that rejects any
spec, identity, label, owner, finalizer, unrelated annotation, namespace, or
name mutation.

`config.helmApplications.enabled` enables direct Argo CD Helm Apps. It requires
protected Argo desired state so the chart has one exact Argo namespace. The API
stores current source-and-values settings with revision history and reconciles deterministic
`kp-h-<application UUID>` Argo `Application` objects. Argo CD resolves, renders,
syncs, and observes OCI, classic Helm repository, and Git chart sources; the
control plane has no chart downloader, approval catalog, package cache, or
renderer Job.

The chart installs one `kuberploy-helm-apps` AppProject that accepts external
sources only into server-derived `kp-*` namespaces and denies cluster-scoped
resources. It exists independently of GitHub and the protected platform Git
binding, so a Helm-only installation does not wait for provider setup.

The chart grants the API only `get`, `create`, `patch`, and `delete` for Argo
Applications in the configured Argo namespace. A matching admission policy
limits this identity to deterministic Kuberploy Helm App names, labels,
finalizer, in-cluster destination, automated synchronization, and immutable
`kuberploy-helm-apps` project/destination identity.

Private source credentials remain operator-owned Argo CD repository Secrets;
the API receives no provider credential or Secret read permission. The managed
registry installer can create its exact OCI repository Secret directly in the
Argo namespace. Other private repositories use the normal Argo CD repository
credential mechanism.

That namespace-local Role grants exact-name `get` only for the installer-owned
root Application. It grants no Secret `get`, `list`, or `watch`; repository
credentials receive only `create`, `patch`, and `delete`. `create` is necessary
because Kubernetes authorizes Server-Side Apply as create when the named object
does not exist and patch otherwise ([Server-Side Apply access
control](https://kubernetes.io/docs/reference/using-api/server-side-apply/#access-control-and-permissions)).
A fail-closed `ValidatingAdmissionPolicy`, matched only to the exact worker
ServiceAccount and Argo namespace, confines those verbs to deterministic
repository names, four exact labels, two exact annotations, `Opaque` type, and
exactly five bounded data keys. Update and delete additionally require an exact
managed `oldObject`; another actor's Argo Secret lifecycle is outside the
policy match. CEL errors deny the request, following Kubernetes admission
failure-policy semantics ([Validating Admission
Policy](https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/)).

`config.managedRegistry.enabled` independently opts the worker into complete
Distribution inventory observation and managed retention execution for one
operator-owned registry target. It is false by default. Enabling it requires an
exact target UUID, origin, repository prefix, opaque lifecycle credential ref,
readable target name, and exact independent runtime-pull/build-push/build-cache
credential references. API and worker idempotently materialize this identity as
a `Managed` target before serving or reconciling; caller updates cannot relabel
or substitute it. It also requires the
registry namespace/Deployment/PVC/immutable ConfigMap identities. Plain HTTP
is separately acknowledged and is intended only for the
cluster-local managed Service; HTTPS is otherwise required.

`config.managedRegistry.lifecycleCredentialRef` identifies only the
operator-owned observation, manifest-delete, and offline-maintenance
credential projected below. The persisted registry target has separate
runtime-pull, build-push, and build-cache credential references; the runtime
rejects a target if any of those identities reuses the lifecycle reference.

The operator supplies a Secret object name and username/password source-key
names. Only those two keys are projected into the worker at the fixed read-only
`/var/run/secrets/kuberploy/managed-registry` root; values and environment hold
references only. The chart creates a tokenless maintenance ServiceAccount and
namespace-local Role with get-only Deployment/PVC/ConfigMap access, exact
Deployment scale access, and the Job/Pod verbs required by the closed runtime
client. Helper Jobs receive no credential volume or API token and match a
deny-all ingress/egress policy. The worker may reach only labeled registry Pods
on TCP 5000 plus the optional Kubernetes API CIDRs; an empty list uses the
port-scoped public fallback. External registry
targets remain observation-only; their retention and GC are always the external
operator's responsibility. Public `registry` reports the credential-free
target/policy/inventory management surface and remains available for external
targets without this worker. `managedRegistry` becomes true only while the API
observes a fresh, epoch-fenced heartbeat from a worker with the exact managed
target, complete configuration digest, and observer/executor contract. The
worker publishes its first heartbeat only after the projected credential is
readable and the exact Deployment, immutable ConfigMap, and bound PVC pass a
read-only Kubernetes inspection. A stale or mismatched worker makes only the
managed capability false; managed inventory and cleanup routes fail closed with
HTTP 503 while external metadata and target configuration remain available.

GitHub App projections use narrowly repository-scoped installation tokens for
both provider verification and private HTTPS Git transport. The worker mints a
fresh token for each network phase and passes it to Git through a per-command,
mode-`0700` Unix askpass broker. The token is never placed in a remote URL, Git
config, command argument, environment variable, or credential file, and local
index/worktree cleanup does not depend on its lifetime. Projection bindings do
not use `config.git.credentialsSecret`.

The separate legacy deployment writer remains available during migration. For
a private repository used by that legacy path only,
`config.git.credentialsSecret` must name an operator-owned Secret containing
the configured `usernameKey` and `passwordKey`. The worker projects only those
two keys as group-readable `0440` files; its fixed askpass helper binds prompts
to the configured HTTPS remote host and username. Leave the Secret name empty
for a public legacy repository. Enabling projection does not silently migrate
or replace existing legacy-writer configuration.

The mandatory `kuberploy-migration` Job is the sole production schema writer.
It runs Prisma `migrate deploy` before both installation and upgrade, uses its
own release image, reads only the PostgreSQL URL Secret, receives no Kubernetes
token or RBAC, and can egress only to the selected PostgreSQL endpoint. Its
selector-only NetworkPolicy remains inert after completion and is replaced
before the next migration hook, avoiding Argo CD passive-hook deletion races. API and
worker startup are read-only and fail closed unless the exact migration names
and checksums compiled into the release have completed successfully.

Optional default-deny control-plane NetworkPolicies separate explicit egress
classes. Every selected control-plane Pod may reach RFC1918 private networks
(`10.0.0.0/8`, `172.16.0.0/12`, and `192.168.0.0/16`) by default. This additive
rule changes no ingress permission and grants no public Internet egress.
When NetworkPolicy hardening is enabled, managed PostgreSQL is limited to labeled Pods in the `kuberploy-postgresql`
namespace on TCP 5432, and managed Valkey is locked to labeled Pods in the
`kuberploy-valkey` namespace on TCP 6379. Adopted external database or cache
endpoints must be named through `externalPostgreSQLEgressCIDRs` or
`externalValkeyEgressCIDRs`; all-address ranges are rejected for both. General
`externalEgressCIDRs` permit only provider protocols: API HTTPS; and worker
HTTPS, SSH Git, and `git://`. Empty uses dual-stack public routes on only those
ports with configured Kubernetes API CIDRs in `ipBlock.except`; it never opens
database or cache ports. Nonempty canonical CIDRs optionally narrow public
egress and remain normalized, unique, and disjoint from Kubernetes API ranges.
Exact host, TLS, redirect, credential, and repository checks remain enforced by
the application regardless of NetworkPolicy breadth.

`config.valkey.mode=external` keeps the explicit compatibility contract: API
and worker read the generic address, username, and password keys. Managed mode
never emits those shared credential variables. The API instead receives exact
cache and limiter identities, while the worker receives exact outbox-publisher
and Stream-consumer identities from the per-role keys in
`config.valkey.secretRef`. The installer must deliver that connection Secret
into `kuberploy-system`; Kubernetes Secrets are never read across namespace
boundaries. Managed Valkey and Argo CD use separate namespace-local Secret
copies, and Argo's credential is not projected into the control plane.

When NetworkPolicy hardening is enabled, `networkPolicy.kubeAPIServerCIDRs` may
be left empty for the port-scoped dual-stack public fallback or set to
infrastructure-owned API Service/control-plane CIDRs to narrow it. Explicit
`0.0.0.0/0` and `::/0` values remain rejected; the empty list is the default.
The worker and migration hook Job receive TCP 443/6443 access to the selected
routes; the API
receives it when `rbac.observedNamespaces` enables manually managed runtime
views, when the protected environment foundation dynamically grants its exact
service account access in each Kuberploy environment, when the strict
runtime-secret provider is enabled, or when source-build logs are enabled.
Certificate-issuer live observation is worker-only; the API consumes its
durable PostgreSQL readiness proof and receives no issuer-driven Kubernetes API
egress.
Managed Prometheus
query access remains namespace-, Pod-label-, and TCP-9090-scoped; adopted
Prometheus uses HTTPS through the optional general egress CIDRs, with the same
public fallback when they are empty. Explicit provider CIDRs remain available
for infrastructure hardening; they are not required for a functional GitHub,
Git, or Prometheus integration.
