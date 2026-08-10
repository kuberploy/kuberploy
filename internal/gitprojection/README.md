# Git projection core

This package separates three revisions that must never be conflated:

- `targetHeadRevision` is accepted only from `HeadVerifier`, whose contract is
  an authenticated provider read of the binding's exact installation,
  repository and fully qualified target ref. A webhook SHA is only a wake-up
  hint.
- `indexedRevision` is the complete active PostgreSQL projection generation.
  Readers keep the prior complete generation while a shadow generation is
  staged and explicitly report target/index lag.
- `configRevision` is the last commit that changed one projected path. Strong
  ETags bind to binding/ref identity plus sorted document and dependency blob
  IDs, parser, chart digest and policy version, so an unrelated branch commit
  does not invalidate an editor.

Repository URLs and paths are not accepted from tenant requests. A production
remote is reconstructed from a provider-verified GitHub repository identity;
the binding constructors derive platform/environment prefixes from immutable
IDs, and application paths are derived from the application ID. The only local
transport seam is an explicitly test-only manager setting.

The disposable Git cache uses a sanitized bare mirror, detached worktrees,
fixed Git environment/config, disabled hooks/filters/credential helpers/file
transport, no-follow filesystem checks, expanded-tree and cache budgets, exact
OID verification, isolated indexes and normal fast-forward pushes. A local
tracking ref may move backwards only while fetching a force-pushed remote so
the indexer can detect divergence; no outbound force push exists.

Path reservations fail closed when a projection is stale. Expiry alone never
permits stealing a candidate reservation: a repair worker must verify provider
history and either remove an absent operation or reconstruct a non-expiring
`committed-pending-index` guard.

The production worker can opt into PostgreSQL-backed safety polling and shadow
indexing. Claims use expiring leases with monotonically increasing epochs;
heartbeats keep long provider/Git operations alive, and generation activation,
failure, and final scheduling all reject stale owners. GitHub target heads are
resolved only after the active stored installation/repository identity is
re-authorized and a one-repository read token is minted. Provider and Git I/O
remain outside database transactions. Private Git transport independently
re-authorizes the same identity and mints an exact one-repository read token.
The token reaches Git through a mode-0700, per-command Unix askpass broker; it
is never placed in a URL, argv, child environment, Git config or credential
file, and its clearable byte copy is erased when the prepared repository closes.
The local cache is mounted only under the explicit worker switch and is bounded
by both filesystem scans and the Pod's `emptyDir.sizeLimit`.

An HMAC-verified, durably claimed GitHub push also enters the production wake
lane. PostgreSQL records one immutable receipt and atomically increments a
monotonic wake generation only for active bindings with the exact GitHub App,
installation, repository and fully qualified ref. The webhook's advertised SHA
is retained only for audit/replay collapse and never becomes
`targetHeadRevision`; the normal coordinator still performs the authenticated
head read. Reconciliation acknowledges only the generation it claimed, so a
concurrent push cannot be lost when an older run schedules the next safety
poll. Jittered polling remains the repair path for missed webhooks.

Human-managed project/environment VariableSets use another closed command
shape on the same hardened writer. The server derives the exact dependency path
and environment binding, persists immutable candidate bytes, parser/base/ETag
authority and the environment-selected `direct` or `pull-request` mode, and
never exposes a generic path writer. Direct commands retain the usual durable
write-base/commit/index recovery. Protected commands create or recover the
operation-specific candidate ref and pull request, then become indexed only
after a provider-verified merge reaches the target head. Command identity,
state transitions, preview consumption, publication receipts and lost-response
idempotency are database-fenced.

The same mutation transport also has two explicit, non-overlapping protected
Helm authorities. They accept only revision-unique payload files below
`helm-manifests/.../revisions/<release>/` and stable Application files below
`argocd/helm-applications/`. Ordinary AppConfig/Argo mutations cannot select
those paths. Helm mutations require their immutable intent trailer, exact
content digest, CAS mode, and (for phase two) the verified payload ancestor;
stable Application deletion is match-ETag only. Recovery accepts one direct
child of the durable write base carrying both the generic operation trailer
and exact Helm intent trailer, then proves the exact bytes or deletion at the
provider-pinned head.

## Remaining qualification and optimization

- Add incremental changed-path generation seeding for ordinary fast-forwards.
  The current core fetches incrementally but performs a bounded full mapped-tree
  scan for each shadow generation; divergence repair already uses the same
  atomic full-scan path.
- Add startup cleanup for worktrees left by a hard process/node crash and
  surface disk/circuit-breaker metrics. Normal and cancelled operations already
  perform bounded worktree cleanup; mirrors remain disposable.

Revisioned config bundle reads, path reservations, the hardened mirror
finalizer, and typed projection-not-ready handling are wired into the ordinary
deployment path. Project/environment `VariableSet` dependencies are also
consumed by config read, preview, validation, save, human management, and
runtime rendering; the strong ETag binds exact parent presence/absence and blob
identities. The remaining external proof is the enabled-stack
conforming-cluster matrix rather than another projection authority.
