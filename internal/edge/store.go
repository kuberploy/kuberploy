package edge

import (
	"context"
	"time"
)

type Store interface {
	SynchronizeTargets(context.Context, string, []DesiredTarget, time.Time) error
	Target(context.Context, string, int64) (Target, error)
	ClaimTarget(context.Context, string, string, string, time.Time, time.Duration) (Lease, bool, error)
	HeartbeatTarget(context.Context, Lease, time.Time, time.Duration) (Lease, error)
	RecordTargetReady(context.Context, Lease, ObservationReceipt, time.Time, time.Time) (Target, error)
	RecordTargetRetry(context.Context, Lease, string, bool, time.Time, time.Time) (Target, error)
	RecordReadiness(context.Context, Readiness) error
	RuntimeReady(context.Context, string, string, int, time.Time, time.Duration) error
}
