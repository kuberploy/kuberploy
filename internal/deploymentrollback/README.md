# Ordinary deployment rollback

An ordinary application rollback is a new Git intent governed by the
environment's immutable publication policy: direct for development or a pull
request for protected environments. It never rewrites deployment history and
never sends an imperative Kubernetes or Argo rollback command.

The caller selects only one exact prior `sourceOperationId` for the deployment
in the request path. The resolver freshly authorizes the logical deployment,
loads the immutable `deployment_operation_inputs` snapshot, and requires the
source operation to be an older successful `deployment.git-write`. A direct
source must have a durable commit revision. A protected source is eligible only
after its pull-request merge reaches `merge-verified`; an open, closed-unmerged,
or merge-pending review is not desired-state history.

For Kuberploy-managed build artifacts, rollback also requires an exact matching
registry release root still marked present. An immutable digest outside that
catalog remains operator-managed and is explicitly reported as external and
unverified, rather than being described as registry-verified.

HTTP must first use `ResolveAuthorized` and attempt exact idempotent replay with
an invalid Git plan. Only a genuinely new command calls `VerifyArtifact`, then
passes the reconstructed `CreateDeployment` through the ordinary deployment
submission path. That path freshly revalidates direct application scheduling, runtime
secret references, private pull policy, sslip/edge policy, protected Git
planning, Git/Argo readiness, and direct-versus-pull-request publication.
