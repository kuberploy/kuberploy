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

## Required inputs

Start from `testdata/managed-values.yaml` or `testdata/adopted-values.yaml` and
replace every placeholder with operator-owned values. The irreducible inputs
are:

- a canonical HTTPS GitHub source URL and explicit semantic release tag;
- a release-manifest package version for every enabled component (recorded on
  the Application for audit; the release tag is Argo's human-readable source
  version while provenance and integrity are verified separately);
- release-tagged, non-secret example files below `examples/installer/` containing the
  existing Secret references, storage classes, API/provider egress CIDRs, DNS
  provider settings and public URL required by each selected wrapper;
- explicit `managed` or `adopted` mode. Adoption still requires each wrapper's
  compatibility attestation and never creates the adopted controller.

Managed Argo CD uses the installer-owned direct Valkey dependency, closing the
former empty-cluster bootstrap cycle. The installer generates only the exact
Valkey, PostgreSQL, and Argo bootstrap Secrets, preserving their values through
Helm `lookup` on upgrades. It creates no privileged in-cluster Helm Job, shell,
temporary shared cache, or reusable cluster credential.

The installer never accepts arbitrary inline child values. It stores only its
small server-owned mode fence in each Application; all operator configuration
comes from the same explicit release tag through bounded `valueFiles`. Credential
bytes remain in pre-existing Kubernetes Secrets and must never be committed to
those files.

Enabling the control plane also requires explicit PostgreSQL authority and the
installer-owned Valkey bootstrap. When Valkey is managed, the installer owns the control-plane
fence for the exact `api-cache`, `api-limiter`, `outbox-publisher`, and
`worker-consumer` usernames/password keys. It never projects the generic shared
username/password into managed-mode processes; the independent Argo credential
remains outside the control plane.

Install only with the fixed identity:

```sh
helm upgrade --install kuberploy-installer charts/kuberploy-installer \
  --namespace kuberploy-system --create-namespace \
  -f installer-values.yaml --wait
```

Source checkouts rebuild the dependency with the repository's deterministic
`release/package_chart_archive.py` and a release `SOURCE_DATE_EPOCH`; do not
replace it with Helm's timestamp-bearing local package output. `Chart.lock`
pins the local wrapper metadata digest, and the checked-in
`charts/kuberploy-argocd-0.1.0-rc.1.tgz` and
`charts/kuberploy-valkey-0.1.0-rc.1.tgz` make bootstrap rendering
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

`helm --wait` proves only that bootstrap objects and the direct Argo workload
were accepted. It does not prove child Application health or Kuberploy runtime
readiness. Admission of users or traffic requires every selected Application to
be `Synced` and `Healthy`, followed by Kuberploy's own database, Git projection,
edge, certificate, registry and monitoring readiness/attestation checks.
