package argo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

type PostgreSQLStore struct{ pool *pgxpool.Pool }

func NewPostgreSQLStore(pool *pgxpool.Pool) (*PostgreSQLStore, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &PostgreSQLStore{pool: pool}, nil
}

func OpenPostgreSQLStore(ctx context.Context, databaseURL string) (*PostgreSQLStore, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, ErrInvalid
	}
	config.ConnConfig.RuntimeParams["application_name"] = "kuberploy-argo-observer"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &PostgreSQLStore{pool: pool}, nil
}

func (s *PostgreSQLStore) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

func (s *PostgreSQLStore) PutObservation(ctx context.Context, value Observation) error {
	if err := value.Validate(); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = putObservationTx(ctx, tx, value); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func putObservationTx(ctx context.Context, tx pgx.Tx, value Observation) error {
	resourceValues := value.Resources
	if resourceValues == nil {
		resourceValues = []ResourceIdentity{}
	}
	resources, err := json.Marshal(resourceValues)
	if err != nil {
		return ErrInvalid
	}
	current, err := scanObservation(tx.QueryRow(ctx, observationSelect+` WHERE deployment_id=$1 FOR UPDATE`, value.DeploymentID))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return classifyPostgres(err)
	}
	if err == nil {
		if value.ObservedAt.Before(current.ObservedAt) || value.UpdatedAt.Before(current.UpdatedAt) || current.ProjectID != value.ProjectID || current.EnvironmentID != value.EnvironmentID || current.ArgoUID != value.ArgoUID || current.ArgoNamespace != value.ArgoNamespace || current.ArgoName != value.ArgoName || current.DestinationNamespace != value.DestinationNamespace {
			return ErrConflict
		}
		if value.ObservedAt.Equal(current.ObservedAt) && value.UpdatedAt.Equal(current.UpdatedAt) {
			if !sameObservation(current, value) {
				return ErrConflict
			}
			return nil
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO argo_application_observations(deployment_id,application_id,project_id,environment_id,argo_uid,argo_namespace,argo_name,destination_namespace,desired_revision,observed_revision,sync_status,health_status,operation_phase,message,resources,observed_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) ON CONFLICT(deployment_id) DO UPDATE SET desired_revision=excluded.desired_revision,observed_revision=excluded.observed_revision,sync_status=excluded.sync_status,health_status=excluded.health_status,operation_phase=excluded.operation_phase,message=excluded.message,resources=excluded.resources,observed_at=excluded.observed_at,updated_at=excluded.updated_at`,
		value.DeploymentID, value.ApplicationID, value.ProjectID, value.EnvironmentID, value.ArgoUID, value.ArgoNamespace, value.ArgoName, value.DestinationNamespace, value.DesiredRevision, value.ObservedRevision, value.Sync, value.Health, value.OperationPhase, value.Message, resources, value.ObservedAt.UTC(), value.UpdatedAt.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	return nil
}

func (s *PostgreSQLStore) Observation(ctx context.Context, deploymentID string) (Observation, error) {
	value, err := scanObservation(s.pool.QueryRow(ctx, observationSelect+` WHERE deployment_id=$1`, deploymentID))
	if err != nil {
		return Observation{}, classifyPostgres(err)
	}
	return value, value.Validate()
}

const observationSelect = `SELECT deployment_id::text,application_id::text,project_id::text,environment_id::text,argo_uid::text,argo_namespace,argo_name,destination_namespace,desired_revision,observed_revision,sync_status,health_status,operation_phase,message,resources,observed_at,updated_at FROM argo_application_observations`

func scanObservation(row pgx.Row) (Observation, error) {
	var value Observation
	var resources []byte
	if err := row.Scan(&value.DeploymentID, &value.ApplicationID, &value.ProjectID, &value.EnvironmentID, &value.ArgoUID, &value.ArgoNamespace, &value.ArgoName, &value.DestinationNamespace, &value.DesiredRevision, &value.ObservedRevision, &value.Sync, &value.Health, &value.OperationPhase, &value.Message, &resources, &value.ObservedAt, &value.UpdatedAt); err != nil {
		return Observation{}, err
	}
	if err := json.Unmarshal(resources, &value.Resources); err != nil {
		return Observation{}, ErrInvalid
	}
	return value, nil
}

func (s *PostgreSQLStore) CreateRollback(ctx context.Context, command RollbackCommand, mutation gitprojection.Mutation) (bool, error) {
	if !validRollbackCreate(command, mutation) {
		return false, ErrInvalid
	}
	sum := sha256.Sum256(mutation.Content)
	if command.CandidateSHA256 != "sha256:"+hex.EncodeToString(sum[:]) {
		return false, ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `INSERT INTO argo_rollback_commands(id,application_id,project_id,environment_id,binding_id,operation_id,base_revision,expected_etag,release_repository,release_digest,path,candidate,candidate_sha256,commit_message,state,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) ON CONFLICT(operation_id) DO NOTHING`, command.ID, command.ApplicationID, command.ProjectID, command.EnvironmentID, command.BindingID, command.OperationID, command.BaseRevision, command.ExpectedETag, command.ReleaseRepository, command.ReleaseDigest, mutation.Path, mutation.Content, command.CandidateSHA256, mutation.Message, command.State, command.CreatedAt, command.UpdatedAt)
	if err != nil {
		return false, classifyPostgres(err)
	}
	if result.RowsAffected() == 1 {
		return true, nil
	}
	current, currentMutation, err := s.Rollback(ctx, command.ID)
	if errors.Is(err, ErrNotFound) {
		err = s.pool.QueryRow(ctx, `SELECT id::text FROM argo_rollback_commands WHERE operation_id=$1`, command.OperationID).Scan(&command.ID)
		if err != nil {
			return false, classifyPostgres(err)
		}
		current, currentMutation, err = s.Rollback(ctx, command.ID)
	}
	if err != nil {
		return false, err
	}
	if !sameRollbackCreate(current, currentMutation, command, mutation) {
		return false, ErrConflict
	}
	return false, nil
}

func (s *PostgreSQLStore) Rollback(ctx context.Context, id string) (RollbackCommand, gitprojection.Mutation, error) {
	var command RollbackCommand
	var mutation gitprojection.Mutation
	err := s.pool.QueryRow(ctx, `SELECT id::text,application_id::text,project_id::text,environment_id::text,binding_id::text,operation_id::text,base_revision,expected_etag,release_repository,release_digest,path,candidate,candidate_sha256,commit_message,state,COALESCE(git_revision,''),failure_code,created_at,updated_at FROM argo_rollback_commands WHERE id=$1`, id).
		Scan(&command.ID, &command.ApplicationID, &command.ProjectID, &command.EnvironmentID, &command.BindingID, &command.OperationID, &command.BaseRevision, &command.ExpectedETag, &command.ReleaseRepository, &command.ReleaseDigest, &mutation.Path, &mutation.Content, &command.CandidateSHA256, &mutation.Message, &command.State, &command.GitRevision, &command.FailureCode, &command.CreatedAt, &command.UpdatedAt)
	if err != nil {
		return RollbackCommand{}, gitprojection.Mutation{}, classifyPostgres(err)
	}
	mutation.BindingID, mutation.OperationID, mutation.BaseRevision, mutation.ExpectedETag = command.BindingID, command.OperationID, command.BaseRevision, command.ExpectedETag
	if err = command.Validate(); err != nil {
		return RollbackCommand{}, gitprojection.Mutation{}, err
	}
	return command, mutation, nil
}

func (s *PostgreSQLStore) CompleteRollback(ctx context.Context, id, revision string, now time.Time) error {
	if !commitRE.MatchString(revision) || now.IsZero() {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE argo_rollback_commands SET state='git-committed',git_revision=$2,updated_at=$3 WHERE id=$1 AND state='pending-git' AND updated_at<=$3`, id, revision, now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	command, _, getErr := s.Rollback(ctx, id)
	if getErr != nil {
		return getErr
	}
	if command.State == RollbackGitCommitted && command.GitRevision == revision {
		return nil
	}
	return ErrConflict
}

func (s *PostgreSQLStore) FailRollback(ctx context.Context, id, code string, now time.Time) error {
	if code == "" || len(code) > 64 || now.IsZero() {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE argo_rollback_commands SET state='failed',failure_code=$2,updated_at=$3 WHERE id=$1 AND state='pending-git' AND updated_at<=$3`, id, code, now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func classifyPostgres(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505", "40001":
			return ErrConflict
		case "23503":
			return ErrNotFound
		case "23514", "22P02", "22001":
			return ErrInvalid
		}
	}
	return fmt.Errorf("Argo projection database operation: %w", err)
}

var _ ObservationStore = (*PostgreSQLStore)(nil)
var _ RollbackStore = (*PostgreSQLStore)(nil)
