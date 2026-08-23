package argo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

type DesiredStateBindingStore interface {
	Binding(context.Context, string) (gitprojection.Binding, error)
}

// PlatformRootRefresher asks the one installer-owned root Application to
// immediately re-read its branch after a verified desired-state commit, then
// proves that Argo observed and reconciled the exact verified provider head.
// The protected command is not completed until this acknowledgement succeeds,
// so a stale provider cache, crash, or transient Kubernetes failure replays the
// same idempotent refresh.
type PlatformRootRefresher interface {
	RefreshPlatformRootApplication(context.Context, PlatformRootApplicationExpectation, time.Time) error
}

type EnvironmentApplicationSetExpectation struct {
	Namespace     string
	Name          string
	ProjectID     string
	EnvironmentID string
}

func NewEnvironmentApplicationSetExpectation(command DesiredStateCommand) (EnvironmentApplicationSetExpectation, error) {
	expectation := EnvironmentApplicationSetExpectation{Namespace: command.ArgoNamespace, Name: ApplicationSetName(command.EnvironmentID),
		ProjectID: command.ProjectID, EnvironmentID: command.EnvironmentID}
	if command.Validate() != nil || expectation.Validate() != nil {
		return EnvironmentApplicationSetExpectation{}, ErrInvalid
	}
	return expectation, nil
}

func (e EnvironmentApplicationSetExpectation) Validate() error {
	if !kubeRE.MatchString(e.Namespace) || !uuidRE.MatchString(e.ProjectID) || !uuidRE.MatchString(e.EnvironmentID) ||
		e.Name != ApplicationSetName(e.EnvironmentID) || !kubeRE.MatchString(e.Name) {
		return ErrInvalid
	}
	return nil
}

// EnvironmentApplicationSetRefresher asks Argo's ApplicationSet controller to
// regenerate exact child Applications after the verified root revision lands.
// It never invokes Argo's imperative sync API.
type EnvironmentApplicationSetRefresher interface {
	RefreshEnvironmentApplicationSet(context.Context, EnvironmentApplicationSetExpectation, time.Time) error
}

// DesiredStateWriter is the sole runtime mutation path for protected Argo
// manifests. It commits immutable server-derived bytes through the hardened
// Git mirror/token broker, then requests exact root-Application and environment
// ApplicationSet metadata refreshes; Argo's automated policy remains the sole
// sync executor.
type DesiredStateWriter struct {
	Store             DesiredStateStore
	Bindings          DesiredStateBindingStore
	ClaimGate         DesiredStateClaimGate
	Provider          gitprojection.HeadVerifier
	Manager           *gitprojection.MirrorManager
	RootRefresher     PlatformRootRefresher
	ApplicationSets   EnvironmentApplicationSetRefresher
	ObservationWaker  ObservationWaker
	Identity          DesiredStateRuntimeIdentity
	Now               func() time.Time
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
}

func (w *DesiredStateWriter) validate() error {
	if w == nil || w.Store == nil || w.Bindings == nil || w.ClaimGate == nil || w.Provider == nil || w.Manager == nil ||
		w.RootRefresher == nil || w.ApplicationSets == nil || w.ObservationWaker == nil || w.Identity.Validate() != nil {
		return ErrInvalid
	}
	leaseDuration, heartbeat := w.leaseSettings()
	if !validDesiredStateLeaseDuration(leaseDuration) || heartbeat < 5*time.Millisecond || heartbeat >= leaseDuration/2 {
		return ErrInvalid
	}
	return nil
}

func (w *DesiredStateWriter) leaseSettings() (time.Duration, time.Duration) {
	leaseDuration := w.LeaseDuration
	if leaseDuration == 0 {
		leaseDuration = maximumDesiredStateLease
	}
	heartbeat := w.HeartbeatInterval
	if heartbeat == 0 {
		heartbeat = 10 * time.Second
	}
	return leaseDuration, heartbeat
}

func (w *DesiredStateWriter) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *DesiredStateWriter) notBefore(values ...time.Time) time.Time {
	current := w.now()
	for _, value := range values {
		if current.Before(value) {
			current = value.UTC()
		}
	}
	return current
}

func (w *DesiredStateWriter) CommitClaim(ctx context.Context, lease DesiredStateLease) (DesiredStateCommand, error) {
	if w.validate() != nil || lease.Validate() != nil || lease.Contract != w.Identity.ContractVersion || lease.ConfigDigest != w.Identity.ConfigDigest {
		return DesiredStateCommand{}, ErrInvalid
	}
	command, err := w.Store.DesiredStateCommand(ctx, lease.CommandID)
	if err != nil {
		return DesiredStateCommand{}, err
	}
	if command.Lease == nil || *command.Lease != lease || !lease.Until.After(w.now()) ||
		(command.State != DesiredStateClaimed && command.State != DesiredStateGitCommitted) {
		return DesiredStateCommand{}, ErrLeaseLost
	}
	leaseDuration, heartbeat := w.leaseSettings()
	guard := newDesiredStateLeaseGuard(ctx, w.Store, lease, leaseDuration, heartbeat, w.now)
	defer guard.Close()
	guard.AdvanceTimeFloor(command.UpdatedAt)
	workContext := guard.Context()
	platform, environment, err := w.bindings(workContext, command)
	if err != nil {
		return DesiredStateCommand{}, guard.Result(fmt.Errorf("resolve bindings: %w", err))
	}
	head, err := w.Provider.VerifyTargetHead(workContext, platform, gitprojection.ObservationWrite)
	if err != nil {
		return DesiredStateCommand{}, guard.Result(fmt.Errorf("verify provider head: %w", err))
	}
	if head.ValidateFor(platform) != nil {
		return DesiredStateCommand{}, guard.Result(ErrInvalid)
	}
	claimMode := DesiredStateClaimRecovery
	if command.WriteBaseRevision == "" {
		claimMode = DesiredStateClaimActive
	}
	if claimMode == DesiredStateClaimActive && command.State != DesiredStateClaimed {
		return DesiredStateCommand{}, guard.Result(ErrInvalid)
	}
	if claimMode == DesiredStateClaimActive && (environment.State != gitprojection.BindingReady ||
		environment.TargetHeadRevision != environment.IndexedRevision || environment.IndexedRevision != command.EnvironmentRevision ||
		environment.ProjectionGeneration != command.EnvironmentGeneration) {
		return DesiredStateCommand{}, guard.Result(ErrDesiredStateProjectionSuperseded)
	}
	if err = w.ClaimGate.ValidateDesiredStateClaim(workContext, command, claimMode); err != nil {
		return DesiredStateCommand{}, guard.Result(fmt.Errorf("validate projection claim: %w", err))
	}
	if err = w.Manager.CleanupOperation(workContext, platform.ID, command.ID); err != nil {
		return DesiredStateCommand{}, guard.Result(fmt.Errorf("clean operation workspace: %w", err))
	}
	prepared, err := w.Manager.Prepare(workContext, platform, head, command.ID)
	if err != nil {
		return DesiredStateCommand{}, guard.Result(fmt.Errorf("prepare repository: %w", err))
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = prepared.Close(cleanup)
	}()

	if command.WriteBaseRevision == "" {
		if err = prepared.VerifyAncestor(workContext, command.BaseRevision); err != nil {
			return DesiredStateCommand{}, guard.Result(err)
		}
		if err = verifyDesiredStateWriteBasePrecondition(workContext, prepared, command); err != nil {
			return DesiredStateCommand{}, guard.Result(err)
		}
		receiptTime := w.notBefore(command.UpdatedAt, head.ObservedAt)
		err = guard.Do(func(current DesiredStateLease) error {
			var bindErr error
			command, bindErr = w.Store.BindDesiredStateWriteBase(workContext, current, head.Commit, head.ObservedAt, receiptTime)
			return bindErr
		})
		if err != nil {
			return DesiredStateCommand{}, guard.Result(err)
		}
		guard.AdvanceTimeFloor(command.UpdatedAt)
	} else if err = prepared.VerifyAncestor(workContext, command.WriteBaseRevision); err != nil {
		return DesiredStateCommand{}, guard.Result(fmt.Errorf("verify durable write base: %w", err))
	}

	if command.State == DesiredStateGitCommitted {
		result, recoverErr := w.recoverAcknowledged(workContext, command, guard, platform, prepared, head.Commit)
		if recoverErr != nil {
			recoverErr = fmt.Errorf("finalize acknowledged commit: %w", recoverErr)
		}
		return result, guard.Result(recoverErr)
	}
	if head.Commit != command.WriteBaseRevision {
		result, recoverErr := w.recoverUnacknowledged(workContext, command, guard, platform, prepared, head.Commit)
		return result, guard.Result(recoverErr)
	}
	if err = verifyDesiredStateWriteBasePrecondition(workContext, prepared, command); err != nil {
		return DesiredStateCommand{}, guard.Result(err)
	}
	revision, err := prepared.Commit(workContext, command.Mutation())
	if err != nil {
		return DesiredStateCommand{}, guard.Result(err)
	}
	var committed DesiredStateCommand
	err = guard.Do(func(current DesiredStateLease) error {
		var markErr error
		committed, markErr = w.Store.MarkDesiredStateGitCommitted(workContext, current, revision, w.notBefore(command.UpdatedAt))
		return markErr
	})
	if err != nil {
		return DesiredStateCommand{}, guard.Result(err)
	}
	guard.AdvanceTimeFloor(committed.UpdatedAt)
	verified, err := w.Provider.VerifyTargetHead(workContext, platform, gitprojection.ObservationWrite)
	if err != nil {
		return DesiredStateCommand{}, guard.Result(err)
	}
	if verified.ValidateFor(platform) != nil || verified.Commit != revision {
		return DesiredStateCommand{}, gitprojection.ErrProviderMismatch
	}
	if err = w.refreshArgoDesiredState(workContext, committed, platform, verified); err != nil {
		return DesiredStateCommand{}, guard.Result(fmt.Errorf("refresh Argo desired state: %w", err))
	}
	var completed DesiredStateCommand
	err = guard.Finish(func(current DesiredStateLease) error {
		var completeErr error
		completed, completeErr = w.Store.CompleteDesiredStateVerified(workContext, current, committed.CommittedRevision, w.notBefore(committed.UpdatedAt, verified.ObservedAt))
		return completeErr
	})
	return completed, guard.Result(err)
}

func verifyDesiredStateWriteBasePrecondition(ctx context.Context, prepared *gitprojection.PreparedRepository, command DesiredStateCommand) error {
	switch command.Precondition {
	case gitprojection.MutationCreateIfAbsent:
		return prepared.VerifyPathAbsent(ctx, command.Path)
	case gitprojection.MutationMatchETag:
		return prepared.VerifyPathContentETag(ctx, command.Path, command.ExpectedETag)
	default:
		return ErrInvalid
	}
}

func (w *DesiredStateWriter) bindings(ctx context.Context, command DesiredStateCommand) (gitprojection.Binding, gitprojection.Binding, error) {
	platform, err := w.Bindings.Binding(ctx, command.PlatformBindingID)
	if err != nil {
		return gitprojection.Binding{}, gitprojection.Binding{}, err
	}
	environment, err := w.Bindings.Binding(ctx, command.EnvironmentBindingID)
	if err != nil {
		return gitprojection.Binding{}, gitprojection.Binding{}, err
	}
	if platform.Validate() != nil || platform.Kind != gitprojection.BindingPlatform || platform.CredentialMode != gitprojection.CredentialGitHubApp ||
		platform.ID != w.Identity.PlatformBindingID || platform.ID != command.PlatformBindingID ||
		platform.TargetRef != command.PlatformTargetRef || environment.Validate() != nil ||
		environment.Kind != gitprojection.BindingEnvironment || environment.ID != command.EnvironmentBindingID ||
		environment.ProjectID != command.ProjectID || environment.EnvironmentID != command.EnvironmentID ||
		environment.TargetRef != command.EnvironmentTargetRef || command.Runtime != w.Identity.Runtime && command.WriteBaseRevision == "" ||
		command.ArgoNamespace != w.Identity.ArgoNamespace || command.DigestEnforcement != w.Identity.DigestEnforcement {
		return gitprojection.Binding{}, gitprojection.Binding{}, ErrInvalid
	}
	return platform, environment, nil
}

func (w *DesiredStateWriter) recoverUnacknowledged(ctx context.Context, command DesiredStateCommand, guard *desiredStateLeaseGuard, platform gitprojection.Binding, prepared *gitprojection.PreparedRepository, providerHead string) (DesiredStateCommand, error) {
	found, present, err := prepared.FindOperationCommit(ctx, command.Mutation())
	if err != nil {
		return DesiredStateCommand{}, err
	}
	if !present {
		return DesiredStateCommand{}, ErrDesiredStateWriteNotFound
	}
	if err = prepared.VerifyPathContentETag(ctx, command.Path, `"`+command.ContentSHA256+`"`); err != nil {
		return DesiredStateCommand{}, err
	}
	var committed DesiredStateCommand
	err = guard.Do(func(current DesiredStateLease) error {
		var markErr error
		committed, markErr = w.Store.MarkDesiredStateGitCommitted(ctx, current, found, w.notBefore(command.UpdatedAt))
		return markErr
	})
	if err != nil {
		return DesiredStateCommand{}, err
	}
	guard.AdvanceTimeFloor(committed.UpdatedAt)
	verified, err := w.Provider.VerifyTargetHead(ctx, platform, gitprojection.ObservationWrite)
	if err != nil {
		return DesiredStateCommand{}, err
	}
	if verified.ValidateFor(platform) != nil || verified.Commit != providerHead {
		return DesiredStateCommand{}, gitprojection.ErrProviderMismatch
	}
	if err = w.refreshArgoDesiredState(ctx, committed, platform, verified); err != nil {
		return DesiredStateCommand{}, fmt.Errorf("refresh Argo desired state: %w", err)
	}
	var completed DesiredStateCommand
	err = guard.Finish(func(current DesiredStateLease) error {
		var completeErr error
		completed, completeErr = w.Store.CompleteDesiredStateVerified(ctx, current, committed.CommittedRevision, w.notBefore(committed.UpdatedAt, verified.ObservedAt))
		return completeErr
	})
	return completed, err
}

func (w *DesiredStateWriter) recoverAcknowledged(ctx context.Context, command DesiredStateCommand, guard *desiredStateLeaseGuard, platform gitprojection.Binding, prepared *gitprojection.PreparedRepository, providerHead string) (DesiredStateCommand, error) {
	found, present, err := prepared.FindOperationCommit(ctx, command.Mutation())
	if err != nil {
		return DesiredStateCommand{}, err
	}
	if !present || found != command.CommittedRevision {
		return DesiredStateCommand{}, ErrConflict
	}
	if err = prepared.VerifyPathContentETag(ctx, command.Path, `"`+command.ContentSHA256+`"`); err != nil {
		return DesiredStateCommand{}, err
	}
	verified, err := w.Provider.VerifyTargetHead(ctx, platform, gitprojection.ObservationWrite)
	if err != nil {
		return DesiredStateCommand{}, err
	}
	if verified.ValidateFor(platform) != nil || verified.Commit != providerHead {
		return DesiredStateCommand{}, gitprojection.ErrProviderMismatch
	}
	if err = w.refreshArgoDesiredState(ctx, command, platform, verified); err != nil {
		return DesiredStateCommand{}, fmt.Errorf("refresh Argo desired state: %w", err)
	}
	var completed DesiredStateCommand
	err = guard.Finish(func(current DesiredStateLease) error {
		var completeErr error
		completed, completeErr = w.Store.CompleteDesiredStateVerified(ctx, current, command.CommittedRevision, w.notBefore(command.UpdatedAt, verified.ObservedAt))
		return completeErr
	})
	return completed, err
}

func (w *DesiredStateWriter) refreshArgoDesiredState(ctx context.Context, command DesiredStateCommand, platform gitprojection.Binding, head gitprojection.VerifiedHead) error {
	expectation, err := NewPlatformRootApplicationExpectation(w.Identity, platform, head)
	if err != nil {
		return err
	}
	refreshedAt := w.notBefore(head.ObservedAt)
	if err = w.RootRefresher.RefreshPlatformRootApplication(ctx, expectation, refreshedAt); err != nil {
		return err
	}
	applicationSet, err := NewEnvironmentApplicationSetExpectation(command)
	if err != nil {
		return err
	}
	if err = w.ApplicationSets.RefreshEnvironmentApplicationSet(ctx, applicationSet, w.notBefore(refreshedAt)); err != nil {
		return err
	}
	return w.ObservationWaker.WakeObservation(ctx, w.Identity.ArgoNamespace, w.notBefore(refreshedAt))
}

type desiredStateLeaseGuard struct {
	mu        sync.Mutex
	store     DesiredStateStore
	lease     DesiredStateLease
	duration  time.Duration
	now       func() time.Time
	timeFloor time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	lost      error
	closed    bool
	done      chan struct{}
}

func newDesiredStateLeaseGuard(parent context.Context, store DesiredStateStore, lease DesiredStateLease, duration, interval time.Duration, now func() time.Time) *desiredStateLeaseGuard {
	ctx, cancel := context.WithCancel(parent)
	guard := &desiredStateLeaseGuard{store: store, lease: lease, duration: duration, now: now, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go guard.run(interval)
	return guard
}

func (g *desiredStateLeaseGuard) run(interval time.Duration) {
	defer close(g.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-ticker.C:
			g.mu.Lock()
			if g.closed {
				g.mu.Unlock()
				return
			}
			heartbeatAt := g.now()
			if heartbeatAt.Before(g.timeFloor) {
				heartbeatAt = g.timeFloor
			}
			updated, err := g.store.HeartbeatDesiredState(g.ctx, g.lease, heartbeatAt, g.duration)
			if err != nil {
				g.lost = errors.Join(ErrLeaseLost, err)
				g.cancel()
				g.mu.Unlock()
				return
			}
			g.lease = updated
			g.mu.Unlock()
		}
	}
}

func (g *desiredStateLeaseGuard) Context() context.Context { return g.ctx }

func (g *desiredStateLeaseGuard) AdvanceTimeFloor(value time.Time) {
	g.mu.Lock()
	if g.timeFloor.Before(value) {
		g.timeFloor = value.UTC()
	}
	g.mu.Unlock()
}

func (g *desiredStateLeaseGuard) Do(operation func(DesiredStateLease) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lost != nil {
		return g.lost
	}
	if g.closed {
		return ErrLeaseLost
	}
	return operation(g.lease)
}

// Finish stops future heartbeats under the same mutex that fences the final
// terminal store transition. Without this boundary a heartbeat could race
// immediately after CompleteDesiredStateVerified clears the lease and report
// a false lease loss for a successfully completed command.
func (g *desiredStateLeaseGuard) Finish(operation func(DesiredStateLease) error) error {
	g.mu.Lock()
	if g.lost != nil {
		err := g.lost
		g.closed = true
		g.cancel()
		g.mu.Unlock()
		<-g.done
		return err
	}
	if g.closed {
		g.mu.Unlock()
		<-g.done
		return ErrLeaseLost
	}
	g.closed = true
	err := operation(g.lease)
	g.cancel()
	g.mu.Unlock()
	<-g.done
	return err
}

func (g *desiredStateLeaseGuard) Result(err error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lost != nil {
		return g.lost
	}
	return err
}

func (g *desiredStateLeaseGuard) Close() {
	g.mu.Lock()
	if !g.closed {
		g.closed = true
		g.cancel()
	}
	g.mu.Unlock()
	<-g.done
}

func IsPermanentDesiredStateError(err error) bool {
	return errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict) || errors.Is(err, gitprojection.ErrInvalid) ||
		errors.Is(err, gitprojection.ErrConflict) || errors.Is(err, gitprojection.ErrProviderMismatch) ||
		errors.Is(err, gitprojection.ErrDiverged) || errors.Is(err, gitprojection.ErrMissingRef)
}
