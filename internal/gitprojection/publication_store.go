package gitprojection

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
)

const publicationColumns = `operation_id::text,binding_id::text,installation_id,repository_id,repository_owner,repository_name,
	target_ref,base_revision,write_base_revision,candidate_ref,candidate_revision,pull_request_number,pull_request_url,pull_request_state,
	merge_revision,target_revision,state,provider_observed_at,created_at,updated_at,version`

func scanPublication(row rowScanner) (gitpublication.Publication, error) {
	var publication gitpublication.Publication
	err := row.Scan(&publication.OperationID, &publication.BindingID, &publication.Repository.InstallationID, &publication.Repository.ID,
		&publication.Repository.Owner, &publication.Repository.Name, &publication.TargetRef, &publication.BaseRevision,
		&publication.WriteBaseRevision, &publication.CandidateRef, &publication.CandidateRevision, &publication.PullRequestNumber, &publication.PullRequestURL,
		&publication.PullRequestState, &publication.MergeRevision, &publication.TargetRevision, &publication.State,
		&publication.ProviderObservedAt, &publication.CreatedAt, &publication.UpdatedAt, &publication.Version)
	if err != nil {
		return gitpublication.Publication{}, classifyPublicationError(err)
	}
	if publication.Validate() != nil {
		return gitpublication.Publication{}, gitpublication.ErrInvalid
	}
	return publication, nil
}

func (s *PostgreSQLStore) CreatePublication(ctx context.Context, publication gitpublication.Publication) error {
	if publication.Validate() != nil || publication.State != gitpublication.StatePendingCandidate || publication.Version != 1 {
		return gitpublication.ErrInvalid
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO git_pull_request_publications(operation_id,binding_id,provider,installation_id,repository_id,
		repository_owner,repository_name,target_ref,base_revision,write_base_revision,candidate_ref,state,created_at,updated_at,version)
		VALUES($1,$2,'github',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, publication.OperationID, publication.BindingID,
		publication.Repository.InstallationID, publication.Repository.ID, publication.Repository.Owner, publication.Repository.Name,
		publication.TargetRef, publication.BaseRevision, publication.WriteBaseRevision, publication.CandidateRef, publication.State, publication.CreatedAt,
		publication.UpdatedAt, publication.Version)
	return classifyPublicationError(err)
}

func (s *PostgreSQLStore) Publication(ctx context.Context, operationID string) (gitpublication.Publication, error) {
	return scanPublication(s.pool.QueryRow(ctx, `SELECT `+publicationColumns+` FROM git_pull_request_publications WHERE operation_id=$1`, operationID))
}

func (s *PostgreSQLStore) CompareAndSwapPublication(ctx context.Context, previous, next gitpublication.Publication) error {
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
		provider_observed_at=$11,updated_at=$12,version=$13 WHERE operation_id=$1 AND version=$2`, previous.OperationID,
		previous.Version, next.WriteBaseRevision, next.CandidateRevision, next.PullRequestNumber, next.PullRequestURL, next.PullRequestState,
		next.MergeRevision, next.TargetRevision, next.State, next.ProviderObservedAt, next.UpdatedAt, next.Version)
	if err != nil {
		return classifyPublicationError(err)
	}
	if result.RowsAffected() != 1 {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM git_pull_request_publications WHERE operation_id=$1)`, previous.OperationID).Scan(&exists); err != nil {
			return classifyPublicationError(err)
		}
		if !exists {
			return gitpublication.ErrNotFound
		}
		return gitpublication.ErrConflict
	}
	deploymentState := "review-pending"
	if next.State == gitpublication.StatePullRequestClosed {
		deploymentState = "review-closed"
	} else if next.State == gitpublication.StateMergePending || next.State == gitpublication.StateMergeVerified {
		deploymentState = "merge-pending-index"
	}
	if _, err = tx.Exec(ctx, `UPDATE deployments d SET state=$2,updated_at=$3 FROM operations o
		WHERE o.id=$1 AND d.id=o.target_id AND d.operation_id=o.id AND d.generation=o.generation`, next.OperationID, deploymentState, next.UpdatedAt); err != nil {
		return classifyPublicationError(err)
	}
	if next.State == gitpublication.StateMergeVerified {
		if err = convergeVerifiedPublicationTx(ctx, tx, next); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func convergeVerifiedPublicationTx(ctx context.Context, tx pgx.Tx, publication gitpublication.Publication) error {
	_, err := tx.Exec(ctx, `WITH indexed AS (
		UPDATE git_write_commands c SET state='indexed',committed_revision=$2,committed_at=$3,
			indexed_generation=b.projection_generation,indexed_at=$3,updated_at=$3
		FROM git_repository_bindings b,git_projection_generations g,git_projected_documents doc
		WHERE c.operation_id=$1 AND c.binding_id=$4 AND c.target_ref=$5
		AND c.publication_mode='pull-request' AND c.state='pending'
		AND b.id=c.binding_id AND b.state='ready' AND b.target_head_revision=b.indexed_revision
		AND b.projection_generation>0 AND g.binding_id=b.id AND g.generation=b.projection_generation AND g.state='active'
		AND doc.binding_id=b.id AND doc.generation=b.projection_generation AND doc.path=c.path
		AND doc.valid AND doc.content_sha256=c.content_sha256 AND doc.raw=c.content
		RETURNING c.operation_id,c.deployment_id,c.command_kind,c.indexed_generation
	)
	UPDATE deployments d SET state='git-committed',desired_revision=$2,updated_at=$3
	FROM indexed i,operations o
	WHERE i.command_kind='deployment' AND i.deployment_id IS NOT NULL AND o.id=i.operation_id
	AND d.id=i.deployment_id AND d.operation_id=i.operation_id AND d.generation=o.generation`,
		publication.OperationID, publication.TargetRevision, publication.UpdatedAt, publication.BindingID, publication.TargetRef)
	return classifyPublicationError(err)
}

func (s *PostgreSQLStore) PendingPublications(ctx context.Context, limit int) ([]gitpublication.Publication, error) {
	if limit <= 0 || limit > 100 {
		return nil, gitpublication.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT `+publicationColumns+` FROM git_pull_request_publications
		WHERE state IN ('pull-request-open','pull-request-closed','merge-pending') ORDER BY updated_at,operation_id LIMIT $1`, limit)
	if err != nil {
		return nil, classifyPublicationError(err)
	}
	defer rows.Close()
	values := make([]gitpublication.Publication, 0, limit)
	for rows.Next() {
		publication, scanErr := scanPublication(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		values = append(values, publication)
	}
	return values, classifyPublicationError(rows.Err())
}

func (s *PostgreSQLStore) GitHubPublicationAuthorization(ctx context.Context, repository gitpublication.Repository, appID int64) (gitpublication.GitHubAuthorization, error) {
	if repository.Validate() != nil || appID <= 0 {
		return gitpublication.GitHubAuthorization{}, gitpublication.ErrInvalid
	}
	var authorization gitpublication.GitHubAuthorization
	err := s.pool.QueryRow(ctx, `SELECT i.github_account_id,i.account_login,i.account_type,
		r.github_repository_id,r.github_owner_id,r.owner_login,r.name FROM github_installations i
		JOIN github_repositories r ON r.installation_id=i.id WHERE i.github_app_id=$1 AND i.github_installation_id=$2
		AND i.lifecycle='active' AND r.github_repository_id=$3 AND r.lifecycle='active'`, appID, repository.InstallationID,
		repository.ID).Scan(&authorization.Account.ID, &authorization.Account.Login, &authorization.Account.Type,
		&authorization.Repository.ID, &authorization.Repository.OwnerID, &authorization.Repository.OwnerLogin, &authorization.Repository.Name)
	if err != nil {
		return gitpublication.GitHubAuthorization{}, classifyPublicationError(err)
	}
	if authorization.Account.ID <= 0 || authorization.Repository.ID != repository.ID || authorization.Repository.Name != repository.Name ||
		authorization.Repository.OwnerID != authorization.Account.ID || !strings.EqualFold(authorization.Repository.OwnerLogin, repository.Owner) ||
		!strings.EqualFold(authorization.Account.Login, repository.Owner) ||
		(authorization.Account.Type != "User" && authorization.Account.Type != "Organization") {
		return gitpublication.GitHubAuthorization{}, gitpublication.ErrProviderMismatch
	}
	return authorization, nil
}

func classifyPublicationError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return gitpublication.ErrNotFound
	}
	var provider *pgconn.PgError
	if errors.As(err, &provider) && (provider.Code == "23503" || provider.Code == "23505" || provider.Code == "23514") {
		return gitpublication.ErrConflict
	}
	return err
}

var _ gitpublication.ReconcileStore = (*PostgreSQLStore)(nil)
var _ gitpublication.GitHubAuthorizationStore = (*PostgreSQLStore)(nil)
