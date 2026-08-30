package environmentfoundation

import (
	"context"
	"errors"
	"time"
)

type DeletionState string

const (
	DeletionPending DeletionState = "pending"
	DeletionClaimed DeletionState = "claimed"
	DeletionReady   DeletionState = "ready"
	DeletionFailed  DeletionState = "failed"
)

type Deletion struct {
	ID, EnvironmentID, ProjectID, Namespace, ArgoProject string
	BindingID, TargetRef, RequiredAncestor, Path         string
	ExpectedManifestDigest, LastFailureCode              string
	State                                                DeletionState
	Attempts                                             int
	LeaseOwner                                           string
	LeaseEpoch                                           int64
	LeaseUntil                                           *time.Time
	NextAttemptAt                                        time.Time
	CommittedRevision, ProviderRequest                   string
	CompletedAt                                          *time.Time
	CreatedAt, UpdatedAt                                 time.Time
}

func (d Deletion) Validate() error {
	if !uuidRE.MatchString(d.ID) || !uuidRE.MatchString(d.EnvironmentID) || !uuidRE.MatchString(d.ProjectID) ||
		!dnsLabelRE.MatchString(d.Namespace) || !dnsLabelRE.MatchString(d.ArgoProject) || !uuidRE.MatchString(d.BindingID) ||
		!gitRefRE.MatchString(d.TargetRef) || !gitCommitRE.MatchString(d.RequiredAncestor) ||
		d.Path != ManifestPath(d.EnvironmentID) || !digestRE.MatchString(d.ExpectedManifestDigest) ||
		d.Attempts < 0 || d.Attempts > MaximumAttempts || d.CreatedAt.IsZero() || d.UpdatedAt.Before(d.CreatedAt) ||
		d.NextAttemptAt.Before(d.CreatedAt) || (d.LastFailureCode != "" && !failureRE.MatchString(d.LastFailureCode)) {
		return ErrInvalid
	}
	claimed := d.State == DeletionClaimed && workerIDRE.MatchString(d.LeaseOwner) && d.LeaseEpoch > 0 && d.LeaseUntil != nil && d.LeaseUntil.After(d.UpdatedAt)
	if (d.LeaseOwner != "" || d.LeaseUntil != nil || d.State == DeletionClaimed) != claimed {
		return ErrInvalid
	}
	switch d.State {
	case DeletionPending:
		if d.CompletedAt != nil || d.CommittedRevision != "" || d.ProviderRequest != "" {
			return ErrInvalid
		}
	case DeletionClaimed:
		if d.CompletedAt != nil {
			return ErrInvalid
		}
	case DeletionReady:
		if d.CompletedAt == nil || !gitCommitRE.MatchString(d.CommittedRevision) || !requestRE.MatchString(d.ProviderRequest) {
			return ErrInvalid
		}
	case DeletionFailed:
		if d.CompletedAt != nil || d.LastFailureCode == "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

type DeletionLease struct {
	Deletion Deletion
	Owner    string
	Epoch    int64
	Until    time.Time
}

type DeletionReceipt struct {
	OperationID, BindingID, TargetRef, Path string
	ParentRevision, CommittedRevision       string
	ProviderRequest                         string
	ObservedAt                              time.Time
}

type DeletionStore interface {
	ClaimDeletion(context.Context, string, time.Time, time.Duration) (DeletionLease, bool, error)
	RecordDeletionReady(context.Context, DeletionLease, DeletionReceipt, time.Time) error
	RecordDeletionRetry(context.Context, DeletionLease, string, bool, time.Time, time.Time) error
}

type ProtectedDeleter interface {
	Delete(context.Context, DeletionLease) (DeletionReceipt, error)
}

type DeletionController struct {
	Store                          DeletionStore
	Deleter                        ProtectedDeleter
	WorkerID                       string
	WorkLease                      time.Duration
	MinimumBackoff, MaximumBackoff time.Duration
	Now                            func() time.Time
}

func (c *DeletionController) Reconcile(ctx context.Context) (bool, error) {
	if c == nil || c.Store == nil || c.Deleter == nil || !workerIDRE.MatchString(c.WorkerID) ||
		c.WorkLease < MinimumLease || c.WorkLease > MaximumLease || c.MinimumBackoff < time.Second ||
		c.MaximumBackoff < c.MinimumBackoff || c.Now == nil {
		return false, ErrUnavailable
	}
	now := c.Now().UTC()
	lease, found, err := c.Store.ClaimDeletion(ctx, c.WorkerID, now, c.WorkLease)
	if err != nil || !found {
		return found, err
	}
	receipt, publishErr := c.Deleter.Delete(ctx, lease)
	if publishErr != nil {
		permanent := errors.Is(publishErr, ErrInvalid)
		code := "protected-git-unavailable"
		if permanent {
			code = "protected-git-rejected"
		}
		delay := c.MinimumBackoff
		for i := 1; i < lease.Deletion.Attempts && delay < c.MaximumBackoff/2; i++ {
			delay *= 2
		}
		if delay > c.MaximumBackoff {
			delay = c.MaximumBackoff
		}
		err = c.Store.RecordDeletionRetry(context.WithoutCancel(ctx), lease, code, permanent, now.Add(delay), now)
		if err != nil {
			return true, err
		}
		if !permanent {
			return true, ErrUnavailable
		}
		return true, nil
	}
	return true, c.Store.RecordDeletionReady(context.WithoutCancel(ctx), lease, receipt, c.Now().UTC())
}
