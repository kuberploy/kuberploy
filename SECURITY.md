# Security Policy

## Reporting a Vulnerability

Please report suspected vulnerabilities privately through
[GitHub Security Advisories](https://github.com/kuberploy/kuberploy/security/advisories/new).
Do not open a public issue, discussion, or pull request containing vulnerability
details, credentials, personal data, or an unredacted proof of concept.

Include the affected Kuberploy version, deployment mode, Kubernetes version,
impact, prerequisites, and the smallest safe reproduction you can provide. Never
include bootstrap tokens, session cookies, GitHub App keys, webhook secrets,
registry credentials, kubeconfigs, or tenant secret values.

Maintainers will acknowledge a complete report, assess its scope and severity,
and coordinate remediation and disclosure. No specific response deadline is
promised while the project is community maintained.

## System and Scope

This policy covers the Kuberploy API, worker, web UI, database migrations,
GitOps writer, release checker, Helm charts, Kubernetes RBAC and network policy,
and the repository's build and release automation. It also covers Kuberploy's
handling of PostgreSQL, Valkey, Git, Argo CD, Traefik, OCI registries, and GitHub
App integrations at their Kuberploy trust boundaries.

Tenant workloads, Kubernetes itself, and upstream services are independently
maintained systems. A defect in one of them is reportable here when Kuberploy's
configuration, credential handling, authorization, or documented integration
turns it into a realistically reachable Kuberploy vulnerability.

The production boundary assumes a dedicated Kubernetes namespace, correctly
configured cluster admission and network controls, TLS at the public edge, and
administrator-controlled infrastructure credentials. Development Docker engines
and operator-supplied Kubernetes test clusters are not production security
boundaries.

## Threat Model and Trust Boundaries

Internet clients, authenticated non-administrators, tenant-controlled Git
content, image and chart references, YAML, domains, headers, webhook payloads,
and integration responses are attacker-controlled. A user may be authorized for
one team, project, namespace, Argo project, or GitHub App installation and remain
untrusted for every other scope.

Platform administrators, the cluster control plane, and explicitly configured
external secret stores are trusted within their documented roles. Git is the
authority for non-secret desired application state; PostgreSQL is the durable
authority for users, authorization, operations, audit records, and the
transactional outbox; Kubernetes and Argo CD are authoritative for observed
runtime state. Valkey and read-model projections are disposable acceleration
layers, not authorization or desired-state authorities.

The control plane, tenant workloads, release-upgrade Jobs, and any future build
Jobs are separate trust zones. GitHub installation access must be checked both
when work is accepted and immediately before a token is minted. Revoking source
access does not delete or stop an already deployed immutable workload.

## Security Invariants

- Every protected read and mutation is authorized server-side against current
  platform, team, project, namespace, and integration grants. UI filtering is
  never an authorization control.
- Browser mutations require an authenticated session and CSRF validation.
  Invitations and other bearer credentials are short-lived, single-use where
  applicable, stored only as hashes or opaque secret references, and never
  exposed by read APIs or logs.
- Automation bearer tokens are project-bound, expire within 90 days, are shown
  only in the first successful issuance response, and are stored as SHA-256
  hashes with a non-secret prefix. Their effective authorization is the
  intersection of the service account's current object grants and the token's
  closed coarse scopes. They cannot carry platform- or organization-admin
  roles, manage credentials or grants, or fall back to a browser session; a
  request that supplies both authentication modes is rejected.
- Plaintext application secrets, GitHub App private keys, webhook secrets,
  registry credentials, and installation tokens never enter Git, ordinary
  database columns, Valkey, build arguments, release metadata, traces, or logs.
  Base64 encoding alone is not protection.
- GitOps writes preserve repository history, validate the expected head, and
  fail on unresolved conflicts. Argo CD is the only normal writer of tenant
  application workloads.
- User-controlled endpoints, redirects, archives, YAML, charts, and rendered
  output are scheme-, address-, size-, time-, and resource-bounded and fail
  closed. Credentials are not forwarded across an unapproved host or scheme
  change.
- Tenant and control-plane Kubernetes identities remain least-privileged and
  namespace-scoped unless a reviewed feature requires a narrower documented
  exception. Kuberploy upgrades must not own, prune, or mutate tenant workload
  resources.
- Release and deployment artifacts are selected by immutable digest. Published
  release metadata, charts, dependency locks, and updater inputs must agree, and
  upgrade eligibility is validated before mutation.
- PostgreSQL records an accepted durable operation before Valkey dispatch.
  Valkey loss or stale projections cannot grant access, lose accepted work, or
  present state as newer than its source revision.

## Reportable Findings and Severity Context

A finding is reportable when a supported or default deployment has a realistic
path to unauthorized cross-team or cross-namespace access, secret disclosure,
arbitrary control-plane or tenant code execution, GitOps integrity loss,
authentication or CSRF bypass, release supply-chain compromise, durable audit or
operation loss, SSRF into protected infrastructure, or denial of service across
security boundaries.

Severity depends on reachable impact, required privileges, default exposure,
scope, persistence, and whether documented controls actually stop exploitation.
Control-plane takeover, release-channel compromise, broadly reusable credential
disclosure, and cross-tenant compromise are normally critical or high. A UI-only
display defect, stale non-security status, or an administrator deliberately
installing an untrusted privileged component is not elevated without a concrete
boundary violation.

## Out of Scope, Exclusions, and Accepted Risk

- Unsupported local modifications, intentionally weakened Kubernetes controls,
  exposed database or Valkey ports, and credentials pasted into public reports
  are outside the supported deployment boundary unless Kuberploy caused the
  exposure despite documented secure configuration.
- Development fixtures may use local hostnames, synthetic credentials, and
  test-only network allowances. Those facts alone are not production findings;
  leakage into production defaults is reportable.
- Vulnerabilities solely in an upstream project should be reported upstream.
  Kuberploy-specific exploitability, unsafe defaults, or failure to ship a
  necessary compatible update remain in scope.
- Social engineering, physical attacks, volumetric attacks requiring control of
  the cluster or underlying network, and availability claims without a bounded
  reproduction are not assessed as product vulnerabilities.
- Source-build, registry/cache, metrics, and log-gateway capabilities are
  supported only when the running API advertises their exact configured and
  freshly observed readiness. A default-off or degraded installation does not
  create a claim that the corresponding live integration is operational.

These exclusions do not suppress a finding that demonstrates a reachable
violation of a security invariant in shipped code.

## Known Limitations and Compensating Controls

The current implementation is a self-hosted platform that relies on a platform
administrator for bootstrap, cluster hardening, backup, provider credentials,
and external credential lifecycle. Advanced identity federation, durable
retained log search, and External Secrets remote-store materialization remain
outside the MVP; live scoped logs, managed monitoring, Sealed Secrets, the
managed OCI registry, and in-cluster source builds are implemented default-off
capabilities.

The current slice compensates by reporting unavailable capabilities explicitly, using
namespace-scoped service accounts, default-deny network policies, immutable
release artifacts, write-only invitation/token handling, server-side team and
installation authorization, PostgreSQL-backed durable operations, and a
namespaced updater that does not own tenant workloads.
