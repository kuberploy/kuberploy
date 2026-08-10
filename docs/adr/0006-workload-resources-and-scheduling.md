# ADR 0006: Workload resources, scheduling profiles and autoscaler compatibility

- Status: Accepted
- Date: 2026-08-09

## Context

A Kubernetes PaaS cannot treat CPU and memory as display-only settings. Resource
requests drive scheduler placement and autoscaler provisioning, while limits
control runtime enforcement. Clusters that use Karpenter may intentionally keep
a NodePool at zero nodes; the pending Pod's standard scheduling requirements are
then the input used to choose and provision compatible capacity.

Allowing arbitrary affinity or tolerations is also unsafe. A tenant could target
expensive hardware, defeat isolation, tolerate system/builder taints, create an
impossible scheduling intersection or make consolidation unexpectedly costly.
Kuberploy therefore needs expressive Pod-side controls inside an administrator-
defined policy boundary.

## Decision

### One Git-backed workload contract

The guided service UI and Advanced AppConfig YAML edit the same versioned
resource and scheduling block. Kuberploy renders standard Kubernetes Pod fields;
it does not store a hidden live-cluster override.

The service resource form exposes requests and limits for CPU, memory and
ephemeral storage. CPU and memory requests must have effective values from the
service or its selected profile. Extended resources such as GPUs are available
only through an approved profile. Validation enforces Kubernetes quantity
syntax, positive values, request-at-most-limit, per-container/platform bounds,
LimitRange defaults and known ResourceQuota headroom.

Creating a service writes explicit primary-container requests of `50m` CPU and
`100Mi` memory into AppConfig. These are durable Git values rather than UI-only
defaults. A scheduling profile may impose a higher minimum; limits remain
separate fields and may be defaulted or required by administrator policy.

### Scheduling profiles

Platform administrators own `SchedulingProfile` objects. A profile contains:

- resource defaults and minimum/maximum bounds;
- allowed or fixed node-selector and node-affinity keys/values;
- allowed topology domains and spread behavior;
- approved same-scope pod affinity/anti-affinity presets;
- allowed tolerations for permanent node/NodePool taints;
- optional PriorityClass and extended-resource choices; and
- team/project/environment scope plus an optional compatible autoscaler pool.

Developers select an authorized exact profile revision; every effective
placement field is server-derived, so they cannot add raw constraints.
Advanced YAML does not bypass the profile. `nodeName`, arbitrary `schedulerName`,
cross-namespace pod affinity, unrestricted label selectors, and tolerations for
control-plane, Kuberploy system, monitoring, registry or builder nodes are
rejected.

For the MVP, the selected profile is an exact immutable revision with spec and
assignment digests. Required node expressions and bounded weighted preferred
node terms are materialized together. Pod anti-affinity is a closed
same-application preset: an administrator chooses required or preferred
enforcement, a topology key, and an optional preferred weight, while the server
derives the sole selector as
`kuberploy.io/application=<current application UUID>`. Neither tenant callers
nor profile JSON can provide a label selector, namespace list,
`namespaceSelector`, `matchLabelKeys`, `mismatchLabelKeys`, or another
application identity. With those namespace controls absent, Kubernetes applies
the term only to the workload's own namespace. Direct-Git activation compares
every materialized field to the exact current assigned revision and rejects
substitutions.

Taints themselves are node or NodePool configuration and are never edited from
the tenant service screen. Services configure tolerations. Kuberploy does not add
tolerations for Karpenter `startupTaints`, because those are temporary NodePool
bootstrap state and do not need to be tolerated for provisioning when declared
correctly by the cluster administrator.

### Karpenter compatibility

Karpenter observation and provisioning diagnostics remain deferred and
default-off in the MVP described above; they are not required for standard
Kubernetes affinity materialization.

Karpenter integration is optional and read-only. When `karpenter.sh/v1` is
available, Kuberploy may observe NodePools and NodeClaims to help administrators
build profiles and to display provisioning diagnostics. It does not install,
create, patch or delete NodePools, NodeClasses, NodeClaims, Nodes or taints.

Preview intersects required Pod selectors/affinity, resources and tolerations
with the selected profile and fresh observed NodePool constraints. A proven
empty intersection is rejected. Unknown/stale discovery follows the
environment's validation policy and is never represented as a confirmed match.

After Argo creates a Pod for a NodePool with zero current nodes, the ordinary
Kubernetes scheduling path can mark it unschedulable and Karpenter can provision
a compatible node. Kuberploy does not need a proprietary workload object or a
special scale-from-zero switch. Hard requirements are never silently relaxed;
preferred affinities and topology choices carry a warning that they may increase
node count or cost.

### Status and audit

The deployment timeline separates namespace quota failure, impossible
constraints, waiting for capacity, observed Karpenter provisioning and ordinary
container startup failures. Saved configuration, effective profile, rendered
Pod fields, validation result and actor are auditable. Cloud-provider details and
credentials are not exposed to tenant users.

## Consequences

- Workloads can select scale-to-zero, spot/on-demand, architecture, GPU and
  isolation pools without Kuberploy becoming a cluster-infrastructure manager.
- Resource requests provide both capacity correctness and useful cost signals;
  limits remain independently visible and policy-controlled.
- Profiles prevent a flexible YAML surface from becoming a path onto privileged
  or unexpectedly expensive nodes.
- Clusters without Karpenter use the same standard PodSpec fields and simply omit
  Karpenter-specific diagnostics.
- The runtime schema, guided form, renderer, policy engine, and OpenAPI now use
  the same bounded preferred node-affinity and server-derived same-application
  pod anti-affinity contract. The enabled path still requires execution in the
  external conforming-cluster qualification before the feature-complete MVP
  gate passes.
