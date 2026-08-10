# Kuberploy managed OCI registry chart

This optional chart deploys one persistent OCI Distribution 3.1.1 replica for
Kuberploy's managed-registry mode. The image is pinned to its multi-platform OCI
index digest. The chart creates only a ClusterIP Service; Traefik and
cert-manager own edge routing and TLS outside this chart.

The default `enabled: false` renders no Kubernetes resources. An enabled
production render is fail-closed and requires:

```yaml
enabled: true
auth:
  mode: htpasswd
  existingSecret: registry-auth
  realm: kuberploy-registry
  secretRevision: initial
networkPolicy:
  enabled: true
  allowedNamespaces:
    - kuberploy
    - kuberploy-build
```

The existing Secret is operator/controller-created and must contain:

- `htpasswd`: bcrypt-formatted htpasswd data;
- `httpSecret`: a high-entropy Distribution HTTP secret shared across restarts.

The chart never generates a user, password, htpasswd file, or Secret. If the
Secret or either key is absent, Kubernetes prevents the registry Pod from
starting. Rotate the Secret out of band and change the non-secret
`auth.secretRevision` value to force a controlled `Recreate` rollout.

`auth.mode: testOnlyUnauthenticated` exists solely for isolated disposable
integration tests. It requires an empty `auth.existingSecret`, retains the
ClusterIP-only Service and NetworkPolicy, annotates the workload with a security
warning, and emits a Helm note.

The registry configuration uses the Distribution 3 path
`/etc/distribution/config.yml`, stores content under `/var/lib/registry`, and
mounts htpasswd at `/auth/htpasswd`. Manifest deletion by digest is enabled with
`storage.delete.enabled: true`.

## Storage and upgrades

The chart creates a PVC unless `persistence.existingClaim` is set. Size,
StorageClass, access mode, and the default Helm keep policy are configurable.
The single-replica Deployment uses `Recreate`, so two registry processes never
mount and write the filesystem backend during an upgrade.

## Garbage collection boundary

This chart deliberately renders no automatic garbage-collection Job or
CronJob. Distribution garbage collection is stop-the-world: Kuberploy's
lifecycle controller must block new writes, make the registry read-only or
fully stop it, verify that no registry process is using the PVC, run exactly one
offline GC operation, and restore service only after it finishes. Manifest
retention/protection decisions occur before that global reachability pass.

## Verification

`make registry-chart-test` runs the disabled/authenticated/test-only render,
schema, security, storage, and NetworkPolicy assertions. The opt-in
`make registry-cache-smoke` additionally proves image push, BuildKit
`mode=max` registry cache export/import and digest deletion against the local
Docker context selected by `KUBERPLOY_TEST_DOCKER_CONTEXT`; it also requires a
unique `KUBERPLOY_DOCKER_RUN_ID` and cleans only resources owned by that run.
`make registry-kubernetes-smoke` uses the explicit conforming-cluster inputs
documented in `LOCAL_TESTING.md`, creates synthetic bcrypt credentials outside
Helm, verifies unauthenticated rejection plus authenticated `/v2/` access and a
bound PVC, and deletes its exactly owned namespace.
