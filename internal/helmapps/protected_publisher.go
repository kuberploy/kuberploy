package helmapps

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

const (
	defaultProtectedPublishLease     = 90 * time.Second
	defaultProtectedPublishHeartbeat = 10 * time.Second
	protectedPublishCleanupTimeout   = 15 * time.Second
)

type ProtectedGitBindingStore interface {
	Binding(context.Context, string) (gitprojection.Binding, error)
}

// ProtectedGitPublisher adapts durable Helm publication intents to the same
// hardened gitprojection mirror, provider verifier, credential broker, and
// mutation transport used by Kuberploy's other protected Git writers. It does
// not advertise readiness or enable the Helm capability; production wiring
// must do that only after this worker and protected Argo are observed ready.
type ProtectedGitPublisher struct {
	Store             ProtectedPublicationStore
	Bindings          ProtectedGitBindingStore
	Provider          gitprojection.HeadVerifier
	Manager           *gitprojection.MirrorManager
	Publisher         ProtectedPublisherIdentity
	WorkerID          string
	WorkerEpoch       int64
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	Now               func() time.Time
}

func (p *ProtectedGitPublisher) Validate() error {
	if p == nil || p.Store == nil || p.Bindings == nil || p.Provider == nil || p.Manager == nil ||
		p.Manager.Validate() != nil || p.Publisher.Validate() != nil ||
		!workerIDRE.MatchString(p.WorkerID) || p.WorkerEpoch < 1 || p.Now == nil {
		return ErrInvalid
	}
	lease, heartbeat := p.settings()
	if !validProtectedLeaseDuration(lease) || heartbeat < 5*time.Millisecond || heartbeat >= lease/2 {
		return ErrInvalid
	}
	return nil
}

func (p *ProtectedGitPublisher) settings() (time.Duration, time.Duration) {
	lease := p.LeaseDuration
	if lease == 0 {
		lease = defaultProtectedPublishLease
	}
	heartbeat := p.HeartbeatInterval
	if heartbeat == 0 {
		heartbeat = defaultProtectedPublishHeartbeat
	}
	return lease, heartbeat
}

func (p *ProtectedGitPublisher) now() time.Time { return p.Now().UTC() }

func (p *ProtectedGitPublisher) ProcessPayloadOne(ctx context.Context) (ProtectedPayloadIntent, error) {
	if p.Validate() != nil || ctx == nil {
		return ProtectedPayloadIntent{}, ErrInvalid
	}
	leaseDuration, heartbeat := p.settings()
	intent, lease, err := p.Store.ClaimPayload(ctx, p.WorkerID, p.Publisher, p.now(), leaseDuration)
	if errors.Is(err, ErrNotFound) {
		intent, lease, err = p.Store.AdoptPayload(ctx, p.WorkerID, p.WorkerEpoch,
			p.Publisher, leaseDuration)
	}
	if err != nil {
		return ProtectedPayloadIntent{}, err
	}
	guard := newProtectedPublicationLeaseGuard(ctx, p.Store, lease, true, leaseDuration, heartbeat,
		p.Now, intent.UpdatedAt)
	defer guard.Close()
	prerequisite, err := p.Store.PublicationPrerequisite(guard.Context(), intent.ReleaseRevisionID)
	if err != nil {
		return intent, guard.Result(err)
	}
	if prerequisite.ValidateFor(intent.ReleaseRevisionID, intent.Target, intent.Binding) != nil {
		return intent, guard.Result(ErrConflict)
	}
	work := protectedPublicationWork{
		state:         func() ProtectedIntentState { return intent.State },
		mutation:      func() (ProtectedMutation, error) { return intent.Mutation() },
		writeBase:     func() string { return intent.WriteBaseRevision },
		committed:     func() string { return intent.CommittedRevision },
		prerequisites: []string{prerequisite.FoundationRevision, prerequisite.DesiredStateRevision},
		bind: func(current ProtectedIntentLease, revision string, observedAt, now time.Time) error {
			var bindErr error
			intent, bindErr = p.Store.BindPayloadWriteBase(ctx, current, revision, observedAt, now)
			return bindErr
		},
		rebind: func(current ProtectedIntentLease, previous, revision string, observedAt, now time.Time) error {
			var rebindErr error
			intent, rebindErr = p.Store.RebindPayloadWriteBase(ctx, current, previous, revision, observedAt, now)
			return rebindErr
		},
		mark: func(current ProtectedIntentLease, revision, parent string, now time.Time) error {
			var markErr error
			intent, markErr = p.Store.MarkPayloadCommitted(ctx, current, revision, parent, now)
			return markErr
		},
		verify: func(current ProtectedIntentLease, revision, digest, request string, now time.Time) error {
			var verifyErr error
			intent, verifyErr = p.Store.VerifyPayload(ctx, current, revision, digest, request, now)
			return verifyErr
		},
	}
	err = p.processClaim(guard, work)
	return intent, guard.Result(err)
}

func (p *ProtectedGitPublisher) ProcessApplicationOne(ctx context.Context) (ProtectedApplicationIntent, error) {
	if p.Validate() != nil || ctx == nil {
		return ProtectedApplicationIntent{}, ErrInvalid
	}
	leaseDuration, heartbeat := p.settings()
	intent, lease, err := p.Store.ClaimApplication(ctx, p.WorkerID, p.Publisher, p.now(), leaseDuration)
	if errors.Is(err, ErrNotFound) {
		intent, lease, err = p.Store.AdoptApplication(ctx, p.WorkerID, p.WorkerEpoch,
			p.Publisher, leaseDuration)
	}
	if err != nil {
		return ProtectedApplicationIntent{}, err
	}
	guard := newProtectedPublicationLeaseGuard(ctx, p.Store, lease, false, leaseDuration, heartbeat,
		p.Now, intent.UpdatedAt)
	defer guard.Close()
	prerequisite, err := p.Store.PublicationPrerequisite(guard.Context(), intent.ReleaseRevisionID)
	if err != nil {
		return intent, guard.Result(err)
	}
	if prerequisite.ValidateFor(intent.ReleaseRevisionID, intent.Target, intent.Binding) != nil {
		return intent, guard.Result(ErrConflict)
	}
	work := protectedPublicationWork{
		state:         func() ProtectedIntentState { return intent.State },
		mutation:      func() (ProtectedMutation, error) { return intent.Mutation() },
		writeBase:     func() string { return intent.WriteBaseRevision },
		committed:     func() string { return intent.CommittedRevision },
		prerequisites: []string{prerequisite.FoundationRevision, prerequisite.DesiredStateRevision},
		bind: func(current ProtectedIntentLease, revision string, observedAt, now time.Time) error {
			var bindErr error
			intent, bindErr = p.Store.BindApplicationWriteBase(ctx, current, revision, observedAt, now)
			return bindErr
		},
		rebind: func(current ProtectedIntentLease, previous, revision string, observedAt, now time.Time) error {
			var rebindErr error
			intent, rebindErr = p.Store.RebindApplicationWriteBase(ctx, current, previous, revision, observedAt, now)
			return rebindErr
		},
		mark: func(current ProtectedIntentLease, revision, parent string, now time.Time) error {
			var markErr error
			intent, markErr = p.Store.MarkApplicationCommitted(ctx, current, revision, parent, now)
			return markErr
		},
		verify: func(current ProtectedIntentLease, revision, digest, request string, now time.Time) error {
			var verifyErr error
			intent, verifyErr = p.Store.VerifyApplication(ctx, current, revision, digest, request, now)
			return verifyErr
		},
	}
	err = p.processClaim(guard, work)
	return intent, guard.Result(err)
}

type protectedPublicationWork struct {
	state         func() ProtectedIntentState
	mutation      func() (ProtectedMutation, error)
	writeBase     func() string
	committed     func() string
	prerequisites []string
	bind          func(ProtectedIntentLease, string, time.Time, time.Time) error
	rebind        func(ProtectedIntentLease, string, string, time.Time, time.Time) error
	mark          func(ProtectedIntentLease, string, string, time.Time) error
	verify        func(ProtectedIntentLease, string, string, string, time.Time) error
}

func (p *ProtectedGitPublisher) processClaim(guard *protectedPublicationLeaseGuard, work protectedPublicationWork) error {
	ctx := guard.Context()
	protectedMutation, err := work.mutation()
	if err != nil {
		return err
	}
	mutation, err := protectedMutation.gitMutation()
	if err != nil {
		return err
	}
	binding, err := p.Bindings.Binding(ctx, protectedMutation.BindingID)
	if err != nil {
		return err
	}
	if binding.Validate() != nil || binding.Kind != gitprojection.BindingPlatform ||
		binding.CredentialMode != gitprojection.CredentialGitHubApp || binding.ID != protectedMutation.BindingID ||
		binding.TargetRef != protectedMutation.TargetRef || binding.ClusterID == "" ||
		!validProtectedBindingPrefix(binding.Prefix, binding.ClusterID, protectedMutation.Path) {
		return ErrInvalid
	}
	head, err := p.Provider.VerifyTargetHead(ctx, binding, gitprojection.ObservationWrite)
	if err != nil {
		return err
	}
	if head.ValidateFor(binding) != nil || head.ObservedAt.After(guard.NotBefore(p.now())) {
		return ErrInvalid
	}
	if err = p.Manager.CleanupOperation(ctx, binding.ID, protectedMutation.IntentID); err != nil {
		return err
	}
	prepared, err := p.Manager.Prepare(ctx, binding, head, protectedMutation.IntentID)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), protectedPublishCleanupTimeout)
		defer cancel()
		_ = prepared.Close(cleanup)
	}()

	if work.writeBase() == "" {
		if err = prepared.VerifyAncestor(ctx, protectedMutation.BaseRevision); err != nil {
			return err
		}
		for _, revision := range work.prerequisites {
			if !gitCommitRE.MatchString(revision) {
				return ErrConflict
			}
			if err = prepared.VerifyAncestor(ctx, revision); err != nil {
				return err
			}
		}
		if protectedMutation.RequiredAncestor != "" {
			if err = prepared.VerifyAncestor(ctx, protectedMutation.RequiredAncestor); err != nil {
				return err
			}
		}
		if err = prepared.VerifyProtectedMutationPrecondition(ctx, mutation); err != nil {
			return err
		}
		receiptAt := guard.NotBefore(p.now(), head.ObservedAt)
		if err = guard.Do(func(current ProtectedIntentLease) error {
			return work.bind(current, head.Commit, receiptAt, receiptAt)
		}); err != nil {
			return err
		}
		guard.AdvanceTimeFloor(receiptAt)
		protectedMutation, err = work.mutation()
		if err != nil {
			return err
		}
		mutation, err = protectedMutation.gitMutation()
		if err != nil {
			return err
		}
	}
	if err = prepared.VerifyAncestor(ctx, work.writeBase()); err != nil {
		return err
	}
	for _, revision := range work.prerequisites {
		if !gitCommitRE.MatchString(revision) {
			return ErrConflict
		}
		if err = prepared.VerifyAncestor(ctx, revision); err != nil {
			return err
		}
	}
	if protectedMutation.RequiredAncestor != "" {
		if err = prepared.VerifyAncestor(ctx, protectedMutation.RequiredAncestor); err != nil {
			return err
		}
	}

	if work.state() == ProtectedGitCommitted {
		return p.recoverClaim(guard, work, binding, prepared, head, mutation)
	}
	if head.Commit != work.writeBase() {
		found, present, findErr := prepared.FindOperationCommit(ctx, mutation)
		if findErr != nil {
			return findErr
		}
		if present {
			return p.recoverFoundClaim(guard, work, binding, prepared, head, mutation, found)
		}
		if work.state() != ProtectedClaimed || work.committed() != "" {
			return ErrConflict
		}
		if err = prepared.VerifyProtectedMutationPrecondition(ctx, mutation); err != nil {
			return errors.Join(ErrConflict, err)
		}
		previous := work.writeBase()
		rebindAt := guard.NotBefore(p.now(), head.ObservedAt)
		if err = guard.Do(func(current ProtectedIntentLease) error {
			return work.rebind(current, previous, head.Commit, rebindAt, rebindAt)
		}); err != nil {
			return err
		}
		guard.AdvanceTimeFloor(rebindAt)
		protectedMutation, err = work.mutation()
		if err != nil {
			return err
		}
		mutation, err = protectedMutation.gitMutation()
		if err != nil {
			return err
		}
	}
	if work.state() != ProtectedClaimed || mutation.BaseRevision != head.Commit {
		return ErrConflict
	}
	if err = prepared.VerifyProtectedMutationPrecondition(ctx, mutation); err != nil {
		return err
	}
	revision, err := prepared.Commit(ctx, mutation)
	if err != nil {
		return err
	}
	commitAt := guard.NotBefore(p.now(), head.ObservedAt)
	if err = guard.Do(func(current ProtectedIntentLease) error {
		return work.mark(current, revision, mutation.BaseRevision, commitAt)
	}); err != nil {
		return err
	}
	guard.AdvanceTimeFloor(commitAt)
	verified, err := p.Provider.VerifyTargetHead(ctx, binding, gitprojection.ObservationWrite)
	if err != nil {
		return err
	}
	if verified.ValidateFor(binding) != nil || verified.Commit != revision ||
		verified.ObservedAt.After(guard.NotBefore(p.now())) {
		return gitprojection.ErrProviderMismatch
	}
	verifyAt := guard.NotBefore(p.now(), verified.ObservedAt)
	return guard.Finish(func(current ProtectedIntentLease) error {
		return work.verify(current, revision, mutation.ContentSHA256, verified.ProviderRequest,
			verifyAt)
	})
}

func (p *ProtectedGitPublisher) recoverClaim(guard *protectedPublicationLeaseGuard, work protectedPublicationWork,
	binding gitprojection.Binding, prepared *gitprojection.PreparedRepository, head gitprojection.VerifiedHead,
	mutation gitprojection.Mutation) error {
	ctx := guard.Context()
	found, present, err := prepared.FindOperationCommit(ctx, mutation)
	if err != nil {
		return err
	}
	if !present {
		return ErrConflict
	}
	return p.recoverFoundClaim(guard, work, binding, prepared, head, mutation, found)
}

func (p *ProtectedGitPublisher) recoverFoundClaim(guard *protectedPublicationLeaseGuard, work protectedPublicationWork,
	binding gitprojection.Binding, prepared *gitprojection.PreparedRepository, head gitprojection.VerifiedHead,
	mutation gitprojection.Mutation, found string) error {
	ctx := guard.Context()
	if work.committed() != "" && work.committed() != found {
		return ErrConflict
	}
	if err := prepared.VerifyProtectedMutationPostimage(ctx, mutation); err != nil {
		return err
	}
	if work.committed() == "" {
		commitAt := guard.NotBefore(p.now(), head.ObservedAt)
		if err := guard.Do(func(current ProtectedIntentLease) error {
			return work.mark(current, found, mutation.BaseRevision, commitAt)
		}); err != nil {
			return err
		}
		guard.AdvanceTimeFloor(commitAt)
	}
	verified, err := p.Provider.VerifyTargetHead(ctx, binding, gitprojection.ObservationWrite)
	if err != nil {
		return err
	}
	if verified.ValidateFor(binding) != nil || verified.Commit != head.Commit ||
		verified.ObservedAt.After(guard.NotBefore(p.now())) {
		return gitprojection.ErrProviderMismatch
	}
	verifyAt := guard.NotBefore(p.now(), verified.ObservedAt)
	return guard.Finish(func(current ProtectedIntentLease) error {
		return work.verify(current, found, mutation.ContentSHA256, verified.ProviderRequest,
			verifyAt)
	})
}

func validProtectedBindingPrefix(prefix, clusterID, protectedPath string) bool {
	expected := "clusters/" + clusterID
	return prefix == expected && len(protectedPath) > len(expected) && protectedPath[:len(expected)+1] == expected+"/"
}

type protectedPublicationLeaseGuard struct {
	mu        sync.Mutex
	store     ProtectedPublicationStore
	lease     ProtectedIntentLease
	payload   bool
	duration  time.Duration
	now       func() time.Time
	timeFloor time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	lost      error
	closed    bool
	done      chan struct{}
}

func newProtectedPublicationLeaseGuard(parent context.Context, store ProtectedPublicationStore,
	lease ProtectedIntentLease, payload bool, duration, interval time.Duration,
	now func() time.Time, timeFloor time.Time) *protectedPublicationLeaseGuard {
	ctx, cancel := context.WithCancel(parent)
	guard := &protectedPublicationLeaseGuard{store: store, lease: lease, payload: payload,
		duration: duration, now: now, timeFloor: timeFloor.UTC(), ctx: ctx, cancel: cancel,
		done: make(chan struct{})}
	go guard.run(interval)
	return guard
}

func (g *protectedPublicationLeaseGuard) run(interval time.Duration) {
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
			heartbeatAt := g.now().UTC()
			if heartbeatAt.Before(g.timeFloor) {
				heartbeatAt = g.timeFloor
			}
			var updated ProtectedIntentLease
			var err error
			if g.payload {
				updated, err = g.store.HeartbeatPayload(g.ctx, g.lease, heartbeatAt, g.duration)
			} else {
				updated, err = g.store.HeartbeatApplication(g.ctx, g.lease, heartbeatAt, g.duration)
			}
			if err != nil {
				g.lost = errors.Join(ErrLeaseLost, err)
				g.cancel()
				g.mu.Unlock()
				return
			}
			g.lease = updated
			if g.timeFloor.Before(heartbeatAt) {
				g.timeFloor = heartbeatAt
			}
			g.mu.Unlock()
		}
	}
}

func (g *protectedPublicationLeaseGuard) Context() context.Context { return g.ctx }

func (g *protectedPublicationLeaseGuard) AdvanceTimeFloor(value time.Time) {
	g.mu.Lock()
	if g.timeFloor.Before(value) {
		g.timeFloor = value.UTC()
	}
	g.mu.Unlock()
}

func (g *protectedPublicationLeaseGuard) NotBefore(now time.Time, values ...time.Time) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	result := now.UTC()
	if result.Before(g.timeFloor) {
		result = g.timeFloor
	}
	for _, value := range values {
		if result.Before(value) {
			result = value.UTC()
		}
	}
	return result
}

func (g *protectedPublicationLeaseGuard) Do(operation func(ProtectedIntentLease) error) error {
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

func (g *protectedPublicationLeaseGuard) Finish(operation func(ProtectedIntentLease) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lost != nil {
		return g.lost
	}
	if g.closed {
		return ErrLeaseLost
	}
	g.closed = true
	err := operation(g.lease)
	g.cancel()
	return err
}

func (g *protectedPublicationLeaseGuard) Result(operationErr error) error {
	g.mu.Lock()
	lost := g.lost
	g.mu.Unlock()
	if lost != nil {
		return errors.Join(operationErr, lost)
	}
	return operationErr
}

func (g *protectedPublicationLeaseGuard) Close() {
	g.mu.Lock()
	if !g.closed {
		g.closed = true
		g.cancel()
	}
	g.mu.Unlock()
	<-g.done
}
