package builds

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/autodeploy"
	"github.com/kuberploy/kuberploy/internal/buildpromotion"
	"github.com/kuberploy/kuberploy/internal/domain"
	platformstore "github.com/kuberploy/kuberploy/internal/store"
)

const APICommandSourceDeployment = "deployment.source-build"

const (
	SourceDeploymentClaimPollInterval = time.Second
	SourceDeploymentLease             = 30 * time.Second
	SourceDeploymentRetryBackoff      = time.Minute
)

var (
	configETagRE            = regexp.MustCompile(`^"(?:sha256:|cfg-sha256-)[0-9a-f]{64}"$`)
	sourceDeploymentImageRE = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
)

type SourceDeploymentMode string

const (
	SourceDeploymentDeploy  SourceDeploymentMode = "deploy"
	SourceDeploymentRebuild SourceDeploymentMode = "rebuild"
)

type SourceDeploymentIntentState string

const (
	SourceDeploymentPending    SourceDeploymentIntentState = "pending"
	SourceDeploymentProcessing SourceDeploymentIntentState = "processing"
	SourceDeploymentSubmitted  SourceDeploymentIntentState = "submitted"
	SourceDeploymentFailed     SourceDeploymentIntentState = "failed"
	SourceDeploymentSuperseded SourceDeploymentIntentState = "superseded"
)

// SourceDeploymentCommand is fully server-derived except for mode and the
// optional historical attempt selected for Rebuild. AppConfig intent is
// canonical and excludes the image and other server-derived runtime fields.
type SourceDeploymentCommand struct {
	ActorID, ProjectID, ApplicationID, EnvironmentID, DeploymentID string
	Mode                                                           SourceDeploymentMode
	DefinitionID, ExpectedDefinitionDigest                         string
	SourceAttemptID, CommitSHA                                     string
	Execution                                                      ExecutionSettings
	SourceDeploymentGeneration                                     int64
	SourceConfigETag                                               string
	ConfigIntent                                                   []byte
	TemplateDigest                                                 string
	IdempotencyKey, Fingerprint, RequestID                         string
	AcceptedAt                                                     time.Time
	StartDraft                                                     bool
}

type SourceDeploymentIntent struct {
	ID, AttemptID, ActorID, ProjectID, ApplicationID, EnvironmentID, DeploymentID string
	Mode                                                                          SourceDeploymentMode
	SourceAttemptID                                                               string
	Sequence                                                                      int64
	DefinitionID, DefinitionDigest                                                string
	SourceDeploymentGeneration                                                    int64
	SourceConfigETag, TemplateDigest, RequestID                                   string
	ConfigIntent                                                                  []byte
	State                                                                         SourceDeploymentIntentState
	Attempts                                                                      int
	AvailableAt                                                                   time.Time
	LeaseOwner                                                                    string
	LeaseUntil                                                                    time.Time
	LeaseEpoch                                                                    int64
	OperationID                                                                   string
	FailureCode                                                                   string
	CreatedAt, UpdatedAt                                                          time.Time
	CompletedAt                                                                   *time.Time
	StartDraft                                                                    bool
}

type SourceDeploymentAcceptance struct {
	Attempt BuildAttempt
	Intent  SourceDeploymentIntent
	Replay  bool
}

type SourceDeploymentLeaseToken struct {
	IntentID string
	Owner    string
	Epoch    int64
	Until    time.Time
}

type SourceDeploymentWork struct {
	Intent SourceDeploymentIntent
	Lease  SourceDeploymentLeaseToken
}

type SourceDeploymentSubmission struct {
	IntentID, ActorID, RequestID, AttemptID, ProjectID, ApplicationID, EnvironmentID, DeploymentID string
	Sequence, SourceDeploymentGeneration                                                           int64
	Image, SourceConfigETag, TemplateDigest                                                        string
	ConfigIntent                                                                                   []byte
	StartDraft                                                                                     bool
}

type SourceDeploymentSubmissionReceipt struct {
	OperationID, DeploymentID string
}

type SourceDeploymentStore interface {
	ClaimNextSourceDeployment(context.Context, string, time.Time, time.Duration) (SourceDeploymentWork, error)
	HeartbeatSourceDeployment(context.Context, SourceDeploymentLeaseToken, time.Time, time.Duration) (SourceDeploymentLeaseToken, error)
	RetrySourceDeployment(context.Context, SourceDeploymentLeaseToken, string, time.Time, time.Time) error
	FailSourceDeployment(context.Context, SourceDeploymentLeaseToken, string, time.Time) error
	CompleteSourceDeployment(context.Context, SourceDeploymentLeaseToken, SourceDeploymentSubmissionReceipt, time.Time) error
}

type SourceDeploymentResolver interface {
	Resolve(context.Context, buildpromotion.Request) (buildpromotion.Source, error)
}

type SourceDeploymentAuthorizer interface {
	Authorize(context.Context, string, domain.Permission, domain.AccessTarget) error
}

type SourceDeploymentPipeline interface {
	SubmitSourceDeployment(context.Context, SourceDeploymentSubmission) (SourceDeploymentSubmissionReceipt, error)
}

type SourceDeploymentController struct {
	Store         SourceDeploymentStore
	Releases      SourceDeploymentResolver
	Authorization SourceDeploymentAuthorizer
	Deployments   SourceDeploymentPipeline
	Owner         string
	LeaseDuration time.Duration
	Now           func() time.Time
}

func (c *SourceDeploymentController) ReconcileNext(ctx context.Context) (bool, error) {
	if c == nil || c.Store == nil || c.Releases == nil || c.Authorization == nil || c.Deployments == nil ||
		c.Owner == "" || len(c.Owner) > 128 || c.LeaseDuration < 15*time.Second || c.LeaseDuration > 5*time.Minute {
		return false, ErrInvalid
	}
	work, err := c.Store.ClaimNextSourceDeployment(ctx, c.Owner, c.now(), c.LeaseDuration)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if work.Intent.validate() != nil || work.Lease.IntentID != work.Intent.ID {
		return true, c.Store.FailSourceDeployment(ctx, work.Lease, "source-deploy-input-invalid", c.now())
	}
	if err = c.Authorization.Authorize(ctx, work.Intent.ActorID, domain.PermissionBuildsManage,
		domain.AccessTarget{Type: "application", ID: work.Intent.ApplicationID}); err != nil {
		return true, c.handle(ctx, work, "source-deploy-authorization-unavailable", err)
	}
	source, err := c.Releases.Resolve(ctx, buildpromotion.Request{ActorID: work.Intent.ActorID,
		AttemptID: work.Intent.AttemptID, EnvironmentID: work.Intent.EnvironmentID})
	if err != nil {
		return true, c.handle(ctx, work, "source-deploy-release-unavailable", err)
	}
	if source.AttemptID != work.Intent.AttemptID || source.ProjectID != work.Intent.ProjectID ||
		source.ApplicationID != work.Intent.ApplicationID || source.EnvironmentID != work.Intent.EnvironmentID ||
		source.DefinitionID != work.Intent.DefinitionID || source.DefinitionDigest != work.Intent.DefinitionDigest {
		return true, c.Store.FailSourceDeployment(ctx, work.Lease, "source-deploy-release-mismatch", c.now())
	}
	lease, err := c.Store.HeartbeatSourceDeployment(ctx, work.Lease, c.now(), c.LeaseDuration)
	if err != nil {
		return true, err
	}
	work.Lease = lease
	submission := SourceDeploymentSubmission{IntentID: work.Intent.ID, ActorID: work.Intent.ActorID,
		RequestID: work.Intent.RequestID, AttemptID: work.Intent.AttemptID, ProjectID: work.Intent.ProjectID,
		ApplicationID: work.Intent.ApplicationID, EnvironmentID: work.Intent.EnvironmentID, DeploymentID: work.Intent.DeploymentID,
		Sequence: work.Intent.Sequence, SourceDeploymentGeneration: work.Intent.SourceDeploymentGeneration,
		Image: source.ImageReference, SourceConfigETag: work.Intent.SourceConfigETag,
		TemplateDigest: work.Intent.TemplateDigest, ConfigIntent: append([]byte(nil), work.Intent.ConfigIntent...),
		StartDraft: work.Intent.StartDraft}
	if submission.Validate() != nil {
		return true, c.Store.FailSourceDeployment(ctx, work.Lease, "source-deploy-command-invalid", c.now())
	}
	receipt, lease, submitErr, heartbeatErr := c.submitWithHeartbeats(ctx, work.Lease, submission)
	if heartbeatErr != nil {
		return true, heartbeatErr
	}
	if submitErr != nil {
		return true, c.handle(ctx, work, "source-deploy-submit-unavailable", submitErr)
	}
	return true, c.Store.CompleteSourceDeployment(ctx, lease, receipt, c.now())
}

type sourceDeploymentSubmissionResult struct {
	receipt SourceDeploymentSubmissionReceipt
	err     error
}

func (c *SourceDeploymentController) submitWithHeartbeats(ctx context.Context, lease SourceDeploymentLeaseToken, submission SourceDeploymentSubmission) (SourceDeploymentSubmissionReceipt, SourceDeploymentLeaseToken, error, error) {
	submitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan sourceDeploymentSubmissionResult, 1)
	go func() {
		receipt, err := c.Deployments.SubmitSourceDeployment(submitCtx, submission)
		result <- sourceDeploymentSubmissionResult{receipt: receipt, err: err}
	}()
	ticker := time.NewTicker(c.LeaseDuration / 3)
	defer ticker.Stop()
	for {
		select {
		case completed := <-result:
			if completed.err != nil {
				return SourceDeploymentSubmissionReceipt{}, lease, completed.err, nil
			}
			updated, err := c.Store.HeartbeatSourceDeployment(ctx, lease, c.now(), c.LeaseDuration)
			return completed.receipt, updated, nil, err
		case <-ticker.C:
			updated, err := c.Store.HeartbeatSourceDeployment(ctx, lease, c.now(), c.LeaseDuration)
			if err != nil {
				cancel()
				<-result
				return SourceDeploymentSubmissionReceipt{}, lease, nil, err
			}
			lease = updated
		case <-ctx.Done():
			cancel()
			<-result
			return SourceDeploymentSubmissionReceipt{}, lease, nil, ctx.Err()
		}
	}
}

func (c *SourceDeploymentController) handle(ctx context.Context, work SourceDeploymentWork, code string, cause error) error {
	if errors.Is(cause, ErrInvalid) || errors.Is(cause, ErrConflict) || errors.Is(cause, buildpromotion.ErrInvalid) ||
		errors.Is(cause, buildpromotion.ErrConflict) || errors.Is(cause, buildpromotion.ErrArtifactUnavailable) ||
		errors.Is(cause, autodeploy.ErrInvalid) || errors.Is(cause, autodeploy.ErrConflict) ||
		errors.Is(cause, platformstore.ErrForbidden) || errors.Is(cause, platformstore.ErrNotFound) ||
		errors.Is(cause, platformstore.ErrPreconditionFailed) || errors.Is(cause, platformstore.ErrIdempotencyConflict) {
		return c.Store.FailSourceDeployment(ctx, work.Lease, code, c.now())
	}
	return c.Store.RetrySourceDeployment(ctx, work.Lease, code, c.now(), c.now().Add(SourceDeploymentRetryBackoff))
}

func (c *SourceDeploymentController) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (command SourceDeploymentCommand) validate() error {
	if !uuidRE.MatchString(command.ActorID) || !uuidRE.MatchString(command.ProjectID) || !uuidRE.MatchString(command.ApplicationID) ||
		!uuidRE.MatchString(command.EnvironmentID) || !uuidRE.MatchString(command.DeploymentID) || !uuidRE.MatchString(command.DefinitionID) ||
		!digestRE.MatchString(command.ExpectedDefinitionDigest) || !commitRE.MatchString(command.CommitSHA) || command.SourceDeploymentGeneration < 1 ||
		!setupIdempotencyRE.MatchString(command.IdempotencyKey) || !setupFingerprintRE.MatchString(command.Fingerprint) || command.RequestID == "" ||
		len(command.RequestID) > 256 || command.AcceptedAt.IsZero() {
		return ErrInvalid
	}
	if command.SourceConfigETag == "" {
		if !command.StartDraft || len(command.ConfigIntent) != 0 || command.TemplateDigest != "" {
			return ErrInvalid
		}
	} else if !configETagRE.MatchString(command.SourceConfigETag) || !appconfig.ValidateAutoDeployIntentTemplate(command.ConfigIntent, command.TemplateDigest) {
		return ErrInvalid
	}
	switch command.Mode {
	case SourceDeploymentDeploy:
		if command.SourceAttemptID != "" {
			return ErrInvalid
		}
	case SourceDeploymentRebuild:
		if !uuidRE.MatchString(command.SourceAttemptID) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func (intent SourceDeploymentIntent) validate() error {
	if !uuidRE.MatchString(intent.ID) || !uuidRE.MatchString(intent.AttemptID) || !uuidRE.MatchString(intent.ActorID) ||
		!uuidRE.MatchString(intent.ProjectID) || !uuidRE.MatchString(intent.ApplicationID) || !uuidRE.MatchString(intent.EnvironmentID) ||
		!uuidRE.MatchString(intent.DeploymentID) || intent.Sequence < 1 || !uuidRE.MatchString(intent.DefinitionID) ||
		!digestRE.MatchString(intent.DefinitionDigest) || intent.SourceDeploymentGeneration < 1 || intent.RequestID == "" || len(intent.RequestID) > 256 ||
		intent.Attempts < 0 || intent.Attempts > 20 ||
		intent.AvailableAt.IsZero() || intent.CreatedAt.IsZero() || intent.UpdatedAt.Before(intent.CreatedAt) {
		return ErrInvalid
	}
	if intent.SourceConfigETag == "" {
		if !intent.StartDraft || len(intent.ConfigIntent) != 0 || intent.TemplateDigest != "" {
			return ErrInvalid
		}
	} else if !configETagRE.MatchString(intent.SourceConfigETag) || !appconfig.ValidateAutoDeployIntentTemplate(intent.ConfigIntent, intent.TemplateDigest) {
		return ErrInvalid
	}
	switch intent.Mode {
	case SourceDeploymentDeploy:
		if intent.SourceAttemptID != "" {
			return ErrInvalid
		}
	case SourceDeploymentRebuild:
		if !uuidRE.MatchString(intent.SourceAttemptID) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	switch intent.State {
	case SourceDeploymentPending:
		if intent.CompletedAt != nil || intent.LeaseOwner != "" || !intent.LeaseUntil.IsZero() || intent.OperationID != "" {
			return ErrInvalid
		}
	case SourceDeploymentProcessing:
		if intent.CompletedAt != nil || intent.LeaseOwner == "" || intent.LeaseUntil.IsZero() || intent.LeaseEpoch < 1 || intent.OperationID != "" {
			return ErrInvalid
		}
	case SourceDeploymentSubmitted:
		if intent.CompletedAt == nil || !uuidRE.MatchString(intent.OperationID) || intent.LeaseOwner != "" || !intent.LeaseUntil.IsZero() || intent.FailureCode != "" {
			return ErrInvalid
		}
	case SourceDeploymentFailed, SourceDeploymentSuperseded:
		if intent.CompletedAt == nil || intent.OperationID != "" || intent.LeaseOwner != "" || !intent.LeaseUntil.IsZero() || intent.FailureCode == "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func (submission SourceDeploymentSubmission) Validate() error {
	if !uuidRE.MatchString(submission.IntentID) || !uuidRE.MatchString(submission.ActorID) || submission.RequestID == "" || len(submission.RequestID) > 256 ||
		!uuidRE.MatchString(submission.AttemptID) || !uuidRE.MatchString(submission.ProjectID) || !uuidRE.MatchString(submission.ApplicationID) ||
		!uuidRE.MatchString(submission.EnvironmentID) || !uuidRE.MatchString(submission.DeploymentID) || submission.Sequence < 1 ||
		submission.SourceDeploymentGeneration < 1 ||
		!sourceDeploymentImageRE.MatchString(submission.Image) {
		return ErrInvalid
	}
	if submission.SourceConfigETag == "" {
		if !submission.StartDraft || len(submission.ConfigIntent) != 0 || submission.TemplateDigest != "" {
			return ErrInvalid
		}
	} else if !configETagRE.MatchString(submission.SourceConfigETag) || !appconfig.ValidateAutoDeployIntentTemplate(submission.ConfigIntent, submission.TemplateDigest) {
		return ErrInvalid
	}
	return nil
}

func SourceDeploymentIntentID(attemptID, deploymentID string) string {
	return deterministicUUID("source-deployment-intent-v1", attemptID, deploymentID)
}
