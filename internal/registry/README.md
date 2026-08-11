# Registry lifecycle foundation

This package plans artifact retention and implements the bounded managed
Distribution manifest-delete/execution contract. Its production worker runtime
is an explicit, default-off binding to one operator-owned Distribution target;
the interfaces remain injectable so tests never need a destructive live GC.

## Ownership boundary

- `managed` targets permit deterministic preview, manifest deletion, and blob
  garbage-collection planning.
- `external` targets support image and Buildx cache push/pull metadata, but
  lifecycle planning returns `ErrRegistryExternalLifecycle`. There is no delete
  credential field in the target model or schema, and PostgreSQL rejects an
  external cleanup plan even if an application bug tries to insert one.
- Registry target mode is immutable. Moving artifacts between ownership modes
  must be an explicit future migration/promotion operation.

## Complete observations

Planning requires a complete registry repository inventory, a complete catalog
for every enumerated repository, and complete Git-intent, runtime, and active-
operation checkpoints. Exact snapshot digests bind catalog revisions to their
manifest/index/blob graphs and authority revisions to their reference sets.
Missing, duplicate, stale, dangling, or cyclic input fails closed.

The snapshot token covers the full graph and all protection roots. Claiming a
preview rechecks that token. Each destructive item then rechecks the authority
token while holding durable registry/repository leases. Authorization changes
the item atomically from `planned` to `deleting`, preventing two workers from
starting the same provider operation.

## Retention and cache

Managed release roots protect the latest distinct `N` successful digests
(`N=10`, bounded 1..100), current Git intent, observed running workloads, pins,
active operations, and the minimum safety age. Protection traverses OCI index
children and shared layers.

Build cache identity includes service, platform set, trust lane, cache schema,
and build-definition hash. Managed defaults retain two generations, expire a
root after seven unused days, and enforce a 10 GiB per-service quota. Multiple
generation rows that resolve to one repository/digest count once for physical
quota; any active or retained alias protects the shared root. External cache
lifecycle remains operator-owned.

## Distribution execution boundary

`delete-manifest` items map to the OCI Distribution digest-delete API. Blob
items are different: `garbage-collect-blob` means that the planner proved a
digest unreachable across a complete registry inventory *after all planned
manifest removals*. It is not an online per-blob delete instruction.

`DistributionClient` is fixed to one managed target origin and repository
prefix. Its platform-owned `ExpectedOrigin` must exactly match the target
endpoint, so changing target metadata cannot redirect an internal credential.
HTTPS is the default; plain HTTP requires an explicit platform option for a
cluster-local endpoint. It obtains a fresh Basic or Bearer authorization value
from `DistributionCredentialSource` for each request and erases the caller-owned
bytes after constructing the request. It never follows redirects, uses
environment proxies, accepts a per-item URL, or retains provider bodies or
redirect locations.

For each manifest item it performs:

1. `HEAD /v2/<repository>/manifests/<digest>` and requires an exact
   `Docker-Content-Digest` match;
2. authenticated digest `DELETE`, requiring `202 Accepted`;
3. a final `HEAD` requiring `404 Not Found`.

An initial or racing `404` is the idempotent already-absent outcome. Repository
grammar, target prefix, digest, timeouts, response bodies, response headers, and
safe error metadata are bounded. The behavior follows the
[OCI Distribution content-management contract](https://github.com/opencontainers/distribution-spec/blob/main/spec.md#deleting-manifests).

`CleanupExecutor` implements the existing `Service` claim/renew/authorize/
record/finish contract. It finishes and durably records every manifest deletion
before authorizing blob items. Blob processing then follows one state machine:

1. acquire a registry-wide exclusive maintenance lease;
2. enter a tested read-only or stopped mode;
3. capture a fresh, complete registry-wide inventory, authority, and
   reachability checkpoint in that mode;
4. require an explicit row for every planned digest (including a proven
   already-absent row) and reject any reachable candidate;
5. invoke one provider GC sweep under a deterministic idempotency key;
6. validate its durable receipt, restore service, release maintenance, and only
   then record blob items.

The adapter contract permits a replay of the same execution key to return the
stored receipt without invoking a second provider sweep only when the new
checkpoint explicitly proves every candidate is now absent. That handles a
worker crash after GC or a partial durable item-recording failure without
mistaking a reintroduced digest for an earlier deletion. There is no online
per-blob HTTP delete method. This matches Distribution's documented
[stop-the-world mark/sweep requirement](https://distribution.github.io/distribution/about/garbage-collection/).

## Production Kubernetes runtime

The stable schema persists observation cursors, fenced
maintenance executions, checkpoint state, and immutable one-sweep receipts.
Expired observation, cleanup, and maintenance leases are reclaimable; epochs
prevent a stale worker from publishing, deleting, completing a receipt, or
restoring another worker's session.

The managed-registry readiness tables separately fence public
readiness. A worker acquires that lease only after its projected credential is
readable, the exact managed Deployment/ConfigMap/PVC pass a read-only
inspection, and the complete observer and cleanup-executor dependency graph
passes runtime validation. Its heartbeat is
bound to the exact managed target, a digest of every operator-owned runtime
setting, the digest-pinned helper image, and a versioned worker contract. The
projected credential is the operator-only lifecycle/maintenance credential; it
is not stored in `registry_targets` and is never equated with the target's
build-push, cache, or runtime-pull credential references. The
API revalidates current target metadata. `registry` reports the local,
credential-free target/policy/inventory management surface, so external-target
metadata remains usable when the built-in registry is disabled.
`managedRegistry`, managed inventory responses, and cleanup preview/execution
require a fresh matching managed-worker lease; stale or mismatched state fails
closed for those feature paths without removing the core API from Service
endpoints. `/readyz` covers PostgreSQL and configured Valkey only. Target
configuration and historical cleanup-plan reads remain local database
operations so administrators can configure or diagnose a stopped runtime
without granting the API registry credentials. External targets never enter
the deletion, maintenance, or garbage-collection path.

The production adapter admits only the exact Helm-managed `Recreate`
Deployment, immutable registry ConfigMap, and bound filesystem PVC named in
operator configuration. It scales that Deployment to zero by UID/resource
version, waits for both zero status replicas and no selected Pods, then creates
or adopts deterministic helper Jobs by their complete input/spec digest. Jobs
are non-privileged, run as the registry filesystem UID, have no ServiceAccount
token, and are selected by a deny-all ingress/egress NetworkPolicy. Checkpoint
Jobs mount the PVC read-only; the single GC Job mounts it read-write and invokes
the fixed Distribution binary and fixed ConfigMap path. Job result JSON is
strictly parsed from a `File` termination message, bounded to 4 KiB, and the
protocol permits at most 16 explicit candidate digests.

Before GC, the filesystem helper walks the complete Distribution v2 tree,
rejects symlinks and non-regular files, verifies every manifest body by digest,
and computes physical manifest/blob reachability. The controller combines that
fresh stopped-registry proof with fresh database authority observations. A
completed deterministic Job may be recovered after a worker crash, but one
immutable database receipt is still required before blob items can be recorded.
The Deployment must become fully ready again before the maintenance lease is
released.

`ProjectedCredentialSource` is the current concrete credential profile. It
maps exactly one configured target ID to only
`/var/run/secrets/kuberploy/managed-registry/{username,password}`, reads bounded
values for each request, and clears request-local buffers. Tenant data cannot
choose paths. Private CAs and bearer-challenge/token-exchange brokers remain
deliberate provider-plugin gaps; the injected HTTP transport and credential
interfaces are their closed extension points. External targets can use the
read-only `DistributionObserver`, but production cleanup construction rejects
them and no external deletion/GC adapter exists.
