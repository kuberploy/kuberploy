package builds

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/id"
)

func (s *PostgreSQLStore) DeleteDefinition(ctx context.Context, actorID, serviceID, definitionID, key, fingerprint, requestID string, now time.Time) (bool, error) {
	if !validAPICommand(APICommandDefinitionDelete, actorID, serviceID, key, fingerprint, definitionID, now) || requestID == "" {
		return false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "build-api-command|"+actorID+"|"+APICommandDefinitionDelete+"|"+serviceID+"|"+key); err != nil {
		return false, err
	}
	var storedFingerprint, storedResource string
	err = tx.QueryRow(ctx, `SELECT request_digest,resource_id::text FROM mutation_receipts
		WHERE actor_id=$1 AND receipt_kind='build-api' AND namespace=$2 AND scope_key=$3::text AND idempotency_key=$4 FOR UPDATE`,
		actorID, APICommandDefinitionDelete, serviceID, key).Scan(&storedFingerprint, &storedResource)
	if err == nil {
		if storedFingerprint != fingerprint || storedResource != definitionID {
			return false, ErrConflict
		}
		return true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, classifyPostgres(err)
	}
	var ownerService string
	if err = tx.QueryRow(ctx, `SELECT service_id::text FROM build_definitions WHERE id=$1 FOR UPDATE`, definitionID).Scan(&ownerService); err != nil {
		return false, classifyPostgres(err)
	}
	if ownerService != serviceID {
		return false, ErrNotFound
	}
	activeQueries := []string{
		`SELECT state FROM build_attempts WHERE definition_id=$1 AND state NOT IN ('succeeded','failed','cancelled') FOR UPDATE`,
		`SELECT p.state FROM build_release_projections p JOIN build_attempts a ON a.id=p.attempt_id WHERE a.definition_id=$1 AND p.state IN ('pending','processing') FOR UPDATE OF p`,
		`SELECT state FROM auto_deploy_runs WHERE definition_id=$1 AND state IN ('pending','processing') FOR UPDATE`,
	}
	for _, query := range activeQueries {
		rows, queryErr := tx.Query(ctx, query, definitionID)
		if queryErr != nil {
			return false, classifyPostgres(queryErr)
		}
		blocked := rows.Next()
		rows.Close()
		if rows.Err() != nil {
			return false, classifyPostgres(rows.Err())
		}
		if blocked {
			return false, ErrDeletionBlocked
		}
	}
	statements := []string{
		`DELETE FROM auto_deploy_runs WHERE definition_id=$1`,
		`DELETE FROM mutation_receipts WHERE receipt_kind='auto-deploy-policy' AND auto_deploy_policy_id IN (SELECT id FROM auto_deploy_policies WHERE build_definition_id=$1)`,
		`DELETE FROM auto_deploy_policy_revisions WHERE policy_id IN (SELECT id FROM auto_deploy_policies WHERE build_definition_id=$1)`,
		`DELETE FROM auto_deploy_policies WHERE build_definition_id=$1`,
		`DELETE FROM build_release_projections WHERE attempt_id IN (SELECT id FROM build_attempts WHERE definition_id=$1)`,
		`DELETE FROM build_attempts WHERE definition_id=$1`,
		`DELETE FROM build_definitions WHERE id=$1`,
	}
	for _, statement := range statements {
		if _, err = tx.Exec(ctx, statement, definitionID); err != nil {
			return false, classifyPostgres(err)
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,idempotency_key,request_digest,resource_id,created_at)
		VALUES($1,'build-api',$2,$3::text,$4,$5,$6,$7)`, actorID, APICommandDefinitionDelete, serviceID, key, fingerprint, definitionID, now.UTC()); err != nil {
		return false, classifyPostgres(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events(id,actor_id,action,target_type,target_id,request_id,detail)
		VALUES($1,$2,'build-definition.delete','build-definition',$3,$4,jsonb_build_object('applicationId',$5::text))`, id.New(), actorID, definitionID, requestID, serviceID); err != nil {
		return false, classifyPostgres(err)
	}
	return false, tx.Commit(ctx)
}
