# cert-manager ClusterIssuer profiles

This package is the durable platform-admin authority for the set of ACME
ClusterIssuers that an application may select. A profile name is the exact
cluster-scoped Kubernetes object name and is never reused. Every change creates
an immutable revision and a new `pending` observation. Actor-bound command
records make retries exact-idempotent and the audit log is immutable.

The accepted specification is intentionally closed:

* HTTP-01 always uses the platform's fixed Traefik IngressClass. The protected
  materializer must add the fixed solver-Ingress ExternalDNS exclusion marker;
  neither ingress annotations nor ingress classes are caller-controlled.
* DNS-01 initially supports only a Cloudflare API-token Secret name/key and an
  exact non-empty `dnsZones` allowlist. Credential bytes and arbitrary provider
  configuration are never accepted.
* ACME servers are limited to the official Let's Encrypt production and staging
  directories. Each profile has exactly one solver.

`Catalog.ForHostname` is the only tenant-facing read model. The caller supplies
an explicit UTC time and a bounded one-minute-to-24-hour freshness window. It returns only an
active profile ID, exact ClusterIssuer name, revision, and solver after an
observer has reported the exact desired spec digest ready. HTTP-01 is excluded
for wildcard names. DNS-01 is returned only when the hostname is equal to or a
subdomain of one of that profile's zones. The DTO cannot expose email, zone,
Secret, or provider details.

Direct-Git admission uses `ReconcileReferencesTx`: issuer names are resolved to
the exact current active, ready, fresh revision for each hostname, then one
AppConfig path's references are replaced in the caller's serializable
transaction. Deletion clears them. A referenced profile can neither advance to
a new revision nor deactivate, preventing mutable-name drift after admission.

`ProtectedController` is the worker-side mutation seam. Production constructs
its `ProtectedGitPublisher` with the existing PostgreSQL `gitprojection.Store`,
the existing authenticated GitHub head verifier, the single hardened
`MirrorManager`, an exact platform binding/cluster identity, and the exact
server-configured observer namespace and ServiceAccount. It renders one closed
three-document bundle at
`clusters/<cluster>/argocd/platform/certificate-issuers/<name>.yaml`. The
writer persists the provider-pinned write base in the shared path reservation,
uses exact path CAS, operation and intent trailers, normal fast-forward pushes,
push-before-receipt recovery, and an exact provider reread. It never accepts a
remote URL, path, manifest, ACME server, solver YAML, or credential bytes.

Each bundle contains the ClusterIssuer plus a deterministic ClusterRole and
ClusterRoleBinding. The role grants only `get` on
`cert-manager.io/clusterissuers` with `resourceNames: [<exact issuer name>]` to
that one observer ServiceAccount. It grants no list/watch, Secret access, or
mutation verb. Observer identity is covered by both rendered content and the
immutable operation identity, and exact-match deletion removes the whole
bundle. Production should derive these publisher fields from the validated
observer runtime configuration with `ProtectedGitConfigForObserver` so the
binding, cluster, namespace, and ServiceAccount cannot drift between runtimes.

HTTP-01 rendering always selects Traefik and sets
`external-dns.alpha.kubernetes.io/ingress-hostname-source: annotation-only` on
the temporary solver Ingress. DNS-01 rendering is limited to Cloudflare and
the exact sorted zone/Secret references stored in the catalog. Deletion is not
exported by the publisher: only deactivated profiles returned by
`PendingDematerialization` can enter the controller's exact-rendered-preimage
delete path, after PostgreSQL has already proved that no application references
remain. Repeated absent-path reconciliation is an idempotent no-op.

Git publication is not certificate readiness. The separately wired, read-only
cert-manager observer dynamically enumerates at most 128 active database
profiles, performs exact-name `GET` calls only, and verifies the live
ClusterIssuer generation, Ready condition, annotations, and reconstructed spec
digest before calling `RecordObservation(Ready)`. The observer-readiness tables
fence the observer's exact
configuration digest, target-set digest/count, worker start, epoch, heartbeat,
and lease. API management readiness requires that durable observation to be
fresh, so a newly created or revised profile cannot be advertised as usable
until both protected Git publication and live cert-manager reconciliation have
converged. The package deliberately
does not include an imperative Kubernetes writer: an API service account able
to create, patch, or delete arbitrary ClusterIssuers would turn the admin UI
into an unbounded cluster-scoped mutation path.

The stable schema additionally rejects command
and audit inserts unless the actor is a PostgreSQL `platform-admin`. Profile
deactivation is one-way and fails while a materialized application reference
exists.
