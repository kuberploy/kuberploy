# Verified build promotion

This package resolves one exact successful source-build attempt into the only
application and immutable image that may be deployed from it.

The caller can select only an environment. The build store derives project,
application, registry target, repository, digest, and release identity from the
immutable attempt plus its completed `build_release_projections` row. The
independent registry lifecycle store must then return the exact present
`RegistryRelease`. Promotion requires both `builds.read` on the derived
application and `resources.write` on the derived application/environment pair.

The package does not write Kubernetes objects, accept arbitrary YAML, or create
a second deployment state machine. HTTP transport reuses the existing durable
deployment idempotency record, protected-Git write plan, private-pull/runtime
secret resolution, and Argo readiness gate. Matching replays are recovered
before mutable readiness probes so a lost accepted response remains safe to
retry.
