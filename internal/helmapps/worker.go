package helmapps

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ChartArtifact is returned by the credentialed fetch boundary. The renderer
// Job never receives registry credentials; it receives only these already
// fetched bytes after both immutable OCI identities have been checked.
type ChartArtifact struct {
	ManifestDigest string
	PackageDigest  string
	PackageBytes   []byte
}

type ChartPackageSource interface {
	Fetch(context.Context, Approval) (ChartArtifact, error)
}

type Worker struct {
	Store                Store
	Packages             ChartPackageSource
	Renderer             RenderExecutor
	LeaseDuration        time.Duration
	Now                  func() time.Time
	OperatorConfigDigest string
}

func (w Worker) Validate() error {
	if w.Store == nil || w.Packages == nil || w.Renderer == nil || w.Now == nil ||
		w.LeaseDuration < RenderTimeout+5*time.Second || w.LeaseDuration > 15*time.Minute ||
		!validDigest(w.OperatorConfigDigest) {
		return ErrInvalid
	}
	return nil
}

// ProcessOne claims one command and renders it twice through the same fixed,
// network-off plan. Only byte-identical output is accepted. Policy and schema
// failures are terminal; fetch/process failures retry with the durable bound.
func (w Worker) ProcessOne(ctx context.Context, owner string) (RenderResult, error) {
	if w.Validate() != nil || !workerIDRE.MatchString(owner) {
		return RenderResult{}, ErrInvalid
	}
	now := w.Now().UTC()
	lease, err := w.Store.Claim(ctx, owner, ExpectedRenderWorkerIdentity(w.OperatorConfigDigest), now, w.LeaseDuration)
	if err != nil {
		return RenderResult{}, err
	}
	renderContext, cancel := context.WithTimeout(ctx, RenderTimeout)
	defer cancel()

	approval, err := w.Store.Approval(renderContext, lease.Command.Approval)
	if err != nil {
		permanent := errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict)
		return RenderResult{}, w.recordFailure(ctx, lease, "approval-unavailable", !permanent, err)
	}
	artifact, err := w.Packages.Fetch(renderContext, approval)
	if err != nil {
		return RenderResult{}, w.recordFailure(ctx, lease, failureCode(err, "chart-fetch-failed"), !permanentRenderError(err), err)
	}
	if artifact.ManifestDigest != approval.ManifestDigest || artifact.PackageDigest != approval.PackageDigest ||
		digestBytes(artifact.PackageBytes) != approval.PackageDigest {
		return RenderResult{}, w.recordFailure(ctx, lease, "chart-identity-mismatch", false, ErrUnsafeChart)
	}
	bundle, err := InspectChartPackage(approval, artifact.PackageBytes)
	if err != nil {
		return RenderResult{}, w.recordFailure(ctx, lease, "chart-policy-rejected", false, err)
	}
	plan, err := NewRenderPlan(approval, lease.Command.DesiredRender, bundle)
	if err != nil {
		return RenderResult{}, w.recordFailure(ctx, lease, "values-policy-rejected", false, err)
	}
	firstPlan, secondPlan := cloneRenderPlan(plan), cloneRenderPlan(plan)
	firstInvocation := RenderInvocation{CommandID: lease.Command.ID, Attempt: lease.Command.Attempts, Pass: 1}
	secondInvocation := RenderInvocation{CommandID: lease.Command.ID, Attempt: lease.Command.Attempts, Pass: 2}
	first, err := w.Renderer.Render(renderContext, firstPlan, firstInvocation)
	if err != nil {
		return RenderResult{}, w.recordFailure(ctx, lease, failureCode(err, "renderer-failed"), !permanentRenderError(err), err)
	}
	if len(first) == 0 || len(first) > MaximumOutputSize {
		return RenderResult{}, w.recordFailure(ctx, lease, "renderer-output-bounds", false, ErrUnsafeChart)
	}
	second, err := w.Renderer.Render(renderContext, secondPlan, secondInvocation)
	if err != nil {
		return RenderResult{}, w.recordFailure(ctx, lease, failureCode(err, "renderer-failed"), !permanentRenderError(err), err)
	}
	if len(second) == 0 || len(second) > MaximumOutputSize {
		return RenderResult{}, w.recordFailure(ctx, lease, "renderer-output-bounds", false, ErrUnsafeChart)
	}
	if !equalBytes(first, second) {
		return RenderResult{}, w.recordFailure(ctx, lease, "nondeterministic-render", false, ErrNondeterministic)
	}
	validated, err := ValidateRenderedManifests(first, plan.Descriptor)
	if err != nil {
		return RenderResult{}, w.recordFailure(ctx, lease, "manifest-policy-rejected", false, err)
	}
	completedAt := w.Now().UTC()
	result, err := w.Store.Complete(ctx, lease, validated, completedAt)
	if err != nil {
		return RenderResult{}, err
	}
	return result, nil
}

// Readiness creates only an exact, leased observation. Consumers still decide
// capability separately and must never infer it merely from schema presence.
func (w Worker) Readiness(workerID string, epoch int64, startedAt, observedAt time.Time, duration time.Duration) (Readiness, error) {
	if w.Validate() != nil || !workerIDRE.MatchString(workerID) || epoch <= 0 ||
		duration < time.Second || duration > 15*time.Minute {
		return Readiness{}, ErrInvalid
	}
	readiness := Readiness{WorkerID: workerID, WorkerEpoch: epoch,
		RenderWorkerIdentity: ExpectedRenderWorkerIdentity(w.OperatorConfigDigest), StartedAt: startedAt.UTC(),
		ObservedAt: observedAt.UTC(), LeaseUntil: observedAt.UTC().Add(duration)}
	if readiness.Validate() != nil {
		return Readiness{}, ErrInvalid
	}
	return readiness, nil
}

func (w Worker) recordFailure(ctx context.Context, lease RenderLease, code string, retryable bool, cause error) error {
	now := w.Now().UTC()
	if _, err := w.Store.Fail(ctx, lease, code, retryable, now); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func permanentRenderError(err error) bool {
	return errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict) || errors.Is(err, ErrUnsafeChart) ||
		errors.Is(err, ErrUnsafeYAML) || errors.Is(err, ErrNondeterministic)
}

func failureCode(err error, fallback string) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "renderer-timeout"
	}
	if failureCodeRE.MatchString(fallback) {
		return fallback
	}
	return fmt.Sprintf("renderer-%x", digestBytes([]byte(fallback))[7:15])
}
