package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
)

const gitPublicationColumns = `operation_id::text,binding_id::text,installation_id,repository_id,repository_owner,repository_name,
	target_ref,base_revision,write_base_revision,candidate_ref,candidate_revision,pull_request_number,pull_request_url,pull_request_state,
	merge_revision,target_revision,state,provider_observed_at,created_at,updated_at,version`

func scanGitPublication(row pgx.Row) (gitpublication.Publication, error) {
	var publication gitpublication.Publication
	err := row.Scan(&publication.OperationID, &publication.BindingID, &publication.Repository.InstallationID, &publication.Repository.ID,
		&publication.Repository.Owner, &publication.Repository.Name, &publication.TargetRef, &publication.BaseRevision,
		&publication.WriteBaseRevision, &publication.CandidateRef, &publication.CandidateRevision, &publication.PullRequestNumber,
		&publication.PullRequestURL, &publication.PullRequestState, &publication.MergeRevision, &publication.TargetRevision,
		&publication.State, &publication.ProviderObservedAt, &publication.CreatedAt, &publication.UpdatedAt, &publication.Version)
	if err != nil {
		return gitpublication.Publication{}, classifyGitPublicationError(err)
	}
	if publication.Validate() != nil {
		return gitpublication.Publication{}, gitpublication.ErrInvalid
	}
	return publication, nil
}

func insertGitPublicationTx(ctx context.Context, tx pgx.Tx, publication gitpublication.Publication) error {
	if publication.Validate() != nil || publication.State != gitpublication.StatePendingCandidate || publication.Version != 1 {
		return gitpublication.ErrInvalid
	}
	_, err := tx.Exec(ctx, `INSERT INTO git_pull_request_publications(operation_id,binding_id,provider,installation_id,repository_id,
		repository_owner,repository_name,target_ref,base_revision,write_base_revision,candidate_ref,candidate_revision,pull_request_number,
		pull_request_url,pull_request_state,merge_revision,target_revision,state,provider_observed_at,created_at,updated_at,version)
		VALUES($1,$2,'github',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
		publication.OperationID, publication.BindingID, publication.Repository.InstallationID, publication.Repository.ID, publication.Repository.Owner,
		publication.Repository.Name, publication.TargetRef, publication.BaseRevision, publication.WriteBaseRevision, publication.CandidateRef,
		publication.CandidateRevision, publication.PullRequestNumber, publication.PullRequestURL, publication.PullRequestState,
		publication.MergeRevision, publication.TargetRevision, publication.State, publication.ProviderObservedAt,
		publication.CreatedAt, publication.UpdatedAt, publication.Version)
	return classifyGitPublicationError(err)
}

func (s *Store) CreatePublication(ctx context.Context, publication gitpublication.Publication) error {
	if publication.Validate() != nil {
		return gitpublication.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	current, readErr := scanGitPublication(tx.QueryRow(ctx, `SELECT `+gitPublicationColumns+` FROM git_pull_request_publications WHERE operation_id=$1`, publication.OperationID))
	if readErr == nil {
		if sameGitPublication(current, publication) {
			return nil
		}
		return gitpublication.ErrConflict
	}
	if !errors.Is(readErr, gitpublication.ErrNotFound) {
		return readErr
	}
	err = insertGitPublicationTx(ctx, tx, publication)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Publication(ctx context.Context, operationID string) (gitpublication.Publication, error) {
	return scanGitPublication(s.pool.QueryRow(ctx, `SELECT `+gitPublicationColumns+` FROM git_pull_request_publications WHERE operation_id=$1`, operationID))
}

func (s *Store) CompareAndSwapPublication(ctx context.Context, previous, next gitpublication.Publication) error {
	if gitpublication.ValidateTransition(previous, next) != nil {
		return gitpublication.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	result, err := tx.Exec(ctx, `UPDATE git_pull_request_publications SET write_base_revision=$3,candidate_revision=$4,pull_request_number=$5,
		pull_request_url=$6,pull_request_state=$7,merge_revision=$8,target_revision=$9,state=$10,
		provider_observed_at=$11,updated_at=$12,version=$13 WHERE operation_id=$1 AND version=$2`,
		previous.OperationID, previous.Version, next.WriteBaseRevision, next.CandidateRevision, next.PullRequestNumber, next.PullRequestURL,
		next.PullRequestState, next.MergeRevision, next.TargetRevision, next.State, next.ProviderObservedAt,
		next.UpdatedAt, next.Version)
	if err != nil {
		return classifyGitPublicationError(err)
	}
	if result.RowsAffected() == 1 {
		deploymentState := "review-pending"
		if next.State == gitpublication.StatePullRequestClosed {
			deploymentState = "review-closed"
		} else if next.State == gitpublication.StateMergePending || next.State == gitpublication.StateMergeVerified {
			deploymentState = "merge-pending-index"
		}
		if _, err = tx.Exec(ctx, `UPDATE deployments d SET state=$2,updated_at=$3 FROM operations o
			WHERE o.id=$1 AND d.id=o.target_id AND d.operation_id=o.id AND d.generation=o.generation`, next.OperationID, deploymentState, next.UpdatedAt); err != nil {
			return classifyGitPublicationError(err)
		}
		return tx.Commit(ctx)
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM git_pull_request_publications WHERE operation_id=$1)`, previous.OperationID).Scan(&exists); err != nil {
		return classifyGitPublicationError(err)
	}
	if !exists {
		return gitpublication.ErrNotFound
	}
	return gitpublication.ErrConflict
}

func (s *Store) AcceptedGitPublicationMode(ctx context.Context, operationID string) (gitpublication.Mode, error) {
	var mode gitpublication.Mode
	if err := s.pool.QueryRow(ctx, `SELECT publication_mode FROM git_write_commands WHERE operation_id=$1 AND command_kind='deployment'`, operationID).Scan(&mode); err != nil {
		return "", classifyGitPublicationError(err)
	}
	if mode != gitpublication.ModeDirect && mode != gitpublication.ModePullRequest {
		return "", gitpublication.ErrInvalid
	}
	return mode, nil
}

func (s *Store) GitHubPublicationAuthorization(ctx context.Context, repository gitpublication.Repository, appID int64) (gitpublication.GitHubAuthorization, error) {
	if repository.Validate() != nil || appID <= 0 {
		return gitpublication.GitHubAuthorization{}, gitpublication.ErrInvalid
	}
	var authorization gitpublication.GitHubAuthorization
	err := s.pool.QueryRow(ctx, `SELECT i.github_account_id,i.account_login,i.account_type,
		r.github_repository_id,r.github_owner_id,r.owner_login,r.name
		FROM github_installations i
		JOIN github_repositories r ON r.installation_id=i.id
		WHERE i.github_app_id=$1 AND i.github_installation_id=$2 AND i.lifecycle='active'
		AND r.github_repository_id=$3 AND r.lifecycle='active'`, appID, repository.InstallationID, repository.ID).Scan(
		&authorization.Account.ID, &authorization.Account.Login, &authorization.Account.Type,
		&authorization.Repository.ID, &authorization.Repository.OwnerID, &authorization.Repository.OwnerLogin, &authorization.Repository.Name)
	if err != nil {
		return gitpublication.GitHubAuthorization{}, classifyGitPublicationError(err)
	}
	if authorization.Account.ID <= 0 || authorization.Repository.ID != repository.ID || authorization.Repository.Name != repository.Name ||
		authorization.Repository.OwnerID != authorization.Account.ID || !strings.EqualFold(authorization.Repository.OwnerLogin, repository.Owner) ||
		!strings.EqualFold(authorization.Account.Login, repository.Owner) ||
		(authorization.Account.Type != "User" && authorization.Account.Type != "Organization") {
		return gitpublication.GitHubAuthorization{}, gitpublication.ErrProviderMismatch
	}
	return authorization, nil
}

func classifyGitPublicationError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return gitpublication.ErrNotFound
	}
	var provider *pgconn.PgError
	if errors.As(err, &provider) && (provider.Code == "23505" || provider.Code == "23514" || provider.Code == "23503") {
		return gitpublication.ErrConflict
	}
	return err
}

func sameGitPublication(left, right gitpublication.Publication) bool {
	return left.OperationID == right.OperationID && left.BindingID == right.BindingID && left.Repository == right.Repository &&
		left.TargetRef == right.TargetRef && left.BaseRevision == right.BaseRevision && left.WriteBaseRevision == right.WriteBaseRevision && left.CandidateRef == right.CandidateRef &&
		left.CandidateRevision == right.CandidateRevision && left.PullRequestNumber == right.PullRequestNumber &&
		left.PullRequestURL == right.PullRequestURL && left.PullRequestState == right.PullRequestState &&
		left.MergeRevision == right.MergeRevision && left.TargetRevision == right.TargetRevision && left.State == right.State &&
		left.CreatedAt.Equal(right.CreatedAt) && left.UpdatedAt.Equal(right.UpdatedAt) && left.Version == right.Version &&
		((left.ProviderObservedAt == nil && right.ProviderObservedAt == nil) ||
			(left.ProviderObservedAt != nil && right.ProviderObservedAt != nil && left.ProviderObservedAt.Equal(*right.ProviderObservedAt)))
}

var _ gitpublication.Store = (*Store)(nil)
var _ gitpublication.GitHubAuthorizationStore = (*Store)(nil)
