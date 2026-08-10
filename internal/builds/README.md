# Durable GitHub source builds

`internal/builds` is the credential-free orchestration core between the
security primitives in `internal/githubapp`, the closed execution protocol in
`internal/builder`, PostgreSQL, and the narrow Kubernetes adapter.

## Durable flow

GitHub App setup is intentionally two-stage. `SetupService.Begin` issues an
actor-bound installation-purpose state for GitHub's App installation URL.
GitHub returns that state and `installation_id` to the fixed Setup URL;
`SetupService.Continue` consumes it, embeds the installation identity in a new
signed OAuth-purpose state, and redirects only to GitHub's fixed authorization
endpoint with the operator-fixed callback URI. The OAuth callback accepts only
`code` plus that second state and derives the installation ID server-side
before exact user/installation verification. Either state is single-use, and
the two purposes are not interchangeable. Because the primary human session is
`SameSite=Strict`, authorization creates a 15-minute HttpOnly, host-only,
`SameSite=Lax` copy scoped to the exact Setup URL path. The Setup return
revalidates the durable session, rotates the copy onto the exact OAuth callback
path, and expires the Setup-path cookie. No other route receives or accepts it,
and successful OAuth completion clears it. The callback stores the verified
random handoff only in a second HttpOnly, host-only, `SameSite=Strict` cookie
scoped to the exact link endpoint and redirects to a fixed same-origin UI path.
The browser then sends an empty, CSRF-protected link request; JavaScript never
receives the handoff.

1. `WebhookService.Handle` authenticates the exact bounded request bytes with
   `githubapp.WebhookVerifier` and parses one supported typed event.
2. `githubapp.ClaimEventDelivery` calls `Store.ClaimDelivery`. The permanent
   delivery tombstone and a closed, credential-free typed receipt are committed
   together. A crash cannot leave a consumed tombstone without a resumable
   receipt. Once terminal and past its retention deadline, that typed payload
   can be purged while the immutable tombstone remains permanently.
3. One leased receipt worker checks the persisted installation, repository,
   and enabled build definition, then re-verifies the installation with GitHub.
   It mints a token scoped to exactly one repository with `metadata:read` and
   `contents:read` and resolves the exact pushed ref. The webhook `after` value
   is never used as build source.
4. The authoritative 40-hex commit, monotonically allocated service
   generation, closed `builder.JobPlanRequest`, closed checkout request, and
   one `build_outbox` record are committed atomically. Uniqueness on delivery
   plus definition makes replays exactly once.
5. `BuildController` leases the attempt and repeats installation/repository
   authorization immediately before minting a fresh source token. Only the
   opaque redacting credential reaches `KubernetesBuildAPI.Ensure`; no token is
   persisted or returned in result metadata.
6. Live Job and NetworkPolicy adoption requires byte-equivalent planned specs
   through `builder.CanAdoptJob`, `builder.CanAdoptNetworkPolicy`, and the
   immutable input digest. Retries retain the operation ID, generation,
   destination candidate, and cache candidate. Cancellation wins races with a
   result or retry, and expired leases cannot publish state.
7. Every build uses two closed Buildx phases. The cache phase can import and
   export only the service's cache namespace and receives only the cache
   credential. The final image phase receives only the release-push credential
   and has no registry-cache flags. Both phases share the same private BuildKit
   content store, so successful work is reused without sharing Docker auth.
   The operator-owned `buildKitImage` is copied into the runtime digest,
   immutable definition, and closed agent request. It must name the explicit
   `v0.32.2` release. Operators with exact-host egress should mirror that
   version into an approved fixed registry instead of relying on Docker Hub's
   rotating endpoints.
   Cache export uses `mode=max`, OCI media types, and an image manifest. A
   confirmed cache promotion becomes `generation-N`; an unavailable candidate
   records `CacheDegraded` and is never advertised.
8. A successful attempt atomically enqueues a lease-fenced release projection.
   The independent worker loop revalidates the immutable definition, registry
   target, service policy, repository scope, image digest and optional cache
   digest before registering rollback and retention roots. Release and cache
   IDs are deterministic, so a crash between either registry write and the
   projection checkpoint is replay-safe. Unknown cache byte size is recorded
   as zero until the complete registry catalog observes it; this fails toward
   under-reclamation, never unsafe deletion.

Managed and external registry targets use the same isolated release-push and
registry-cache execution paths. Managed retention remains the managed registry
controller's responsibility; this package never deletes from either target.
The target catalog, immutable definition, attempt snapshot, Job plan, and
release projection all preserve two distinct Kubernetes Secret identities.

## Persisted data boundary

Migration `009_github_build_orchestration.sql` adds immutable GitHub account
and repository IDs, suspension/removal state, generic one-time claims,
permanent delivery tombstones, resumable typed receipts, definitions,
monotonic service generations, attempts, and a transactional build outbox.
It stores Kubernetes Secret object names and mounted file paths only. GitHub
tokens, webhook secrets, App keys, registry passwords, build-secret values,
raw logs, and raw webhook bodies are absent.

Migration `017_build_release_projection.sql` adds only the recoverable
post-success handoff. A database trigger creates one pending projection when an
attempt first becomes `succeeded`; expiring leases use monotonically increasing
epochs, and terminal rows retain the deterministic release/cache IDs. It stores
no registry credential, manifest body, build log, or webhook payload.

Migration `014_github_setup_build_api.sql` holds only setup identities,
one-time handoff digests and API idempotency metadata. Migration
`016_source_build_runtime_readiness.sql` records a bounded worker identity,
configuration digest and heartbeat timestamps. The digest covers the exact App
ID, builder namespace, pod ServiceAccount, agent image, three required worker
loops, resources and source/registry host egress profile; it contains no
credential bytes or credential references.

The memory and PostgreSQL implementations share the same `Store` contract.
The PostgreSQL integration test applies the real migration and exercises
managed and external targets, terminal payload expiry/replay, immutable retry,
cancellation backoff, and successful result persistence. Permanent claim
deletion is also rejected at the database layer.

## Kubernetes execution boundary

`KubernetesAdapter` is pinned to one builder namespace. It creates or adopts
the exact immutable request ConfigMap, checkout-only source credential Secret,
run NetworkPolicy, and planned Job. Every object carries the operation,
generation, plan hash, and immutable input digest; mutations fail closed.
Source credentials are deleted after checkout terminates, on terminal state,
on cancellation, and on pre-Job errors. Deletes use UID/resource-version
preconditions. The adapter never reads either registry credential Secret and
has no exec, attach, port-forward, update, or patch path.

The checkout container receives neither registry authority. The agent mounts
the two Secret volumes read-only at disjoint fixed roots and constructs two
private Docker configurations. Its cache build, cache inspection, and
generation promotion always use the cache configuration; its final image push
and digest/platform verification always use the push configuration. The agent
verifies the promoted cache digest and records the derived `generation-N`
reference in the typed result. Promotion failure degrades cache only.
Kubernetes captures the agent's result from `/result/result.json`; publication
and collection are both strictly below the 4 KiB termination-message boundary,
so truncation is a typed retryable failure rather than partial success. The
TTL-governed Job and Pod remain the bounded result/log authority while
auxiliary resources are removed.

## Production integration

The human UI now owns GitHub App setup, build definitions, logs/history,
cancellation/retry, explicit promotion, and immutable per-environment
auto-deploy policy revisions with run receipts. A verified release enters the
deployment path only through one exact policy and service-account authority;
Kuberploy never guesses an environment.

The production worker uses durable PostgreSQL polling and outbox recovery as
the correctness boundary. Valkey Streams provide bounded wake/dispatch
acceleration, while an outage or lost wake is repaired from PostgreSQL without
silently losing accepted work.
