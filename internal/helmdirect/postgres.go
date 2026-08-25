package helmdirect

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
)

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) (*PostgresStore, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &PostgresStore{pool: pool}, nil
}

const revisionColumns = `revision.id::text,revision.generation,revision.project_id::text,revision.environment_id::text,
	revision.application_id::text,revision.release_name,revision.destination_namespace,revision.argo_project,
	revision.source_kind,revision.repository_url,revision.chart,revision.target_revision,revision.chart_path,
	revision.values_yaml,revision.values_digest,revision.action,
	revision.desired_enabled,revision.state,revision.failure_code,revision.actor_id::text,revision.idempotency_key,
	revision.request_id,COALESCE(revision.parent_revision_id::text,''),COALESCE(revision.rollback_source_revision_id::text,''),
	revision.created_at,revision.updated_at`

type rowScanner interface{ Scan(...any) error }

func scanRevision(row rowScanner) (Revision, error) {
	var result Revision
	var sourceKind string
	var action, state string
	err := row.Scan(&result.ID, &result.Generation, &result.Target.ProjectID, &result.Target.EnvironmentID,
		&result.Target.ApplicationID, &result.ReleaseName, &result.DestinationNamespace, &result.ArgoProject,
		&sourceKind, &result.Source.RepositoryURL, &result.Source.Chart, &result.Source.TargetRevision, &result.Source.Path,
		&result.ValuesYAML, &result.ValuesDigest, &action, &result.DesiredEnabled,
		&state, &result.FailureCode, &result.ActorID, &result.IdempotencyKey, &result.RequestID,
		&result.ParentRevisionID, &result.RollbackSourceRevisionID, &result.CreatedAt, &result.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Revision{}, ErrNotFound
	}
	if err != nil {
		return Revision{}, err
	}
	result.Source.Kind, result.Action, result.State = SourceKind(sourceKind), Action(action), State(state)
	if result.Validate() != nil {
		return Revision{}, ErrConflict
	}
	return result, nil
}

func (s *PostgresStore) Deploy(ctx context.Context, request DeployRequest, now time.Time) (Revision, bool, error) {
	source, err := request.Source.Normalize()
	values, valuesErr := NormalizeValues(request.Values)
	if s == nil || s.pool == nil || request.Target.Validate() != nil || request.Actor.Validate() != nil || err != nil || valuesErr != nil || now.IsZero() {
		return Revision{}, false, ErrInvalid
	}
	request.Source, request.Values = source, values
	return s.create(ctx, request.Target, request.Actor, ActionDeploy, request.Source, request.Values, "", now.UTC())
}

func (s *PostgresStore) Retry(ctx context.Context, request MutationRequest, now time.Time) (Revision, bool, error) {
	return s.copyCurrent(ctx, request, ActionRetry, now)
}

func (s *PostgresStore) Disable(ctx context.Context, request MutationRequest, now time.Time) (Revision, bool, error) {
	return s.copyCurrent(ctx, request, ActionDisable, now)
}

func (s *PostgresStore) Rollback(ctx context.Context, request MutationRequest, now time.Time) (Revision, bool, error) {
	if !uuidRE.MatchString(request.RollbackSourceID) {
		return Revision{}, false, ErrInvalid
	}
	return s.copyCurrent(ctx, request, ActionRollback, now)
}

func (s *PostgresStore) copyCurrent(ctx context.Context, request MutationRequest, action Action, now time.Time) (Revision, bool, error) {
	if s == nil || s.pool == nil || request.Target.Validate() != nil || request.Actor.Validate() != nil || now.IsZero() {
		return Revision{}, false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Revision{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if existing, replay, replayErr := replayRevision(ctx, tx, request.Actor); replayErr != nil || replay {
		if replay && (existing.Target != request.Target || existing.Action != action ||
			existing.RollbackSourceRevisionID != request.RollbackSourceID) {
			return Revision{}, false, ErrConflict
		}
		return existing, replay, replayErr
	}
	current, err := headForUpdate(ctx, tx, request.Target)
	if err != nil {
		return Revision{}, false, err
	}
	if action == ActionDisable && !current.DesiredEnabled || action == ActionRetry && !current.DesiredEnabled {
		return Revision{}, false, ErrConflict
	}
	source, values, rollbackID := current.Source, current.ValuesYAML, ""
	if action == ActionRollback {
		rollbackID = request.RollbackSourceID
		rollback, rollbackErr := scanRevision(tx.QueryRow(ctx, `SELECT `+revisionColumns+` FROM helm_app_revisions revision
			WHERE revision.id=$1 AND revision.project_id=$2 AND revision.environment_id=$3 AND revision.application_id=$4`,
			rollbackID, request.Target.ProjectID, request.Target.EnvironmentID, request.Target.ApplicationID))
		if rollbackErr != nil || !rollback.DesiredEnabled {
			if rollbackErr != nil {
				return Revision{}, false, rollbackErr
			}
			return Revision{}, false, ErrConflict
		}
		source, values = rollback.Source, rollback.ValuesYAML
	}
	revision, err := insertRevision(ctx, tx, current, request.Actor, action, source, values, rollbackID, now.UTC())
	if err != nil {
		return Revision{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Revision{}, false, err
	}
	return revision, false, nil
}

func (s *PostgresStore) create(ctx context.Context, target Target, actor Actor, action Action, source Source, values []byte, rollbackID string, now time.Time) (Revision, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Revision{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if existing, replay, replayErr := replayRevision(ctx, tx, actor); replayErr != nil || replay {
		if replay && (existing.Target != target || existing.Action != action || existing.Source != source ||
			!bytes.Equal(existing.ValuesYAML, values)) {
			return Revision{}, false, ErrConflict
		}
		return existing, replay, replayErr
	}
	current, err := headForUpdate(ctx, tx, target)
	if errors.Is(err, ErrNotFound) {
		current, err = targetIdentity(ctx, tx, target, now)
	}
	if err != nil {
		return Revision{}, false, err
	}
	revision, err := insertRevision(ctx, tx, current, actor, action, source, values, rollbackID, now)
	if err != nil {
		return Revision{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Revision{}, false, err
	}
	return revision, false, nil
}

func replayRevision(ctx context.Context, tx pgx.Tx, actor Actor) (Revision, bool, error) {
	value, err := scanRevision(tx.QueryRow(ctx, `SELECT `+revisionColumns+` FROM helm_app_revisions revision WHERE actor_id=$1 AND idempotency_key=$2`, actor.ID, actor.IdempotencyKey))
	if errors.Is(err, ErrNotFound) {
		return Revision{}, false, nil
	}
	return value, err == nil, err
}

func targetIdentity(ctx context.Context, tx pgx.Tx, target Target, now time.Time) (Revision, error) {
	var result Revision
	result.Target, result.Generation, result.CreatedAt, result.UpdatedAt = target, 0, now, now
	err := tx.QueryRow(ctx, `SELECT application.slug,environment.namespace,environment.argo_project
		FROM applications application
		JOIN environment_app_placements placement ON placement.application_id=application.id AND placement.project_id=application.project_id
		JOIN environments environment ON environment.id=placement.environment_id AND environment.project_id=placement.project_id
		WHERE application.id=$1 AND application.project_id=$2 AND application.source_kind='helm' AND environment.id=$3`,
		target.ApplicationID, target.ProjectID, target.EnvironmentID).Scan(&result.ReleaseName, &result.DestinationNamespace, &result.ArgoProject)
	if errors.Is(err, pgx.ErrNoRows) {
		return Revision{}, ErrNotFound
	}
	return result, err
}

func headForUpdate(ctx context.Context, tx pgx.Tx, target Target) (Revision, error) {
	return scanRevision(tx.QueryRow(ctx, `SELECT `+revisionColumns+` FROM helm_app_heads head
		JOIN helm_app_revisions revision ON revision.id=head.revision_id
		WHERE head.environment_id=$1 AND head.application_id=$2 AND head.project_id=$3 FOR UPDATE`,
		target.EnvironmentID, target.ApplicationID, target.ProjectID))
}

func insertRevision(ctx context.Context, tx pgx.Tx, current Revision, actor Actor, action Action, source Source, values []byte, rollbackID string, now time.Time) (Revision, error) {
	desiredEnabled := action != ActionDisable
	revision := Revision{ID: id.New(), Generation: current.Generation + 1, Target: current.Target,
		ReleaseName: current.ReleaseName, DestinationNamespace: current.DestinationNamespace, ArgoProject: current.ArgoProject,
		Source: source, ValuesYAML: append([]byte(nil), values...), ValuesDigest: Digest(values), Action: action,
		DesiredEnabled: desiredEnabled, State: StatePending, ActorID: actor.ID, IdempotencyKey: actor.IdempotencyKey,
		RequestID: actor.RequestID, CreatedAt: now, UpdatedAt: now, RollbackSourceRevisionID: rollbackID}
	if current.Generation > 0 {
		revision.ParentRevisionID = current.ID
	}
	if revision.Validate() != nil {
		return Revision{}, ErrInvalid
	}
	_, err := tx.Exec(ctx, `INSERT INTO helm_app_revisions(id,generation,project_id,environment_id,application_id,
		release_name,destination_namespace,argo_project,source_kind,repository_url,chart,target_revision,chart_path,
		values_yaml,values_digest,action,desired_enabled,state,failure_code,actor_id,idempotency_key,
		request_id,parent_revision_id,rollback_source_revision_id,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,'',$19,$20,$21,NULLIF($22,'')::uuid,NULLIF($23,'')::uuid,$24,$24)`,
		revision.ID, revision.Generation, revision.Target.ProjectID, revision.Target.EnvironmentID, revision.Target.ApplicationID,
		revision.ReleaseName, revision.DestinationNamespace, revision.ArgoProject, revision.Source.Kind, revision.Source.RepositoryURL,
		revision.Source.Chart, revision.Source.TargetRevision, revision.Source.Path, revision.ValuesYAML,
		revision.ValuesDigest, revision.Action, revision.DesiredEnabled, revision.State, revision.ActorID, revision.IdempotencyKey,
		revision.RequestID, revision.ParentRevisionID, revision.RollbackSourceRevisionID, now)
	if err != nil {
		return Revision{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO helm_app_heads(project_id,environment_id,application_id,revision_id,generation,updated_at)
		VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(environment_id,application_id) DO UPDATE SET
		revision_id=EXCLUDED.revision_id,generation=EXCLUDED.generation,updated_at=EXCLUDED.updated_at`,
		revision.Target.ProjectID, revision.Target.EnvironmentID, revision.Target.ApplicationID, revision.ID, revision.Generation, now)
	if err != nil {
		return Revision{}, err
	}
	placementState, placementDesired := "active", "running"
	if !revision.DesiredEnabled {
		placementState, placementDesired = "draft", "stopped"
	}
	result, err := tx.Exec(ctx, `UPDATE environment_app_placements SET state=$4,desired_state=$5,updated_at=$6
		WHERE project_id=$1 AND environment_id=$2 AND application_id=$3`, revision.Target.ProjectID,
		revision.Target.EnvironmentID, revision.Target.ApplicationID, placementState, placementDesired, now)
	if err != nil || result.RowsAffected() != 1 {
		if err != nil {
			return Revision{}, err
		}
		return Revision{}, ErrConflict
	}
	return revision, nil
}

func (s *PostgresStore) Head(ctx context.Context, target Target) (Revision, error) {
	if s == nil || s.pool == nil || target.Validate() != nil {
		return Revision{}, ErrInvalid
	}
	return scanRevision(s.pool.QueryRow(ctx, `SELECT `+revisionColumns+` FROM helm_app_heads head
		JOIN helm_app_revisions revision ON revision.id=head.revision_id
		WHERE head.environment_id=$1 AND head.application_id=$2 AND head.project_id=$3`, target.EnvironmentID, target.ApplicationID, target.ProjectID))
}

func (s *PostgresStore) History(ctx context.Context, target Target, limit int) ([]Revision, error) {
	if s == nil || s.pool == nil || target.Validate() != nil || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT `+revisionColumns+` FROM helm_app_revisions revision
		WHERE project_id=$1 AND environment_id=$2 AND application_id=$3 ORDER BY generation DESC LIMIT $4`,
		target.ProjectID, target.EnvironmentID, target.ApplicationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Revision, 0, limit)
	for rows.Next() {
		item, scanErr := scanRevision(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *PostgresStore) Pending(ctx context.Context, limit int) ([]Revision, error) {
	if s == nil || s.pool == nil || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT `+revisionColumns+` FROM helm_app_heads head
		JOIN helm_app_revisions revision ON revision.id=head.revision_id WHERE revision.state='pending'
		ORDER BY revision.created_at,revision.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Revision, 0, limit)
	for rows.Next() {
		item, scanErr := scanRevision(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *PostgresStore) MarkApplied(ctx context.Context, revisionID string, now time.Time) error {
	return s.mark(ctx, revisionID, StateApplied, "", now)
}

func (s *PostgresStore) MarkFailed(ctx context.Context, revisionID, code string, now time.Time) error {
	if code == "" || len(code) > 63 || stringsContainControl(code) {
		return ErrInvalid
	}
	return s.mark(ctx, revisionID, StateFailed, code, now)
}

func (s *PostgresStore) mark(ctx context.Context, revisionID string, state State, code string, now time.Time) error {
	if s == nil || s.pool == nil || !uuidRE.MatchString(revisionID) || now.IsZero() {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE helm_app_revisions SET state=$2,failure_code=$3,updated_at=$4
		WHERE id=$1 AND state='pending'`, revisionID, state, code, now.UTC())
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}
