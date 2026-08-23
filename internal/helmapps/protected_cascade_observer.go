package helmapps

import (
	"context"
	"errors"
	"time"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

// ProtectedCascadeObserver records only bounded read receipts. It cannot
// mutate Argo or Kubernetes: Git remains the sole finalizer/delete authority.
type ProtectedCascadeObserver struct {
	Store           ProtectedCascadeObservationStore
	Activations     ProtectedObserverActivationStore
	Bindings        ProtectedGitBindingStore
	Provider        gitprojection.HeadVerifier
	Manager         *gitprojection.MirrorManager
	Argo            argo.DesiredStateRuntimeIdentity
	ArgoObservation argo.DesiredStateRuntimeWorkerObservation
	Roots           argo.PlatformRootCascadeSource
	Applications    argo.ProtectedApplicationSource
	Publisher       ProtectedPublisherIdentity
	WorkerID        string
	WorkerEpoch     int64
	LeaseDuration   time.Duration
	NewID           func() string
	Now             func() time.Time
}

func (o *ProtectedCascadeObserver) Validate() error {
	if o == nil || o.Store == nil || o.Activations == nil || o.Bindings == nil || o.Provider == nil || o.Manager == nil ||
		o.Manager.Validate() != nil || o.Argo.Validate() != nil || o.ArgoObservation.Validate() != nil ||
		o.ArgoObservation.DesiredStateRuntimeIdentity != o.Argo || o.Roots == nil || o.Applications == nil ||
		o.Publisher.Validate() != nil || !workerIDRE.MatchString(o.WorkerID) || o.WorkerEpoch < 1 ||
		o.NewID == nil || o.Now == nil {
		return ErrInvalid
	}
	if lease := o.leaseDuration(); !validProtectedLeaseDuration(lease) {
		return ErrInvalid
	}
	return nil
}

func (o *ProtectedCascadeObserver) leaseDuration() time.Duration {
	if o.LeaseDuration > 0 {
		return o.LeaseDuration
	}
	return defaultProtectedPublishLease
}

func (o *ProtectedCascadeObserver) ProcessOne(ctx context.Context) (result ProtectedApplicationCascadeReceipt, resultErr error) {
	if o.Validate() != nil || ctx == nil {
		return ProtectedApplicationCascadeReceipt{}, ErrInvalid
	}
	now := o.Now().UTC()
	activationEpoch, err := o.Activations.ActivateCascadeObserver(ctx, o.WorkerID,
		o.WorkerEpoch, o.Publisher, now)
	if err != nil {
		return ProtectedApplicationCascadeReceipt{}, err
	}
	if activationEpoch < 1 {
		return ProtectedApplicationCascadeReceipt{}, ErrConflict
	}
	return o.processOneActivated(ctx)
}

func (o *ProtectedCascadeObserver) processOneActivated(ctx context.Context) (result ProtectedApplicationCascadeReceipt, resultErr error) {
	if o.Validate() != nil || ctx == nil {
		return ProtectedApplicationCascadeReceipt{}, ErrInvalid
	}
	now := o.Now().UTC()
	preflight, lease, err := o.Store.ClaimCascadeObservation(ctx, o.WorkerID, o.WorkerEpoch,
		o.Publisher, now, o.leaseDuration())
	if err != nil {
		return ProtectedApplicationCascadeReceipt{}, err
	}
	defer func() {
		if resultErr == nil {
			return
		}
		retryNow := o.Now().UTC()
		next := retryNow.Add(5 * time.Second)
		if retryErr := o.Store.RetryCascadeObservation(context.WithoutCancel(ctx), lease,
			"cascade-observation-not-ready", next, retryNow); retryErr != nil && !errors.Is(retryErr, ErrConflict) {
			resultErr = errors.Join(resultErr, retryErr)
		}
	}()
	if preflight.Validate() != nil || preflight.State != ProtectedVerified {
		return ProtectedApplicationCascadeReceipt{}, ErrConflict
	}
	binding, err := o.Bindings.Binding(ctx, preflight.Binding.PlatformBindingID)
	if err != nil {
		return ProtectedApplicationCascadeReceipt{}, err
	}
	if binding.Validate() != nil || binding.Kind != gitprojection.BindingPlatform ||
		binding.CredentialMode != gitprojection.CredentialGitHubApp || binding.ID != preflight.Binding.PlatformBindingID ||
		binding.TargetRef != preflight.Binding.PlatformTargetRef {
		return ProtectedApplicationCascadeReceipt{}, ErrConflict
	}
	head, err := o.Provider.VerifyTargetHead(ctx, binding, gitprojection.ObservationWrite)
	if err != nil {
		return ProtectedApplicationCascadeReceipt{}, err
	}
	// Provider resolution is a network operation and stamps the head after the
	// observation lease was claimed. Use a fresh upper bound for that receipt;
	// comparing it with the pre-request claim time rejects every real provider
	// response whose clock is correctly later than the claim.
	now, err = cascadeObservationUpperBound(o.Now, head.ObservedAt)
	if err != nil || head.ValidateFor(binding) != nil {
		return ProtectedApplicationCascadeReceipt{}, ErrInvalid
	}
	receiptID := o.NewID()
	if !uuidRE.MatchString(receiptID) {
		return ProtectedApplicationCascadeReceipt{}, ErrInvalid
	}
	if err = o.Manager.CleanupOperation(ctx, binding.ID, receiptID); err != nil {
		return ProtectedApplicationCascadeReceipt{}, err
	}
	prepared, err := o.Manager.Prepare(ctx, binding, head, receiptID)
	if err != nil {
		return ProtectedApplicationCascadeReceipt{}, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), protectedPublishCleanupTimeout)
		defer cancel()
		_ = prepared.Close(cleanup)
	}()
	mutation, err := preflight.Mutation()
	if err != nil {
		return ProtectedApplicationCascadeReceipt{}, err
	}
	gitMutation, err := mutation.gitMutation()
	if err != nil {
		return ProtectedApplicationCascadeReceipt{}, err
	}
	adoptionRevision, adoptionParent := preflight.WriteBaseRevision, preflight.WriteBaseRevision
	if preflight.Operation == "update" {
		adoptionRevision, adoptionParent = preflight.CommittedRevision, preflight.CommittedParentRevision
	}
	for _, revision := range []string{adoptionRevision, preflight.PayloadRevision} {
		if !gitCommitRE.MatchString(revision) || prepared.VerifyAncestor(ctx, revision) != nil {
			return ProtectedApplicationCascadeReceipt{}, ErrConflict
		}
	}
	if err = prepared.VerifyProtectedMutationPostimage(ctx, gitMutation); err != nil {
		return ProtectedApplicationCascadeReceipt{}, err
	}
	rootExpectation, err := argo.NewPlatformRootApplicationExpectation(o.Argo, binding, head)
	if err != nil {
		return ProtectedApplicationCascadeReceipt{}, ErrInvalid
	}
	root, err := o.Roots.ObservePlatformRootApplicationForCascade(ctx, rootExpectation, now)
	if err != nil {
		return ProtectedApplicationCascadeReceipt{}, err
	}
	childExpectation, err := preflight.ApplicationExpectation()
	if err != nil {
		return ProtectedApplicationCascadeReceipt{}, err
	}
	child, err := o.Applications.ObserveProtectedApplication(ctx, childExpectation, now)
	if err != nil {
		return ProtectedApplicationCascadeReceipt{}, err
	}
	receipt := ProtectedApplicationCascadeReceipt{ID: receiptID,
		DeleteIntentID: preflight.DeleteIntentID, CascadePreflightID: preflight.ID,
		ObservationEpoch: 1, ObservationLeaseEpoch: lease.Epoch,
		ObserverActivationEpoch: lease.ObserverActivationEpoch, ReleaseRevisionID: preflight.ReleaseRevisionID,
		PayloadIntentID: preflight.PayloadIntentID, BaseApplicationIntentID: preflight.BaseApplicationIntentID,
		ProjectID: preflight.Target.ProjectID, EnvironmentID: preflight.Target.EnvironmentID,
		ApplicationID:   preflight.Target.ApplicationID,
		ApplicationPath: preflight.ApplicationPath, SourceContentDigest: preflight.SourceContentDigest,
		AdoptedContentDigest: preflight.AdoptedContentDigest, AdoptionRevision: adoptionRevision,
		AdoptionParentRevision: adoptionParent, ProviderHead: head.Commit,
		RootObservedRevision: root.ObservedRevision, RootUID: root.UID,
		RootSyncStatus:      root.SyncStatus,
		RootResourceVersion: root.ResourceVersion, RootSpecDigest: root.SpecDigest,
		ChildUID: child.UID, ChildResourceVersion: child.ResourceVersion,
		ChildSpecDigest: child.SpecDigest, FinalizerDigest: child.FinalizerDigest,
		ChildReleaseRevisionID: childExpectation.ReleaseRevisionID,
		ChildPayloadRevision:   childExpectation.TargetRevision,
		ChildPayloadPath:       childExpectation.PayloadPath, ChildPayloadDigest: childExpectation.PayloadDigest,
		Publisher: o.Publisher, WorkerID: o.WorkerID, WorkerEpoch: o.WorkerEpoch,
		ArgoContract: o.Argo.ContractVersion, ArgoConfigDigest: o.Argo.ConfigDigest, ObservedAt: now}
	if receipt.validateProposal(now) != nil {
		return ProtectedApplicationCascadeReceipt{}, ErrConflict
	}
	return o.Store.RecordCascadeObservation(ctx, lease, receipt, now)
}

func cascadeObservationUpperBound(clock func() time.Time, providerObservedAt time.Time) (time.Time, error) {
	if clock == nil || providerObservedAt.IsZero() {
		return time.Time{}, ErrInvalid
	}
	now := clock().UTC()
	if now.IsZero() || providerObservedAt.After(now) {
		return time.Time{}, ErrInvalid
	}
	return now, nil
}
