package gitprojection

import (
	"context"
	"errors"
	"time"

	"github.com/kuberploy/kuberploy/internal/gitpublication"
)

const defaultWriteLease = 90 * time.Second

// ProjectionWriter executes one immutable accepted command. Provider reads,
// Git I/O, and token minting happen outside database transactions; path
// reservation and finalization fence concurrent operations. A retry discovers
// an already-pushed exact operation commit before it can consider new work.
type ProjectionWriter struct {
	Store         Store
	Commands      WriteCommandStore
	Provider      HeadVerifier
	Manager       *MirrorManager
	Publications  gitpublication.Store
	PullRequests  *gitpublication.Service
	Protection    RepositoryProtectionVerifier
	Owner         string
	LeaseDuration time.Duration
	Now           func() time.Time
}

type PublicationResult struct {
	Mode        PublicationMode
	Revision    string
	Publication *gitpublication.Publication
}

func (r PublicationResult) Validate() error {
	switch r.Mode {
	case PublicationDirect:
		if !commitRE.MatchString(r.Revision) || r.Publication != nil {
			return ErrInvalid
		}
	case PublicationPullRequest:
		if r.Revision != "" || r.Publication == nil || r.Publication.Validate() != nil || r.Publication.PullRequestNumber <= 0 ||
			(r.Publication.State != gitpublication.StatePullRequestOpen && r.Publication.State != gitpublication.StatePullRequestClosed &&
				r.Publication.State != gitpublication.StateMergePending && r.Publication.State != gitpublication.StateMergeVerified) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func (w *ProjectionWriter) validate() error {
	if w == nil || w.Store == nil || w.Commands == nil || w.Provider == nil || w.Manager == nil || !ownerRE.MatchString(w.Owner) {
		return ErrInvalid
	}
	duration := w.LeaseDuration
	if duration == 0 {
		duration = defaultWriteLease
	}
	if duration < 30*time.Second || duration > 2*time.Minute {
		return ErrInvalid
	}
	return nil
}

func (w *ProjectionWriter) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *ProjectionWriter) CommitOperation(ctx context.Context, operationID string) (string, error) {
	if err := w.validate(); err != nil || !uuidRE.MatchString(operationID) {
		return "", ErrInvalid
	}
	command, err := w.Commands.WriteCommand(ctx, operationID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrInvalid) {
			return "", pendingReconciliation("GitCommandLoadPending", "The accepted Git projection command will be loaded again.", err)
		}
		return "", err
	}
	if command.State == WriteCommandGitCommitted || command.State == WriteCommandIndexed {
		return command.CommittedRevision, nil
	}
	if command.PublicationMode != PublicationDirect {
		return "", ErrInvalid
	}
	binding, err := w.Store.Binding(ctx, command.Plan.BindingID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrInvalid) {
			return "", pendingReconciliation("GitBindingLoadPending", "The authorized Git binding will be loaded again.", err)
		}
		return "", err
	}
	commandBinding := writeCommandBinding(command, binding)
	if command.Validate(commandBinding) != nil {
		return "", ErrInvalid
	}
	reservation, reservationErr := w.Store.PathReservation(ctx, binding.ID, binding.TargetRef, command.Path)
	if reservationErr == nil {
		if reservation.OperationID != operationID || reservation.BaseRevision != command.Plan.BaseRevision {
			return "", ErrLeaseHeld
		}
		if reservation.State == ReservationCommittedPendingIndex {
			committed, markErr := w.Commands.MarkWriteCommandCommitted(ctx, operationID, reservation.CommittedRevision, w.now())
			if markErr != nil {
				return "", pendingReconciliation("GitFinalizationPending", "The verified Git commit receipt will be finalized again.", markErr)
			}
			return committed.CommittedRevision, nil
		}
	} else if !errors.Is(reservationErr, ErrNotFound) {
		return "", pendingReconciliation("GitReservationLoadPending", "The Git path reservation will be inspected again.", reservationErr)
	}

	head, err := w.Provider.VerifyTargetHead(ctx, binding, ObservationWrite)
	if err != nil {
		if errors.Is(err, ErrInvalid) || errors.Is(err, ErrMissingRef) {
			return "", err
		}
		return "", pendingReconciliation("GitProviderVerificationPending", "The authoritative Git provider head will be verified again.", err)
	}
	if err = w.Manager.CleanupOperation(ctx, binding.ID, operationID); err != nil {
		return "", pendingReconciliation("GitRepositoryPreparationPending", "The isolated Git operation workspace will be prepared again.", err)
	}
	prepared, err := w.Manager.Prepare(ctx, binding, head, operationID)
	if err != nil {
		return "", pendingReconciliation("GitRepositoryPreparationPending", "The isolated Git operation workspace will be prepared again.", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultProjectionCleanup)
		defer cancel()
		_ = prepared.Close(cleanup)
	}()

	mutation := command.Mutation()
	if head.Commit != command.Plan.BaseRevision {
		return w.recoverCommit(ctx, binding, command, reservation, reservationErr, prepared, head)
	}
	if command.Plan.Validate(commandBinding) != nil {
		return "", ErrStale
	}
	duration := w.LeaseDuration
	if duration == 0 {
		duration = defaultWriteLease
	}
	now := w.now()
	until := now.Add(duration)
	candidate := PathReservation{BindingID: binding.ID, TargetRef: binding.TargetRef, Path: command.Path, OperationID: operationID,
		Owner: w.Owner, BaseRevision: command.Plan.BaseRevision, State: ReservationCandidate, LeaseUntil: &until, CreatedAt: now, UpdatedAt: now}
	reservation, _, err = w.Store.AcquirePath(ctx, candidate, now, duration)
	if err != nil {
		if errors.Is(err, ErrInvalid) || errors.Is(err, ErrStale) {
			return "", err
		}
		return "", pendingReconciliation("GitReservationPending", "The Git path reservation will be acquired again.", err)
	}
	revision, err := prepared.Commit(ctx, mutation)
	if err != nil {
		// A transport error can occur after the provider accepted the push.
		// Never mark that ambiguous result terminal: the operation trailer and
		// pinned provider head make the next attempt a deterministic recovery.
		return "", pendingReconciliation("GitCommitResultPending", "The Git push result is uncertain and will be reconciled from authoritative history.", err)
	}
	verifiedAfter, err := w.Provider.VerifyTargetHead(ctx, binding, ObservationWrite)
	if err != nil {
		return "", pendingReconciliation("GitPostPushVerificationPending", "The pushed Git commit will be verified with the provider again.", err)
	}
	if verifiedAfter.Commit != revision {
		// The provider may already expose a descendant. A retry prepares that
		// exact verified head and uses the bounded operation-trailer recovery
		// path; never finalize against an uninspected descendant here.
		return "", pendingReconciliation("GitPostPushVerificationPending", "The pushed Git commit will be reconciled against the provider head again.", ErrProviderMismatch)
	}
	if _, err = w.Store.FinalizeVerifiedPath(ctx, binding.ID, binding.TargetRef, command.Path, operationID, revision, verifiedAfter, w.now()); err != nil {
		return "", pendingReconciliation("GitFinalizationPending", "The verified Git commit receipt will be finalized again.", err)
	}
	return revision, nil
}

func (w *ProjectionWriter) PublishOperation(ctx context.Context, operationID string) (PublicationResult, error) {
	if err := w.validate(); err != nil || !uuidRE.MatchString(operationID) {
		return PublicationResult{}, ErrInvalid
	}
	command, err := w.Commands.WriteCommand(ctx, operationID)
	if err != nil {
		return PublicationResult{}, err
	}
	if command.PublicationMode == PublicationDirect {
		revision, commitErr := w.CommitOperation(ctx, operationID)
		if commitErr != nil {
			return PublicationResult{}, commitErr
		}
		return PublicationResult{Mode: PublicationDirect, Revision: revision}, nil
	}
	if command.PublicationMode != PublicationPullRequest || w.Publications == nil || w.PullRequests == nil {
		return PublicationResult{}, ErrInvalid
	}
	binding, err := w.Store.Binding(ctx, command.Plan.BindingID)
	if err != nil {
		return PublicationResult{}, err
	}
	if validatePersistedWriteCommand(command, binding) != nil {
		return PublicationResult{}, ErrInvalid
	}
	publication, err := w.Publications.Publication(ctx, operationID)
	if err != nil || !publicationMatchesCommand(publication, command, binding) {
		if err != nil {
			return PublicationResult{}, err
		}
		return PublicationResult{}, ErrInvalid
	}
	if publication.State == gitpublication.StatePendingCandidate || publication.State == gitpublication.StateWriteBaseReady {
		if w.Protection == nil {
			return PublicationResult{}, ErrProtectionUnavailable
		}
		protectionStartedAt := w.now()
		head, verifyErr := w.Provider.VerifyTargetHead(ctx, binding, ObservationWrite)
		if verifyErr != nil {
			if terminalProjectionPublicationError(verifyErr) {
				return PublicationResult{}, verifyErr
			}
			return PublicationResult{}, pendingReconciliation("GitProviderVerificationPending", "The authoritative Git provider head will be verified again.", verifyErr)
		}
		if _, protectionErr := w.Protection.VerifyRepositoryProtection(ctx, binding, head, protectionStartedAt); protectionErr != nil {
			return PublicationResult{}, pendingReconciliation("GitRepositoryProtectionPending", "The exact protected target-branch policy will be freshly verified again.", errors.Join(ErrProtectionUnavailable, protectionErr))
		}
		if cleanupErr := w.Manager.CleanupOperation(ctx, binding.ID, operationID); cleanupErr != nil {
			return PublicationResult{}, pendingReconciliation("GitRepositoryPreparationPending", "The isolated Git operation workspace will be prepared again.", cleanupErr)
		}
		prepared, prepareErr := w.Manager.Prepare(ctx, binding, head, operationID)
		if prepareErr != nil {
			return PublicationResult{}, pendingReconciliation("GitRepositoryPreparationPending", "The isolated Git operation workspace will be prepared again.", prepareErr)
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultProjectionCleanup)
			defer cancel()
			_ = prepared.Close(cleanup)
		}()
		if publication.State == gitpublication.StatePendingCandidate {
			if verifyErr = prepared.VerifyMutationUnchangedSince(ctx, command.Mutation()); verifyErr != nil {
				if terminalProjectionPublicationError(verifyErr) {
					return PublicationResult{}, verifyErr
				}
				return PublicationResult{}, pendingReconciliation("GitWriteBaseVerificationPending", "The protected publication write base will be verified again.", verifyErr)
			}
			publication, err = w.PullRequests.RecordWriteBase(ctx, operationID, head.Commit)
			if err != nil {
				if terminalProviderPublicationError(err) {
					return PublicationResult{}, err
				}
				return PublicationResult{}, pendingReconciliation("GitWriteBaseReceiptPending", "The protected publication write base will be recorded again.", err)
			}
		}
		mutation := command.Mutation()
		mutation.BaseRevision = publication.WriteBaseRevision
		revision, present, candidateErr := prepared.FindCandidateOperationCommit(ctx, mutation, publication.CandidateRef)
		if candidateErr != nil {
			if terminalProjectionPublicationError(candidateErr) {
				return PublicationResult{}, candidateErr
			}
			return PublicationResult{}, pendingReconciliation("GitCandidateInspectionPending", "The deterministic protected candidate ref will be inspected again.", candidateErr)
		}
		if !present {
			if candidateErr = prepared.VerifyMutationUnchangedSince(ctx, mutation); candidateErr != nil {
				if terminalProjectionPublicationError(candidateErr) {
					return PublicationResult{}, candidateErr
				}
				return PublicationResult{}, pendingReconciliation("GitWriteBaseVerificationPending", "The protected publication write base will be verified again.", candidateErr)
			}
			revision, candidateErr = prepared.CommitCandidate(ctx, mutation, publication.CandidateRef)
			if candidateErr != nil {
				if terminalProjectionPublicationError(candidateErr) {
					return PublicationResult{}, candidateErr
				}
				return PublicationResult{}, pendingReconciliation("GitCandidateResultPending", "The protected candidate push result is uncertain and will be reconciled from its deterministic ref.", candidateErr)
			}
		}
		publication, err = w.PullRequests.RecordCandidate(ctx, operationID, revision)
		if err != nil {
			if terminalProviderPublicationError(err) {
				return PublicationResult{}, err
			}
			return PublicationResult{}, pendingReconciliation("GitCandidateReceiptPending", "The protected candidate receipt will be recorded again.", err)
		}
	}
	publication, err = w.PullRequests.EnsurePullRequest(ctx, operationID)
	if err != nil {
		if terminalProviderPublicationError(err) {
			return PublicationResult{}, err
		}
		return PublicationResult{}, pendingReconciliation("GitPullRequestPending", "The exact protected pull request will be created or recovered again.", err)
	}
	result := PublicationResult{Mode: PublicationPullRequest, Publication: &publication}
	if result.Validate() != nil {
		return PublicationResult{}, ErrInvalid
	}
	return result, nil
}

func terminalProjectionPublicationError(err error) bool {
	return errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict) || errors.Is(err, ErrStale) ||
		errors.Is(err, ErrMissingRef) || errors.Is(err, ErrProviderMismatch)
}

func terminalProviderPublicationError(err error) bool {
	return errors.Is(err, gitpublication.ErrInvalid) || errors.Is(err, gitpublication.ErrProviderMismatch)
}

func publicationMatchesCommand(publication gitpublication.Publication, command WriteCommand, binding Binding) bool {
	return publication.Validate() == nil && publication.OperationID == command.OperationID && publication.BindingID == binding.ID &&
		publication.Repository.InstallationID == binding.Repository.InstallationID && publication.Repository.ID == binding.Repository.RepositoryID &&
		publication.Repository.Owner == binding.Repository.Owner && publication.Repository.Name == binding.Repository.Name &&
		publication.TargetRef == command.TargetRef && publication.BaseRevision == command.Plan.BaseRevision
}

func (w *ProjectionWriter) recoverCommit(ctx context.Context, binding Binding, command WriteCommand, reservation PathReservation, reservationErr error, prepared *PreparedRepository, head VerifiedHead) (string, error) {
	if errors.Is(reservationErr, ErrNotFound) {
		return "", ErrConflict
	}
	if reservationErr != nil {
		return "", reservationErr
	}
	found, present, err := prepared.FindOperationCommit(ctx, command.Mutation())
	if err != nil {
		return "", pendingReconciliation("GitRecoveryInspectionPending", "Authoritative Git history will be inspected for the accepted operation again.", err)
	}
	if !present {
		if reservation.LeaseUntil != nil && !reservation.LeaseUntil.After(w.now()) {
			if repairErr := w.Store.RepairExpiredPath(ctx, binding.ID, binding.TargetRef, command.Path, false, "", w.now()); repairErr != nil {
				return "", pendingReconciliation("GitReservationPending", "The expired Git path reservation will be reconciled again.", repairErr)
			}
		}
		return "", ErrConflict
	}
	if _, err = w.Store.FinalizeVerifiedPath(ctx, binding.ID, binding.TargetRef, command.Path, command.OperationID, found, head, w.now()); err != nil {
		return "", pendingReconciliation("GitFinalizationPending", "The recovered Git commit receipt will be finalized again.", err)
	}
	return found, nil
}

var _ interface {
	CommitOperation(context.Context, string) (string, error)
} = (*ProjectionWriter)(nil)
