package store

import (
	"context"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

// MinimumRegistryGarbageCollectionInterval bounds expensive target-wide
// offline sweeps. Manifest cleanup can be planned at any time, but a new plan
// that needs blob GC is not dispatched until the previous sweep is at least
// this old. In-progress and recovery work is never delayed by this throttle.
const MinimumRegistryGarbageCollectionInterval = time.Hour

// RegistryObservationLease fences a complete registry inventory/catalog
// publication. Revision is allocated durably and is reused when expired work
// is reclaimed; Epoch changes on every ownership change.
type RegistryObservationLease struct {
	TargetID string
	Owner    string
	Epoch    int64
	Revision int64
	Until    time.Time
}

type RegistryObservationWork struct {
	Target              domain.RegistryTarget
	Lease               RegistryObservationLease
	ConsecutiveFailures int
	Reclaimed           bool
}

type RegistryObservationPublication struct {
	Inventory  domain.RegistryInventoryObservation
	Catalogs   []domain.RegistryCatalogSnapshot
	ObservedAt time.Time
	NextAt     time.Time
}

type RegistryObservationOutcome struct {
	FailureCode string
	NextAt      time.Time
}

// RegistryMaintenanceLease is target-wide. The database epoch fences stale
// workers, while the deterministic Kubernetes Job UID fences a provider sweep
// across process crashes.
type RegistryMaintenanceLease struct {
	TargetID             string
	PlanID               string
	ExecutionKey         string
	CandidateSetDigest   string
	Owner                string
	Epoch                int64
	Until                time.Time
	State                string
	Mode                 string
	DeploymentUID        string
	OriginalReplicas     int32
	CheckpointRevision   string
	CheckpointDigest     string
	CheckpointObservedAt time.Time
	SweepJobUID          string
	RestoredAt           time.Time
	ReleasedAt           time.Time
}

type RegistryGCSweepReceipt struct {
	TargetID           string
	ExecutionKey       string
	PlanID             string
	CandidateSetDigest string
	CheckpointRevision string
	ProviderSweepID    string
	HelperJobUID       string
	StartedAt          time.Time
	CompletedAt        time.Time
}

// RegistryRuntimeStore is deliberately separate from Store. A registry
// controller receives no access to users, tokens, Git credentials, or tenant
// mutation surfaces.
type RegistryRuntimeStore interface {
	ClaimRegistryObservation(context.Context, string, string, time.Time, time.Duration) (RegistryObservationWork, error)
	HeartbeatRegistryObservation(context.Context, RegistryObservationLease, time.Time, time.Duration) (RegistryObservationLease, error)
	PublishRegistryObservation(context.Context, RegistryObservationLease, RegistryObservationPublication) error
	FailRegistryObservation(context.Context, RegistryObservationLease, RegistryObservationOutcome, time.Time) error
	RegistryObservationRoots(context.Context, string) (map[string][]string, error)
	NextAcceptedRegistryCleanup(context.Context, string, time.Time) (string, error)

	AcquireRegistryMaintenance(context.Context, string, string, string, string, string, time.Time, time.Duration) (RegistryMaintenanceLease, error)
	HeartbeatRegistryMaintenance(context.Context, RegistryMaintenanceLease, time.Time, time.Duration) (RegistryMaintenanceLease, error)
	PrepareRegistryMaintenanceStop(context.Context, RegistryMaintenanceLease, string, int32, time.Time) (RegistryMaintenanceLease, error)
	EnterRegistryMaintenance(context.Context, RegistryMaintenanceLease, string, int32, string, time.Time) (RegistryMaintenanceLease, error)
	RecordRegistryCheckpoint(context.Context, RegistryMaintenanceLease, string, string, time.Time, time.Time) (RegistryMaintenanceLease, error)
	RegistryGCSweepReceipt(context.Context, RegistryMaintenanceLease, time.Time) (RegistryGCSweepReceipt, bool, error)
	BeginRegistryGCSweep(context.Context, RegistryMaintenanceLease, string, time.Time) (RegistryGCSweepReceipt, bool, error)
	CompleteRegistryGCSweep(context.Context, RegistryMaintenanceLease, RegistryGCSweepReceipt, time.Time) error
	MarkRegistryMaintenanceRestored(context.Context, RegistryMaintenanceLease, time.Time) error
	ReleaseRegistryMaintenance(context.Context, RegistryMaintenanceLease, time.Time) error
}
