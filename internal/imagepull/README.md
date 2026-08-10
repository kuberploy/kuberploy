# Runtime registry pulls

This package is the private-image pull boundary. It converts an operator-owned,
projected Docker config into a revisioned `kubernetes.io/dockerconfigjson`
Secret in an exact environment namespace. PostgreSQL stores only target,
profile, namespace, Secret identity, lease and readiness metadata. It never
stores credential bytes, base64 values, or hashes of credential bytes.

## Closed profile and artifact contract

- Runtime configuration is default-off and accepts at most 32 canonically
  ordered profiles. A profile binds one registry target UUID, canonical
  registry `host[:port]`, opaque `pullCredentialRef`, positive operator
  revision, and one projected source Secret/key.
- Source material is read only from
  `/var/run/secrets/kuberploy/registry-pulls/<profile>/dockerconfigjson`.
  Kubelet's in-root atomic symlinks are supported; root symlinks, escapes,
  non-regular files and values above 64 KiB are rejected.
- Docker config is the closed single-registry `auths` form. Duplicate keys,
  helper stores, extra registries, unknown fields, ambiguous credential modes,
  non-canonical basic-auth encoding, control characters and trailing documents
  fail closed. Every configured source is read and validated before the worker
  publishes its first readiness observation.
- Destination names are derived from the immutable globally unique environment
  namespace, registry target ID and profile revision. This lets the chart
  predeclare exact `resourceNames` RBAC. The Kubernetes adapter can only get or create that exact
  immutable Secret. It rejects data, label, annotation, owner, finalizer,
  deletion, UID or resource-version drift and clears every owned credential
  buffer after use.
- Rotation creates a new revisioned Secret and deactivates the prior database
  artifact without deleting it. Retaining the old immutable Secret avoids a
  pull outage for an old ReplicaSet or an in-flight rollout. A later bounded
  garbage collector may delete old artifacts only after desired Git, observed
  workloads and retained rollback releases all prove absence.

## Durability and readiness

Migration `025_runtime_registry_pulls` provides one active artifact per
environment and target, epoch-fenced work claims with heartbeats, safe
retry/permanent failure state, and an exact config/contract worker readiness
lease. Git projection policy can call `EnsureArtifactTx` inside its serializable
generation activation transaction. Argo eligibility must require the resulting
artifact to be active, ready and freshly observed before it materializes an
Application for an AppConfig that references the pull profile.

Infrastructure failures while reading projected credentials or calling the
Kubernetes API persist bounded retry state and stop the worker so its readiness
lease expires. Semantic credential or live-object mutation failures are
terminal for that profile revision; operators rotate to a new revision after
correcting the source.

## Production integration status

The PostgreSQL/in-cluster worker, exact API readiness probe, main-chart profile
schema, worker-only credential projection, namespace-scoped RBAC, admission
policy, locked AppConfig/runtime-chart metadata, server-side repository
resolution, and direct-Git projection policy are wired and tested. Argo command
planning also revalidates every active indexed AppConfig and blocks a private
application unless its exact artifact is active, ready, fresh, and matches the
approved profile.

The protected Argo desired-state worker, root Application, and per-binding
repository-credential observations are now production-wired behind the same
exact readiness fence. The public private-pull feature flag can become true
only when that fence and the exact pull-materialization worker receipt are both
fresh; disabled, stale, or mismatched configurations remain false. Public
images continue to render without `imagePullSecrets`.
