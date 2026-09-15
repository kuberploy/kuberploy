# ADR 0011: Squash every schema change into the single baseline until stable

- Status: Accepted
- Date: 2026-09-15

## Context

[ADR 0010](0010-baseline-reset-before-stable.md) squashed the two-migration
history back into a single `001_initial` at `0.1.0-rc.483` and stated it was
"the last time Kuberploy squashes shipped migration history," intending
append-only migrations to resume immediately after.

The project owner has since given standing direction that, since there is
still no stable (non-`-rc.`) release, every future schema change before the
first stable release should also reset the baseline to a single migration
rather than accumulate append-only history — not just the one `0.1.0-rc.483`
event ADR 0010 described.

## Decision

Supersede ADR 0010's "last time" framing. Until the first stable release
ships, every schema change is folded directly into `001_initial` (its
content edited in place, a new checksum computed, `migrations.CurrentSchema`
and `embed_test.go` updated in the same change) rather than added as a new
ordered migration file. This keeps exactly one migration at all times
pre-stable, at the cost of upgrading-in-place from any earlier release
candidate never being supported (already true and already accepted per ADR
0010 — this ADR just makes it the standing policy instead of a one-time
event).

Each such change still gets recorded in the RC's bug ledger the same way
ADR 0010's event was, and `release/metadata.json` still sets
`breakingChanges: true` and a `supportedUpgradeFrom` lower bound pinned to
the RC introducing the change.

Append-only migrations (ADR 0009's original policy) begin at the first
stable release and are permanent from that point on — this squash-every-time
exception applies only to the pre-stable release-candidate line.

## Consequences

- No release candidate before stable ever needs an in-place database
  upgrade; every RC before 1.0 is a fresh-install-only qualification target
  for schema purposes.
- `migrations/prisma/migrations/` has exactly one directory, `001_initial`,
  for the entire pre-stable lifetime of the project.
- The first stable release's migration becomes the new permanent, genuinely
  frozen `001_initial` baseline, and append-only history begins from there.
