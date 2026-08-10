# Environment namespace foundation

This package owns the cluster-level, protected-Git intent that makes one
Kuberploy environment namespace safe to schedule into. It does not call the
Kubernetes API and does not expose a generic Git writer or arbitrary manifest
surface.

## Authority and lifecycle

`PostgresStore.EnsureIntent` accepts an intent ID, an environment ID, and one
operator-owned bounded profile. Inside a serializable transaction it derives
the project ID, namespace, Argo project, and the exact configured ready
platform Git binding for the configured cluster. The binding's exact protected
ref, verified head, and projection
generation are frozen into an immutable intent. Replaying the same intent ID
returns that snapshot only when the durable binding ID and complete profile
still match, even after the branch advances.

Workers claim due intents using an owner and monotonically increasing lease
epoch. Claims are exclusive per platform binding, so two workers cannot freeze
the same branch head for different foundation writes. Heartbeats compare the
complete owner/epoch/expiry fence. Expired work
can be recovered by another worker, while the stale worker cannot finalize a
retry or publication receipt. Permanent failures and exhausted retries close
the intent; a new operator profile supersedes the old active intent without
deleting its immutable publication receipt.

The only outbound mutation contract is `ProtectedPublisher.Publish`. The
concrete adapter re-resolves the provider head, proves the immutable planned
head is its ancestor, and persists that verified descendant as the fenced write
base before pushing. A retry recovers only the exact operation and authority
trailers, direct parent, path, and bytes. It uses the shared hardened mirror,
credential broker, and normal fast-forward writer; there is no second writer.

## Fixed manifest inventory

The deterministic bundle is written only to:

`clusters/<cluster UUID>/argocd/foundations/<environment UUID>.yaml`

It contains:

- the server-derived Namespace with pinned `restricted` Pod Security
  enforce/audit/warn labels and Kuberploy ownership labels;
- one bounded ResourceQuota and one bounded container LimitRange;
- default-deny ingress and egress;
- DNS-only egress to the exact kube-system/kube-dns selectors;
- a read-only Role and RoleBinding for the exact control-plane observation
  ServiceAccount. It grants only get/list/watch on workload status, events,
  Services, and Pods, plus get-only for Pod logs—never Secrets, exec/attach/
  port-forward, or mutation verbs.

Default-deny intentionally leaves ordinary workload egress and workload-to-
workload traffic closed. Application-specific, typed policy must explicitly
open those paths later; foundation rendering is not an arbitrary policy escape.

## Readiness

The default-off production runtime scans the authoritative environment catalog,
idempotently ensures one deterministic intent per environment, and reconciles
through the concrete protected publisher with lease heartbeats. `ExactReady`
succeeds only when the caller-derived environment count and the database's
same-statement environment count both exactly match the active/ready intent
counts, and there is a fresh worker heartbeat for the exact profile, binding,
and protected-publisher identity. A newly inserted environment, mismatched
count, stale worker, pending intent, changed profile, or changed publisher
digest fails closed. The Argo production runtime checks this probe before
credential/root reconciliation and does not publish Argo readiness until the
root Application observes the resulting platform head.
