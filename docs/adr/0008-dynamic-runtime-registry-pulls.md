# ADR 0008: Dynamic managed-Environment registry pulls

- Status: Accepted
- Date: 2026-08-26

## Context

Source builds push immutable images to a private managed registry. Earlier
runtime-pull configuration accepted only exact namespaces rendered during a
Helm upgrade. Environments are created later through the product, so a fresh
single-VM installation could build and push an image but could not deploy it
without another operator Helm upgrade.

## Decision

Runtime registry-pull configuration accepts either exact namespaces or bounded
managed namespace prefixes. Starter installations use the existing `kp-`
Environment namespace contract.

The pull Secret name is derived from registry target ID and profile revision;
the Kubernetes namespace remains its isolation scope. This gives every managed
Environment the same finite set of allowed Secret names. For prefix mode the
control-plane chart grants the exact worker ServiceAccount:

- `get` only on those finite profile-derived Secret names;
- `create` on Secrets, because Kubernetes RBAC cannot restrict create by name.

A fail-closed admission policy selects only foundation-labeled managed
Environment namespaces. It rejects every non-reserved Secret creation by the
worker and requires the exact immutable pull-Secret name, labels, annotation,
type, and bounded data shape. Updates and deletions of reserved pull Secrets
remain denied. The configured Argo namespace is exempt because its existing
repository-credential admission policy independently constrains the shared
worker. No list, watch, update, patch, delete, or arbitrary Secret-read
permission is granted.

## Consequences

- A newly created Environment can deploy a private image without a Helm
  upgrade.
- Exact namespace mode remains available for operator-managed namespaces.
- Rotating a profile revision creates a new immutable Secret name in each used
  namespace.
- The worker receives cluster-scoped create authority only inside the
  admission-enforced managed namespace boundary; arbitrary Secret creation and
  arbitrary Secret reads remain denied.
