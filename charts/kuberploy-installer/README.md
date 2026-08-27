# Kuberploy single-invocation installer

This chart is the single-invocation convergence path for a blank target cluster.
The installer release runs in `kuberploy-system`, directly bootstraps Valkey and
Argo CD, and then lets Argo reconcile every selected component from one exact
semantic release tag.

One Helm invocation installs or adopts Argo CD through the pinned local
`kuberploy-argocd` dependency and submits one retained Argo
`Application` per explicitly enabled component. Each Application renders the
existing wrapper at an explicit semantic release tag into its fixed namespace and owns that
release independently. No privileged in-cluster Helm Job, shell, or reusable
cluster credential is created.

For a truly blank cluster, the installer carries the exact Argo CD 3.5.0
`Application`, `ApplicationSet`, and `AppProject` CRDs in Helm's `crds/`
phase. The nested Argo bootstrap disables its duplicate CRD templates and
attests this parent ownership, so Helm discovers the APIs before mapping the
retained resources in the same invocation. Standalone Argo wrapper installs
continue to own those same CRDs themselves; both/neither ownership modes are
rejected.

## Required inputs

Installation is intentionally Helm-native. Operators provide one reviewed
values file to the public OCI installer chart; there is no shell installer that
discovers or mutates cluster configuration on their behalf. Start from
[`examples/installer/managed-platform-values.yaml`](../../examples/installer/managed-platform-values.yaml)
and replace every empty cluster-specific field with operator-owned values. The
irreducible inputs are:

- the fixed public Kuberploy OCI chart repository, a canonical HTTPS GitHub
  values repository, and an explicit semantic release tag;
- a release-manifest package version for every enabled component. Argo records
  that readable OCI chart version on the Application; the matching Git tag
  selects only reviewed non-secret value files;
- release-tagged, non-secret example files below `examples/installer/` containing the
  existing Secret references, storage classes, optional Kubernetes API CIDRs and
  provider-egress narrowing, DNS provider settings and public URL required by
  each selected wrapper. API CIDRs are needed when NetworkPolicy hardening is
  enabled;
- explicit `managed` or `adopted` mode. Adoption still requires each wrapper's
  compatibility attestation and never creates the adopted controller.
- optional `cluster.kubeAPIServerCIDRs` for NetworkPolicy-hardening rules, plus
  an optional closed `publicEndpoint` hostname/TLS Secret reference for the
  control-plane Ingress.

All enabled child charts permit egress to the three RFC1918 private ranges by
default: `10.0.0.0/8`, `172.16.0.0/12`, and `192.168.0.0/16`. Ingress remains
default-denied. Provider HTTPS egress uses the required dual-stack public route
by default; operators can add bounded CIDRs when infrastructure hardening is
ready.

Managed Argo CD uses the installer-owned direct Valkey dependency, closing the
former empty-cluster bootstrap cycle. The installer generates the exact Valkey,
PostgreSQL, Argo, Git SSH, and optional managed-registry bootstrap Secrets,
preserving their values through Helm `lookup` on upgrades. It creates no
privileged in-cluster Helm Job, shell, temporary shared cache, or reusable
cluster credential.

The installer never accepts arbitrary inline child values. Every Application
executes the release-packaged OCI chart selected by
`components.<name>.expectedPackageVersion`; it never executes chart templates
from a Git checkout. Git is a separate, read-only `$values` source pinned to the
matching explicit release tag and may supply only bounded paths below
`examples/installer`. Credential bytes remain in pre-existing Kubernetes
Secrets and must never be committed to those files.

`integrations.runtimeSecrets` is the installer-owned authority for runtime
Secret and custom-certificate materialization. Enabling it requires the
GitOps control plane and Sealed Secrets component, exact non-platform
Environment namespaces or the installer-owned `kp-` Environment namespace
prefix, the fingerprint Secret/key, and the active public
sealing-certificate Secret/key. The installer injects those fixed identities
into the control-plane chart and adds only the exact namespaces and managed
Environment prefix to its Argo AppProject. API authorization still derives the
destination namespace from the persisted Project and Environment; callers
cannot submit an arbitrary namespace. A remote values file cannot expand this
cross-namespace authority.
Rotate the public certificate reference whenever the Sealed Secrets controller
rotates its active key; no private sealing key enters installer values.

Enabling the control plane also requires explicit PostgreSQL authority and the
installer-owned Valkey bootstrap. When Valkey is managed, the installer owns the control-plane
fence for the exact `api-cache`, `api-limiter`, `outbox-publisher`, and
`worker-consumer` usernames/password keys. It never projects the generic shared
username/password into managed-mode processes; the independent Argo credential
remains outside the control plane.

Install the released chart only with the fixed identity and explicit version:

```sh
helm upgrade --install kuberploy-installer \
  oci://ghcr.io/kuberploy/charts/kuberploy-installer \
  --version 0.1.0-rc.375 \
  --namespace kuberploy-system --create-namespace \
  -f installer-values.yaml \
  --reset-values \
  --server-side=false \
  --wait
```

Keep `--reset-values` and `--server-side=false` on every upgrade. The enabled
child package versions must match the installer chart version; reusing stored
values can retain an older RC and is rejected before Argo resources are
changed. Argo CD legitimately records
server-side managed-field ownership while reconciling its `Application`
objects; Helm's client-side three-way update avoids force-taking that live
ownership while still applying the explicit installer package delta.

For a public HTTPS endpoint, set `publicEndpoint.enabled`, its exact hostname,
and `publicEndpoint.tls.enabled`. Managed TLS requires the managed cert-manager
component plus all three non-empty TLS fields: `secretName`,
`clusterIssuerName`, and `accountEmail`. The email registers the Let's Encrypt
ACME account; it is configuration metadata, not a provider credential. The
installer creates the bounded production ClusterIssuer configuration and adds
the exact issuer annotation to the control-plane Ingress. TLS traffic is bound
to Traefik's `websecure` entrypoint, while a dedicated port-80 Ingress and
route-scoped `redirectScheme` Middleware permanently redirect HTTP to HTTPS.
It rejects partial TLS configuration before Helm writes resources.

For an already issued, operator-owned TLS Secret, keep TLS enabled and set
`publicEndpoint.tls.manageCertificate: false`. Ingress continues serving
`secretName` without adding cert-manager issuer annotations or creating an ACME
order. Use `integrations.registry.manageCertificate: false` for an existing
registry TLS Secret.

The source checkout form `charts/kuberploy-installer` is for development and
chart tests. Operators should use the public OCI package so the selected chart
and its nested bootstrap dependencies share one readable release version.

Source checkouts generate the ignored nested wrapper archives with
`scripts/helm/prepare-dependencies.sh`; the archives are not tracked. The
preparer stages only reviewed chart files plus the exact locked upstream
archive, then uses `release/package_chart_archive.py` and the explicit
`dependencies.source-date-epoch`. It never uses Helm's timestamp-bearing local
wrapper packaging. `Chart.lock` pins the wrapper dependency metadata, while
`dependencies.lock` pins the exact generated archive bytes. Preparation and
render tests both reject missing, extra, or checksum-mismatched archives. The
validated replacement is staged beside the target and uses a recoverable,
same-filesystem rename/swap so interruption cannot leave partial archive bytes.
Release packaging independently repeats the same staging rules with the release
`SOURCE_DATE_EPOCH` so published nested archives are byte-identical to their
standalone release artifacts.

## Ownership and readiness truth

Installer-owned AppProjects and Applications carry `helm.sh/resource-policy:
keep` and no Argo deletion finalizer. Uninstalling or disabling this bootstrap
therefore does not prune independently owned foundations or workloads. Mode
changes are decommission/migration operations; do not use a values flip from
`managed` to `adopted` as an uninstall mechanism.

Sync waves record the intended order: base foundations at wave 0,
cert-manager at 5, DNS/monitoring/builder/registry at 10, and the control plane
at 20. Because Helm creates peer Applications directly, waves are metadata for
the protected app-of-apps/root reconciler and are not a dependency barrier for
this first bootstrap. Argo converges them independently. Pre-create all
operator Secrets before installation; otherwise affected Applications remain
degraded until those inputs exist.

Every install, upgrade, and rollback records the enabled entries from the
chart's fixed component catalog in a content-addressed immutable ConfigMap. A
post-lifecycle reconciler reapplies only those exact AppProject/Application
documents, including after rollback. A later hook then waits for every enabled
Application to report both `Synced` and `Healthy` at the requested readable
package version. The check binds the desired Helm source revision and Argo's
observed sync revision, so a self-healed or stale Application cannot make the
lifecycle falsely succeed. Its namespace Role grants only `get` and `watch` on
the exact enabled Application names; it has no list, wildcard, Secret, or
cluster-wide access. A failed or timed-out child leaves the Helm lifecycle
failed instead of reporting a completed platform transition.

Do not use automatic Helm rollback across a database migration. Before a
manual `helm rollback`, verify that the target release manifest accepts the
database's current schema; otherwise the older control-plane binaries must
remain blocked rather than starting against an unsupported history.

An enabled control-plane Application also requires an explicit
`bootstrap.controlPlaneToken.mode`. The child hook creates and prints the
one-time token without putting it in Git, values, or Helm release storage.
`precreated` uses the operator-owned `kuberploy-bootstrap` Secret and ignores
dormant API CIDRs; it never reads or discloses that Secret. There is no implicit
mode, so enabling the control plane while this authority is `disabled` fails
before Helm mutation. NetworkPolicy defaults off through
`cluster.networkPolicyEnabled`; empty API CIDRs use port-scoped public API
fallback rules, while explicit API CIDRs narrow the policy.

Managed Argo repository egress works without a rotating provider CIDR list.
Bundled Argo CD and bootstrap Valkey NetworkPolicies are disabled by default
for a frictionless install. Enable their chart-specific NetworkPolicy settings
when your infrastructure team is ready to supply cluster-specific API and
private-service ranges. Empty
`argoCD.argoFoundation.networkPolicy.repositoryEgressCIDRs` value
allows dual-stack public HTTPS only, excluding the configured Kubernetes API
CIDRs. Operators may supply bounded CIDRs to narrow that infrastructure rule;
repository URL, TLS, redirect, and credential checks remain authoritative.

## GitHub App and builder

`integrations.github` is the Helm-native switch for the complete source-build
entry path. Enabling it requires the control-plane and builder components plus
the public HTTPS endpoint. Supply only the GitHub App ID, client ID, slug,
platform binding UUID, and the name of a pre-created `kuberploy-system` Secret. Empty
provider CIDR lists allow dual-stack public HTTPS or the verified registry port
while excluding the configured Kubernetes API CIDRs. Operators may supply
canonical bounded CIDRs to narrow those infrastructure rules; no live provider
metadata lookup is required. The installer enables GitHub setup/webhooks, Git
projection,
the stage-one platform Git binding, and the hardened builder together; the
builder boundary is embedded in the control-plane Application so Argo never
assigns the same cluster-scoped admission resources to two Applications. The
installer rejects partial combinations. The published installer package owns
the exact `kuberploy-runtime` semantic version and OCI digest as release
metadata; operators neither copy nor override that digest in Helm values. The
Secret must contain the private key, webhook
secret, state-signing secret, and OAuth client secret under the control-plane
chart's fixed keys. No credential bytes enter Helm values or Argo Applications.
The same switch enables the read-only source-build log boundary; it remains
scoped to exact build Jobs. Published releases still bind their release images
by digest, while source installs may use explicit operator image tags.

The control-plane, source checkout, and registry lanes remain separate so a
registry route cannot silently become GitHub API access. Optional strict CIDRs
or egress proxies can provide additional infrastructure hardening.
Set `integrations.github.buildKitImage` to the pinned `v0.32.2` tag or a sha256
mirror, and optionally set `integrations.github.dindImage` to a semantic-version
tag or sha256 mirror. The installer passes both references to the control plane
and builder boundary.

`integrations.registry` makes the managed registry component installable from
the same Helm release without placing registry credentials in an Argo
Application. The installer also registers the exact `targetID`, `targetName`,
endpoint, repository prefix and separate pull/push/cache credential references
as the operator-owned `Managed` registry target; no UI create step or
metadata-only `External` placeholder is needed. API and worker both reconcile
that row idempotently and reject a conflicting admin redefinition.
Starter installs keep `generateCredentials: true`. The installer creates
separate lifecycle, builder-push, build-cache, Helm-OCI, and runtime-pull
identities, builds the registry `htpasswd`, and preserves the credential state
through Helm upgrades. Set `generateCredentials: false` only when the named
Secrets are pre-created and operator-managed; advance the readable
`secretRevision` whenever registry authentication rotates. The separate
`lifecycleCredentialSecretName` contains only the username/password used for
bounded observation and cleanup. The Helm-OCI identity is installed as an Argo
CD repository Secret in the Argo namespace, so direct Helm Apps use Argo's
native OCI pull path; it is never mounted into the Kuberploy API or worker. Select
shared-Ingress or dedicated-LoadBalancer exposure and set
the registry endpoint and TLS Secret. LoadBalancer mode supports bounded
provider annotations, an LB class, requested IP, and optional source ranges;
empty ranges use the infrastructure provider's public default;
both modes reuse Traefik and the installer-owned cert-manager ClusterIssuer.
The installer grants
registry Pod access only to `kuberploy-system` and the isolated
`kuberploy-build-dind` namespace. Workloads authenticate with their selected
Kubernetes `imagePullSecrets`; nodes need no insecure-registry or custom-CA
configuration.
Set `integrations.registry.runtimePull.enabled` when services should select
this private registry through the project credential catalog. Supply the exact
operator registry target ID, one readable profile/revision, the allowed
workload namespaces or a managed Environment namespace prefix such as `kp-`,
and a pre-created Docker config JSON Secret/key in
`kuberploy-system`. The installer passes only those references into the
control plane and grants its worker access only to the derived Secret names in
the allowed namespaces; credential bytes never enter Helm values, Argo, Git, or
the API.
Cloudflare registry DNS defaults to DNS-only. Set
`integrations.registry.cloudflareProxied: true` only for bounded test images;
the installer still rejects arbitrary annotation overrides.

The lifecycle hook proves that every selected Application reached the requested
revision in `Synced` and `Healthy` state. It does not replace Kuberploy runtime
attestations. Admission of users or traffic still requires the database, Git
projection, edge, certificate, registry, and monitoring readiness checks.
