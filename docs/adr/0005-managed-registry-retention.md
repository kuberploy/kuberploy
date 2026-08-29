# ADR 0005: Managed OCI registry and bounded rollback retention

- Status: Accepted
- Date: 2026-08-09

## Context

Source builds need a registry that builders can push to and Kubernetes nodes can
pull from. Requiring every small self-hosted installation to provide an external
registry weakens the intended PaaS experience, while retaining every successful,
failed and superseded build forever makes local storage consumption unbounded.

Git records immutable image digests but does not store image bytes. Consequently,
Git history alone cannot promise that an arbitrarily old revision remains
deployable after registry retention runs. Kuberploy needs an explicit rollback
window and a lifecycle algorithm that cannot delete an image still required by a
different environment or an in-flight operation.

## Decision

### Registry modes

Kuberploy supports `managed` and `external` registry modes. Managed mode deploys
one tested OCI Distribution-compatible registry in `kuberploy-registry`, backed
by a persistent volume or an administrator-configured object store. It has a
stable TLS endpoint reachable by builders and Kubernetes nodes. Global
credentials are never placed in build Pods or application namespaces; the
credential broker supplies repository-scoped push and pull credentials.

External mode adopts an existing OCI-compatible registry for image and Buildx
cache push/pull. Kuberploy never requests delete permission, deletes its
manifests or blobs, applies retention, or runs garbage collection there. The
external registry operator owns lifecycle, quota and backup policy. The UI marks
those fields `Operator managed` rather than implying Kuberploy protects a last-N
window. Existing-image deployments remain in their source registry unless a
user starts an explicit copy/promotion operation into a Kuberploy-owned managed
repository.

Every application service receives a repository derived from immutable IDs, for
example `<owned-prefix>/<project-id>/<application-id>/<service-id>`. Display-name
changes therefore do not move artifacts, and one service's retention rule cannot
select another service's manifests. The P0 single-service runtime uses the stable
service ID `main`.

### Retention semantics

The platform has a default `keepLastSuccessful` value of 10 distinct image
digests. A platform administrator may set a bounded per-service override from 1
through 100. Setting 1 is allowed but the UI warns that no historical rollback
slot is guaranteed.

The latest `N` means the latest successfully built Release digests for the
stable App, not the latest tags, layers, upload timestamps or build
attempts. A multi-platform OCI index counts as one release and protects its
referenced platform manifests. Releases are ordered by their durable creation
time and ID rather than a provider upload timestamp; redeploying the same digest
does not consume another slot. The following digests are protected in addition
to that window:

- every digest selected by current indexed Git intent in any environment;
- every digest observed in an active or terminating rollout;
- every digest referenced by a pending/running build, promotion, deployment or
  rollback Operation;
- explicitly pinned releases, subject to administrator policy and audit; and
- artifacts younger than `unreferencedGracePeriod`, 24 hours by default.

This makes `N` a bounded rollback target rather than an unsafe physical hard
cap. A service can temporarily retain more than `N` images when production is on
an older release, an operation is active, or a release is pinned. Shared layers
also remain on disk while any protected manifest references them.

Successful builds older than the latest `N` that were never deployed, failed
uploads, and superseded build artifacts become eligible after the grace period.
Build cache has a separate size/age policy and never counts as a rollback image.

### Registry-backed build cache

The first source-builder release supports BuildKit's registry cache importer and
exporter. It uses `mode=max`, `image-manifest=true` and `oci-mediatypes=true`,
equivalent to the `cache-from`/`cache-to` options commonly used with
`docker/build-push-action`, but the trusted Kuberploy build agent invokes Buildx
directly.

Cache identity is scoped by immutable service ID, platform set, builder/cache
schema, App source digest and trust lane. Protected-branch and untrusted
pull-request writes never share a lane. Cache credentials cannot push release
images and runtime pull credentials cannot read cache repositories. Because
`mode=max` may contain intermediate source-derived layers, cache repositories
are treated as private build data.

An export uses a unique build candidate before a short leased alias update. In
managed mode, the starting lifecycle protects the latest successful generation
per service/platform/trust lane across App source edits, expires unused older generations after seven days and
applies an administrator byte quota. Active cache imports and exports are
protected. Cache manifests and their unreachable blobs are garbage-collected
independently from the service's last-`N` release window. External mode uses the
configured cache references but leaves every cleanup decision to the operator.

Cache is never a correctness dependency. Import/export failure is reported as a
build warning and falls back to a clean build; only failure to push and verify the
final application image is terminal. Under storage pressure, eligible caches are
reclaimed before expired release artifacts, but no cache policy may delete a
current or retained rollback image.

### Mark, delete and garbage collect

Managed-registry cleanup is a durable, idempotent Operation with a per-registry
and per-repository lease. This protocol is never executed for an external
registry:

1. Snapshot the fully indexed Git heads, current and observed deployment
   digests, nonterminal Operations, Release records, pins and registry catalog.
2. Calculate and persist a dry-run protection/deletion plan with a reason for
   every manifest.
3. Immediately before deletion, revalidate the service identity, Git projection
   freshness, current target heads, active Operations and manifest digest. If any
   authority is stale or unavailable, skip deletion and retry later.
4. Delete only unprotected manifests through the registry's digest API. Tags are
   conveniences and never the protection or release identity.
5. Let the managed registry reclaim only blobs unreachable from every remaining
   manifest. Registry-wide blob garbage collection never guesses reachability
   from Kuberploy's per-service count.
6. Record reclaimed manifests/bytes, skipped protection reasons, provider
   responses and failures in the audit timeline.

Cleanup runs on a schedule and after successful builds when storage crosses a
soft watermark. A hard storage watermark rejects new builds with an actionable
error; it never deletes protected rollback/current images to make room. An
administrator can preview cleanup and pin/unpin releases from the UI, but cannot
force-delete a currently selected or in-flight digest through the normal API.

### Rollback contract

For managed mode, the rollback picker exposes only retained and
registry-verified releases. A Git revision whose digest has expired remains
valid history but is shown as `ArtifactExpired` and cannot be presented as a
one-click rollback. Restoring it requires republishing the exact artifact or
creating a new build/release and a new Git commit. For external mode, Kuberploy
probes the selected digest but cannot promise how long the operator will retain
it; a missing digest is reported as `ArtifactMissing`.

Managed registry storage is durable application infrastructure, but the default
retention window is not an archival backup. Operators needing longer disaster
recovery must increase retention or back the registry with their normal
PVC/object-store backup policy.

## Consequences

- A small installation works without purchasing or separately administering a
  registry.
- Disk growth is bounded by an understandable release window, grace period,
  active/current references, pins and shared-layer reachability.
- Rollback availability is explicit and testable instead of being inferred from
  Git history.
- Managed mode introduces persistent storage, TLS/node trust, credential,
  capacity-alert and restore responsibilities.
- OCI Distribution 3.1.1 is the first release-locked managed-registry data
  plane. Its pinned multi-architecture image passed native image push,
  BuildKit registry-cache export/import and digest-deletion checks. Kuberploy
  owns the hardened Kubernetes templates rather than inheriting a third-party
  chart's lifecycle assumptions; the full cluster/TLS/auth/GC suite remains a
  release gate.
