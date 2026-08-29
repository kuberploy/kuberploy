package autodeploy

import (
	"context"
	"errors"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

type RunStore interface {
	ClaimNextRun(context.Context, string, time.Time, time.Duration) (Work, error)
	HeartbeatRun(context.Context, Lease, time.Time, time.Duration) (Lease, error)
	RetryRun(context.Context, Lease, string, time.Time, time.Time) error
	FailRun(context.Context, Lease, string, time.Time) error
	CompleteRun(context.Context, Lease, SubmissionReceipt, time.Time) error
}

type ReleaseVerifier interface {
	ResolveVerifiedRelease(context.Context, string) (VerifiedRelease, error)
}

// ServiceAuthorizer applies the app.edit automation scope semantics to an
// enabled service_accounts row and freshly resolves its current AccessGrant.
// No service-account bearer token is created, stored, or passed to a worker.
type ServiceAuthorizer interface {
	AuthorizeAutoDeploy(context.Context, string, domain.AutomationScope, string, string, string) error
}

// CanonicalDeploymentPipeline is the same server-authoritative path used by
// interactive deployment and build promotion. Its implementation freshly
// resolves Git direct-vs-PR publication, scheduling material, runtime secrets,
// registry pulls, edge/sslip/TLS, and Git/Argo readiness before persistence.
type CanonicalDeploymentPipeline interface {
	// SubmitAutoDeployment must honor context cancellation promptly. The
	// controller cancels it as soon as the fenced run lease cannot be renewed.
	SubmitAutoDeployment(context.Context, Submission) (SubmissionReceipt, error)
}

type Controller struct {
	Store          RunStore
	Releases       ReleaseVerifier
	Authorization  ServiceAuthorizer
	Deployments    CanonicalDeploymentPipeline
	Owner          string
	LeaseDuration  time.Duration
	Now            func() time.Time
	heartbeatEvery func(time.Duration) time.Duration
}

func (c *Controller) ReconcileNext(ctx context.Context) (bool, error) {
	if c == nil || c.Store == nil || c.Releases == nil || c.Authorization == nil || c.Deployments == nil ||
		c.Owner == "" || len(c.Owner) > 128 || c.LeaseDuration < 15*time.Second || c.LeaseDuration > 5*time.Minute {
		return false, ErrInvalid
	}
	now := c.now()
	work, err := c.Store.ClaimNextRun(ctx, c.Owner, now, c.LeaseDuration)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if validateWork(work) != nil {
		return true, c.Store.FailRun(ctx, work.Lease, "auto-deploy-input-invalid", c.now())
	}
	release, err := c.Releases.ResolveVerifiedRelease(ctx, work.Run.AttemptID)
	if err != nil {
		return true, c.handle(ctx, work, "auto-deploy-release-unavailable", err)
	}
	if release.Validate() != nil || release.AttemptID != work.Run.AttemptID ||
		release.DefinitionID != work.Run.DefinitionID || release.DefinitionDigest != work.Run.DefinitionDigest || release.ProjectID != work.Policy.ProjectID ||
		release.ApplicationID != work.Policy.ApplicationID || release.ReleaseID != work.Run.ReleaseID {
		return true, c.Store.FailRun(ctx, work.Lease, "auto-deploy-release-mismatch", c.now())
	}
	if err = c.Authorization.AuthorizeAutoDeploy(ctx, work.Revision.ServiceActorID, RequiredAutomationScope,
		work.Policy.ProjectID, work.Policy.EnvironmentID, work.Policy.ApplicationID); err != nil {
		return true, c.handle(ctx, work, "auto-deploy-authorization-unavailable", err)
	}
	lease, err := c.Store.HeartbeatRun(ctx, work.Lease, c.now(), c.LeaseDuration)
	if err != nil {
		return true, err
	}
	work.Lease = lease
	submission := Submission{ActorID: work.Revision.ServiceActorID, IdempotencyKey: work.Run.IdempotencyKey,
		RequestID: "auto-deploy/" + work.Run.AttemptID, AttemptID: work.Run.AttemptID, PolicyID: work.Policy.ID,
		PolicyRevision: work.Revision.Revision, ProjectID: work.Policy.ProjectID, ApplicationID: work.Policy.ApplicationID,
		EnvironmentID: work.Policy.EnvironmentID, Image: release.Image, ConfigIntent: append([]byte(nil), work.Revision.Template.ConfigIntent...),
		TemplateDigest: work.Revision.TemplateDigest, SourceDeploymentID: work.Revision.Template.SourceDeploymentID,
		SourceDeploymentGeneration: work.Revision.Template.SourceDeploymentGeneration, SourceConfigETag: work.Revision.Template.SourceConfigETag}
	if submission.Validate() != nil {
		return true, c.Store.FailRun(ctx, work.Lease, "auto-deploy-command-invalid", c.now())
	}
	receipt, lease, submitErr, heartbeatErr := c.submitWithHeartbeats(ctx, work.Lease, submission)
	if heartbeatErr != nil {
		// The submission may have been accepted immediately before the lease
		// loss became visible. Never mutate the run with the stale fence; the
		// deterministic idempotency key lets the next owner converge by replay.
		return true, heartbeatErr
	}
	work.Lease = lease
	if submitErr != nil {
		return true, c.handle(ctx, work, "auto-deploy-submit-unavailable", submitErr)
	}
	if !uuidRE.MatchString(receipt.OperationID) || !uuidRE.MatchString(receipt.DeploymentID) {
		return true, c.Store.FailRun(ctx, work.Lease, "auto-deploy-receipt-invalid", c.now())
	}
	return true, c.Store.CompleteRun(ctx, work.Lease, receipt, c.now())
}

type deploymentSubmissionResult struct {
	receipt SubmissionReceipt
	err     error
}

// submitWithHeartbeats keeps the only authority to submit fenced for the full
// canonical pipeline call. A final heartbeat fences the receipt even when the
// pipeline returns before the first periodic tick.
func (c *Controller) submitWithHeartbeats(ctx context.Context, lease Lease, submission Submission) (SubmissionReceipt, Lease, error, error) {
	submitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan deploymentSubmissionResult, 1)
	go func() {
		receipt, err := c.Deployments.SubmitAutoDeployment(submitCtx, submission)
		result <- deploymentSubmissionResult{receipt: receipt, err: err}
	}()

	ticker := time.NewTicker(c.heartbeatInterval())
	defer ticker.Stop()
	for {
		select {
		case completed := <-result:
			if completed.err != nil {
				return SubmissionReceipt{}, lease, completed.err, nil
			}
			updated, err := c.Store.HeartbeatRun(ctx, lease, c.now(), c.LeaseDuration)
			if err != nil {
				return SubmissionReceipt{}, lease, nil, err
			}
			return completed.receipt, updated, nil, nil
		case <-ticker.C:
			updated, err := c.Store.HeartbeatRun(ctx, lease, c.now(), c.LeaseDuration)
			if err != nil {
				cancel()
				// Do not return while an implementation could continue an unfenced
				// submission. Implementations are contractually cancellation-aware.
				<-result
				return SubmissionReceipt{}, lease, nil, err
			}
			lease = updated
		case <-ctx.Done():
			cancel()
			<-result
			return SubmissionReceipt{}, lease, nil, ctx.Err()
		}
	}
}

func (c *Controller) heartbeatInterval() time.Duration {
	if c.heartbeatEvery != nil {
		if interval := c.heartbeatEvery(c.LeaseDuration); interval > 0 {
			return interval
		}
	}
	interval := c.LeaseDuration / 3
	if interval < 5*time.Second {
		return 5 * time.Second
	}
	if interval > 30*time.Second {
		return 30 * time.Second
	}
	return interval
}

func validateWork(work Work) error {
	if work.Policy.Validate() != nil || work.Revision.ValidateFor(work.Policy) != nil || work.Run.Validate() != nil ||
		work.Policy.CurrentRevision != work.Revision.Revision || !work.Revision.Enabled || work.Run.PolicyID != work.Policy.ID ||
		work.Run.PolicyRevision != work.Revision.Revision || work.Run.TemplateDigest != work.Revision.TemplateDigest ||
		work.Run.SourceDeploymentID != work.Revision.Template.SourceDeploymentID ||
		work.Run.SourceDeploymentGeneration != work.Revision.Template.SourceDeploymentGeneration ||
		work.Run.SourceConfigETag != work.Revision.Template.SourceConfigETag ||
		work.Lease.AttemptID != work.Run.AttemptID || work.Lease.PolicyID != work.Policy.ID || work.Lease.Owner == "" ||
		work.Lease.Epoch < 1 || work.Lease.Until.IsZero() {
		return ErrInvalid
	}
	return nil
}

func (c *Controller) handle(ctx context.Context, work Work, code string, cause error) error {
	if errors.Is(cause, ErrInvalid) || errors.Is(cause, ErrConflict) {
		return c.Store.FailRun(ctx, work.Lease, code, c.now())
	}
	return c.Store.RetryRun(ctx, work.Lease, code, c.now(), c.now().Add(time.Minute))
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}
