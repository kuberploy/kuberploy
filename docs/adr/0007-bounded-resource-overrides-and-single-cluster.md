# ADR 0007: Bounded resource overrides and single-cluster scope

- Status: Accepted
- Date: 2026-08-23

## Context

Provider integrations regularly need Kubernetes fields that do not justify a
dedicated product control. Examples include
`eks.amazonaws.com/role-arn` on a ServiceAccount and
`external-dns.alpha.kubernetes.io/cloudflare-proxied` on an Ingress. Requiring
a new release for each annotation makes the PaaS less useful, while arbitrary
additional manifests would bypass App ownership and preview boundaries.

Kuberploy also has no multi-cluster product requirement. A selectable cluster
catalog, remote kubeconfig store and placement scheduler would add durable API,
credential and reconciliation complexity without helping a single-cluster
installation.

## Decision

Non-Helm Apps keep one Git-backed `AppConfig`. Guided controls write ordinary
runtime and route fields. A collapsed expert section edits four optional
partial Kubernetes resources under `spec.overrides`: `deployment`, `service`,
`ingress` and `serviceAccount`.

The pinned runtime chart constructs the Guided resource, merges the matching
override second and then restores platform-owned boundaries. Therefore the
expert value wins for an ordinary matching field, while these fields remain
protected:

- apiVersion, kind, name and namespace;
- Kuberploy ownership identity and generated selectors;
- primary App image, container identity and ServiceAccount selection;
- host namespace sharing, HostPath, host ports, added Linux capabilities and
  privileged or host-equivalent container settings.

Additional container images must use exact digest references. A Deployment
override is rejected when the App renders a StatefulSet. Overrides pass the
same JSON Schema, semantic validation, pinned Helm render, policy and diff flow
as Guided edits. Helm-source Apps continue to use chart `values.yaml`; this
resource-override contract does not rewrite third-party charts.

Each Kuberploy installation permanently manages only the Kubernetes cluster in
which it runs. Public API and UI surfaces do not expose cluster registration,
selection, placement, remote kubeconfigs or cross-cluster credentials. The
database and runtime configuration contain no cluster identity. One singleton
platform Git binding owns the fixed `platform/` protected root. Independent
installations use separate platform repositories or refs rather than sharing
one writable platform root.

## Consequences

- Provider-specific annotations and ordinary Kubernetes fields work without
  expanding the Guided form.
- Preview and Git remain the only desired-state path; there is no hidden live
  override.
- The product avoids a second cluster inventory, credential store and placement
  control plane.
- Removing cluster identity simplifies APIs, Helm values, reconciliation guards
  and database foreign keys without weakening the single writable authority.
