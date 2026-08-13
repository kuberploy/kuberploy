# ADR 0006: Workload resources, direct scheduling and autoscaler compatibility

- Status: Accepted
- Date: 2026-08-09

## Context

A Kubernetes PaaS cannot treat CPU and memory as display-only settings. Resource
requests drive scheduler placement and autoscaler provisioning, while limits
control runtime enforcement. Clusters that use Karpenter may intentionally keep
a NodePool at zero nodes; the pending Pod's standard scheduling requirements are
then the input used to choose and provision compatible capacity.

Scheduling fields also need a closed application boundary. A caller must not
target another application's Pods, tolerate system or control-plane taints, or
place application workloads on Kuberploy's privileged builder nodes. Kuberploy
therefore exposes a bounded subset of the standard Pod scheduling fields and
validates them when the deployment is created, previewed, saved, projected and
rendered.

## Decision

### One Git-backed workload contract

The guided service UI and Advanced AppConfig YAML edit the same versioned
resource and scheduling block. Kuberploy renders standard Kubernetes Pod fields;
it does not store a hidden live-cluster override.

The service resource form exposes requests and limits for CPU, memory and
ephemeral storage. CPU and memory requests are explicit service configuration.
Validation enforces Kubernetes quantity syntax, positive values,
request-at-most-limit, per-container/platform bounds, LimitRange defaults and
known ResourceQuota headroom.

Creating a service writes explicit primary-container requests of `50m` CPU and
`100Mi` memory into AppConfig. These are durable Git values rather than UI-only
defaults. Limits remain separate fields and may be defaulted or required by
cluster policy.

### Direct per-application scheduling

Each deployment stores its scheduling fields directly in the application's
`AppConfig`. Guided controls and Advanced YAML expose the same bounded fields:

- node selectors and required or preferred node affinity;
- pod affinity and anti-affinity terms scoped to the current application;
- topology-spread constraints scoped to the current application;
- explicit tolerations and an optional priority class; and
- resource requests, limits, probes, replicas and rollout strategy.

Advanced YAML does not bypass validation. `nodeName`, arbitrary
`schedulerName`, namespace selectors, selector expressions for Pod
relationships, and tolerations for control-plane or Kuberploy-owned nodes are
not part of the contract. Reserved `kuberploy.io/*` and control-plane placement
keys are rejected.

Pod affinity, pod anti-affinity and topology-spread selectors must contain only
`kuberploy.io/application=<current application UUID>`. Another application ID,
an additional label, a selector expression, a namespace list,
`namespaceSelector`, `matchLabelKeys` or `mismatchLabelKeys` is rejected. With
namespace controls absent, Kubernetes applies affinity terms only to the
workload's namespace. Direct-Git activation validates these exact selectors
again before projection, so changing Git cannot substitute another workload
identity.

Taints themselves are node or NodePool configuration and are never edited from
the service screen. Services configure tolerations. Kuberploy does not add
tolerations for Karpenter `startupTaints`, because those are temporary NodePool
bootstrap state and do not need to be tolerated for provisioning when declared
correctly by the cluster administrator.

### Karpenter compatibility

Karpenter observation and provisioning diagnostics remain deferred and
default-off in the MVP described above; they are not required for standard
Kubernetes scheduling fields.

Karpenter integration is optional and read-only. When `karpenter.sh/v1` is
available, Kuberploy may observe NodePools and NodeClaims to display
provisioning diagnostics. It does not install,
create, patch or delete NodePools, NodeClasses, NodeClaims, Nodes or taints.

Preview may intersect required Pod selectors, affinity, resources and
tolerations with fresh observed NodePool constraints. A proven empty
intersection is rejected. Unknown or stale discovery follows the
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
container startup failures. Saved configuration, rendered Pod fields,
validation result and actor are auditable. Cloud-provider details and
credentials are not exposed to tenant users.

## Consequences

- Workloads can select scale-to-zero, spot/on-demand, architecture, GPU and
  isolation pools without Kuberploy becoming a cluster-infrastructure manager.
- Resource requests provide both capacity correctness and useful cost signals;
  limits remain independently visible and policy-controlled.
- Closed validation prevents the YAML surface from becoming a path onto
  privileged Kuberploy or control-plane nodes.
- Clusters without Karpenter use the same standard PodSpec fields and simply omit
  Karpenter-specific diagnostics.
- The runtime schema, guided form, renderer, policy engine and OpenAPI use the
  same bounded scheduling contract with exact same-application selectors. The
  enabled path still requires execution in the
  external conforming-cluster qualification before the feature-complete MVP
  gate passes.
