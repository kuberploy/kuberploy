# ADR 0010: Squash the release-candidate migration history before stable

- Status: Accepted; its "last time" framing (Decision, Consequences) is
  superseded by [ADR 0011](0011-squash-every-pre-stable-schema-change.md)
- Date: 2026-09-15

## Context

The published `001_initial` baseline plus the single append-only
`002_secret_history_retention` migration (ADR 0009) were the only two Prisma
migrations Kuberploy had ever shipped. No stable (non-`-rc.`) release existed
yet, and no production installation depended on preserving data across this
specific pair. `DEVELOPMENT.md`'s release contract already anticipates this
exact situation: "Put all schema squashing, code, chart, documentation, and
workflow changes in a new RC and qualify it before creating the stable
promotion commit."

Carrying two migrations into the 1.0 line for no reason other than history
adds a permanent, meaningless step to every future fresh install and a second
checksum to keep straight for no compatibility benefit, since nothing has
shipped that depends on the two-step history existing.

## Decision

At `0.1.0-rc.483`, squash `002_secret_history_retention` back into
`001_initial`: concatenate the two migration SQL files in their original
order into a single new `001_initial/migration.sql`, delete the
`002_secret_history_retention` directory, and recompute the baseline
checksum. This is a one-time, pre-stable event. It does not reopen ADR 0009's
append-only policy for migrations added after this point — the very next
schema change after `0.1.0-rc.483` must again be a new ordered, append-only
migration, never a rewrite of `001_initial`.

Upgrading in place from any `0.1.0-rc.*` install published before this reset
is explicitly not supported. `internal/store/postgres.VerifySchema` and the
migration image's own history check both fail closed (refuse to start,
without mutating any data) against a database whose `_prisma_migrations` row
count or checksum no longer matches the new single-migration history, rather
than attempting a silent or partial upgrade. Those installs must be
reinstalled against a fresh database.

`release/metadata.json`'s `supportedUpgradeFrom` for this release is
`>=0.1.0-rc.483 <0.2.0`, reflecting that no earlier release candidate can
upgrade into it.

## Consequences

- Fresh installs on `0.1.0-rc.483` or later, including the eventual stable
  `1.0.0`, apply exactly one migration.
- Any environment still running a pre-`0.1.0-rc.483` release candidate
  (including internal staging) must be reinstalled from a fresh database to
  move onto this baseline; its existing data is not preserved across the
  reset.
- This is the last time Kuberploy squashes shipped migration history. ADR
  0009's secret-history behavior is unaffected; only its physical migration
  file moved.
