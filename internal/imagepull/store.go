package imagepull

import (
	"context"
	"time"
)

type Store interface {
	EnsureArtifact(context.Context, DesiredArtifact, time.Time) (Artifact, error)
	Artifact(context.Context, ArtifactKey) (Artifact, error)
	ClaimArtifact(context.Context, string, string, string, time.Time, time.Duration) (Lease, bool, error)
	HeartbeatArtifact(context.Context, Lease, time.Time, time.Duration) (Lease, error)
	RecordArtifactReady(context.Context, Lease, string, string, time.Time, time.Time) (Artifact, error)
	RecordArtifactRetry(context.Context, Lease, string, bool, time.Time, time.Time) (Artifact, error)
	ActiveArtifactsHealthy(context.Context, time.Time) (bool, error)
	RecordReadiness(context.Context, Readiness) error
	RuntimeReady(context.Context, string, string, int, time.Time) error
}

// ConfigurationReconciler retires durable artifact rows that no longer match
// the operator's current namespace/profile authority. Rows remain available
// for explicit rollback, but inactive rows are never claimed by the worker.
type ConfigurationReconciler interface {
	RetireUnconfiguredArtifacts(context.Context, RuntimeConfig, time.Time) (int, error)
}
