# ADR 0004: Team-scoped GitHub App sharing

- Status: accepted for MVP
- Date: 2026-08-06

## Context

A GitHub App installation can expose private repository names and mint tokens
that read source code. Treating an installation as a platform-wide credential
would let unrelated users discover or deploy repositories they do not own.
Kuberploy also needs a small self-hosted user model that works before an
external identity provider is configured.

## Decision

Kuberploy has first-class users, teams, and team memberships. The bootstrap
administrator can create a short-lived, single-use invitation; only its hash is
stored. Accepting an invitation creates a normal server-side session. Membership
changes advance the affected user's grant revision so an old session cannot
retain removed access.

A GitHub App installation has one Kuberploy owner and one of two visibility
modes:

- `private`: only the owner and platform administrators can discover or use it;
- `team`: the selected team's members can discover and use only the repositories
  granted to that GitHub installation.

An installation can be shared with at most one team in the MVP. The owner, a
team owner, or a platform administrator can change sharing, subject to the
server-side authorization rules. Platform administrators can inspect all teams
and installations. Ordinary users never receive a global installation list.

Projects may be assigned to a team. Creating an application from GitHub or
starting a source deployment requires both access to the target project and
access to the selected installation/repository. The API repeats this check when
the durable operation executes, immediately before minting a GitHub token. A
queued operation therefore fails closed if sharing or membership was revoked
after acceptance.

Revoking sharing blocks new source access and queued work. It does not delete
Git state, images, Argo Applications, or running tenant workloads. Already
deployed content-addressed images continue running, matching the control-plane
availability and upgrade isolation promises.

GitHub App private keys, webhook secrets, installation access tokens, and user
invitation tokens are never returned by read APIs or stored as ordinary database
values. The database holds installation IDs and an opaque Kubernetes/external
secret reference. Installation tokens are minted with the narrowest repository
permissions, kept in memory, and allowed to expire. API responses and audit
events may contain repository identity but never token material.

## API consequences

The public OpenAPI contract includes:

- user invitation creation and acceptance;
- teams and memberships;
- accessible GitHub App installations;
- a sharing mutation with `private` or `team` visibility;
- repository listing and source-command inputs that identify an installation
  and repository by stable internal IDs.

Every mutation is authenticated, CSRF-protected where a browser session is
used, idempotent where retry is expected, and audited. Repository lists are
filtered by the backend; hiding a row in the React UI is never an authorization
control.

## Rejected alternatives

- Platform-wide GitHub App credentials: too broad for multi-team use.
- Copying one installation token per team: tokens are short-lived secrets, not
  durable sharing objects.
- Encoding membership only in GitHub teams: Kuberploy must also authorize image
  deployments, namespaces, Argo projects, and non-GitHub identities.
- Stopping or deleting workloads when access is revoked: source authorization
  and runtime availability are separate concerns.
