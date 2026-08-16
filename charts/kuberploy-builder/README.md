# Kuberploy isolated builder boundary

This optional chart creates the namespace security boundary used by the
ARC-style DinD build runtime. It is disabled by default and does not schedule a
builder. A controller creates one deterministic Job and one fail-closed
ValidatingAdmissionPolicy for each enabled builder installation; an optional
run-scoped NetworkPolicy can further narrow build egress.

The Pod ServiceAccount has token automount disabled and receives no RBAC. The
controller gets only namespaced Job, NetworkPolicy, request ConfigMap,
deterministic ephemeral source-Secret, and log access. Its Secret verbs are
exactly `create`, `get`, and `delete`; it cannot list, watch, update, or patch
Secrets. Admission is mandatory when the builder is enabled and constrains the
privileged DinD sidecar to the exact generated Job shape. NetworkPolicy remains
optional for environments that do not need egress narrowing. The checkout and
trusted agent remain unprivileged.

When embedded under the control-plane chart's `builder` alias, the parent passes
optional Kubernetes API exclusions plus optional source and registry CIDRs.
Empty lists generate dual-stack public rules on only HTTPS and the verified
registry port; configured API CIDRs are excluded, and no exclusion is required
when cluster API ranges are not supplied. Nonempty source ranges and exact
registry hosts optionally narrow those rules. The controller binds the
normalized result to each run-scoped policy and immutable build definition;
persisted strict definitions without an `except` field remain replayable.

`buildKitImage` is the pinned `v0.32.2` release tag or an immutable `sha256`
reference for an operator mirror. `dindImage` accepts an explicit semantic
version or immutable digest. The control plane binds both exact references into
every immutable request. Tags remain supported for local/mirror functionality;
digests are recommended when the registry is stable enough for them.

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

When embedded under the control-plane chart's `builder` alias, `buildSecret`
and `sshSecret` values are inherited here for strict release-time schema
validation. Each configured profile requires a non-empty `applicationIds`
allowlist; this standalone chart does not mount those Secrets itself.
