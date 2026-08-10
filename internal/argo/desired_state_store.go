package argo

import (
	"context"
	"time"
)

type DesiredStateWorkerIdentity struct {
	ContractVersion string `json:"contractVersion"`
	ConfigDigest    string `json:"configDigest"`
}

func (i DesiredStateWorkerIdentity) Validate() error {
	if i.ContractVersion != DesiredStateContract || !digestRE.MatchString(i.ConfigDigest) {
		return ErrInvalid
	}
	return nil
}

type DesiredStateWork struct {
	Command DesiredStateCommand `json:"command"`
	Lease   DesiredStateLease   `json:"lease"`
}

type DesiredStateRetry struct {
	FailureCode   string    `json:"failureCode"`
	NextAttemptAt time.Time `json:"nextAttemptAt"`
}

func (r DesiredStateRetry) Validate(now time.Time) error {
	if now.IsZero() || !failureCodeRE.MatchString(r.FailureCode) || r.NextAttemptAt.IsZero() || r.NextAttemptAt.Before(now) {
		return ErrInvalid
	}
	return nil
}

type DesiredStateStore interface {
	CreateDesiredState(context.Context, DesiredStateCommand) (bool, error)
	DesiredStateCommand(context.Context, string) (DesiredStateCommand, error)
	LatestDesiredState(context.Context, string, string) (DesiredStateStatus, error)
	ClaimDesiredState(context.Context, string, DesiredStateWorkerIdentity, time.Time, time.Duration) (DesiredStateWork, error)
	HeartbeatDesiredState(context.Context, DesiredStateLease, time.Time, time.Duration) (DesiredStateLease, error)
	BindDesiredStateWriteBase(context.Context, DesiredStateLease, string, time.Time, time.Time) (DesiredStateCommand, error)
	MarkDesiredStateGitCommitted(context.Context, DesiredStateLease, string, time.Time) (DesiredStateCommand, error)
	CompleteDesiredStateVerified(context.Context, DesiredStateLease, string, time.Time) (DesiredStateCommand, error)
	RetryDesiredState(context.Context, DesiredStateLease, DesiredStateRetry, time.Time) (DesiredStateCommand, error)
	FailDesiredState(context.Context, DesiredStateLease, string, time.Time) (DesiredStateCommand, error)
}
