# AppConfig projection policy

This package runs database-backed semantic policy inside the serializable Git
projection activation transaction. A schema-valid AppConfig is converted once
to `AppConfigPolicyDocument`, which contains only the server-resolved document
scope, typed delivery and route fields, a detached workload runtime, and the
closed middleware-name catalog. Child policies never receive an editable
parsed map.

## Edge routes and TLS

`EdgeRouteReferencePolicy` binds every route to the exact configured and fresh
Traefik observation. The effective ingress class, referenced TCP port, route
ID, middleware names, and cross-application hostname/path ownership are
validated before a projected document can become eligible. Let's Encrypt is
accepted only when the exact cert-manager profile is fresh, its solver ingress
class matches Traefik, and the issuer is one of the operator-approved
production/staging issuers. External DNS uses the same fresh exact edge target
after durable environment assignment and suffix validation.

`customCertificate.secretRef` accepts only the immutable metadata identity
`{bindingId,name,version}`. The policy re-resolves that identity under the
exact organization/project/environment/application/namespace lock, derives
the Kubernetes Secret name itself, checks the append-only public X.509
attestation and exact route SAN, and requires both a fresh continuous
SealedSecret observation and a fresh exact observer-worker receipt. Legacy raw
Secret names, stale/rotated versions, cross-scope substitutions, expired
certificates, provider drift, and stale workers remain non-deployable.

## Private registry pulls

`RegistryPullReferencePolicy` accepts only locked
`delivery.registryPull.targetId`, `profileName`, and `profileRevision` metadata.
It re-resolves the exact application registry policy, registry target, release
repository, destination namespace, and operator profile before calling
`imagepull.EnsureArtifactTx` in the activation transaction. Secret names and
credential references are not representable in the policy document or Git.

Two narrow integration seams are intentionally available without enabling the
feature:

- `ResolveRegistryPullTx` is the config-writer seam. It omits metadata for an
  unmatched/public repository, returns the unique locked target/profile tuple
  for an authorized private repository, and fails on ambiguity or operator
  profile drift.
- `RegistryPullArtifactEligibleTx` is the Argo approval seam. A private
  AppConfig is eligible only when the artifact for the exact environment,
  namespace, target, profile name, and profile revision is active, ready, and
  freshly observed. Public AppConfigs resolve without an artifact. Global
  feature readiness remains a separate exact worker-readiness decision.

The Argo planner consumes the second seam through its PostgreSQL exact
indexed-document resolver; a caller-provided approval boolean cannot bypass
it. The HTTP/config writer, production Argo projection/claim orchestration,
runtime chart, and capability response are wired to the same exact policy.
Private-image and Argo capabilities still fail closed unless every configured
runtime identity and fresh readiness receipt matches.

Schema 025 has no exact desired-reference and observed-workload retention table
for shared environment/target pull artifacts. AppConfig deletion is therefore
an explicit no-op: the active artifact and old immutable Secrets are retained.
Deactivation and garbage collection must wait for a later table that can prove
absence across every desired AppConfig, running/terminating workload, and
retained rollback release. One deleted AppConfig is never sufficient proof.
