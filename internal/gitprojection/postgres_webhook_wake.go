package gitprojection

import (
	"context"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5"
)

func (s *PostgreSQLStore) WakeGitHubPush(ctx context.Context, wake GitHubPushWake) (GitHubPushWakeResult, error) {
	if s == nil || s.pool == nil || wake.Validate() != nil {
		return GitHubPushWakeResult{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return GitHubPushWakeResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	insert, err := tx.Exec(ctx, `INSERT INTO git_projection_push_wakes(delivery_hash,github_app_id,installation_id,repository_id,target_ref,after_commit,received_at)
		VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, wake.DeliveryHash, wake.GitHubAppID, wake.InstallationID,
		wake.RepositoryID, wake.TargetRef, wake.AfterCommit, wake.ReceivedAt.UTC())
	if err != nil {
		return GitHubPushWakeResult{}, classifyPostgres(err)
	}
	if insert.RowsAffected() == 0 {
		if err = tx.Commit(ctx); err != nil {
			return GitHubPushWakeResult{}, classifyPostgres(err)
		}
		return GitHubPushWakeResult{Replay: true}, nil
	}
	rows, err := tx.Query(ctx, `SELECT b.id::text
		FROM git_repository_bindings b
		JOIN github_installations i ON i.github_app_id=$1 AND i.github_installation_id=$2 AND i.lifecycle='active'
		JOIN github_repositories r ON r.installation_id=i.id AND r.github_repository_id=$3 AND r.lifecycle='active'
		WHERE b.provider='github' AND b.credential_mode='github-app' AND b.installation_id=$2 AND b.repository_id=$3 AND b.target_ref=$4
		ORDER BY b.id FOR UPDATE OF b`, wake.GitHubAppID, wake.InstallationID, wake.RepositoryID, wake.TargetRef)
	if err != nil {
		return GitHubPushWakeResult{}, classifyPostgres(err)
	}
	ids := []string{}
	for rows.Next() {
		var bindingID string
		if scanErr := rows.Scan(&bindingID); scanErr != nil {
			rows.Close()
			return GitHubPushWakeResult{}, classifyPostgres(scanErr)
		}
		ids = append(ids, bindingID)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return GitHubPushWakeResult{}, classifyPostgres(err)
	}
	sort.Strings(ids)
	result := GitHubPushWakeResult{Bindings: make([]BindingWake, 0, len(ids))}
	for _, bindingID := range ids {
		var generation int64
		err = tx.QueryRow(ctx, `SELECT wake_generation FROM git_safety_poll_cursors WHERE binding_id=$1 FOR UPDATE`, bindingID).Scan(&generation)
		if errors.Is(err, pgx.ErrNoRows) {
			generation = 1
			_, err = tx.Exec(ctx, `INSERT INTO git_safety_poll_cursors(binding_id,last_commit,provider_cursor,consecutive_failures,next_poll_at,updated_at,
				lease_epoch,last_error_code,wake_generation,reconciled_wake_generation)
				VALUES($1,NULL,'',0,$2,$2,0,'',1,0)`, bindingID, wake.ReceivedAt.UTC())
		} else if err == nil {
			generation++
			_, err = tx.Exec(ctx, `UPDATE git_safety_poll_cursors SET wake_generation=$2,next_poll_at=LEAST(next_poll_at,$3)
				WHERE binding_id=$1`, bindingID, generation, wake.ReceivedAt.UTC())
		}
		if err != nil {
			return GitHubPushWakeResult{}, classifyPostgres(err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO git_projection_push_wake_targets(delivery_hash,binding_id,wake_generation) VALUES($1,$2,$3)`,
			wake.DeliveryHash, bindingID, generation); err != nil {
			return GitHubPushWakeResult{}, classifyPostgres(err)
		}
		result.Bindings = append(result.Bindings, BindingWake{BindingID: bindingID, WakeGeneration: generation})
	}
	if err = tx.Commit(ctx); err != nil {
		return GitHubPushWakeResult{}, classifyPostgres(err)
	}
	return result, nil
}

var _ GitHubPushWakeStore = (*PostgreSQLStore)(nil)
