# Kuberploy Governance

Kuberploy is an open-source project maintained in the `kuberploy` GitHub
organization. The current code owners are recorded in `.github/CODEOWNERS`.

## Decision making

Routine changes are decided through pull-request review. Maintainers seek rough
consensus, using test results, compatibility, user impact, and the documented
architecture as evidence. A maintainer may block a change that weakens a stated
security invariant or breaks a supported upgrade path.

Durable API, Git, schema, identity, tenancy, build-isolation, secret-handling,
and installation decisions require an ADR. When consensus is not reached, the
project owner makes the final decision and records the tradeoff publicly.

## Maintainers

Maintainers can triage issues, review and merge pull requests, manage releases,
and respond to security reports. New maintainers are nominated based on
sustained, constructive contributions and approved by the existing maintainers.
Inactive maintainers may step down or be moved to emeritus status without
losing attribution for their work.

## Releases

Only repository workflows may build official images and charts. Stable releases
use semantic version tags, locked GitHub Releases, and exact OCI digests.
Provenance attestations become a release control only when clients verify them.
At least one maintainer other than the change author
should approve a stable release once the project has more than one active
maintainer.

## Changes to governance

Governance changes use the same public pull-request process and should remain
open long enough for active contributors to comment.
