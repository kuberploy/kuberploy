# Isolated source-build runtime

Kuberploy's first source-builder implementation follows the Kubernetes
restartable-init-sidecar pattern used by Actions Runner Controller: checkout
runs first, a privileged DinD daemon runs as a native sidecar, and the trusted
Buildx agent runs unprivileged. Nothing mounts the host Docker socket or a host
path.

## Closed protocol

`internal/builder` rejects unknown or oversized JSON fields and validates all
security-sensitive inputs before starting tools. A build is bound to canonical
operation, project, and service UUIDs; an exact lowercase 40-hex commit; paths
below the read-only checkout; the `linux/amd64` and/or `linux/arm64` platform
set; and named resource and egress profiles. Runtime path checks reject a
symlink in the checkout root, any intermediate component, or the final context
and Dockerfile paths.

Image and cache references are controller-owned namespaces, not arbitrary user
strings. The exact registry repository prefix, immutable project/service IDs,
cache schema, trust lane, platform set, and build-definition digest determine
the permitted repositories. Destination and cache-export tags are unique to an
operation/generation; the deployment result contains only the registry-verified
`repository@sha256:digest` and exact platform set. A late or parallel build
therefore cannot change what deployment consumes.

Plain build arguments with secret-like names are rejected. Release-push login,
cache login, source credentials, BuildKit secrets, and SSH files come from
separate read-only mounts. Their values never enter a command argument or
environment variable. The agent creates separate private push and cache Docker
configurations, uses only the private Unix socket, suppresses hostile build
output, writes a bounded result through fsync plus atomic rename, and removes
private auth and Buildx state before exit.

Buildx always creates a named `docker-container` driver with the pinned
BuildKit image. A cache-only phase uses only the cache Docker configuration,
exact registry `cache-from`, and one registry `cache-to` candidate with
`mode=max`, `ignore-error=true`, OCI media types, and an image manifest. The
final phase has no cache flags, uses only the release-push Docker configuration,
and publishes the application image with `--push`. The phases share the same
private BuildKit content store, not credentials. Missing cache imports or a
failed cache phase return `ColdBuild`/`CacheDegraded`; failure of the final
build or push is terminal.

## Kubernetes boundary

`PlanJob` produces one deterministic Job and one operation-scoped
NetworkPolicy. The Pod has:

- a checkout init container with only source credentials and a writable source
  emptyDir;
- one pinned, privileged, restartable DinD init-sidecar with private socket and
  Docker data emptyDirs;
- one unprivileged agent with the workspace read-only and distinct
  release-push, registry-cache, build-secret, and SSH mounts;
- no host namespace, host path, host socket, or ServiceAccount token;
- UID/GID and fsGroup `65532`, group-readable `0440` projections, restrictive
  container security contexts, size limits, CPU/memory/ephemeral-storage
  requests and limits, deadline, zero Job retries, and TTL;
- an exact dedicated-node label and taint toleration plus resolved, bounded
  egress CIDRs. No `0.0.0.0/0` or `::/0` egress is accepted.

The default-disabled `charts/kuberploy-builder` chart creates the isolated
namespace, tokenless Pod ServiceAccount, controller-only namespaced RBAC,
quota, default-deny NetworkPolicy, and fail-closed admission policy. It does not
schedule workloads. Enable it only after provisioning the dedicated tainted
builder node pool described in the chart README.

The controller must attach the planner's operation, generation, and spec-hash
labels to the Job, NetworkPolicy, request ConfigMap, and credential Secrets.
Recovery adopts an existing Job only when all identity fields match. After
collecting the result, cleanup must address exact names and verify those labels;
Job TTL does not remove auxiliary objects.

## Integration boundary

The durable build store, controller loop, GitHub installation-token broker,
cache-candidate promotion, result collector, and builder image workflow use
this protocol. Registry Secrets remain operator-provisioned references: the
controller and Kubernetes adapter never read their values.

Run focused verification with:

```bash
go test ./internal/builder ./cmd/kuberploy-build-agent
make builder-chart-test
```

The local Docker engine can build and inspect
`build/package/builder-agent.Dockerfile`; this is independent of Kubernetes.
