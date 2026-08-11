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
  existing Secret references, storage classes, API/provider egress CIDRs, DNS
  provider settings and public URL required by each selected wrapper;
- explicit `managed` or `adopted` mode. Adoption still requires each wrapper's
  compatibility attestation and never creates the adopted controller.
- exact `cluster.kubeAPIServerCIDRs` for any enabled controller that reads the
  Kubernetes API, plus an optional closed `publicEndpoint` hostname/TLS Secret
  reference for the control-plane Ingress.

Managed Argo CD uses the installer-owned direct Valkey dependency, closing the
former empty-cluster bootstrap cycle. The installer generates only the exact
Valkey, PostgreSQL, and Argo bootstrap Secrets, preserving their values through
Helm `lookup` on upgrades. It creates no privileged in-cluster Helm Job, shell,
temporary shared cache, or reusable cluster credential.

The installer never accepts arbitrary inline child values. Every Application
executes the release-packaged OCI chart selected by
`components.<name>.expectedPackageVersion`; it never executes chart templates
from a Git checkout. Git is a separate, read-only `$values` source pinned to the
matching explicit release tag and may supply only bounded paths below
`examples/installer`. Credential bytes remain in pre-existing Kubernetes
Secrets and must never be committed to those files.

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
  --version 0.1.0-rc.69 \
  --namespace kuberploy-system --create-namespace \
  -f installer-values.yaml \
  --server-side=true --force-conflicts \
  --wait
```

Keep both server-side flags on every upgrade. Argo CD legitimately records
managed-field ownership while reconciling its `Application` objects; Helm must
reclaim the installer-rendered desired fields when their explicit package
version changes. The flag does not grant ownership of objects outside this
release.

For a public HTTPS endpoint, set `publicEndpoint.enabled`, its exact hostname,
and `publicEndpoint.tls.enabled`. Managed TLS requires the managed cert-manager
component plus all three non-empty TLS fields: `secretName`,
`clusterIssuerName`, and `accountEmail`. The email registers the Let's Encrypt
ACME account; it is configuration metadata, not a provider credential. The
installer creates the bounded production ClusterIssuer configuration and adds
the exact issuer annotation to the control-plane Ingress. It rejects partial
TLS configuration before Helm writes resources.

The source checkout form `charts/kuberploy-installer` is for development and
chart tests. Operators should use the public OCI package so the selected chart
and its nested bootstrap dependencies share one readable release version.

Source checkouts rebuild the dependency with the repository's deterministic
`release/package_chart_archive.py` and a release `SOURCE_DATE_EPOCH`; do not
replace it with Helm's timestamp-bearing local package output. `Chart.lock`
pins the local wrapper metadata digest, and the checked-in
`charts/kuberploy-argocd-0.1.0-rc.69.tgz` and
`charts/kuberploy-valkey-0.1.0-rc.69.tgz` make bootstrap rendering
network-independent.
`dependencies.lock` records package-integrity checks for both archives; the
render test verifies those bytes independently from their readable filenames.
Release packaging must rebuild that archive from the reviewed wrapper source
and verify the lock rather than resolving a newer dependency.

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

An enabled control-plane Application also requires an explicit
`bootstrap.controlPlaneToken.mode`. `generated` requires exact Kubernetes API
endpoint `/32` or `/128` CIDRs; the installer injects only the child chart's
token-generator switch and those CIDRs. The child hook creates and prints the
one-time token without putting it in Git, values, or Helm release storage.
`precreated` uses the operator-owned `kuberploy-bootstrap` Secret and rejects
dormant API CIDRs; it never reads or discloses that Secret. There is no implicit
mode, so enabling the control plane while this authority is `disabled` fails
before Helm mutation.

Managed Argo repository egress is also explicit. Populate
`argoCD.argoFoundation.networkPolicy.repositoryEgressCIDRs` with the current
published GitHub `git` ranges for the pinned values source and `packages`
ranges for `ghcr.io` OCI charts. A Git-only allowlist cannot pull release
charts; the installer never substitutes an all-address egress rule.

## GitHub App and builder

`integrations.github` is the Helm-native switch for the complete source-build
entry path. Enabling it requires the control-plane and builder components plus
the public HTTPS endpoint. Supply only the GitHub App ID, client ID, slug,
cluster UUID, the name of a pre-created `kuberploy-system` Secret, and exact
egress host CIDRs. The installer enables GitHub setup/webhooks, Git projection,
the stage-one platform Git binding, and the hardened builder together; the
builder boundary is embedded in the control-plane Application so Argo never
assigns the same cluster-scoped admission resources to two Applications. The
installer rejects partial combinations. The Secret must contain the private key, webhook
secret, state-signing secret, and OAuth client secret under the control-plane
chart's fixed keys. No credential bytes enter Helm values or Argo Applications.
The same switch enables the read-only source-build log boundary; it remains
scoped to exact build Jobs and uses the digest-pinned API image from the
published control-plane chart.

Use stable egress proxies when GitHub or registry destinations do not have
stable host routes. The control-plane, source checkout, and registry lanes are
separate arrays so a registry route cannot silently become GitHub API access.
Set `integrations.github.buildKitImage` to the explicit `v0.32.2` image in a
fixed approved mirror when Docker Hub endpoints rotate; the installer passes
the same reference to both the control plane and builder boundary.

`integrations.registry` makes the managed registry component installable from
the same Helm release without placing registry credentials in an Argo
Application. The installer also registers the exact `targetID`, `targetName`,
endpoint, repository prefix and separate pull/push/cache credential references
as the operator-owned `Managed` registry target; no UI create step or
metadata-only `External` placeholder is needed. API and worker both reconcile
that row idempotently and reject a conflicting admin redefinition.
Set `authSecretName` to a pre-created Secret in
`kuberploy-system` containing the registry chart's exact `htpasswd` and
`httpSecret` keys, and advance the readable `secretRevision` whenever those
values rotate. The separate `lifecycleCredentialSecretName` contains only the
username/password used for bounded observation and cleanup. Select
shared-Ingress or dedicated-LoadBalancer exposure and set
the registry endpoint and TLS Secret. LoadBalancer mode supports bounded
provider annotations, an LB class, requested IP, and required source ranges;
both modes reuse Traefik and the installer-owned cert-manager ClusterIssuer.
The installer grants
registry Pod access only to `kuberploy-system` and the isolated
`kuberploy-build-dind` namespace. Workloads authenticate with their selected
Kubernetes `imagePullSecrets`; nodes need no insecure-registry or custom-CA
configuration.
Set `integrations.registry.runtimePull.enabled` when services should select
this private registry through the project credential catalog. Supply the exact
operator registry target ID, one readable profile/revision, the allowed
workload namespaces, and a pre-created Docker config JSON Secret/key in
`kuberploy-system`. The installer passes only those references into the
control plane and grants its worker access only to the derived Secret names in
the listed namespaces; credential bytes never enter Helm values, Argo, Git, or
the API.
Cloudflare registry DNS is always forced to DNS-only; proxy mode cannot be
overridden through installer values.

`helm --wait` proves only that bootstrap objects and the direct Argo workload
were accepted. It does not prove child Application health or Kuberploy runtime
readiness. Admission of users or traffic requires every selected Application to
be `Synced` and `Healthy`, followed by Kuberploy's own database, Git projection,
edge, certificate, registry and monitoring readiness/attestation checks.
