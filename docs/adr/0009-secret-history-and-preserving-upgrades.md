# ADR 0009: Retain secret history through resource deletion

- Status: Accepted; the append-only baseline this ADR built on was squashed by
  [ADR 0010](0010-baseline-reset-before-stable.md) at `0.1.0-rc.483`. The
  secret-history behavior described below is unchanged and still in force —
  only its physical migration (`002_secret_history_retention`) was folded back
  into `001_initial`.
- Date: 2026-09-13

## Context

Deleting a stopped App after its secret was deleted failed because cleanup tried
to delete immutable secret deliveries and permanent lifecycle events. Their
foreign keys also prevented removal of the binding's operational metadata.
The existing resource-deletion test had no delivery or event history.

The published release-candidate database contains retained users, Apps, and
workloads. Replacing the pre-stable baseline would invalidate its migration
checksum and require a reset, contrary to the retained-data requirement.

## Decision

Freeze the published `001_initial` migration and use ordered, append-only
migrations for subsequent release candidates and stable releases.
`002_secret_history_retention` changes native PostgreSQL authority atomically:

- New delivery and event records must reference an existing binding and, when
  present, its exact version. A database trigger takes key-share locks on those
  identities, preserving serialization with concurrent resource deletion.
- Existing immutable delivery and event update/delete guards remain enforced.
- Historical records retain their original binding and version identifiers
  after deleted operational metadata is removed. The three history foreign
  keys that formerly required that metadata forever are replaced by the native
  insertion guards. Other foreign keys and tenant authorization remain intact.
- App, Environment, and Project cleanup removes only operational metadata for
  bindings already in the terminal deleted state. It does not erase deliveries,
  lifecycle events, or mutation receipts.

The migration does not rewrite historical rows or the baseline checksum. It
does not authorize direct edits to a deployed database outside the normal
migration Job.

## Compatibility and recovery

Old running processes can continue reads and secret writes during the rollout;
their old deletion path still refuses immutable history. Updated processes
complete resource deletion without deleting that history.

Binaries that know only `001_initial` reject the additional migration on
startup. After this upgrade, recovery must use a chart and binary that support
`002_secret_history_retention` or a later compatible schema. Helm may restore
a compatible revision; it must not downgrade the database or reintroduce the
removed history foreign keys after their parent metadata has been deleted.

## Validation

Qualification covers an existing baseline database containing secret history,
the additive migration, repeated migration execution, and a fresh database
using the full history. Resource deletion must preserve every historical row,
reject new dangling or cross-binding history, and keep immutable history
mutation denied. The exact stopped-App failure is replayed after the candidate
is deployed to remote staging.
