package gitprojection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQLStore is intentionally package-owned so the projection subsystem
// can be wired without widening the main command Store interface. The caller
// supplies the already-migrated shared pool.
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
	config.ConnConfig.RuntimeParams["application_name"] = "kuberploy-git-projection"
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

func (s *PostgreSQLStore) GitHubAuthorization(ctx context.Context, binding Binding, appID int64) (GitHubAuthorization, error) {
	if binding.Validate() != nil || appID <= 0 {
		return GitHubAuthorization{}, ErrInvalid
	}
	var authorization GitHubAuthorization
	err := s.pool.QueryRow(ctx, `SELECT i.github_account_id,i.account_login,i.account_type,
		r.github_repository_id,r.github_owner_id,r.owner_login,r.name
		FROM github_installations i
		JOIN github_repositories r ON r.installation_id=i.id
		WHERE i.github_app_id=$1 AND i.github_installation_id=$2 AND i.lifecycle='active'
		AND r.github_repository_id=$3 AND r.lifecycle='active'`, appID, binding.Repository.InstallationID, binding.Repository.RepositoryID).Scan(
		&authorization.Account.ID, &authorization.Account.Login, &authorization.Account.Type,
		&authorization.Repository.ID, &authorization.Repository.OwnerID, &authorization.Repository.OwnerLogin, &authorization.Repository.Name)
	if err != nil {
		return GitHubAuthorization{}, classifyPostgres(err)
	}
	if authorization.ValidateFor(binding) != nil {
		return GitHubAuthorization{}, ErrProviderMismatch
	}
	return authorization, nil
}

var _ GitHubAuthorizationStore = (*PostgreSQLStore)(nil)

type rowScanner interface{ Scan(...any) error }
type rowQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func scanBinding(row rowScanner) (Binding, error) {
	var binding Binding
	var target, indexed *string
	var targetAt, indexedAt *time.Time
	err := row.Scan(&binding.ID, &binding.Kind, &binding.ScopeID, &binding.ProjectID, &binding.EnvironmentID,
		&binding.Repository.Provider, &binding.Repository.InstallationID, &binding.Repository.RepositoryID, &binding.Repository.Owner, &binding.Repository.Name,
		&binding.TargetRef, &binding.Prefix, &binding.CredentialMode, &binding.CredentialSecretName, &binding.State, &target, &indexed, &binding.ProjectionGeneration,
		&binding.ParserVersion, &targetAt, &indexedAt, &binding.CreatedAt, &binding.UpdatedAt)
	if err != nil {
		return Binding{}, classifyPostgres(err)
	}
	if target != nil {
		binding.TargetHeadRevision = *target
	}
	if indexed != nil {
		binding.IndexedRevision = *indexed
	}
	if targetAt != nil {
		binding.TargetHeadObservedAt = *targetAt
	}
	if indexedAt != nil {
		binding.IndexedAt = *indexedAt
	}
	return binding, binding.Validate()
}

const bindingColumns = `id,kind,scope_id::text,COALESCE(project_id::text,''),COALESCE(environment_id::text,''),provider,installation_id,repository_id,repository_owner,repository_name,target_ref,path_prefix,credential_mode,credential_secret_name,state,target_head_revision,indexed_revision,projection_generation,parser_version,target_head_observed_at,indexed_at,created_at,updated_at`

func getBinding(ctx context.Context, query rowQueryer, id string, suffix string) (Binding, error) {
	return scanBinding(query.QueryRow(ctx, `SELECT `+bindingColumns+` FROM git_repository_bindings WHERE id=$1 `+suffix, id))
}

func (s *PostgreSQLStore) PutBinding(ctx context.Context, binding Binding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	result, err := s.pool.Exec(ctx, `INSERT INTO git_repository_bindings(id,kind,scope_id,project_id,environment_id,provider,installation_id,repository_id,repository_owner,repository_name,target_ref,path_prefix,credential_mode,credential_secret_name,state,target_head_revision,indexed_revision,projection_generation,parser_version,target_head_observed_at,indexed_at,created_at,updated_at)
		VALUES($1,$2,$3,NULLIF($4,'')::uuid,NULLIF($5,'')::uuid,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,NULLIF($16,''),NULLIF($17,''),$18,$19,NULLIF($20,'0001-01-01T00:00:00Z'::timestamptz),NULLIF($21,'0001-01-01T00:00:00Z'::timestamptz),$22,$23) ON CONFLICT(id) DO NOTHING`,
		binding.ID, binding.Kind, binding.ScopeID, binding.ProjectID, binding.EnvironmentID, binding.Repository.Provider, binding.Repository.InstallationID, binding.Repository.RepositoryID, binding.Repository.Owner, binding.Repository.Name,
		binding.TargetRef, binding.Prefix, binding.CredentialMode, binding.CredentialSecretName, binding.State, binding.TargetHeadRevision, binding.IndexedRevision, binding.ProjectionGeneration, binding.ParserVersion, binding.TargetHeadObservedAt, binding.IndexedAt, binding.CreatedAt, binding.UpdatedAt)
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	current, err := s.Binding(ctx, binding.ID)
	if err != nil {
		return err
	}
	if immutableBinding(current) != immutableBinding(binding) {
		return ErrConflict
	}
	return nil
}

func (s *PostgreSQLStore) Binding(ctx context.Context, id string) (Binding, error) {
	return getBinding(ctx, s.pool, id, "")
}

func (s *PostgreSQLStore) BindingsForScope(ctx context.Context, kind BindingKind, scopeID string) ([]Binding, error) {
	if (kind != BindingEnvironment && kind != BindingPlatform) || !uuidRE.MatchString(scopeID) {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT `+bindingColumns+` FROM git_repository_bindings WHERE kind=$1 AND scope_id=$2 ORDER BY id LIMIT 2`, kind, scopeID)
	if err != nil {
		return nil, classifyPostgres(err)
	}
	defer rows.Close()
	values := make([]Binding, 0, 1)
	for rows.Next() {
		binding, scanErr := scanBinding(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		values = append(values, binding)
	}
	if err = rows.Err(); err != nil {
		return nil, classifyPostgres(err)
	}
	if len(values) > 1 {
		return nil, ErrConflict
	}
	return values, nil
}

func (s *PostgreSQLStore) SetBindingState(ctx context.Context, id, expectedHead string, state BindingState, now time.Time) error {
	if now.IsZero() || expectedHead != "" && !commitRE.MatchString(expectedHead) || state != BindingDiverged && state != BindingMissingRef && state != BindingWaiting && state != BindingIndexing {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE git_repository_bindings SET state=$3,updated_at=$4 WHERE id=$1 AND target_head_revision IS NOT DISTINCT FROM NULLIF($2,'') AND updated_at<=$4`, id, expectedHead, state, now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// InvalidateMatchingProfileMismatch forces same-head policy revalidation only
// when the active document still carries the exact corrected registry profile
// diagnostic. Already-valid projections remain untouched on periodic Secret
// observation.
func (s *PostgreSQLStore) InvalidateMatchingProfileMismatch(ctx context.Context, environmentID, targetID, profileName string, profileRevision int64, now time.Time) (bool, error) {
	if s == nil || s.pool == nil || !uuidRE.MatchString(environmentID) || !uuidRE.MatchString(targetID) ||
		!nameRE.MatchString(profileName) || profileRevision <= 0 || now.IsZero() {
		return false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, `SELECT `+bindingColumns+` FROM git_repository_bindings
		WHERE kind='environment' AND environment_id=$1 AND state='ready' AND target_head_revision=indexed_revision
		AND EXISTS (
			SELECT 1 FROM git_projected_documents d
			WHERE d.binding_id=git_repository_bindings.id AND d.generation=git_repository_bindings.projection_generation
			AND NOT d.valid AND d.diagnostics @> '[{"code":"RegistryPullProfileMismatch"}]'::jsonb
			AND d.parsed #>> '{spec,delivery,registryPull,targetId}'=$2
			AND d.parsed #>> '{spec,delivery,registryPull,profileName}'=$3
			AND d.parsed #>> '{spec,delivery,registryPull,profileRevision}'=$4::text
		) ORDER BY id FOR UPDATE`, environmentID, targetID, profileName, fmt.Sprintf("%d", profileRevision))
	if err != nil {
		return false, classifyPostgres(err)
	}
	bindings := make([]Binding, 0, 1)
	for rows.Next() {
		binding, scanErr := scanBinding(rows)
		if scanErr != nil {
			rows.Close()
			return false, scanErr
		}
		bindings = append(bindings, binding)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return false, classifyPostgres(err)
	}
	if len(bindings) == 0 {
		if err = tx.Commit(ctx); err != nil {
			return false, classifyPostgres(err)
		}
		return false, nil
	}
	if len(bindings) != 1 || bindings[0].State != BindingReady || bindings[0].TargetHeadRevision == "" ||
		bindings[0].TargetHeadRevision != bindings[0].IndexedRevision || now.Before(bindings[0].UpdatedAt) {
		return false, ErrConflict
	}
	result, err := tx.Exec(ctx, `UPDATE git_repository_bindings SET state='indexing',updated_at=$2
		WHERE id=$1 AND state='ready' AND target_head_revision=indexed_revision AND updated_at<=$2`, bindings[0].ID, now.UTC())
	if err != nil {
		return false, classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return false, ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return false, classifyPostgres(err)
	}
	return true, nil
}

func (s *PostgreSQLStore) RecordVerifiedHead(ctx context.Context, head VerifiedHead) (Binding, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Binding{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	binding, err := getBinding(ctx, tx, head.BindingID, "FOR UPDATE")
	if err != nil {
		return Binding{}, false, err
	}
	if err = head.ValidateFor(binding); err != nil {
		return Binding{}, false, err
	}
	if head.ObservedAt.Before(binding.CreatedAt) || !binding.TargetHeadObservedAt.IsZero() && (head.ObservedAt.Before(binding.TargetHeadObservedAt) || head.ObservedAt.Equal(binding.TargetHeadObservedAt) && head.Commit != binding.TargetHeadRevision) {
		return Binding{}, false, ErrConflict
	}
	result, err := tx.Exec(ctx, `INSERT INTO git_verified_head_observations(binding_id,provider,installation_id,repository_id,repository_owner,repository_name,target_ref,commit_revision,source,provider_request,observed_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING`, head.BindingID, head.Repository.Provider, head.Repository.InstallationID, head.Repository.RepositoryID, head.Repository.Owner, head.Repository.Name, head.TargetRef, head.Commit, head.Source, head.ProviderRequest, head.ObservedAt.UTC())
	if err != nil {
		return Binding{}, false, classifyPostgres(err)
	}
	replay := result.RowsAffected() == 0
	state := BindingIndexing
	// BindingIndexing can also mean metadata policy changed while the Git head
	// stayed constant. Preserve that durable revalidation request across the
	// provider observation instead of incorrectly turning it back into ready.
	if binding.IndexedRevision == head.Commit && binding.State != BindingIndexing {
		state = BindingReady
	}
	if binding.State == BindingDiverged {
		state = binding.State
	}
	updatedAt := head.ObservedAt.UTC()
	if !updatedAt.After(binding.UpdatedAt) {
		updatedAt = binding.UpdatedAt.Add(time.Microsecond)
	}
	_, err = tx.Exec(ctx, `UPDATE git_repository_bindings SET target_head_revision=$2,target_head_observed_at=$3,state=$4,updated_at=$5 WHERE id=$1`, binding.ID, head.Commit, head.ObservedAt.UTC(), state, updatedAt)
	if err != nil {
		return Binding{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Binding{}, false, classifyPostgres(err)
	}
	binding.TargetHeadRevision, binding.TargetHeadObservedAt, binding.State, binding.UpdatedAt = head.Commit, head.ObservedAt.UTC(), state, updatedAt
	return binding, replay, nil
}

func (s *PostgreSQLStore) ClaimWebhook(ctx context.Context, value WebhookTombstone) (bool, error) {
	if err := value.Validate(); err != nil {
		return false, err
	}
	result, err := s.pool.Exec(ctx, `INSERT INTO git_webhook_tombstones(provider,repository_id,target_ref,after_commit,delivery_hash,received_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, value.Provider, value.RepositoryID, value.TargetRef, value.AfterCommit, value.DeliveryHash, value.ReceivedAt.UTC())
	if err != nil {
		return false, classifyPostgres(err)
	}
	return result.RowsAffected() == 1, nil
}

func (s *PostgreSQLStore) PollCursor(ctx context.Context, bindingID string) (PollCursor, error) {
	var value PollCursor
	var commit *string
	err := s.pool.QueryRow(ctx, `SELECT binding_id::text,last_commit,provider_cursor,consecutive_failures,next_poll_at,updated_at FROM git_safety_poll_cursors WHERE binding_id=$1`, bindingID).Scan(&value.BindingID, &commit, &value.ProviderCursor, &value.ConsecutiveFail, &value.NextPollAt, &value.UpdatedAt)
	if err != nil {
		return PollCursor{}, classifyPostgres(err)
	}
	if commit != nil {
		value.LastCommit = *commit
	}
	return value, value.Validate()
}

func (s *PostgreSQLStore) PutPollCursor(ctx context.Context, value PollCursor) error {
	if err := value.Validate(); err != nil {
		return err
	}
	result, err := s.pool.Exec(ctx, `INSERT INTO git_safety_poll_cursors(binding_id,last_commit,provider_cursor,consecutive_failures,next_poll_at,updated_at,reconciled_binding_updated_at) VALUES($1,NULLIF($2,''),$3,$4,$5,$6,(SELECT updated_at FROM git_repository_bindings WHERE id=$1))
		ON CONFLICT(binding_id) DO UPDATE SET last_commit=excluded.last_commit,provider_cursor=excluded.provider_cursor,consecutive_failures=excluded.consecutive_failures,next_poll_at=excluded.next_poll_at,updated_at=excluded.updated_at
		,reconciled_binding_updated_at=excluded.reconciled_binding_updated_at,last_error_code=''
		WHERE git_safety_poll_cursors.lease_owner IS NULL AND (excluded.updated_at>git_safety_poll_cursors.updated_at OR
			(excluded.updated_at=git_safety_poll_cursors.updated_at AND excluded.last_commit IS NOT DISTINCT FROM git_safety_poll_cursors.last_commit
			AND excluded.provider_cursor=git_safety_poll_cursors.provider_cursor AND excluded.consecutive_failures=git_safety_poll_cursors.consecutive_failures
			AND excluded.next_poll_at=git_safety_poll_cursors.next_poll_at))`, value.BindingID, value.LastCommit, value.ProviderCursor, value.ConsecutiveFail, value.NextPollAt, value.UpdatedAt)
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *PostgreSQLStore) ClaimReconciliation(ctx context.Context, owner string, now time.Time, duration time.Duration) (ReconciliationWork, error) {
	if !ownerRE.MatchString(owner) || now.IsZero() || !validReconciliationLeaseDuration(duration) {
		return ReconciliationWork{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ReconciliationWork{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var bindingID string
	err = tx.QueryRow(ctx, `SELECT b.id::text
		FROM git_repository_bindings b
		LEFT JOIN git_safety_poll_cursors c ON c.binding_id=b.id
		WHERE b.credential_mode='github-app' AND (c.binding_id IS NULL OR c.updated_at<=$1) AND b.updated_at<=$1
		AND (c.lease_until IS NULL OR c.lease_until<=$1)
		AND (c.binding_id IS NULL OR c.lease_until IS NOT NULL OR c.next_poll_at<=$1 OR c.reconciled_binding_updated_at IS DISTINCT FROM b.updated_at)
		ORDER BY CASE WHEN c.lease_until IS NOT NULL THEN 0 WHEN c.reconciled_binding_updated_at IS DISTINCT FROM b.updated_at THEN 1 ELSE 2 END,
			COALESCE(c.next_poll_at,b.created_at),b.id
		FOR UPDATE OF b SKIP LOCKED LIMIT 1`, now.UTC()).Scan(&bindingID)
	if err != nil {
		return ReconciliationWork{}, classifyPostgres(err)
	}
	binding, err := getBinding(ctx, tx, bindingID, "")
	if err != nil {
		return ReconciliationWork{}, err
	}
	var failures int
	var epoch int64
	var oldLeaseUntil *time.Time
	var reconciledBindingUpdatedAt *time.Time
	var wakeGeneration int64
	err = tx.QueryRow(ctx, `SELECT consecutive_failures,lease_epoch,lease_until,reconciled_binding_updated_at,wake_generation FROM git_safety_poll_cursors WHERE binding_id=$1 FOR UPDATE`, bindingID).Scan(&failures, &epoch, &oldLeaseUntil, &reconciledBindingUpdatedAt, &wakeGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		epoch = 1
		_, err = tx.Exec(ctx, `INSERT INTO git_safety_poll_cursors(binding_id,last_commit,provider_cursor,consecutive_failures,next_poll_at,updated_at,lease_owner,lease_epoch,lease_until,last_error_code)
			VALUES($1,NULL,'',0,$2,$2,$3,$4,$5,'')`, bindingID, now.UTC(), owner, epoch, now.UTC().Add(duration))
	} else if err == nil {
		epoch++
		_, err = tx.Exec(ctx, `UPDATE git_safety_poll_cursors SET lease_owner=$2,lease_epoch=$3,lease_until=$4,updated_at=$5
			WHERE binding_id=$1`, bindingID, owner, epoch, now.UTC().Add(duration), now.UTC())
	}
	if err != nil {
		return ReconciliationWork{}, classifyPostgres(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return ReconciliationWork{}, classifyPostgres(err)
	}
	lease := ReconciliationLease{BindingID: bindingID, Owner: owner, Epoch: epoch, WakeGeneration: wakeGeneration, Until: now.UTC().Add(duration)}
	bindingChanged := reconciledBindingUpdatedAt == nil || !reconciledBindingUpdatedAt.Equal(binding.UpdatedAt)
	return ReconciliationWork{Binding: binding, Lease: lease, ConsecutiveFailure: failures, Reclaimed: oldLeaseUntil != nil, BindingChanged: bindingChanged}, nil
}

func (s *PostgreSQLStore) HeartbeatReconciliation(ctx context.Context, lease ReconciliationLease, now time.Time, duration time.Duration) (ReconciliationLease, error) {
	if lease.Validate() != nil || now.IsZero() || !validReconciliationLeaseDuration(duration) {
		return ReconciliationLease{}, ErrInvalid
	}
	until := now.UTC().Add(duration)
	result, err := s.pool.Exec(ctx, `UPDATE git_safety_poll_cursors SET lease_until=$4,updated_at=$5
		WHERE binding_id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND lease_until>$5 AND lease_until<$4 AND updated_at<=$5`, lease.BindingID, lease.Owner, lease.Epoch, until, now.UTC())
	if err != nil {
		return ReconciliationLease{}, classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ReconciliationLease{}, ErrLeaseLost
	}
	lease.Until = until
	return lease, nil
}

func (s *PostgreSQLStore) FinishReconciliation(ctx context.Context, lease ReconciliationLease, outcome ReconciliationOutcome, now time.Time) error {
	if lease.Validate() != nil || outcome.Validate() != nil || now.IsZero() || !outcome.NextPollAt.After(now) {
		return ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	binding, err := getBinding(ctx, tx, lease.BindingID, "FOR UPDATE")
	if err != nil {
		return err
	}
	if now.Before(binding.UpdatedAt) {
		return ErrConflict
	}
	if outcome.ConsecutiveFailure == 0 && (binding.State != BindingReady || binding.TargetHeadRevision != outcome.LastCommit || binding.IndexedRevision != outcome.LastCommit) ||
		outcome.ConsecutiveFailure > 0 && outcome.LastCommit != "" && binding.TargetHeadRevision != outcome.LastCommit {
		return ErrConflict
	}
	result, err := tx.Exec(ctx, `UPDATE git_safety_poll_cursors
		SET last_commit=NULLIF($4,''),consecutive_failures=$5,
			next_poll_at=CASE WHEN wake_generation>$10 THEN LEAST(next_poll_at,$7) ELSE $6 END,updated_at=$7,
			reconciled_binding_updated_at=$8,reconciled_wake_generation=$10,last_error_code=$9,lease_owner=NULL,lease_until=NULL
		WHERE binding_id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND lease_until>$7 AND updated_at<=$7`, lease.BindingID, lease.Owner, lease.Epoch,
		outcome.LastCommit, outcome.ConsecutiveFailure, outcome.NextPollAt.UTC(), now.UTC(), binding.UpdatedAt.UTC(), outcome.FailureCode, lease.WakeGeneration)
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return classifyPostgres(tx.Commit(ctx))
}

func (s *PostgreSQLStore) ReleaseReconciliation(ctx context.Context, lease ReconciliationLease, now time.Time) error {
	if lease.Validate() != nil || now.IsZero() {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE git_safety_poll_cursors
		SET next_poll_at=LEAST(next_poll_at,$4),updated_at=$4,lease_owner=NULL,lease_until=NULL
		WHERE binding_id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND lease_until>$4 AND updated_at<=$4`, lease.BindingID, lease.Owner, lease.Epoch, now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func requireActiveReconciliation(ctx context.Context, query rowQueryer, lease ReconciliationLease, now time.Time) error {
	if lease.Validate() != nil || now.IsZero() {
		return ErrInvalid
	}
	var active bool
	err := query.QueryRow(ctx, `SELECT lease_owner=$2 AND lease_epoch=$3 AND lease_until>$4
		FROM git_safety_poll_cursors WHERE binding_id=$1 FOR UPDATE`, lease.BindingID, lease.Owner, lease.Epoch, now.UTC()).Scan(&active)
	if err != nil {
		return classifyPostgres(err)
	}
	if !active {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgreSQLStore) BeginGeneration(ctx context.Context, lease ReconciliationLease, expectedHead, parser string, now time.Time) (Generation, error) {
	if lease.Validate() != nil || now.IsZero() || !commitRE.MatchString(expectedHead) || parser == "" || len(parser) > 64 {
		return Generation{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Generation{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	bindingID := lease.BindingID
	binding, err := getBinding(ctx, tx, bindingID, "FOR UPDATE")
	if err != nil {
		return Generation{}, err
	}
	if err = requireActiveReconciliation(ctx, tx, lease, now); err != nil {
		return Generation{}, err
	}
	if binding.State == BindingDiverged {
		return Generation{}, ErrDiverged
	}
	if binding.State == BindingMissingRef {
		return Generation{}, ErrMissingRef
	}
	if binding.TargetHeadRevision != expectedHead || binding.ParserVersion != parser {
		return Generation{}, ErrConflict
	}
	if now.Before(binding.UpdatedAt) {
		return Generation{}, ErrConflict
	}
	if _, err = tx.Exec(ctx, `DELETE FROM git_projected_documents WHERE binding_id=$1 AND generation IN
		(SELECT generation FROM git_projection_generations WHERE binding_id=$1 AND state='staging')`, bindingID); err != nil {
		return Generation{}, classifyPostgres(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE git_projection_generations SET state='failed' WHERE binding_id=$1 AND state='staging'`, bindingID); err != nil {
		return Generation{}, classifyPostgres(err)
	}
	var number int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(generation),0)+1 FROM git_projection_generations WHERE binding_id=$1`, bindingID).Scan(&number); err != nil {
		return Generation{}, err
	}
	g := Generation{BindingID: bindingID, Number: number, HeadRevision: expectedHead, ParserVersion: parser, State: ProjectionStaging, StartedAt: now.UTC()}
	_, err = tx.Exec(ctx, `INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at) VALUES($1,$2,$3,$4,$5,$6)`, g.BindingID, g.Number, g.HeadRevision, g.ParserVersion, g.State, g.StartedAt)
	if err != nil {
		return Generation{}, classifyPostgres(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return Generation{}, classifyPostgres(err)
	}
	return g, nil
}

func (s *PostgreSQLStore) PutDocuments(ctx context.Context, generation Generation, documents []Document) error {
	if generation.State != ProjectionStaging || len(documents) > 1000 {
		return ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	binding, err := getBinding(ctx, tx, generation.BindingID, "")
	if err != nil {
		return err
	}
	var state ProjectionState
	var head, parser string
	if err = tx.QueryRow(ctx, `SELECT state,head_revision,parser_version FROM git_projection_generations WHERE binding_id=$1 AND generation=$2 FOR UPDATE`, generation.BindingID, generation.Number).Scan(&state, &head, &parser); err != nil {
		return classifyPostgres(err)
	}
	if state != ProjectionStaging || head != generation.HeadRevision || parser != generation.ParserVersion {
		return ErrConflict
	}
	for _, document := range documents {
		if document.Validate(binding) != nil || document.Generation != generation.Number || document.SourceRevision != generation.HeadRevision || document.IndexedAt.Before(generation.StartedAt) {
			return ErrInvalid
		}
		parsed, marshalErr := json.Marshal(document.Parsed)
		if marshalErr != nil {
			return ErrInvalid
		}
		diagnosticValues := document.Diagnostics
		if diagnosticValues == nil {
			diagnosticValues = []Diagnostic{}
		}
		diagnostics, marshalErr := json.Marshal(diagnosticValues)
		if marshalErr != nil {
			return ErrInvalid
		}
		result, execErr := tx.Exec(ctx, `INSERT INTO git_projected_documents(binding_id,generation,path,application_id,source_revision,config_revision,blob_id,content_sha256,raw,parsed,valid,diagnostics,schema_version,parser_version,indexed_at)
			VALUES($1,$2,$3,NULLIF($4,'')::uuid,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) ON CONFLICT(binding_id,generation,path) DO UPDATE SET path=excluded.path
			WHERE git_projected_documents.application_id IS NOT DISTINCT FROM excluded.application_id
			AND git_projected_documents.source_revision=excluded.source_revision AND git_projected_documents.config_revision=excluded.config_revision
			AND git_projected_documents.blob_id=excluded.blob_id AND git_projected_documents.content_sha256=excluded.content_sha256 AND git_projected_documents.raw=excluded.raw
			AND git_projected_documents.parsed IS NOT DISTINCT FROM excluded.parsed AND git_projected_documents.valid=excluded.valid
			AND git_projected_documents.diagnostics=excluded.diagnostics AND git_projected_documents.schema_version=excluded.schema_version
			AND git_projected_documents.parser_version=excluded.parser_version`, document.BindingID, document.Generation, document.Path, document.ApplicationID, document.SourceRevision, document.ConfigRevision, document.BlobID, document.ContentSHA256, document.Raw, parsed, document.Valid, diagnostics, document.SchemaVersion, document.ParserVersion, document.IndexedAt)
		if execErr != nil {
			return classifyPostgres(execErr)
		}
		if result.RowsAffected() != 1 {
			return ErrConflict
		}
	}
	return tx.Commit(ctx)
}

func (s *PostgreSQLStore) ActivateGeneration(ctx context.Context, lease ReconciliationLease, generation Generation, policy AppConfigPolicyValidator, now time.Time) (Binding, error) {
	if lease.Validate() != nil || now.IsZero() || generation.State != ProjectionStaging || lease.BindingID != generation.BindingID || policy == nil {
		return Binding{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Binding{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	binding, err := getBinding(ctx, tx, generation.BindingID, "FOR UPDATE")
	if err != nil {
		return Binding{}, err
	}
	if err = requireActiveReconciliation(ctx, tx, lease, now); err != nil {
		return Binding{}, err
	}
	var state ProjectionState
	var head, parser string
	var started time.Time
	if err = tx.QueryRow(ctx, `SELECT state,head_revision,parser_version,started_at FROM git_projection_generations WHERE binding_id=$1 AND generation=$2 FOR UPDATE`, generation.BindingID, generation.Number).Scan(&state, &head, &parser, &started); err != nil {
		return Binding{}, classifyPostgres(err)
	}
	if state != ProjectionStaging || head != generation.HeadRevision || parser != generation.ParserVersion || binding.TargetHeadRevision != generation.HeadRevision || binding.ParserVersion != generation.ParserVersion || now.Before(started) || now.Before(binding.UpdatedAt) {
		return Binding{}, ErrConflict
	}
	currentDocuments, err := postgresPolicyDocuments(ctx, tx, binding.ID, generation.Number)
	if err != nil {
		return Binding{}, err
	}
	previousDocuments := []Document{}
	if binding.ProjectionGeneration > 0 {
		previousDocuments, err = postgresPolicyDocuments(ctx, tx, binding.ID, binding.ProjectionGeneration)
		if err != nil {
			return Binding{}, err
		}
	}
	input := AppConfigPolicyInput{Binding: binding, Generation: generation, Current: currentDocuments, Previous: previousDocuments}
	validation := AppConfigPolicyValidation{Diagnostics: map[string][]Diagnostic{}}
	if binding.Kind == BindingPlatform {
		if len(currentDocuments) != 0 || len(previousDocuments) != 0 {
			return Binding{}, ErrConflict
		}
	} else {
		transactionPolicy, ok := policy.(PostgreSQLAppConfigPolicyValidator)
		if !ok {
			return Binding{}, ErrPolicyUnavailable
		}
		validation, err = transactionPolicy.ValidateAppConfigsTx(ctx, tx, input, now.UTC())
		if err != nil {
			return Binding{}, err
		}
	}
	if validation.ValidateFor(input) != nil {
		return Binding{}, ErrInvalid
	}
	validatedDocuments := []Document{}
	if binding.Kind != BindingPlatform {
		validatedDocuments, err = applyPolicyValidation(binding, currentDocuments, validation)
		if err != nil {
			return Binding{}, err
		}
		validatedDocuments, err = applyEffectiveConfigRevisions(validatedDocuments, previousDocuments, generation.HeadRevision)
		if err != nil {
			return Binding{}, err
		}
	}
	for _, document := range validatedDocuments {
		diagnostics := document.Diagnostics
		if diagnostics == nil {
			diagnostics = []Diagnostic{}
		}
		diagnosticsJSON, marshalErr := json.Marshal(diagnostics)
		if marshalErr != nil {
			return Binding{}, ErrInvalid
		}
		result, updateErr := tx.Exec(ctx, `UPDATE git_projected_documents SET config_revision=$4,valid=$5,diagnostics=$6
			WHERE binding_id=$1 AND generation=$2 AND path=$3 AND blob_id=$7 AND source_revision=$8`,
			binding.ID, generation.Number, document.Path, document.ConfigRevision, document.Valid, diagnosticsJSON, document.BlobID, generation.HeadRevision)
		if updateErr != nil {
			return Binding{}, classifyPostgres(updateErr)
		}
		if result.RowsAffected() != 1 {
			return Binding{}, ErrConflict
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE git_projection_generations SET state='active',activated_at=$3 WHERE binding_id=$1 AND generation=$2`, generation.BindingID, generation.Number, now.UTC()); err != nil {
		return Binding{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE git_repository_bindings SET indexed_revision=$2,projection_generation=$3,indexed_at=$4,state='ready',updated_at=$4 WHERE id=$1`, binding.ID, generation.HeadRevision, generation.Number, now.UTC()); err != nil {
		return Binding{}, err
	}
	// Projection activation alone does not make a new effective values revision
	// desired. The Argo ApplicationSet can still be pinned to the last verified
	// generation, especially while dynamic provider readiness changes. Deployment
	// status advances after the matching Argo desired-state command is verified.
	// The exact activated provider head converges every committed reservation
	// visible under the binding lock, including an operation commit followed by
	// a later normal fast-forward before indexing. Advance only commands linked
	// to those durable reservations, then release the path fences atomically.
	if _, err = tx.Exec(ctx, `UPDATE git_write_commands c SET state='indexed',indexed_generation=$2,indexed_at=$3,updated_at=$3
		FROM git_path_reservations r
		WHERE r.binding_id=$1 AND r.state='committed-pending-index' AND c.operation_id=r.operation_id
		AND c.binding_id=r.binding_id AND c.target_ref=r.target_ref AND c.path=r.path AND c.state='git-committed'`, binding.ID, generation.Number, now.UTC()); err != nil {
		return Binding{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE git_write_commands SET state='indexed',indexed_generation=$3,indexed_at=$4,updated_at=$4
		WHERE binding_id=$1 AND state='git-committed' AND committed_revision=$2`, binding.ID, generation.HeadRevision, generation.Number, now.UTC()); err != nil {
		return Binding{}, err
	}
	// A direct operation records the provider commit provisionally. Indexing is
	// the first authority that can distinguish that commit from the effective
	// config revision retained when the exact document bytes did not change.
	// Converge the deployment to that projected revision so Argo and the API
	// observe the same immutable content identity.
	if _, err = tx.Exec(ctx, `UPDATE deployments d SET state='git-committed',desired_revision=doc.config_revision,updated_at=$3
		FROM git_write_commands c,git_projected_documents doc,operations o
		WHERE c.binding_id=$1 AND c.command_kind='deployment' AND c.action='upsert' AND c.publication_mode='direct'
		AND c.state='indexed' AND c.indexed_generation=$2 AND c.deployment_id IS NOT NULL
		AND doc.binding_id=c.binding_id AND doc.generation=c.indexed_generation AND doc.path=c.path
		AND doc.valid AND doc.content_sha256=c.content_sha256 AND doc.raw=c.content
		AND o.id=c.operation_id AND d.id=c.deployment_id AND d.operation_id=c.operation_id AND d.generation=o.generation`,
		binding.ID, generation.Number, now.UTC()); err != nil {
		return Binding{}, err
	}
	// A protected command becomes desired only after both independent proofs:
	// the provider receipt verified its merge on the authoritative target ref,
	// and this activated generation contains the exact accepted path bytes.
	// This also permits later descendants without trusting ancestry guesses.
	if _, err = tx.Exec(ctx, `UPDATE git_write_commands c SET state='indexed',committed_revision=p.target_revision,
		committed_at=p.updated_at,indexed_generation=$2,indexed_at=$3,updated_at=$3
		FROM git_pull_request_publications p
		WHERE c.binding_id=$1 AND c.publication_mode='pull-request' AND c.state='pending' AND p.operation_id=c.operation_id
		AND p.state='merge-verified' AND p.binding_id=c.binding_id AND p.target_ref=c.target_ref
		AND p.updated_at<=$3 AND ((c.action='upsert' AND EXISTS (
			SELECT 1 FROM git_projected_documents d WHERE d.binding_id=c.binding_id AND d.generation=$2 AND d.path=c.path
			AND d.valid AND d.content_sha256=c.content_sha256 AND d.raw=c.content)) OR
			(c.action='delete' AND NOT EXISTS (
				SELECT 1 FROM git_projected_documents d WHERE d.binding_id=c.binding_id AND d.generation=$2 AND d.path=c.path)))`, binding.ID, generation.Number, now.UTC()); err != nil {
		return Binding{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE deployments d SET state='git-committed',desired_revision=doc.config_revision,updated_at=$3
		FROM git_write_commands c,git_pull_request_publications p,git_projected_documents doc
		WHERE c.binding_id=$1 AND c.command_kind='deployment' AND c.action='upsert' AND c.publication_mode='pull-request' AND c.state='indexed' AND c.indexed_generation=$2
		AND p.operation_id=c.operation_id AND p.state='merge-verified' AND d.id=c.deployment_id
		AND doc.binding_id=c.binding_id AND doc.generation=c.indexed_generation AND doc.path=c.path
		AND doc.valid AND doc.content_sha256=c.content_sha256 AND doc.raw=c.content
		AND d.operation_id=c.operation_id AND d.generation=(SELECT generation FROM operations WHERE id=c.operation_id)`, binding.ID, generation.Number, now.UTC()); err != nil {
		return Binding{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE deployments d SET state='stopped',desired_revision=c.committed_revision,updated_at=$3
		FROM git_write_commands c,git_pull_request_publications p
		WHERE c.binding_id=$1 AND c.command_kind='deployment' AND c.action='delete' AND c.publication_mode='pull-request'
		AND c.state='indexed' AND c.indexed_generation=$2 AND p.operation_id=c.operation_id AND p.state='merge-verified'
		AND d.id=c.deployment_id AND d.operation_id=c.operation_id
		AND d.generation=(SELECT generation FROM operations WHERE id=c.operation_id)`, binding.ID, generation.Number, now.UTC()); err != nil {
		return Binding{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE environment_app_placements placement SET state='draft',desired_state='stopped',updated_at=$3
		FROM deployments d,git_write_commands c WHERE c.binding_id=$1 AND c.action='delete' AND c.state='indexed'
		AND c.indexed_generation=$2 AND d.id=c.deployment_id AND d.state='stopped'
		AND placement.environment_id=d.environment_id AND placement.application_id=d.application_id`, binding.ID, generation.Number, now.UTC()); err != nil {
		return Binding{}, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM git_path_reservations WHERE binding_id=$1 AND state='committed-pending-index'`, binding.ID); err != nil {
		return Binding{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Binding{}, classifyPostgres(err)
	}
	binding.IndexedRevision, binding.ProjectionGeneration, binding.IndexedAt, binding.State, binding.UpdatedAt = generation.HeadRevision, generation.Number, now.UTC(), BindingReady, now.UTC()
	return binding, nil
}

func postgresPolicyDocuments(ctx context.Context, tx pgx.Tx, bindingID string, generation int64) ([]Document, error) {
	rows, err := tx.Query(ctx, `SELECT `+documentColumns+` FROM git_projected_documents
		WHERE binding_id=$1 AND generation=$2 ORDER BY path FOR UPDATE`, bindingID, generation)
	if err != nil {
		return nil, classifyPostgres(err)
	}
	defer rows.Close()
	documents := []Document{}
	for rows.Next() {
		document, scanErr := scanDocument(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		documents = append(documents, document)
	}
	if err = rows.Err(); err != nil {
		return nil, classifyPostgres(err)
	}
	return documents, nil
}

func (s *PostgreSQLStore) FailGeneration(ctx context.Context, lease ReconciliationLease, generation Generation, now time.Time) error {
	if lease.Validate() != nil || now.IsZero() || generation.State != ProjectionStaging || lease.BindingID != generation.BindingID {
		return ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = getBinding(ctx, tx, generation.BindingID, "FOR UPDATE"); err != nil {
		return err
	}
	if err = requireActiveReconciliation(ctx, tx, lease, now); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE git_projection_generations SET state='failed' WHERE binding_id=$1 AND generation=$2 AND state='staging' AND head_revision=$3 AND parser_version=$4`, generation.BindingID, generation.Number, generation.HeadRevision, generation.ParserVersion)
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return classifyPostgres(tx.Commit(ctx))
}

func scanDocument(row rowScanner) (Document, error) {
	var value Document
	var parsed, diagnostics []byte
	err := row.Scan(&value.BindingID, &value.Generation, &value.Path, &value.ApplicationID, &value.SourceRevision, &value.ConfigRevision, &value.BlobID, &value.ContentSHA256, &value.Raw, &parsed, &value.Valid, &diagnostics, &value.SchemaVersion, &value.ParserVersion, &value.IndexedAt)
	if err != nil {
		return Document{}, classifyPostgres(err)
	}
	if len(parsed) > 0 && string(parsed) != "null" {
		if err = json.Unmarshal(parsed, &value.Parsed); err != nil {
			return Document{}, ErrInvalid
		}
	}
	if err = json.Unmarshal(diagnostics, &value.Diagnostics); err != nil {
		return Document{}, ErrInvalid
	}
	return value, nil
}

const documentColumns = `binding_id::text,generation,path,COALESCE(application_id::text,''),source_revision,config_revision,blob_id,content_sha256,raw,parsed,valid,diagnostics,schema_version,parser_version,indexed_at`

func (s *PostgreSQLStore) Document(ctx context.Context, bindingID, documentPath string) (Document, error) {
	binding, err := s.Binding(ctx, bindingID)
	if err != nil {
		return Document{}, err
	}
	if binding.ProjectionGeneration == 0 {
		return Document{}, ErrNotFound
	}
	document, err := scanDocument(s.pool.QueryRow(ctx, `SELECT `+documentColumns+` FROM git_projected_documents WHERE binding_id=$1 AND generation=$2 AND path=$3`, bindingID, binding.ProjectionGeneration, documentPath))
	if err != nil {
		return Document{}, err
	}
	if err = document.Validate(binding); err != nil {
		return Document{}, err
	}
	return document, nil
}

func (s *PostgreSQLStore) Bundle(ctx context.Context, bindingID, documentPath string, dependencies []string, chartDigest, policyVersion string) (Bundle, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Bundle{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	binding, err := getBinding(ctx, tx, bindingID, "")
	if err != nil {
		return Bundle{}, err
	}
	if binding.ProjectionGeneration == 0 {
		return Bundle{}, ErrNotFound
	}
	document, err := scanDocument(tx.QueryRow(ctx, `SELECT `+documentColumns+` FROM git_projected_documents WHERE binding_id=$1 AND generation=$2 AND path=$3`, bindingID, binding.ProjectionGeneration, documentPath))
	if err != nil {
		return Bundle{}, err
	}
	dependencyDocuments := make([]Document, 0, len(dependencies))
	dependencyStates := make([]DependencyState, 0, len(dependencies))
	for _, dependencyPath := range dependencies {
		dependency, queryErr := scanDocument(tx.QueryRow(ctx, `SELECT `+documentColumns+` FROM git_projected_documents WHERE binding_id=$1 AND generation=$2 AND path=$3`, bindingID, binding.ProjectionGeneration, dependencyPath))
		if errors.Is(queryErr, ErrNotFound) {
			dependencyStates = append(dependencyStates, DependencyState{Path: dependencyPath})
			continue
		}
		if queryErr != nil {
			return Bundle{}, queryErr
		}
		dependencyDocuments = append(dependencyDocuments, dependency)
		dependencyStates = append(dependencyStates, DependencyState{Path: dependencyPath, Present: true, BlobID: dependency.BlobID})
	}
	var etag string
	if len(dependencies) == 0 {
		etag, err = StrongETag(binding, []Document{document}, nil, chartDigest, policyVersion)
	} else {
		etag, err = StrongETagWithDependencies(binding, []Document{document}, dependencies, dependencyDocuments, chartDigest, policyVersion)
	}
	if err != nil {
		return Bundle{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Bundle{}, err
	}
	documents := append([]Document{document}, dependencyDocuments...)
	return Bundle{BindingID: binding.ID, TargetRef: binding.TargetRef, TargetHeadRevision: binding.TargetHeadRevision, IndexedRevision: binding.IndexedRevision, ConfigRevision: document.ConfigRevision, ETag: etag, Stale: binding.TargetHeadRevision == "" || binding.TargetHeadRevision != binding.IndexedRevision, Documents: documents, Dependencies: dependencyStates, IndexedAt: binding.IndexedAt}, nil
}

func scanReservation(row rowScanner) (PathReservation, error) {
	var value PathReservation
	var committed *string
	err := row.Scan(&value.BindingID, &value.TargetRef, &value.Path, &value.OperationID, &value.Owner, &value.BaseRevision, &committed, &value.State, &value.LeaseUntil, &value.CreatedAt, &value.UpdatedAt)
	if err != nil {
		return PathReservation{}, classifyPostgres(err)
	}
	if committed != nil {
		value.CommittedRevision = *committed
	}
	return value, nil
}

const reservationColumns = `binding_id::text,target_ref,path,operation_id::text,owner,base_revision,committed_revision,state,lease_until,created_at,updated_at`

func (s *PostgreSQLStore) PathReservation(ctx context.Context, bindingID, targetRef, documentPath string) (PathReservation, error) {
	binding, err := s.Binding(ctx, bindingID)
	if err != nil {
		return PathReservation{}, err
	}
	reservation, err := scanReservation(s.pool.QueryRow(ctx, `SELECT `+reservationColumns+` FROM git_path_reservations WHERE binding_id=$1 AND target_ref=$2 AND path=$3`, bindingID, targetRef, documentPath))
	if err != nil {
		return PathReservation{}, err
	}
	if reservation.Validate(binding) != nil {
		return PathReservation{}, ErrInvalid
	}
	return reservation, nil
}

func (s *PostgreSQLStore) AcquirePath(ctx context.Context, candidate PathReservation, now time.Time, lease time.Duration) (PathReservation, bool, error) {
	if now.IsZero() || lease <= 0 || lease > 2*time.Minute || candidate.State != ReservationCandidate || candidate.LeaseUntil == nil {
		return PathReservation{}, false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return PathReservation{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	binding, err := getBinding(ctx, tx, candidate.BindingID, "FOR UPDATE")
	if err != nil {
		return PathReservation{}, false, err
	}
	if candidate.Validate(binding) != nil || !candidate.CreatedAt.Equal(now) || !candidate.UpdatedAt.Equal(now) || !candidate.LeaseUntil.Equal(now.Add(lease)) {
		return PathReservation{}, false, ErrInvalid
	}
	if binding.State != BindingReady || binding.TargetHeadRevision == "" || binding.TargetHeadRevision != binding.IndexedRevision || binding.TargetHeadRevision != candidate.BaseRevision {
		return PathReservation{}, false, ErrStale
	}
	result, err := tx.Exec(ctx, `INSERT INTO git_path_reservations(binding_id,target_ref,path,operation_id,owner,base_revision,state,lease_until,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`, candidate.BindingID, candidate.TargetRef, candidate.Path, candidate.OperationID, candidate.Owner, candidate.BaseRevision, candidate.State, candidate.LeaseUntil, candidate.CreatedAt, candidate.UpdatedAt)
	if err != nil {
		return PathReservation{}, false, classifyPostgres(err)
	}
	if result.RowsAffected() == 1 {
		if err = tx.Commit(ctx); err != nil {
			return PathReservation{}, false, classifyPostgres(err)
		}
		return candidate, false, nil
	}
	current, err := scanReservation(tx.QueryRow(ctx, `SELECT `+reservationColumns+` FROM git_path_reservations WHERE binding_id=$1 AND target_ref=$2 AND path=$3 FOR UPDATE`, candidate.BindingID, candidate.TargetRef, candidate.Path))
	if err != nil {
		return PathReservation{}, false, err
	}
	if current.OperationID == candidate.OperationID && current.Owner == candidate.Owner && current.BaseRevision == candidate.BaseRevision {
		if err = tx.Commit(ctx); err != nil {
			return PathReservation{}, false, err
		}
		return current, true, nil
	}
	return PathReservation{}, false, ErrLeaseHeld
}

func (s *PostgreSQLStore) FinalizePath(ctx context.Context, bindingID, targetRef, documentPath, operationID, committedRevision string, now time.Time) (PathReservation, error) {
	if now.IsZero() || !commitRE.MatchString(committedRevision) {
		return PathReservation{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return PathReservation{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	binding, err := getBinding(ctx, tx, bindingID, "FOR UPDATE")
	if err != nil {
		return PathReservation{}, err
	}
	reservation, err := scanReservation(tx.QueryRow(ctx, `SELECT `+reservationColumns+` FROM git_path_reservations WHERE binding_id=$1 AND target_ref=$2 AND path=$3 FOR UPDATE`, bindingID, targetRef, documentPath))
	if err != nil {
		return PathReservation{}, err
	}
	if reservation.OperationID != operationID {
		return PathReservation{}, ErrLeaseLost
	}
	if now.Before(reservation.CreatedAt) {
		return PathReservation{}, ErrInvalid
	}
	if reservation.State == ReservationCommittedPendingIndex {
		if reservation.CommittedRevision == committedRevision {
			return reservation, nil
		}
		return PathReservation{}, ErrConflict
	}
	if reservation.LeaseUntil == nil || !reservation.LeaseUntil.After(now) || binding.TargetHeadRevision != reservation.BaseRevision {
		if binding.TargetHeadRevision != committedRevision {
			return PathReservation{}, ErrLeaseLost
		}
	}
	_, err = tx.Exec(ctx, `UPDATE git_path_reservations SET state='committed-pending-index',committed_revision=$5,lease_until=NULL,updated_at=$6 WHERE binding_id=$1 AND target_ref=$2 AND path=$3 AND operation_id=$4`, bindingID, targetRef, documentPath, operationID, committedRevision, now.UTC())
	if err != nil {
		return PathReservation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return PathReservation{}, classifyPostgres(err)
	}
	reservation.State, reservation.CommittedRevision, reservation.LeaseUntil, reservation.UpdatedAt = ReservationCommittedPendingIndex, committedRevision, nil, now.UTC()
	return reservation, nil
}

func (s *PostgreSQLStore) FinalizeVerifiedPath(ctx context.Context, bindingID, targetRef, documentPath, operationID, committedRevision string, head VerifiedHead, now time.Time) (PathReservation, error) {
	if now.IsZero() || !commitRE.MatchString(committedRevision) || head.Source != ObservationWrite || head.ObservedAt.After(now) {
		return PathReservation{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return PathReservation{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	binding, err := getBinding(ctx, tx, bindingID, "FOR UPDATE")
	if err != nil {
		return PathReservation{}, err
	}
	if head.BindingID != bindingID || head.TargetRef != targetRef || head.ValidateFor(binding) != nil || head.ObservedAt.Before(binding.CreatedAt) ||
		!binding.TargetHeadObservedAt.IsZero() && (head.ObservedAt.Before(binding.TargetHeadObservedAt) || head.ObservedAt.Equal(binding.TargetHeadObservedAt) && head.Commit != binding.TargetHeadRevision) {
		return PathReservation{}, ErrConflict
	}
	reservation, err := scanReservation(tx.QueryRow(ctx, `SELECT `+reservationColumns+` FROM git_path_reservations WHERE binding_id=$1 AND target_ref=$2 AND path=$3 FOR UPDATE`, bindingID, targetRef, documentPath))
	if err != nil {
		return PathReservation{}, err
	}
	if reservation.OperationID != operationID {
		return PathReservation{}, ErrLeaseLost
	}
	if reservation.State == ReservationCommittedPendingIndex && reservation.CommittedRevision != committedRevision {
		return PathReservation{}, ErrConflict
	}
	var commandKind string
	if err = tx.QueryRow(ctx, `SELECT command_kind FROM git_write_commands WHERE operation_id=$1 FOR UPDATE`, operationID).Scan(&commandKind); err != nil {
		return PathReservation{}, classifyPostgres(err)
	}
	var command WriteCommand
	switch commandKind {
	case "deployment":
		command, err = scanWriteCommand(tx.QueryRow(ctx, `SELECT `+writeCommandColumns+` FROM git_write_commands WHERE operation_id=$1 AND command_kind=$2`, operationID, commandKind))
	case "variable-set":
		command, err = scanVariableWriteCommand(tx.QueryRow(ctx, `SELECT `+variableWriteCommandColumns+` FROM git_write_commands WHERE operation_id=$1 AND command_kind=$2`, operationID, commandKind))
	default:
		return PathReservation{}, ErrConflict
	}
	if err != nil {
		return PathReservation{}, err
	}
	if validatePersistedWriteCommand(command, binding) != nil || command.Plan.BindingID != bindingID || command.TargetRef != targetRef || command.Path != documentPath ||
		command.State != WriteCommandPending && command.CommittedRevision != committedRevision {
		return PathReservation{}, ErrConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO git_verified_head_observations(binding_id,provider,installation_id,repository_id,repository_owner,repository_name,target_ref,commit_revision,source,provider_request,observed_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING`, head.BindingID, head.Repository.Provider, head.Repository.InstallationID, head.Repository.RepositoryID, head.Repository.Owner, head.Repository.Name, head.TargetRef, head.Commit, head.Source, head.ProviderRequest, head.ObservedAt.UTC()); err != nil {
		return PathReservation{}, classifyPostgres(err)
	}
	alreadyIndexed := binding.IndexedRevision == head.Commit && binding.ProjectionGeneration > 0
	state := BindingIndexing
	if alreadyIndexed && binding.State != BindingIndexing {
		state = BindingReady
	} else if binding.State == BindingDiverged {
		state = BindingDiverged
	}
	updatedAt := head.ObservedAt.UTC()
	if !updatedAt.After(binding.UpdatedAt) {
		updatedAt = binding.UpdatedAt.Add(time.Microsecond)
	}
	if _, err = tx.Exec(ctx, `UPDATE git_repository_bindings SET target_head_revision=$2,target_head_observed_at=$3,state=$4,updated_at=$5 WHERE id=$1`, binding.ID, head.Commit, head.ObservedAt.UTC(), state, updatedAt); err != nil {
		return PathReservation{}, err
	}
	if alreadyIndexed {
		result, deleteErr := tx.Exec(ctx, `DELETE FROM git_path_reservations WHERE binding_id=$1 AND target_ref=$2 AND path=$3 AND operation_id=$4`, bindingID, targetRef, documentPath, operationID)
		if deleteErr != nil {
			return PathReservation{}, deleteErr
		}
		if result.RowsAffected() != 1 {
			return PathReservation{}, ErrLeaseLost
		}
		commandUpdated := false
		if command.State == WriteCommandPending {
			if commandKind == "variable-set" && command.PublicationMode == PublicationDirect {
				result, err = tx.Exec(ctx, `UPDATE git_write_commands SET state='git-committed',committed_revision=$2,committed_at=$3,updated_at=$3
					WHERE operation_id=$1 AND command_kind=$4 AND state='pending'`, operationID, committedRevision, now.UTC(), commandKind)
				if err == nil && result.RowsAffected() == 1 {
					result, err = tx.Exec(ctx, `UPDATE git_write_commands SET state='indexed',indexed_generation=$2,indexed_at=$3,updated_at=$3
						WHERE operation_id=$1 AND command_kind=$4 AND state='git-committed'`, operationID, binding.ProjectionGeneration, now.UTC(), commandKind)
				}
			} else {
				result, err = tx.Exec(ctx, `UPDATE git_write_commands SET state='indexed',committed_revision=$2,committed_at=$3,
					indexed_generation=$4,indexed_at=$3,updated_at=$3 WHERE operation_id=$1 AND command_kind=$5 AND state='pending'`, operationID, committedRevision, now.UTC(), binding.ProjectionGeneration, commandKind)
			}
			commandUpdated = true
		} else if command.State == WriteCommandGitCommitted {
			result, err = tx.Exec(ctx, `UPDATE git_write_commands SET state='indexed',indexed_generation=$2,indexed_at=$3,updated_at=$3
				WHERE operation_id=$1 AND command_kind=$4 AND state='git-committed'`, operationID, binding.ProjectionGeneration, now.UTC(), commandKind)
			commandUpdated = true
		}
		if err != nil {
			return PathReservation{}, err
		}
		if commandUpdated && result.RowsAffected() != 1 {
			return PathReservation{}, ErrConflict
		}
	} else {
		result, updateErr := tx.Exec(ctx, `UPDATE git_path_reservations SET state='committed-pending-index',committed_revision=$5,lease_until=NULL,updated_at=$6
			WHERE binding_id=$1 AND target_ref=$2 AND path=$3 AND operation_id=$4`, bindingID, targetRef, documentPath, operationID, committedRevision, now.UTC())
		if updateErr != nil {
			return PathReservation{}, updateErr
		}
		if result.RowsAffected() != 1 {
			return PathReservation{}, ErrLeaseLost
		}
		if command.State == WriteCommandPending {
			result, err = tx.Exec(ctx, `UPDATE git_write_commands SET state='git-committed',committed_revision=$2,committed_at=$3,updated_at=$3 WHERE operation_id=$1 AND command_kind=$4 AND state='pending'`, operationID, committedRevision, now.UTC(), commandKind)
			if err != nil {
				return PathReservation{}, err
			}
			if result.RowsAffected() != 1 {
				return PathReservation{}, ErrConflict
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return PathReservation{}, classifyPostgres(err)
	}
	reservation.State, reservation.CommittedRevision, reservation.LeaseUntil, reservation.UpdatedAt = ReservationCommittedPendingIndex, committedRevision, nil, now.UTC()
	return reservation, nil
}

func (s *PostgreSQLStore) RepairExpiredPath(ctx context.Context, bindingID, targetRef, documentPath string, commitPresent bool, committedRevision string, now time.Time) error {
	if now.IsZero() || commitPresent && !commitRE.MatchString(committedRevision) {
		return ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	reservation, err := scanReservation(tx.QueryRow(ctx, `SELECT `+reservationColumns+` FROM git_path_reservations WHERE binding_id=$1 AND target_ref=$2 AND path=$3 FOR UPDATE`, bindingID, targetRef, documentPath))
	if err != nil {
		return err
	}
	if reservation.State != ReservationCandidate || reservation.LeaseUntil == nil || reservation.LeaseUntil.After(now) {
		return ErrConflict
	}
	if commitPresent {
		_, err = tx.Exec(ctx, `UPDATE git_path_reservations SET state='committed-pending-index',committed_revision=$4,lease_until=NULL,updated_at=$5 WHERE binding_id=$1 AND target_ref=$2 AND path=$3`, bindingID, targetRef, documentPath, committedRevision, now.UTC())
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM git_path_reservations WHERE binding_id=$1 AND target_ref=$2 AND path=$3`, bindingID, targetRef, documentPath)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func classifyPostgres(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505", "40001", "23P01":
			return ErrConflict
		case "23503":
			return ErrNotFound
		case "23514", "22P02", "22001":
			return ErrInvalid
		}
	}
	return fmt.Errorf("Git projection database operation: %w", err)
}

var _ Store = (*PostgreSQLStore)(nil)
var _ BindingCatalog = (*PostgreSQLStore)(nil)
