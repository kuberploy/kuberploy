# Kuberploy Valkey

This independent chart either installs the bounded single-instance managed
Valkey profile or records adoption of an operator-managed compatible endpoint.
Install it in the fixed `kuberploy-valkey` namespace. Managed mode is disabled
by default and requires an existing Secret containing independent
an authenticated health-only `default` user plus independent `api-cache`,
`api-limiter`, `outbox-publisher`, `worker-consumer`, and `argocd` ACL users'
passwords; the chart never accepts an inline password.

The managed profile is ClusterIP-only, has default-deny ingress and egress,
accepts TCP clients only from `kuberploy-system` and `argocd`, uses a
digest-pinned Valkey image, runs restricted without a service-account token,
retains a RWO PVC, enforces `noeviction`, and enables AOF. Argo CD is isolated
in database 1 under its own ACL identity; its password is separately delivered
to the `argocd` namespace so no credential is stored in Git. It is intentionally
a small standalone profile because PostgreSQL, Git, and external providers
remain authoritative; Valkey data loss may increase recovery work but cannot
lose an accepted command. The publisher may only `GET`/`SET` the exact
`kp:v1:work:*` keyspace in addition to `XADD`: it maintains an opaque dataset
sentinel there. A missing or changed sentinel makes PostgreSQL reopen only
published outbox rows whose operations are still queued/running, so a replaced
dataset is reconstructed without granting the publisher consume or admin
commands.

```sh
helm dependency build charts/kuberploy-valkey
helm upgrade --install kuberploy-valkey charts/kuberploy-valkey \
  --namespace kuberploy-valkey --create-namespace \
  -f charts/kuberploy-valkey/testdata/managed-values.yaml
```

Create `kuberploy-valkey-auth` in that namespace out of band with strong
`health-password`, `api-cache-password`, `api-limiter-password`, `outbox-publisher-password`,
`worker-consumer-password`, and `argocd-password` keys. Deliver the four
Kuberploy identities through the protected connection Secret expected by the
control-plane runtime and the last through the Argo wrapper's fixed
external-Valkey Secret. Cache access cannot publish work, the outbox publisher
cannot consume it, and the worker consumer cannot mutate cache or abuse-limit
state. External or adopted mode installs only immutable profile metadata and
never owns the remote Valkey lifecycle.
