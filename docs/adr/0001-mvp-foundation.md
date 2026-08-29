# ADR 0001: MVP repository, identity and first delivery slice

- Status: Accepted
- Date: 2026-08-06

## Context

The architecture is broad enough to support the full product, but implementation needs a smaller set of contracts that cannot change casually after migrations, public APIs and Git paths exist.

## Decision

### Repository

The product is one public Go/TypeScript/Helm monorepo with module identity `github.com/kuberploy/kuberploy`. The source license is Apache-2.0. Operator-specific deployment values and provider identities never belong in this repository.

### P0 human identity

Human principals have a provider-neutral stable key `(issuer, subject)`. Local
password credentials use a normalized email address; display name is
presentation-only. Future SSO providers may supply email metadata, but email is
not the durable external identity key.

The first implementation provides a one-time bootstrap exchange:

1. the installer supplies a high-entropy token through a Kubernetes Secret/environment reference;
2. `POST /v1/auth/bootstrap` consumes it only when no platform administrator exists;
3. the server creates the first stable principal and platform-scoped grant in one transaction;
4. it returns an opaque server-side session cookie and never returns or stores the bootstrap token;
5. after consumption, bootstrap permanently returns a conflict unless an explicit offline recovery procedure resets the installation.

Sessions use random opaque credentials, store only a hash in PostgreSQL, rotate on privilege change, have absolute and idle expiry, and are revoked when the principal or grant revision changes. Primary browser cookies are `HttpOnly`, `SameSite=Strict`, path `/`, and `Secure` whenever the public URL is HTTPS. The GitHub App's cross-site top-level Setup URL and OAuth returns use only a 15-minute HttpOnly, host-only, `SameSite=Lax` copy of the same revocable session, scoped first to the exact Setup path and then rotated to the exact OAuth callback path; successful completion clears it and no other route accepts it. The verified one-time handoff is held only in a second HttpOnly, host-only, `SameSite=Strict` cookie scoped to the link route, then consumed by an empty same-origin request after a fixed UI redirect; it is never exposed to JavaScript. Unsafe browser requests require same-origin validation and a CSRF token. Development bypass identity is compiled/configured only for disposable local tests and cannot be enabled in a release profile.

GitHub user authorization is an authentication provider layered onto the same principal model. The OAuth callback and GitHub App installation setup callback are separate endpoints and state machines. Generic OIDC remains a later provider without requiring a user-table rewrite.

### Control-plane credentials

Runtime application secrets and platform credentials are different stores. P0 defines a `PlatformCredentialStore` interface. Kubernetes Secret references are the self-contained backend; External Secrets may materialize the same named keys in production.

The API/auth component may read session/bootstrap material. A credential-broker boundary alone may read GitHub App private keys, webhook secrets and registry/provider roots. General workers receive only short-lived scoped credentials or opaque credential references for a single operation. Render workers receive no provider credentials and no unrestricted network. ServiceAccounts, mounts, NetworkPolicies and audit events enforce this split.

### Canonical application contract

`apps/<app-id>/app.yaml` is the only editable application-scoped managed-runtime document. It is a versioned `AppConfig` with stable opaque IDs and mutable display names. Namespace, Argo Application, Helm release, Deployment and Service names derive from stable IDs, not display names. Optional project and environment `variables.yaml` documents provide inherited ordinary values without becoming application-owned overrides.

The protected Application/ApplicationSet passes exactly three ordered value-file paths to the pinned `kuberploy-runtime` chart: project variables, environment variables, then the mandatory application document. Missing parent VariableSets are empty scopes. Operator-owned expected-identity Helm parameters ensure that the chart rejects a missing or substituted application document. There is no separately editable generated application values file. Both the API compiler and Argo render the same chart/version and schema.

### MVP delivery boundaries

The MVP was implemented through these durable boundaries:

1. A minimal AppConfig in Git deploys a public image digest through Argo and `kuberploy-runtime`, exposes HTTP through Traefik on an explicitly selected conforming test cluster, and rolls back by creating a new protected Git intent selecting an eligible prior deployment input.
2. The API records an idempotent Deployment command, Operation, audit event and PostgreSQL outbox row; the relay signals a Valkey Stream; a worker writes the Git commit; the projection and status APIs converge on Argo health.
3. Signed GitHub webhook replay triggers an isolated DinD/Buildx build, pushes to a local test registry and commits the resulting digest.
4. Secret backends, TLS/DNS, external Helm, metrics/logs and broader RBAC follow on the proven command/Git/reconcile seam.

## Consequences

- A complete Dokploy-like UI is not allowed to get ahead of the durable Git/reconcile path.
- The first schema and Git names receive compatibility tests before user data exists.
- Local bootstrap auth is not mistaken for the final provider surface.
- GitHub App credentials never become a convenient general worker secret.
- Changing a display name does not rename or recreate runtime resources.
