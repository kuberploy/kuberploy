package builds

import (
	"context"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/githubapp"
)

// Store is the durable boundary for the webhook receipt and build state
// machines. Every method that accepts an owner verifies the current lease.
type Store interface {
	githubapp.ReplayClaimer
	PutInstallation(context.Context, Installation) error
	PutRepository(context.Context, Repository) error
	PutDefinition(context.Context, BuildDefinition) error
	Definition(context.Context, string) (BuildDefinition, error)
	ApplyInstallationEvent(context.Context, int64, githubapp.InstallationEvent, time.Time) error
	ApplyRepositoryEvent(context.Context, int64, githubapp.InstallationRepositoriesEvent, time.Time) error

	ClaimDelivery(context.Context, githubapp.OneTimeClaim, DeliveryReceipt) (bool, error)
	Delivery(context.Context, string) (DeliveryReceipt, error)
	PendingDeliveries(context.Context, time.Time, int) ([]string, error)
	PurgeExpiredDeliveryPayloads(context.Context, time.Time) (int64, error)
	AcquireDelivery(context.Context, string, string, time.Time, time.Duration) (DeliveryReceipt, bool, error)
	HeartbeatDelivery(context.Context, string, string, time.Time, time.Duration) error
	RetryDelivery(context.Context, string, string, string, time.Time, time.Time) error
	FinishDelivery(context.Context, string, string, DeliveryState, string, time.Time) error
	AuthorizePush(context.Context, int64, int64, githubapp.RepositoryIdentity, string) (AuthorizedPush, error)
	EnqueuePushBuilds(context.Context, EnqueuePush, string, []AttemptDefinition, time.Time) ([]BuildAttempt, error)

	Attempt(context.Context, string) (BuildAttempt, error)
	AttemptAuthorization(context.Context, string) (Installation, Repository, error)
	ClaimNextAttempt(context.Context, string, time.Time, time.Duration) (BuildAttempt, error)
	HeartbeatAttempt(context.Context, string, string, time.Time, time.Duration) error
	MarkAttemptRunning(context.Context, string, string, time.Time) error
	DeferAttempt(context.Context, string, string, string, time.Time, time.Time) error
	ScheduleAttemptRetry(context.Context, string, string, string, time.Time, time.Time) (bool, error)
	FailAttempt(context.Context, string, string, string, time.Time) error
	CompleteAttempt(context.Context, string, string, BuildCompletion, time.Time) error
	RequestCancel(context.Context, string, time.Time) (BuildAttempt, error)
	CompleteCancellation(context.Context, string, string, time.Time) error

	PendingOutbox(context.Context, int) ([]OutboxMessage, error)
	MarkOutboxPublished(context.Context, string, time.Time) error
}

type BuildCompletion struct {
	Result         builder.BuildResult
	CacheReference string
	LogReference   string
}

func terminalAttempt(state AttemptState) bool {
	return state == AttemptSucceeded || state == AttemptFailed || state == AttemptCancelled
}

func terminalDelivery(state DeliveryState) bool {
	return state == DeliveryEnqueued || state == DeliveryIgnored || state == DeliveryFailed
}
