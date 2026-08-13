# ADR 0003: Public releases and installer-owned upgrades

Status: accepted, replacing the pre-stable child self-upgrade design.

## Decision

The public `kuberploy/kuberploy` GitHub repository is the canonical release
channel. Kuberploy may fetch and display an immutable published release only
after validating its exact tag, manifest bytes, source commit, OCI artifacts,
compatibility window, and digests.

Installation, upgrade, and rollback authority belongs to the
`kuberploy-installer` Helm release. Operators run Helm with the reviewed values
and cluster credentials they already own. Kuberploy does not store or mint a
reusable cluster-wide Helm credential.

Each installer release contains a canonical enabled-Application inventory. A
lifecycle reconciler runs on install, upgrade, and rollback. A later hook may
read only the exact enabled Argo Application names and succeeds only when every
entry reports:

- the requested component chart revision;
- the same observed sync revision;
- `Synced`; and
- `Healthy`.

The immutable inventory and exact-name Role prevent an enabled child from being
silently omitted from the health decision.

## Rejected design

The control-plane API and worker must not execute `helm upgrade` or
`helm rollback` against the Argo-owned control-plane child:

- an Argo CD Helm source renders Kubernetes objects; it does not create Helm
  release storage for the child;
- automated Argo self-heal would revert an imperative child mutation;
- safely upgrading the installer itself requires operator-owned cluster-wide
  authority that the application must not retain.

Therefore the mutation routes are not registered or advertised. The platform
page is read-only release inspection plus historical status. Rollback uses
`helm rollback kuberploy-installer REVISION` with the same namespace and values
authority as installation, and the same exact Argo lifecycle gate runs after
rollback.

## Consequences

- Tenant workloads remain owned by environment Git and Argo.
- Database migrations remain expand/contract compatible and run through the
  installer-selected control-plane chart.
- A Helm success means every enabled platform Application reached the exact
  requested revision and healthy state, not merely that templates rendered.
- Release candidates can be qualified by installing exact explicit RC chart
  versions; the public in-product release feed remains stable-only.
