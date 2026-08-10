# Kuberploy isolated builder boundary

This optional chart creates the namespace security boundary used by the
ARC-style DinD build runtime. It is disabled by default and does not schedule a
builder. A controller creates one deterministic Job and one run-scoped
NetworkPolicy for each build operation.

The Pod ServiceAccount has token automount disabled and receives no RBAC. The
controller gets only namespaced Job, NetworkPolicy, request ConfigMap,
deterministic ephemeral source-Secret, and log access. Its Secret verbs are
exactly `create`, `get`, and `delete`; it cannot list, watch, update, or patch
Secrets. A fail-closed ValidatingAdmissionPolicy permits the single pinned,
privileged, restartable DinD init-sidecar while requiring the checkout and
trusted agent containers to stay unprivileged. Default-deny networking remains
in force until the controller supplies the operation's resolved, bounded egress
policy.

When embedded under the control-plane chart's `builder` alias, the parent also
passes `networkPolicy.sourceEgressCIDRs` and `registryEgressCIDRs`. This chart
validates them as exact IPv4 `/32` or IPv6 `/128` hosts but does not render a
broad static allow rule; the controller binds the resolved hosts to each
run-scoped policy.

Before enabling the chart, dedicate a node pool with both:

```text
label: kuberploy.io/node-class=dind-builder
taint: kuberploy.io/dind-builder=true:NoSchedule
```

Do not enable or test a real build on a general-purpose node. The production
controller, durable operation store, and GitHub App installation-token broker
are composed by the Kuberploy API/worker; this boundary chart intentionally
does not duplicate or deploy those control-plane processes. The trusted agent
promotes an exported cache candidate while its mounted registry credential is
still available and publishes a typed result through the exact
`/result/result.json` termination-message file; results exceeding 4 KiB are
rejected before publication.

The controller must put the operation, generation, and deterministic spec-hash
labels emitted by the Job planner on the request ConfigMap, credential Secrets,
Job, and run NetworkPolicy. Recovery may adopt an existing Job only when all
three values match. After collecting the atomic result, cleanup must address
those exact names and re-check all ownership labels before deleting them. Job
TTL is only a fallback for the Job itself; it does not clean the auxiliary
ConfigMap, Secrets, or NetworkPolicy.

The admission boundary also protects the static `default-deny` policy from
controller deletion; only a `system:masters` cluster administrator may remove
it. Plan chart removal and namespace teardown with that break-glass authority.
