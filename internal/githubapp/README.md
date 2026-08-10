# GitHub App security primitives

This package is the credential-broker side of Kuberploy's GitHub App boundary.
It intentionally contains no HTTP handlers, database implementation, shared
domain models, or operator configuration parser.

## Security invariants

- The App RSA key, webhook secret, state-signing key, and OAuth client secret come only from an
  injected, operator-scoped `SecretReader`. A tenant never supplies a file path
  or secret namespace.
- The production projected-volume reader is fixed to
  `/var/run/secrets/kuberploy/github-app`, resolves only validated
  `<secret-name>/<key>` entries, permits kubelet's atomic symlinks only when
  they remain inside that root, bounds every read, and never caches bytes.
- App JWTs are RS256, use the GitHub App client ID as `iss`, backdate `iat` by at
  most 60 seconds, and expire no more than ten minutes after issuance. RSA keys
  smaller than 2048 bits, encrypted/multiple PEM blocks, and non-RSA keys fail
  closed.
- Installation-token requests always include non-empty `repository_ids` and an
  explicit permission map. Requests are checked against configured maxima,
  remote installation ownership/permissions are rechecked, and the repositories
  visible through the minted token are compared by immutable repository and
  owner IDs before the token is returned.
- Configured maxima are themselves bounded by a broker hard cap. Git projection
  may request repository-scoped `contents:write` and `pull_requests:write` for
  deterministic candidates/PRs plus `administration:read` solely to attest the
  exact branch/ruleset policy; administration write is never allowed. Workflow,
  package/organization, hook, and unrelated permissions remain unavailable.
- GitHub App setup uses two distinct signed states. The Setup URL return is the
  only endpoint that accepts GitHub's `installation_id`; it consumes the
  installation-purpose state and issues an actor/account/installation-bound
  OAuth-purpose state. The OAuth callback accepts only `code` and that second
  state, derives the installation ID server-side, then calls exact `GET /user`,
  binds the token to a previously associated immutable GitHub user ID, and
  enumerates `GET /user/installations` before trusting the installation. The
  HTTP layer keeps the primary session `SameSite=Strict` and authenticates the
  two cross-site top-level returns with a 15-minute HttpOnly, host-only,
  `SameSite=Lax` copy of that same revocable opaque session, scoped first to the
  exact Setup URL path and then rotated to the exact OAuth callback path. The
  callback clears that copy, stores the verified handoff only in a second
  HttpOnly, host-only, `SameSite=Strict` cookie scoped to the exact link route,
  and redirects to a fixed same-origin completion page. The empty link request
  consumes the cookie; handoff bytes are never exposed to browser JavaScript.
- OAuth/setup state is signed, purpose/actor/team-bound, short-lived, and paired
  with an atomic replay claim. Return values are server-side route keys, never
  arbitrary redirect URLs. Handoff tokens contain 256 random bits; only a
  domain-separated SHA-256 digest is handed to durable storage.
- Webhook HMAC uses SHA-256 over the byte-exact bounded request body and
  `hmac.Equal`. Event and delivery headers must occur exactly once. A delivery
  claim is scoped by provider, App ID, installation ID, and delivery GUID. The
  payload-retention deadline does not expire the permanent dedupe tombstone:
  GitHub supplies no signed delivery timestamp, so an old signed body must
  never become usable again after cleanup.
- The provider client sets a bounded request timeout, a fixed User-Agent,
  `Accept: application/vnd.github+json`, and
  `X-GitHub-Api-Version: 2026-03-10`. It never follows redirects or retains
  response/error bodies. Rate-limit scheduling metadata is parsed only from
  bounded headers.
- JSON is one bounded document: duplicate keys, malformed shapes, and trailing
  documents are rejected. Signed state and the one-time OAuth token exchange
  use closed field schemas. Versioned REST-provider and webhook response
  structs tolerate additive GitHub fields, because GitHub documents additive
  fields as non-breaking within an API version, while all security-relevant
  fields are validated exactly.
- A push webhook SHA is named `UntrustedAfter` and cannot select build source.
  The provider resolves an exact `refs/heads/...` or `refs/tags/...` endpoint,
  rechecks immutable repository identity, and peels annotated tags. Kuberploy's
  P0 builder accepts 40-hex Git object IDs; a future valid 64-hex provider object
  returns typed `ErrUnsupportedObjectFormat` before reaching the builder.
- Credentials use an opaque redacting type whose backing value is not a
  reflectable string field. Raw values are exposed only by explicit `Reveal`
  calls at outbound request/source-fetch boundaries.

## Current GitHub contracts

The implementation was checked on 2026-08-09 against GitHub's primary
documentation:

- [Generating a JSON Web Token for a GitHub App](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-for-a-github-app)
- [Generating an installation access token](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app)
- [REST endpoint: create an installation access token](https://docs.github.com/en/rest/apps/apps?apiVersion=2026-03-10#create-an-installation-access-token-for-an-app)
- [Setup URL spoofing warning](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/about-the-setup-url)
- [Generating a user access token for a GitHub App](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app)
- [REST endpoints for user installations](https://docs.github.com/en/rest/apps/installations?apiVersion=2026-03-10)
- [Validating webhook deliveries](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries)
- [Webhook headers and event payloads](https://docs.github.com/en/webhooks/webhook-events-and-payloads)
- [REST API versions](https://docs.github.com/en/rest/about-the-rest-api/api-versions)
- [REST Git references](https://docs.github.com/en/rest/git/refs?apiVersion=2026-03-10)
- [REST rate-limit best practices](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api?apiVersion=2026-03-10)

Installation tokens are treated as opaque and length-independent. GitHub's
current endpoint documentation notes the 2026 staged rollout of the stateless
`ghs_APPID_JWT` format, so no legacy 40-character assumption appears here.

## Package boundaries

The following deliberately lives outside this package and is wired by the
production `builds`, API, worker, and chart layers:

1. Map protected operator settings into `Config` and project only selected
   Secret keys at the fixed reader root. Every operation rereads its key, so
   kubelet rotation does not require storing credentials in process config.
2. Persist state claims atomically and persist webhook delivery tombstones with
   a permanent unique constraint. Payload rows may expire; tombstone keys may
   not.
3. Bind projected OAuth code exchange to CSRF/session handling and the durable mapping from
   a state-bound local actor to the immutable GitHub user ID returned by
   `VerifyAuthenticatedUser`.
4. Run the webhook handler sequence: limit and authenticate bytes, parse a
   supported typed event, claim its signed installation scope permanently with
   `ClaimEventDelivery`, re-authorize the installation/repository, and enqueue
   exactly once.
5. Bind installation/repository records to team/project access
   checks and repeat those checks immediately before each build token mint.
6. Pass the short-lived token only to the isolated source fetcher, pass the
   authoritative 40-hex resolved commit to the builder, and ensure audit/log/API
   serialization never receives `Credential.Reveal()` values.

This package intentionally remains unaware of HTTP sessions, PostgreSQL,
Kubernetes namespaces, registry targets, and public response models.
