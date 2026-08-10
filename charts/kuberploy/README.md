# Kuberploy control-plane chart

## First-administrator bootstrap token

For a first install, set `config.bootstrapSecret.generate=true` and provide the
exact Kubernetes API endpoint CIDR in `networkPolicy.kubeAPIServerCIDRs`. A
pre-install hook runs the API image's fixed `/kuberploy-bootstrap-token`
command. It creates only the configured Opaque Secret and prints one strict
`KUBERPLOY_BOOTSTRAP_TOKEN=kp_bootstrap_...` assignment in the hook Job log;
the token is never a Helm value or rendered manifest field. Extract only the
token value before the Job's 24-hour TTL expires:

```sh
kubectl -n <release-namespace> logs job/<release-name>-bootstrap-token \
  | sed -nE 's/^KUBERPLOY_BOOTSTRAP_TOKEN=(kp_bootstrap_[A-Za-z0-9_-]{43})$/\1/p'
```

If the Secret already exists, the hook succeeds without reading or disclosing
its value. Set `generate=false` when an operator provisions the Secret by a
different protected mechanism. The temporary hook ServiceAccount can only
create Secrets, its Role and RoleBinding are removed after success, and its
NetworkPolicy permits only the explicitly supplied API CIDR. Bootstrap remains
single-use at the PostgreSQL authority even if the Kubernetes Secret is later
replayed.

This chart owns only the API, worker, web UI and namespaced control-plane
support resources. It never templates an Argo `Application`, tenant Namespace,
or tenant workload, so an in-place Helm upgrade cannot prune application state.

Source defaults use the explicit `0.1.0-rc.15` release-candidate tags. Stable
release packaging must inject immutable `image@sha256` references
for all five release images (API, worker, web, upgrader, and builder-agent) and set
`global.requireImageDigest=true`; rendering then
fails closed if any component is not digest pinned.

Published packages embed the hardened `kuberploy-builder` boundary as the
disabled `builder` subchart and pin its builder-agent image by digest. It must
remain disabled until a cluster administrator deliberately provisions the
dedicated builder node pool; the source chart does not auto-install privileged
builder resources.

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
GitHub webhook ingress path, build-definition/history commands, durable worker
receipt/build reconciliation, and post-build registry release/cache projection.
`builder.networkPolicy.sourceEgressCIDRs` and `registryEgressCIDRs` must contain
only exact `/32` or `/128` hosts (or stable egress-proxy hosts); they are copied
into each immutable build definition and cannot overlap a configured Kubernetes
API address. `builder` and `builds` remain false in `/v1/capabilities` until a
worker with the exact matching App ID, namespace, agent digest, and runtime
profile has proved its projected App key, configured all three required worker
loops, and reported a fresh durable heartbeat.

`config.buildLogs.enabled` is a separate default-off, read-only API boundary
for source-build logs. It requires GitHub builds, the builder subchart,
NetworkPolicy enforcement, explicit Kubernetes API CIDRs, and a digest-pinned
API image. The API receives a Role only in `builder.namespace.name`: exact Job
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

`config.runtimeSecrets.enabled` opts into the strict Sealed Secrets runtime and
requires Git projection, NetworkPolicy enforcement, explicit Kubernetes API
CIDRs, a sorted nonempty destination-namespace allowlist, and digest-pinned API
and worker images. External Secrets is not a production runtime option until a
concrete audited `RemoteMaterialWriter` exists. Disabled configuration rejects
dormant namespaces, Secret references, and timing overrides.

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
and rejects dormant timing overrides. Enabling it requires runtime secrets and
Git projection, NetworkPolicy enforcement with explicit Kubernetes API CIDRs,
and digest-pinned API and worker images. It inherits the exact sorted
`runtimeSecrets.namespaces` allowlist; there is no second namespace setting.
The API and worker share an exact metadata-only observation identity, including
all bounded timings, while the enclosing Git policy digest also binds the
runtime-secret public sealing-certificate projection. The observer reads only
the named `bitnami.com/v1alpha1` SealedSecret described by the immutable
artifact receipt. It cannot list/watch resources, read Kubernetes Secrets, or
mutate, proxy, exec, attach, or port-forward. A custom-certificate reference is
eligible only while both the exact observation and the matching observer worker
heartbeat are fresh.

`config.certificateIssuerObserver.enabled` separately enables the admin-managed
cert-manager `ClusterIssuer` catalog. It is false by default and disabled mode
rejects dormant binding, cluster, or timing values. Enabling it requires Git
projection, protected Argo desired state, environment foundation, the exact
cert-manager edge profile, NetworkPolicy enforcement, and explicit Kubernetes
API CIDRs. Its platform-binding and cluster UUIDs must exactly equal the Argo
and foundation identities. The chart derives the observer namespace from the
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
false by default. Enabling it requires Git projection, NetworkPolicy
enforcement, explicit Kubernetes API CIDRs, sorted unique destination
namespaces, canonically sorted and unique pull profiles, and digest-pinned API
and worker images. Disabled configuration rejects dormant namespaces, profiles,
and timing overrides.

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
namespace-local Secret name from that tuple and the destination namespace; an
API caller cannot choose an `imagePullSecret` name.

For each allowed destination namespace the chart grants the worker `get` only
on the exact derived Secret names and `create` on Secrets. Kubernetes cannot
scope `create` by `resourceNames`, so the worker's closed Secret client and a
fail-closed `ValidatingAdmissionPolicy` enforce the remaining boundary. The
policy admits reserved `kuberploy-pull-*` creation only from this release's
exact worker ServiceAccount, in its exact configured namespace, with the exact
name/profile/credential tuple, five strict labels, one credential-reference
annotation, and one bounded immutable `kubernetes.io/dockerconfigjson` data
key. Reserved updates and deletes are denied. No pull runtime Role grants
`list`, `watch`, `update`, `patch`, or `delete`, and no pull ClusterRole is
rendered.

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
configuration rejects dormant profiles or timing overrides. An enabled runtime
requires NetworkPolicy enforcement, explicit Kubernetes API CIDRs, digest-pinned
API and worker images, and at least one Traefik, cert-manager, or external-dns
profile. Every observed Deployment image must also be pinned by digest. Profiles
carry an increasing revision, management mode, exact controller version,
namespace, object names and spec digests, and the exact immutable profile
ConfigMap name rendered by the corresponding standalone edge chart. Traefik and
cert-manager profiles additionally carry the complete fixed CRD set;
cert-manager names only explicitly approved ready ClusterIssuers. External-dns
profiles bind one exact integration UUID, TXT owner, policy, filters, and sorted
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
permits only TCP 443/6443 to the explicit Kubernetes API CIDRs; enabling edge
observation does not grant API Pods Kubernetes API egress. Because this runtime
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
the same platform-binding and cluster UUIDs, a supported Pod Security version,
and a bounded poll interval. The worker publishes the server-owned Namespace,
quota, limits, default-deny/DNS policies, and observer RBAC beneath the exact
root path before Argo can advertise desired-state readiness. The API derives
the current authoritative environment count on every readiness probe; a new
environment therefore closes readiness until its exact foundation intent is
published.

Fresh installs use two explicit stages. First,
`config.platformGitBinding.enabled` exposes the admin-only platform Git binding
workflow with one operator-owned cluster UUID while foundation and protected
Argo remain disabled; only the API receives that bootstrap identity. After the
workflow returns the server-generated binding UUID, the operator enables the
foundation and protected Argo with that exact binding and the same cluster
UUID. The worker receives the shared cluster identity only in this second
stage, when a configured runtime actually needs it.

Protected Argo Git materialization can be enabled only with the foundation,
GitHub App, Git projection, and Argo observation runtimes enabled; Argo observation,
desired state, and `rbac.argoNamespace` must name the same namespace. The
operator also supplies one canonical platform-binding UUID, one canonical
cluster UUID, an OCI chart repository, semantic chart version, exact
`sha256:` chart digest, and bounded poll/catalog ages. The desired-state chart
digest must equal the Git projection renderer digest. API and worker images
must both be digest pinned, and Kubernetes API egress must contain only exact
`/32` or `/128` hosts. All values participate in the immutable ConfigMap name.

The root Application name is fixed in the binary as
`kuberploy-platform-root`; it is not a chart value or environment variable.
Repository credential names are likewise derived only from catalog binding
UUIDs as `kuberploy-repo-<UUID-without-hyphens>`. The renderer identity is
always the chart's exact worker image and cannot be configured independently.
The API and worker receive the same closed production identity, while only the
worker receives Kubernetes mutation authority.

`config.helmApplications.enabled` is the final default-off approved external
Helm/OCI runtime. It can be enabled only after Git projection, GitHub App,
environment foundations, and protected Argo desired state are enabled. The
operator supplies one dedicated renderer namespace, sorted exact OCI registry
and token-service host allowlists, bounded render/publication/readiness
timings, and a bounded package cache. API and worker images must be digest
pinned; Kubernetes API and external OCI egress lists must contain only exact
`/32` or `/128` hosts. Every value participates in the immutable ConfigMap
name and therefore in the renderer/publisher operator digest.

Private OCI access is optional and remains operator-owned. Each
`ociCredentialProfiles` entry maps one exact registry host to one unique
profile, one exact auth host, and either a Basic username/password Secret pair
or one Bearer identity-token key. Profiles must be sorted by registry host;
hosts must already belong to the registry/auth allowlists. The chart projects
only those exact Secret keys, mode `0440`, beneath
`/var/run/secrets/kuberploy/helm-oci/<profile>` in the API and worker. It adds
no Secret RBAC. API and worker NetworkPolicies derive every explicit HTTPS
port from the same sorted registry/auth host allowlists (defaulting to 443),
so a private registry on a non-default port is not silently denied. The
ConfigMap carries only the safe host/profile/mode map plus
an irreversible projection-identity digest; Secret names, keys, and values are
not exposed there. Changing any projection identity changes the immutable
ConfigMap and complete Helm operator digest. Public hosts without a profile
continue to use anonymous token exchange.

The chart creates a tokenless renderer ServiceAccount in that dedicated
namespace and grants only the worker the ConfigMap, Job, Pod-log, and
NetworkPolicy verbs required by the closed renderer. It grants no Secret
access and no update or patch authority. Renderer Pods receive already-fetched
chart bytes, no registry credentials and no Kubernetes token, and match both a
permanent and per-Job deny-all ingress/egress policy. Deployment and rollback
capabilities remain false until the exact renderer, protected publisher, and
protected Argo readiness leases are simultaneously fresh.

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
registry namespace/Deployment/PVC/immutable ConfigMap identities, explicit
Kubernetes API CIDRs, NetworkPolicy enforcement, and a digest-pinned worker
image. Plain HTTP is separately acknowledged and is intended only for the
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
on TCP 5000 plus explicitly configured Kubernetes API CIDRs. External registry
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

API and worker startup migrations use PostgreSQL's advisory lock. The optional
`upgrade.migrationJob` is a narrowly scoped, pre-upgrade-only seam for a future
dedicated migration command: its ServiceAccount has token automount disabled
and receives no Kubernetes RBAC. Leave it disabled until that command exists.

The default-deny control-plane NetworkPolicies separate four egress classes.
Managed PostgreSQL is locked to labeled Pods in the `kuberploy-postgresql`
namespace on TCP 5432, and managed Valkey is locked to labeled Pods in the
`kuberploy-valkey` namespace on TCP 6379. Adopted external database or cache
endpoints must be named through `externalPostgreSQLEgressCIDRs` or
`externalValkeyEgressCIDRs`; all-address ranges are rejected for both. General
`externalEgressCIDRs` permit only provider protocols: API HTTPS; worker HTTPS,
SSH Git, and `git://`; and upgrader HTTPS. They never permit database, cache, or
Kubernetes API-specific ports. General provider CIDRs are also default-empty
and reject all-address ranges so an HTTPS allowance cannot silently include a
Kubernetes API on port 443. Configure exact provider ranges or a controlled
egress-proxy CIDR before enabling GitHub, external Git/OCI, release checks, or
an adopted Prometheus endpoint. Provider ranges must be disjoint from every
Kubernetes API range; exact duplicates are rejected at render time, while the
installer must reject broader CIDR containment before supplying chart values.

`config.valkey.mode=external` keeps the explicit compatibility contract: API
and worker read the generic address, username, and password keys. Managed mode
never emits those shared credential variables. The API instead receives exact
cache and limiter identities, while the worker receives exact outbox-publisher
and Stream-consumer identities from the per-role keys in
`config.valkey.secretRef`. The installer must deliver that connection Secret
into `kuberploy-system`; Kubernetes Secrets are never read across namespace
boundaries. Managed Valkey and Argo CD use separate namespace-local Secret
copies, and Argo's credential is not projected into the control plane.

Set `networkPolicy.kubeAPIServerCIDRs` to the exact API Service/control-plane
CIDRs seen by the cluster CNI. `0.0.0.0/0` and `::/0` are rejected. The worker
and generated upgrade Job receive TCP 443/6443 access to those CIDRs; the API
receives it only when `rbac.observedNamespaces` enables live runtime views, the
strict runtime-secret provider is enabled, or source-build logs are enabled.
Certificate-issuer live observation is worker-only; the API consumes its
durable PostgreSQL readiness proof and receives no issuer-driven Kubernetes API
egress.
An empty list intentionally
blocks those Kubernetes clients. Managed Prometheus
query access remains namespace-, Pod-label-, and TCP-9090-scoped; adopted
Prometheus uses HTTPS through the explicitly configured general egress CIDRs.
When NetworkPolicy is enabled, rendering fails if runtime/build RBAC or the
GitHub build controller is enabled without API CIDRs. It also fails if a GitHub
App, Git remote, or adopted Prometheus endpoint is configured without provider
CIDRs.
