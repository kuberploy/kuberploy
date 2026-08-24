# Contributing to Kuberploy

Thanks for helping build a Kubernetes-native, GitOps-first PaaS. Kuberploy is
Apache-2.0 licensed and welcomes fixes, documentation, design discussion, and
carefully scoped features.

## Before you start

- Search existing issues and discussions before opening a duplicate.
- For a security vulnerability, do not open a public issue. Follow
  `SECURITY.md` once the public disclosure policy is approved and published.
- Open an issue or discussion before a large architectural change. Small bug
  fixes and documentation improvements can go directly to a pull request.
- Keep pull requests focused. Dependency bumps, refactors, and behavior changes
  should normally be separate changes.

## Development environment

Read [DEVELOPMENT.md](DEVELOPMENT.md) for repository layout, exact tool versions,
local-file policy, verification commands, and the release workflow.

The supported tool versions are pinned in `mise.toml`; dependency versions and
their review policy are documented in `DEPENDENCIES.md`.

```bash
mise install
corepack enable
pnpm --dir web install --frozen-lockfile
make check
```

An explicitly selected Docker context is the local build-cache lane. Kubernetes
integration uses an operator-supplied conforming cluster selected through an
explicit absolute `KUBECONFIG` path and exact `KUBERPLOY_TEST_CONTEXT`; tooling
must not trust ambient kubectl state. Read `LOCAL_TESTING.md` before running any
cluster-facing test.

## Pull requests

1. Add or update tests for behavior changes.
2. Keep public API, JSON Schema, Helm values schema, UI types, and examples in
   sync when a contract changes.
3. Run `make check`; include the relevant conforming-cluster result for
   cluster-facing changes and native build results for architecture-sensitive
   image changes.
4. Update an ADR when a trust boundary or durable contract changes.
5. Do not commit credentials, kubeconfigs, private keys, registry auth, local
   `.env` files, generated cluster state, or mutable image tags.
6. Use readable semantic versions for operator-facing releases. Use immutable
   digests only for content-integrity and OCI immutability checks.

Commit signing is encouraged. A contributor certifies that they are entitled
to submit their work under Apache-2.0; Kuberploy does not currently require a
separate contributor license agreement.

## Design principles

- Git is authoritative for non-secret desired state; Argo CD performs normal
  workload reconciliation.
- Plaintext or base64-only application secrets never enter Git.
- Tenant isolation and authorization are enforced server-side, not only in the
  UI.
- Build Pods never mount the host Docker socket. DinD is explicitly privileged;
  default single-node scheduling accepts node-level risk, while optional node
  isolation requires the exact dedicated label and taint.
- Public inputs are bounded, validated, and fail closed.
- A control-plane Helm upgrade must not own or mutate tenant workloads.

## Review and release

Maintainers review correctness, tests, compatibility, security boundaries, and
operational impact. Merging does not guarantee immediate release. Releases are
cut from reviewed tags by the repository workflow, published with immutable
digests, and announced through immutable GitHub Releases.
