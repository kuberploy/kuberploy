# ADR 0003: Public releases and tenant-safe self-upgrade

- Status: Accepted for MVP
- Date: 2026-08-06

## Context

Kuberploy is installed as a Helm release, and a platform administrator must be
able to discover and apply a newer Kuberploy release from the UI. An upgrade of
the control plane must not restart, re-render, or otherwise mutate applications
that Kuberploy previously delivered through Git and Argo CD.

## Decision

### Release trust and discovery

The canonical source is the public `kuberploy/kuberploy` GitHub repository. The
release checker calls only the fixed GitHub Releases API endpoint and accepts
published, non-draft, non-prerelease releases on the configured channel. It uses
conditional requests, a bounded response, strict timeouts, and a short Valkey
cache; administrators cannot supply an arbitrary release URL.

Every release contains `release-manifest.json`. The manifest binds the semantic
Kuberploy version to:

- the exact source commit;
- the control-plane OCI Helm chart reference and digest;
- an exact thirteen-chart component set for the installer, Argo CD, builder,
  cert-manager, Traefik edge, external-dns, External Secrets, monitoring,
  PostgreSQL, managed registry, runtime, Sealed Secrets, and Valkey releases;
- immutable API, worker, web, upgrader, and builder-agent image digests;
- supported Kubernetes and source-version ranges;
- the database schema compatibility window; and
- human-readable release notes and breaking-change flags.

The release workflow builds amd64 and arm64 platform images natively on their
corresponding GitHub-hosted Ubuntu runners and passes the tagged commit
timestamp into every Docker build as `SOURCE_DATE_EPOCH`. Platform manifests
are pushed without mutable tags. The workflow verifies their reported
architectures, assembles uniquely tagged per-attempt OCI indexes, and verifies
that each index contains exactly the two expected child digests. Only merged
index digests are accepted by the chart and release-manifest stages. It then
creates byte-reproducible control-plane and component chart packages and
computes every Helm OCI manifest digest locally. Locked upstream dependencies
are included only after their exact SHA-256 is verified. The release manifest,
semantic checks, and checksums must all succeed before any canonical chart
version is published. A rerun may reuse each existing chart only after pulling
it and matching both its predicted manifest digest and exact package bytes; a
different artifact is rejected without tag overwrite. An existing GitHub
release is rejected for explicit administrator review.

GitHub immutable releases are an externally verified repository prerequisite.
A repository administrator enables and verifies the setting before approving
the protected `release` environment. The job-level `GITHUB_TOKEN` deliberately
has only release and package publication permissions and cannot read repository
Administration settings. Releases are created as drafts, populated, then made
public, after which the workflow retains a fail-closed assertion that the API
reports `.immutable == true`. Attestations are not an MVP trust control because
the upgrader does not yet verify them. The in-cluster upgrader rejects a mutable
release, a manifest or chart digest mismatch, an unsupported Kubernetes
version, a skipped required upgrade, or an incompatible schema transition.

Repository provisioning also requires a protected `refs/tags/v*` ruleset and a
reviewer-gated `release` Actions environment. The repository must be public, or
be on an organization plan that supplies equivalent ruleset and immutable
release enforcement for private repositories. These are manual administration
controls: the release token cannot inspect or create them, and validation-only
workflow dispatches never publish.

### Ownership boundary

The `kuberploy` Helm release owns only Kuberploy control-plane resources in its
installation namespace. It never adopts or renders tenant Namespaces,
Deployments, Services, Ingresses, Argo Applications, or application Secrets.
Those remain owned by the environment GitOps repository and Argo CD. Optional
platform dependencies are separate releases or protected Argo Applications,
not subcharts silently upgraded by a Kuberploy control-plane update.

This boundary is the primary no-application-disruption guarantee: a Helm
upgrade has no tenant workload objects in its release manifest.

### Upgrade execution

`POST /v1/platform/upgrades` is platform-admin-only and requires an
idempotency key plus the exact target version and manifest digest previously
returned by the release-check endpoint. The API records an Operation and audit
event in PostgreSQL before dispatch.

The worker creates one namespaced, deterministic upgrade Job. The runner and Job:

1. recompute and verify the persisted immutable release-manifest digest;
2. verify the live Kubernetes version against the manifest constraint;
3. pull the chart by exact OCI digest;
4. run `helm upgrade` with existing user values, wait, cleanup-on-failure, and
   rollback-on-failure; and
5. expose a terminal Kubernetes Job result that a replacement worker reconciles
   into PostgreSQL before cleanup.

The Job continues even if the API, worker, or web Deployment is replaced by the
upgrade. A deterministic Job name and PostgreSQL generation prevent duplicate
upgrades. A new request is rejected while another platform upgrade is active.

The upgrader ServiceAccount is confined to the installation namespace. It
cannot modify tenant namespaces or Argo-managed workloads. Its token is
projected only into the ephemeral Job and expires with the Job.

### Availability and database rules

Control-plane Deployments use rolling updates with `maxUnavailable: 0`,
readiness probes, termination grace, and PodDisruptionBudgets when replica count
allows them. Tenant traffic flows directly through Traefik to tenant Services,
so a control-plane rollout is not in the application request path.

Database changes used by a self-upgrade must be expand/contract compatible with
the immediately previous supported release. Destructive contraction is shipped
only after every supported old binary has left the compatibility window. Helm
rollback never claims to undo an irreversible database mutation.

## Consequences

- “Upgrade Kuberploy” can briefly replace control-plane Pods but cannot restart
  tenant application Pods by chart ownership.
- Operators can inspect the exact release, compatibility decision, chart diff,
  Operation timeline, and rollback result from one API/UI workflow.
- Air-gapped installations may disable release checking and apply the same
  verified chart out of band.
- Updating Argo CD, Traefik, PostgreSQL, Valkey, or another platform dependency
  is a separate, explicit operation with its own compatibility test.

## References

- [GitHub immutable releases](https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases)
- [Helm upgrade](https://helm.sh/docs/helm/helm_upgrade/)
