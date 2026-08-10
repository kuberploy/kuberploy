package helmapps

import (
	"context"
	"time"
)

// Store is the complete durable boundary for approved chart identities and
// render work. Implementations must provide exact idempotent replay and fence
// every worker write with both lease owner and monotonically increasing epoch.
type Store interface {
	PutApproval(context.Context, Approval) (Approval, bool, error)
	Approval(context.Context, ApprovalKey) (Approval, error)
	Submit(context.Context, DesiredRender, time.Time) (RenderCommand, bool, error)
	Command(context.Context, string) (RenderCommand, error)
	Result(context.Context, string) (RenderResult, error)
	Claim(context.Context, string, RenderWorkerIdentity, time.Time, time.Duration) (RenderLease, error)
	Heartbeat(context.Context, RenderLease, time.Time, time.Duration) (RenderLease, error)
	Complete(context.Context, RenderLease, ValidatedManifests, time.Time) (RenderResult, error)
	Fail(context.Context, RenderLease, string, bool, time.Time) (RenderCommand, error)
	PutReadiness(context.Context, Readiness) error
	RuntimeReady(context.Context, time.Time) (bool, error)
}

func desiredMatchesApproval(desired DesiredRender, approval Approval) bool {
	if desired.Validate() != nil || approval.Validate() != nil || desired.Approval != approval.ApprovalKey {
		return false
	}
	left, leftErr := desired.Descriptor.approvalIdentityDigest()
	right, rightErr := approval.IdentityDigest()
	return leftErr == nil && rightErr == nil && left == right
}

func desiredReplayEqual(left, right DesiredRender) bool {
	return left.ID == right.ID && left.IdempotencyScope == right.IdempotencyScope &&
		left.IdempotencyKey == right.IdempotencyKey && left.InputDigest == right.InputDigest &&
		left.Approval == right.Approval && equalBytes(left.DescriptorYAML, right.DescriptorYAML) &&
		equalBytes(left.ValuesYAML, right.ValuesYAML)
}

func validLeaseRequest(owner string, runtime RenderWorkerIdentity, now time.Time, duration time.Duration) bool {
	return workerIDRE.MatchString(owner) && runtime.Validate() == nil && !now.IsZero() &&
		duration >= time.Second && duration <= 15*time.Minute
}

func RetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 8 {
		shift = 8
	}
	return time.Duration(1<<shift) * time.Second
}
