package environmentfoundation

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

const foundationCleanupTimeout = 15 * time.Second

type ProtectedGitBindingStore interface {
	Binding(context.Context, string) (gitprojection.Binding, error)
}

// ProtectedGitPublisher is the sole adapter from durable foundation intents to
// gitprojection's shared hardened writer. It derives the one permitted path
// and mutation shape again; it never accepts generic Git paths or YAML.
type ProtectedGitPublisher struct {
	Store     Store
	Bindings  ProtectedGitBindingStore
	Provider  gitprojection.HeadVerifier
	Manager   *gitprojection.MirrorManager
	Publisher PublisherIdentity
	Now       func() time.Time
}

func (p *ProtectedGitPublisher) Identity() PublisherIdentity { return p.Publisher }

func (p *ProtectedGitPublisher) Validate() error {
	if p == nil || p.Store == nil || p.Bindings == nil || p.Provider == nil || p.Manager == nil ||
		p.Manager.Validate() != nil || p.Publisher.Validate() != nil || p.Now == nil {
		return ErrInvalid
	}
	return nil
}

func (p *ProtectedGitPublisher) Publish(ctx context.Context, lease Lease, request PublicationRequest) (PublicationReceipt, error) {
	if ctx == nil || p.Validate() != nil || lease.Validate(p.now().Add(-time.Nanosecond)) != nil ||
		request.Validate(lease.Intent, p.Publisher) != nil {
		return PublicationReceipt{}, ErrInvalid
	}
	binding, err := p.Bindings.Binding(ctx, request.BindingID)
	if err != nil {
		return PublicationReceipt{}, classifyFoundationGit(err)
	}
	if binding.Validate() != nil || binding.Kind != gitprojection.BindingPlatform ||
		binding.CredentialMode != gitprojection.CredentialGitHubApp || binding.CredentialSecretName != "" ||
		binding.ID != request.BindingID ||
		binding.Prefix != gitprojection.PlatformPrefix() || binding.TargetRef != request.TargetRef ||
		binding.ProjectionGeneration < request.BindingGeneration {
		return PublicationReceipt{}, ErrInvalid
	}
	head, err := p.Provider.VerifyTargetHead(ctx, binding, gitprojection.ObservationWrite)
	if err != nil {
		return PublicationReceipt{}, classifyFoundationGit(err)
	}
	now := p.now()
	if head.ValidateFor(binding) != nil || head.Source != gitprojection.ObservationWrite || head.ObservedAt.After(now) {
		return PublicationReceipt{}, ErrInvalid
	}
	if err = p.Manager.CleanupOperation(ctx, binding.ID, request.IntentID); err != nil {
		return PublicationReceipt{}, classifyFoundationGit(err)
	}
	prepared, err := p.Manager.Prepare(ctx, binding, head, request.IntentID)
	if err != nil {
		return PublicationReceipt{}, classifyFoundationGit(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), foundationCleanupTimeout)
		defer cancel()
		_ = prepared.Close(cleanup)
	}()
	if err = prepared.VerifyAncestor(ctx, request.PlannedHead); err != nil {
		return PublicationReceipt{}, classifyFoundationGit(err)
	}
	expectedPreimage, hasPreimage, err := p.Store.ExpectedPreimage(ctx, request.IntentID)
	if err != nil {
		return PublicationReceipt{}, err
	}

	intent := lease.Intent
	if intent.WriteBaseRevision == "" {
		candidate := foundationMutation(request, head.Commit, expectedPreimage, hasPreimage)
		if candidate.Validate(binding) != nil {
			return PublicationReceipt{}, ErrInvalid
		}
		if err = prepared.VerifyProtectedMutationPrecondition(ctx, candidate); err != nil {
			return PublicationReceipt{}, classifyFoundationGit(err)
		}
		boundAt := p.notBefore(head.ObservedAt)
		intent, err = p.Store.BindWriteBase(ctx, lease, head.Commit, head.ObservedAt, boundAt)
		if err != nil {
			return PublicationReceipt{}, err
		}
	}
	mutation := foundationMutation(request, intent.WriteBaseRevision, expectedPreimage, hasPreimage)
	if mutation.Validate(binding) != nil || intent.WriteBaseObservedAt == nil {
		return PublicationReceipt{}, ErrInvalid
	}
	if err = prepared.VerifyAncestor(ctx, mutation.BaseRevision); err != nil {
		return PublicationReceipt{}, classifyFoundationGit(err)
	}
	found, present, err := prepared.FindOperationCommit(ctx, mutation)
	if err != nil {
		return PublicationReceipt{}, classifyFoundationGit(err)
	}
	if present {
		if err = prepared.VerifyProtectedMutationPostimage(ctx, mutation); err != nil {
			return PublicationReceipt{}, classifyFoundationGit(err)
		}
		return p.verifyReceipt(ctx, binding, head.Commit, request, mutation.BaseRevision, found)
	}
	if head.Commit != mutation.BaseRevision {
		return PublicationReceipt{}, fmt.Errorf("%w: durable foundation write base advanced without the exact operation: %w", ErrConflict, errRebaseRequired)
	}
	if err = prepared.VerifyProtectedMutationPrecondition(ctx, mutation); err != nil {
		return PublicationReceipt{}, classifyFoundationGit(err)
	}
	committed, err := prepared.Commit(ctx, mutation)
	if err != nil {
		return PublicationReceipt{}, classifyFoundationGit(err)
	}
	return p.verifyReceipt(ctx, binding, committed, request, mutation.BaseRevision, committed)
}

func (p *ProtectedGitPublisher) Delete(ctx context.Context, lease DeletionLease) (DeletionReceipt, error) {
	deletion := lease.Deletion
	if ctx == nil || p.Validate() != nil || deletion.Validate() != nil || lease.Owner != deletion.LeaseOwner ||
		lease.Epoch != deletion.LeaseEpoch || !lease.Until.Equal(*deletion.LeaseUntil) {
		return DeletionReceipt{}, ErrInvalid
	}
	binding, err := p.Bindings.Binding(ctx, deletion.BindingID)
	if err != nil {
		return DeletionReceipt{}, classifyFoundationGit(err)
	}
	if binding.Validate() != nil || binding.Kind != gitprojection.BindingPlatform || binding.TargetRef != deletion.TargetRef ||
		binding.Prefix != gitprojection.PlatformPrefix() {
		return DeletionReceipt{}, ErrInvalid
	}
	head, err := p.deleteEnvironmentDesiredState(ctx, binding, deletion)
	if err != nil {
		return DeletionReceipt{}, err
	}
	if err = p.Manager.CleanupOperation(ctx, binding.ID, deletion.ID); err != nil {
		return DeletionReceipt{}, classifyFoundationGit(err)
	}
	prepared, err := p.Manager.Prepare(ctx, binding, head, deletion.ID)
	if err != nil {
		return DeletionReceipt{}, classifyFoundationGit(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), foundationCleanupTimeout)
		defer cancel()
		_ = prepared.Close(cleanup)
	}()
	if err = prepared.VerifyAncestor(ctx, deletion.RequiredAncestor); err != nil {
		return DeletionReceipt{}, classifyFoundationGit(err)
	}
	present, etag, existingDigest, err := prepared.ProtectedFoundationPreimage(ctx, deletion.Path, head.Commit)
	if err != nil {
		return DeletionReceipt{}, classifyFoundationGit(err)
	}
	if !present {
		return p.verifyDeletionReceipt(ctx, binding, head.Commit, head.Commit, deletion)
	}
	if existingDigest != deletion.ExpectedManifestDigest {
		return DeletionReceipt{}, errors.Join(ErrConflict, errors.New("foundation delete preimage differs from the exact published Environment manifest"))
	}
	mutation := gitprojection.Mutation{BindingID: binding.ID, OperationID: deletion.ID, Path: deletion.Path,
		BaseRevision: head.Commit, Precondition: gitprojection.MutationMatchETag, ExpectedETag: etag,
		Message: "delete environment foundation " + deletion.EnvironmentID, Action: gitprojection.MutationDelete,
		Authority:     gitprojection.MutationAuthorityFoundation,
		CommitTrailer: "Kuberploy-Environment-Foundation-Intent: " + deletion.ID, RequiredAncestor: deletion.RequiredAncestor}
	if mutation.Validate(binding) != nil {
		return DeletionReceipt{}, ErrInvalid
	}
	if err = prepared.VerifyProtectedMutationPrecondition(ctx, mutation); err != nil {
		return DeletionReceipt{}, classifyFoundationGit(err)
	}
	committed, err := prepared.Commit(ctx, mutation)
	if err != nil {
		return DeletionReceipt{}, classifyFoundationGit(err)
	}
	return p.verifyDeletionReceipt(ctx, binding, head.Commit, committed, deletion)
}

func (p *ProtectedGitPublisher) deleteEnvironmentDesiredState(ctx context.Context, binding gitprojection.Binding, deletion Deletion) (gitprojection.VerifiedHead, error) {
	head, err := p.Provider.VerifyTargetHead(ctx, binding, gitprojection.ObservationWrite)
	if err != nil {
		return gitprojection.VerifiedHead{}, classifyFoundationGit(err)
	}
	if head.ValidateFor(binding) != nil || head.Source != gitprojection.ObservationWrite || head.ObservedAt.After(p.now()) {
		return gitprojection.VerifiedHead{}, ErrInvalid
	}
	operationID := desiredStateDeletionOperationID(deletion.ID)
	if err = p.Manager.CleanupOperation(ctx, binding.ID, operationID); err != nil {
		return gitprojection.VerifiedHead{}, classifyFoundationGit(err)
	}
	prepared, err := p.Manager.Prepare(ctx, binding, head, operationID)
	if err != nil {
		return gitprojection.VerifiedHead{}, classifyFoundationGit(err)
	}
	closePrepared := func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), foundationCleanupTimeout)
		defer cancel()
		_ = prepared.Close(cleanup)
	}
	if err = prepared.VerifyAncestor(ctx, deletion.RequiredAncestor); err != nil {
		closePrepared()
		return gitprojection.VerifiedHead{}, classifyFoundationGit(err)
	}
	path, present, etag, _, err := prepared.ProtectedPlatformEnvironmentPreimage(ctx, deletion.EnvironmentID, head.Commit)
	if err != nil {
		closePrepared()
		return gitprojection.VerifiedHead{}, classifyFoundationGit(err)
	}
	if !present {
		closePrepared()
		return head, nil
	}
	mutation := gitprojection.Mutation{BindingID: binding.ID, OperationID: operationID, Path: path,
		BaseRevision: head.Commit, Precondition: gitprojection.MutationMatchETag, ExpectedETag: etag,
		Message: "delete environment desired state " + deletion.EnvironmentID, Action: gitprojection.MutationDelete}
	if mutation.Validate(binding) != nil {
		closePrepared()
		return gitprojection.VerifiedHead{}, ErrInvalid
	}
	if err = prepared.VerifyPathContentETag(ctx, path, etag); err != nil {
		closePrepared()
		return gitprojection.VerifiedHead{}, classifyFoundationGit(err)
	}
	committed, err := prepared.Commit(ctx, mutation)
	closePrepared()
	if err != nil {
		return gitprojection.VerifiedHead{}, classifyFoundationGit(err)
	}
	verified, err := p.Provider.VerifyTargetHead(ctx, binding, gitprojection.ObservationWrite)
	if err != nil {
		return gitprojection.VerifiedHead{}, classifyFoundationGit(err)
	}
	if verified.ValidateFor(binding) != nil || verified.Source != gitprojection.ObservationWrite ||
		verified.ObservedAt.After(p.now()) || verified.Commit != committed {
		return gitprojection.VerifiedHead{}, errors.Join(ErrConflict, gitprojection.ErrProviderMismatch)
	}
	return verified, nil
}

func desiredStateDeletionOperationID(deletionID string) string {
	sum := sha256.Sum256([]byte("kuberploy-environment-desired-state-deletion:" + deletionID))
	sum[6] = sum[6]&0x0f | 0x50
	sum[8] = sum[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func (p *ProtectedGitPublisher) verifyDeletionReceipt(ctx context.Context, binding gitprojection.Binding, parent, committed string, deletion Deletion) (DeletionReceipt, error) {
	verified, err := p.Provider.VerifyTargetHead(ctx, binding, gitprojection.ObservationWrite)
	if err != nil {
		return DeletionReceipt{}, classifyFoundationGit(err)
	}
	if verified.ValidateFor(binding) != nil || verified.Source != gitprojection.ObservationWrite || verified.ObservedAt.After(p.now()) || verified.Commit != committed {
		return DeletionReceipt{}, errors.Join(ErrConflict, gitprojection.ErrProviderMismatch)
	}
	return DeletionReceipt{OperationID: deletion.ID, BindingID: binding.ID, TargetRef: binding.TargetRef,
		Path: deletion.Path, ParentRevision: parent, CommittedRevision: committed,
		ProviderRequest: verified.ProviderRequest, ObservedAt: p.notBefore(verified.ObservedAt)}, nil
}

func foundationMutation(request PublicationRequest, base, expectedPreimage string, hasPreimage bool) gitprojection.Mutation {
	mutation := gitprojection.Mutation{
		BindingID: request.BindingID, OperationID: request.IntentID,
		Path: ManifestPath(request.EnvironmentID), BaseRevision: base,
		Precondition: gitprojection.MutationCreateIfAbsent, Action: gitprojection.MutationUpsert,
		Content: append([]byte(nil), request.Content...), ContentSHA256: request.ContentDigest,
		Message:   "publish environment foundation " + request.EnvironmentID,
		Authority: gitprojection.MutationAuthorityFoundation, CommitTrailer: request.CommitTrailer,
		RequiredAncestor: request.PlannedHead,
	}
	if hasPreimage {
		mutation.Precondition = gitprojection.MutationMatchETag
		mutation.ExpectedETag = `"` + expectedPreimage + `"`
	}
	return mutation
}

func (p *ProtectedGitPublisher) verifyReceipt(ctx context.Context, binding gitprojection.Binding,
	expectedProviderHead string, request PublicationRequest, parent, committed string) (PublicationReceipt, error) {
	verified, err := p.Provider.VerifyTargetHead(ctx, binding, gitprojection.ObservationWrite)
	if err != nil {
		return PublicationReceipt{}, classifyFoundationGit(err)
	}
	if verified.ValidateFor(binding) != nil || verified.Source != gitprojection.ObservationWrite ||
		verified.ObservedAt.After(p.now()) || verified.Commit != expectedProviderHead {
		return PublicationReceipt{}, errors.Join(ErrConflict, gitprojection.ErrProviderMismatch)
	}
	receipt := PublicationReceipt{IntentID: request.IntentID, BindingID: request.BindingID,
		TargetRef: request.TargetRef, Path: ManifestPath(request.EnvironmentID),
		ContentDigest: request.ContentDigest, ParentRevision: parent, CommittedRevision: committed,
		ProviderRequest: verified.ProviderRequest, ObservedAt: p.notBefore(verified.ObservedAt)}
	return receipt, nil
}

func (p *ProtectedGitPublisher) now() time.Time { return p.Now().UTC() }
func (p *ProtectedGitPublisher) notBefore(values ...time.Time) time.Time {
	now := p.now()
	for _, value := range values {
		if now.Before(value) {
			now = value.UTC()
		}
	}
	return now
}

func classifyFoundationGit(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gitprojection.ErrInvalid):
		return errors.Join(ErrInvalid, err)
	case errors.Is(err, gitprojection.ErrConflict), errors.Is(err, gitprojection.ErrStale),
		errors.Is(err, gitprojection.ErrProviderMismatch):
		return errors.Join(ErrConflict, err)
	default:
		return errors.Join(ErrUnavailable, err)
	}
}

var _ ProtectedPublisher = (*ProtectedGitPublisher)(nil)
