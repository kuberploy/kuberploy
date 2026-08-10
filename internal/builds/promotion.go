package builds

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/buildpromotion"
)

// SuccessfulReleaseProjection returns only immutable build metadata after the
// attempt and its exact recoverable release projection are both successful.
// The registry release is intentionally revalidated by buildpromotion against
// the independent registry lifecycle store before any deployment is created.
func (s *PostgreSQLStore) SuccessfulReleaseProjection(ctx context.Context, attemptID string) (buildpromotion.ProjectedBuild, error) {
	if !uuidRE.MatchString(attemptID) {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return buildpromotion.ProjectedBuild{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	attempt, err := attemptByIDQuery(ctx, tx, attemptID, false)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return buildpromotion.ProjectedBuild{}, buildpromotion.ErrNotFound
		}
		return buildpromotion.ProjectedBuild{}, err
	}
	if attempt.State != AttemptSucceeded || attempt.Result == nil || attempt.CompletedAt == nil || validateStoredAttempt(attempt) != nil {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrNotReady
	}
	definition, err := definitionByIDQuery(ctx, tx, attempt.DefinitionID, false)
	if err != nil {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrConflict
	}
	if definition.ID != attempt.DefinitionID || definition.ProjectID != attempt.ProjectID || definition.ServiceID != attempt.ServiceID {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrConflict
	}
	var state string
	var releaseID *string
	var completedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT state,release_id::text,completed_at FROM build_release_projections WHERE attempt_id=$1`, attemptID).Scan(&state, &releaseID, &completedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrNotReady
	}
	if err != nil {
		return buildpromotion.ProjectedBuild{}, err
	}
	if state == string(ReleaseProjectionFailed) {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrConflict
	}
	if state != string(ReleaseProjectionSucceeded) || releaseID == nil || completedAt == nil || *releaseID != attempt.ID || completedAt.Before(*attempt.CompletedAt) {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrNotReady
	}
	server := attempt.PlanRequest.Build.Registry.Server
	repository, err := targetRepository(attempt.PlanRequest.Build.Destination.Repository, server)
	if err != nil {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrConflict
	}
	result := buildpromotion.ProjectedBuild{AttemptID: attempt.ID, DefinitionID: attempt.DefinitionID, ProjectID: attempt.ProjectID, ApplicationID: attempt.ServiceID,
		Generation: attempt.Generation, CommitSHA: attempt.CommitSHA, DefinitionDigest: attempt.DefinitionDigest,
		RegistryTargetID: definition.Spec.Registry.TargetID, RegistryServer: server, Repository: repository,
		ImageReference: attempt.Result.Image.Reference, ImageDigest: attempt.Result.Image.Digest, ReleaseID: *releaseID,
		CreatedAt: attempt.CreatedAt.UTC(), CompletedAt: attempt.CompletedAt.UTC(), ProjectionCompletedAt: completedAt.UTC()}
	if result.Validate() != nil {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return buildpromotion.ProjectedBuild{}, err
	}
	return result, nil
}

func (s *MemoryStore) SuccessfulReleaseProjection(_ context.Context, attemptID string) (buildpromotion.ProjectedBuild, error) {
	if !uuidRE.MatchString(attemptID) {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt, ok := s.attempts[attemptID]
	if !ok {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrNotFound
	}
	if attempt.State != AttemptSucceeded || attempt.Result == nil || attempt.CompletedAt == nil || validateStoredAttempt(attempt) != nil {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrNotReady
	}
	definition, ok := s.definitions[attempt.DefinitionID]
	if !ok || definition.ID != attempt.DefinitionID || definition.ProjectID != attempt.ProjectID || definition.ServiceID != attempt.ServiceID {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrConflict
	}
	projection, ok := s.releaseProjections[attemptID]
	if !ok {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrNotReady
	}
	if projection.state == ReleaseProjectionFailed {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrConflict
	}
	if projection.state != ReleaseProjectionSucceeded || projection.releaseID != attempt.ID || projection.completedAt == nil || projection.completedAt.Before(*attempt.CompletedAt) {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrNotReady
	}
	server := attempt.PlanRequest.Build.Registry.Server
	repository, err := targetRepository(attempt.PlanRequest.Build.Destination.Repository, server)
	if err != nil {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrConflict
	}
	result := buildpromotion.ProjectedBuild{AttemptID: attempt.ID, DefinitionID: attempt.DefinitionID, ProjectID: attempt.ProjectID, ApplicationID: attempt.ServiceID,
		Generation: attempt.Generation, CommitSHA: attempt.CommitSHA, DefinitionDigest: attempt.DefinitionDigest,
		RegistryTargetID: definition.Spec.Registry.TargetID, RegistryServer: server, Repository: repository,
		ImageReference: attempt.Result.Image.Reference, ImageDigest: attempt.Result.Image.Digest, ReleaseID: projection.releaseID,
		CreatedAt: attempt.CreatedAt.UTC(), CompletedAt: attempt.CompletedAt.UTC(), ProjectionCompletedAt: projection.completedAt.UTC()}
	if result.Validate() != nil {
		return buildpromotion.ProjectedBuild{}, buildpromotion.ErrConflict
	}
	return result, nil
}

var _ buildpromotion.ProjectionCatalog = (*PostgreSQLStore)(nil)
var _ buildpromotion.ProjectionCatalog = (*MemoryStore)(nil)
