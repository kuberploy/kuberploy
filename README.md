# Kuberploy

Kuberploy is a self-hosted, Kubernetes-native PaaS with a simple application
workflow and a strict GitOps deployment model. It is currently a pre-release
MVP implementation, not a production release.

The repository contains the security and durability foundations for users,
teams and scoped projects; verified GitHub App setup and sharing;
webhook-triggered, multi-platform DinD builds with registry-backed Buildx
cache; durable image-only push auto-deploy policies with immutable revision and
run history in the Source Builds UI; managed or external OCI registries with
managed-only bounded retention; immutable-image application configuration;
human-managed Git-backed project/environment VariableSets; resource,
scheduling, route and typed Traefik middleware editors; server-derived
`sslip.io` convenience hostnames; write-only strict-Sealed runtime-secret and
custom-certificate lifecycle with metadata-only reads;
scoped metrics and Kubernetes log/event views; a public OpenAPI/agent contract;
and namespaced control-plane self-upgrades. Accepted commands, leases,
idempotency records and recovery state are durable in PostgreSQL; Valkey is a
required transport, limiter and cache dependency, never desired-state
authority.

Production enablement remains deliberately fail-closed. Authoritative
AppConfig Git write/index, strict runtime-secret delivery, private-image pull
materialization, managed-registry lifecycle, edge observation, protected Argo
desired-state publication, repository credentials and branch/ruleset
attestation are wired behind exact runtime identities. Approved external
Helm/OCI delivery uses its two protected Git publication phases, and ordinary
deployment rollback creates a new Git intent for an eligible prior immutable
release, using the environment's immutable direct-or-pull-request publication
policy. An HMAC-verified GitHub push durably wakes exact matching projection
bindings while safety polling repairs missed deliveries. Successful verified
build releases can enter the canonical deployment path only through an enabled,
revisioned auto-deploy policy and a freshly authorized project service account;
AppConfig or inherited VariableSet drift pauses image automation. Mutable image
references are resolved to digests before save; custom-certificate observation,
reusable middleware profiles and server-derived `sslip.io` endpoint selection
are also implemented. The running API advertises any of these default-off
features only while its real service and required fresh worker/controller
observations are available.

These completed code paths are no longer the release blockers previously
listed here. For the implemented paths, the remaining external release proof is
the full P0 qualification on an operator-selected, non-production conforming
cluster. No cluster identity or credential is stored in this repository, and no
live qualification can be claimed until the operator supplies the exact
`KUBECONFIG`, `KUBERPLOY_TEST_CONTEXT`, `KUBERPLOY_TEST_SERVER`, and
`KUBERPLOY_E2E_RUN_ID`.
The generic harness provides the read-only preflight, run-scoped smoke test,
and a fail-closed full-qualification orchestrator. The first two checks do not
by themselves satisfy the enabled-stack Git/Argo/Traefik/build/rollback matrix.
The full orchestrator owns its stage implementations and accepts no executable
operator driver. It requires a strict declarative scenario containing exact
external identities, field values and endpoints plus mode-0600 credentials;
repository code initiates the supported product workflows, collects evidence,
and enforces the inventory/cleanup or disposable-cluster boundary. See
[LOCAL_TESTING.md](LOCAL_TESTING.md) and the
[qualification scenario contract](scripts/kubernetes/test/e2e/README.md).

## Repository layout

```text
api/                    OpenAPI 3.2 contract
cmd/                    Go API and worker entry points
internal/               Control-plane implementation
migrations/             PostgreSQL migrations
schema/                 AppConfig and related schemas
web/                    React/Vite UI
charts/kuberploy/        Control-plane Helm chart
charts/kuberploy-installer Safe single-invocation Argo/bootstrap installer chart
charts/kuberploy-runtime Application runtime Helm chart
deploy/                 GitOps and local integration assets
scripts/                Reproducible development/e2e commands
test/e2e/               End-to-end fixtures and assertions
```

## Design contracts

- [Architecture](ARCHITECTURE.md)
- [Dependency baseline](DEPENDENCIES.md)
- [Development and Kubernetes integration contract](LOCAL_TESTING.md)

## Development

Tool versions are pinned in `mise.toml`. local Docker runtime is used as the explicit local
Docker/Buildx engine for development builds and registry-cache checks, not as
the Kubernetes integration cluster. Kubernetes tests use an operator-supplied
conforming cluster and require all four exact inputs shown below; no cluster
identity is committed to this repository.

```bash
mise install
make check

# Required only when invoking the Kubernetes integration harness:
export KUBECONFIG=/absolute/path/to/test-kubeconfig
export KUBERPLOY_TEST_CONTEXT=<exact-test-context>
export KUBERPLOY_TEST_SERVER=https://api.test.example:6443
export KUBERPLOY_E2E_RUN_ID=<unique-run-id>
make kubernetes-preflight
make kubernetes-smoke
```

The target command path is:

```text
existing image digest or verified source build
  -> PostgreSQL operation and outbox
  -> Valkey worker signal
  -> AppConfig Git commit
  -> Argo CD sync
  -> Traefik route on the selected test cluster
```

The existing `scripts/local-docker-runtime/` Kubernetes helpers are legacy, local Docker runtime-only
walking-slice tooling; they are not the environment-neutral integration harness.
See `LOCAL_TESTING.md` before running any cluster-facing command.

## Security posture

- Git is authoritative for non-secret desired state.
- Argo CD is the normal workload writer.
- Plaintext secrets never enter Git, Valkey, logs or ordinary database columns.
- Mutable image tags are resolved before deployment; Git stores an immutable digest.
- The DinD builder is isolated as a privileged builder-node trust boundary and never mounts the host Docker socket; its capability stays off unless the exact worker and builder boundary report healthy.
- The UI, CLI and agents use the same documented API and authorization checks.

## License

Apache License 2.0. See [LICENSE](LICENSE).
