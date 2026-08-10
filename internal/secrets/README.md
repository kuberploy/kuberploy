# Runtime-secret core

This package is the bounded runtime-secret lifecycle core. It is intentionally
independent of the central HTTP/store facades so provider and Git integration
can be wired without ever adding plaintext to an ordinary request model,
database row, outbox payload, worker message, or API response.

## Invariants implemented here

- `Material` is write-only, bounded to 64 keys, 64 KiB per value and 256 KiB
  total. It copies caller input, refuses JSON serialization, redacts formatting,
  gives providers only callback-scoped copies and zeroes its owned bytes after
  every service operation.
- Fingerprints are HMAC-SHA-256 values made with a key supplied through
  `FingerprintKeyProvider`. Raw SHA hashes of low-entropy values are never
  persisted. The key provider must return caller-owned key bytes; the service
  clears them before returning.
- `ProjectedFingerprintKeyProvider` reads a fixed, configuration-time absolute
  path on every call, accepts Kubernetes' in-volume atomic projection symlinks,
  rejects symlink escapes, permits group-read only when the file group is one
  of the process's exact groups, rejects every other/group-write permission,
  and returns only caller-owned 32-128 byte keys. Its configured key ID must
  change whenever the projected HMAC key changes.
- The exact authorization/storage scope is
  `(team/organization, project, environment, namespace, application)`. Migration
  012 enforces every ancestor relationship with composite foreign keys.
- The domain can model External Secrets Operator and strict namespace/name-bound
  Sealed Secrets, but the production runtime is deliberately **strict Sealed
  Secrets only**. External requests require explicit keys,
  `refreshPolicy: CreatedOnce` semantics and an immutable target. Sealed Secret
  requests require strict scope. The concrete Kubernetes adapters use only
  exact `get`, server-side `create` and UID/resourceVersion-preconditioned
  `delete`; they have no list, watch, update, patch, Secret-read, log, exec,
  proxy or caller-selected endpoint capability. Provider adapters are
  idempotent by the immutable version ID.
- A provider cannot choose a destination. Namespace and deterministic target
  Secret name are checked on stage, readiness and deletion. Sealed ciphertext,
  secret values and base64 are not accepted by any persisted model; only opaque
  provider revisions and cryptographic digests cross the store boundary.
- Environment-variable and file deliveries are immutable per version. File
  destinations are restricted to `/var/run/secrets/kuberploy`, use exact selected
  keys and permit only `0400` or `0440`.
- Create/rotate idempotency stores actor, operation, application, key, result IDs
  and a keyed request fingerprint. A replay returns metadata for the same version
  and never returns material. One pending rotation per binding and an expected
  active-version compare-and-swap close concurrent rotation races.
- A staged version cannot become active until the provider reports readiness for
  the exact immutable artifact. Production workers claim only configured
  namespaces and only `sealed-secrets` versions. Activation atomically retains
  the prior version.
- Awaiting versions have a durable claim cursor. Every claim increments an
  epoch and binds owner, expiry, worker contract and complete config digest.
  Heartbeats, pending/backoff release and terminal apply all compare that exact
  lease, so an expired or superseded process cannot commit. Ready/failed state
  and its credential-free event commit in one transaction.
- Worker readiness is a separate epoch-fenced heartbeat. The public readiness
  probe requires a fresh observation matching the exact sorted namespace
  allowlist, Kubernetes Secret reference names, fixed projection paths, HMAC key
  ID, public sealing-key fingerprint, timing configuration and worker contract.
- Git-current, current-release and retained-release references are typed rows.
  Delete fails closed while any exists. Once deletion starts, new references are
  rejected; provider cleanup must confirm the exact artifact is absent before the
  binding and its version metadata become `deleted`.
- Every mutation commits a credential-free outbox event in the same transaction.
  Reads return binding/version/delivery/provider metadata only.

## Lifecycle

```text
create/rotate -> staging -> provider write/seal -> awaiting-readiness
             -> exact provider Ready -> active (old active becomes retained)
             -> failed on a closed, safe failure code

unreferenced binding -> deleting -> exact provider absence for every artifact
                     -> deleted
```

An interrupted provider cleanup is resumable by calling `Service.Delete` again;
the durable `deleting` state prevents new Git/release pins meanwhile. An
interrupted provider stage may retry the same `staging` version, so provider
implementations must use `Version.ID` as their idempotency identity.

The production reconciliation loop never calls `Service.ReconcileVersion` and
never receives material. It claims an `awaiting-readiness` metadata row, performs
one exact `GET` of its SealedSecret, and commits one of three fenced outcomes:

```text
pending -> release lease, schedule poll (healthy) or bounded exponential backoff
ready   -> active + retain old active + safe event, atomically
failed  -> failed + safe fixed failure code + safe event, atomically
```

A crashed worker does nothing: after lease expiry another worker increments the
epoch and re-observes. Provider errors are reduced to fixed codes; error strings
and Kubernetes response bodies are not persisted or logged by this runtime.

## Operator runtime configuration

The runtime is default-off. `KUBERPLOY_RUNTIME_SECRETS_ENABLED` must be exactly
`true` before any other setting is accepted; when it is absent or exactly
`false`, even an empty dormant runtime setting is rejected. Enabled API and
worker processes parse the same exact contract:

- `KUBERPLOY_RUNTIME_SECRET_NAMESPACES` is a nonempty, sorted, unique
  comma-separated namespace allowlist;
- `KUBERPLOY_RUNTIME_SECRET_FINGERPRINT_SECRET_REF` and
  `KUBERPLOY_RUNTIME_SECRET_FINGERPRINT_SECRET_KEY` plus
  `KUBERPLOY_RUNTIME_SECRET_FINGERPRINT_KEY_ID` identify the operator-owned HMAC
  projection;
- `KUBERPLOY_RUNTIME_SECRET_SEALING_CERTIFICATE_SECRET_REF` and
  `KUBERPLOY_RUNTIME_SECRET_SEALING_CERTIFICATE_SECRET_KEY` identify the
  public-certificate projection; and
- optional canonical integer-second poll, lease, heartbeat, idle and backoff
  overrides use the `KUBERPLOY_RUNTIME_SECRET_*_SECONDS` constants in
  `runtime_config.go`.

The key and certificate bytes are never environment variables. Their Secret
references, fixed projection paths, namespace allowlist, timing values, key ID,
public certificate fingerprint and worker contract all enter the readiness
digest. Whitespace, duplicate/unsorted namespaces, noncanonical numbers and
partial enabled configuration fail closed.

## Kubernetes provider adapters

`StrictSealedSecretsAdapter` is the self-contained provider path. It reads the
controller's public certificate from the fixed
`/var/run/secrets/kuberploy-system/sealed-secrets/tls.crt` projection, requires
an in-validity RSA certificate, and implements the Sealed Secrets hybrid
encryption format with the exact `namespace/name` OAEP label. Namespace-wide
and cluster-wide scopes are never emitted. The immutable `SealedSecret` is
created or adopted only when its complete owned metadata, template, explicit
keys, keyed content fingerprint and manifest digest match. Readiness accepts
only the exact current-generation `Synced=True` object. Ciphertext is cleared
from request-local byte buffers and only its digest crosses the provider
boundary.

`ExternalSecretsAdapter` creates an exact namespaced `SecretStore` and an
explicit-key immutable `ExternalSecret`. The store controller class is fixed to
`kuberploy`; the external secret is `CreatedOnce`, uses an owner target, and
cannot use `dataFrom`. Plaintext must first be written to the remote backend by
the injected `RemoteMaterialWriter`, which receives `Material` only through its
write-only synchronous boundary and returns an opaque revision plus exact
versioned references.

The External Secrets production constructor deliberately rejects a writer that
does not also carry a package-approved, typed store profile. No generic provider
JSON, temporary Kubernetes Secret, arbitrary backend URL or credential bytes
are accepted. A concrete Vault/cloud writer and its closed profile remain a
provider-plugin gap; therefore External Secrets must stay disabled in runtime
configuration until one is installed. The Sealed Secrets adapter can be wired
independently.

Both adapters pin namespace and object names to the durable binding/version,
adopt only exact immutable specs, reduce controller failures to fixed codes,
ignore untrusted condition messages, and confirm absence after preconditioned
deletion. Kubernetes response bodies and provider errors never enter returned
errors.

## PostgreSQL

Migration `012_runtime_secrets.sql` adds:

- `secret_bindings` and immutable `secret_binding_versions`;
- relational `secret_binding_deliveries` with mutation-rejecting triggers;
- `secret_binding_idempotency` containing keyed fingerprints only;
- typed `secret_binding_references`; and
- `secret_binding_events`, a safe transactional outbox.

The schema has no secret value, generic payload, ciphertext bytes or base64
column; `ciphertext_digest` is only a fixed-format SHA-256 integrity identity.
Version triggers permit only closed lifecycle transitions and the one-time
staging of typed provider artifact identities.

Migration `023_runtime_secret_runtime.sql` adds:

- `secret_binding_runtime_reconciliations`, containing only version/binding
  identity, safe scheduling/failure metadata and an epoch-fenced lease;
- a due index used with PostgreSQL `FOR UPDATE ... SKIP LOCKED` for safe
  multi-worker claims; and
- `runtime_secret_runtime_readiness`, an exact config/contract worker heartbeat
  with restart epochs and freshness checks.

The migration backfills only strict SealedSecret versions already awaiting
readiness. Database constraints reject an ExternalSecret reconciliation cursor,
lease-epoch skips, identity mutation and terminal-state rewrites.

The optional PostgreSQL contract test requires a fresh, disposable database:

```sh
KUBERPLOY_TEST_DATABASE_URL='postgres://...' \
  go test ./internal/secrets -run TestPostgreSQLRuntimeSecretContract -count=1
```

Secret version/delivery metadata is intentionally immutable and retained for
audit, so the test database should be disposable rather than shared.

## Production boundary and remaining extensions

The strict Sealed Secrets path is wired default-off through the API and worker.
The API alone reads the fixed private HMAC projection; the worker reads only the
fixed public sealing certificate and verifies the exact HMAC key metadata/key ID
through the shared runtime digest. Per-namespace RBAC grants API
`get/create/delete` and worker `get` on SealedSecrets only. A fail-closed
ValidatingAdmissionPolicy restricts create/delete to the exact API
ServiceAccount and exact immutable Kuberploy object identity. Neither process
has Kubernetes Secret read/list/watch/update/patch access or a runtime-secret
ClusterRole.

AppConfig preview/create/save and direct-Git projection resolve locked binding
UUID, active integer version, reviewed name/key, delivery and namespace. The
PostgreSQL write boundary repeats `secrets.bind` authorization and exact
resolution under locks, atomically replaces Git-current references with the
immutable Git command, and wakes same-head indexing when resolution metadata
changes. `secretBindings` remains false and write routes return 503 unless the
strict provider, exact Git policy digest and a fresh matching worker heartbeat
are all proven.

The remaining extensions are explicit:

1. implement and audit a concrete typed `RemoteMaterialWriter` (Vault/cloud),
   including credential projection and backend-specific retention/deletion,
   before enabling the existing External Secrets library adapter;
2. add full browser end-to-end coverage for the write-only/no-telemetry editor
   and exact reference picker;
3. add rollout-health and retention-deadline policy before reclaiming old,
   unreferenced provider artifacts; and
4. publish `secret_binding_events` into the platform audit/event stream using
   credential-free details only.
