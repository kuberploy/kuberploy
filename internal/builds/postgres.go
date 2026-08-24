package builds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/githubapp"
)

// PostgreSQLStore is intentionally package-owned so the build controller can
// be tested and run independently of the HTTP store facade. It uses the same
// pool/database and migration set in production.
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
	config.ConnConfig.RuntimeParams["application_name"] = "kuberploy-build-orchestrator"
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

func (s *PostgreSQLStore) Close() { s.pool.Close() }

func (s *PostgreSQLStore) ClaimOnce(ctx context.Context, claim githubapp.OneTimeClaim) (bool, error) {
	if err := validateClaim(claim); err != nil {
		return false, err
	}
	command, err := s.pool.Exec(ctx, `INSERT INTO github_one_time_claims(kind,claim_key,retain_until,permanent) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, claim.Kind, claim.ClaimKey, claim.RetainUntil, claim.Permanent)
	return command.RowsAffected() == 1, classifyPostgres(err)
}

func (s *PostgreSQLStore) PutInstallation(ctx context.Context, installation Installation) error {
	if err := installation.validate(); err != nil {
		return err
	}
	permissions, _ := json.Marshal(installation.Permissions)
	command, err := s.pool.Exec(ctx, `UPDATE github_installations SET github_app_id=$2,github_account_id=$3,account_login=$4,account_type=$5,repository_selection=$6,permissions=$7,lifecycle=$8,suspended_at=$9,deleted_at=$10,last_verified_at=$11,updated_at=$12 WHERE id=$1 AND github_installation_id=$13 AND (github_app_id IS NULL OR github_app_id=$2) AND (github_account_id IS NULL OR github_account_id=$3)`,
		installation.ID, installation.AppID, installation.Account.ID, installation.Account.Login, installation.Account.Type,
		installation.RepositorySelection, permissions, installation.Lifecycle, installation.SuspendedAt, installation.DeletedAt,
		installation.LastVerifiedAt, installation.UpdatedAt, installation.GitHubInstallationID)
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgreSQLStore) PutRepository(ctx context.Context, repository Repository) error {
	if err := repository.validate(); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var accountID int64
	var accountLogin string
	if err = tx.QueryRow(ctx, `SELECT github_account_id,account_login FROM github_installations WHERE id=$1 AND lifecycle<>'deleted' FOR SHARE`, repository.InstallationID).Scan(&accountID, &accountLogin); err != nil {
		return classifyPostgres(err)
	}
	if accountID != repository.Identity.OwnerID || !strings.EqualFold(accountLogin, repository.Identity.OwnerLogin) {
		return ErrUnauthorized
	}
	command, err := tx.Exec(ctx, `INSERT INTO github_repositories(id,installation_id,github_repository_id,github_owner_id,owner_login,name,lifecycle,last_verified_at,removed_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT(id) DO UPDATE SET github_owner_id=EXCLUDED.github_owner_id,owner_login=EXCLUDED.owner_login,name=EXCLUDED.name,lifecycle=EXCLUDED.lifecycle,last_verified_at=EXCLUDED.last_verified_at,removed_at=EXCLUDED.removed_at,updated_at=EXCLUDED.updated_at
		WHERE github_repositories.installation_id=EXCLUDED.installation_id AND github_repositories.github_repository_id=EXCLUDED.github_repository_id`,
		repository.ID, repository.InstallationID, repository.Identity.ID, repository.Identity.OwnerID, repository.Identity.OwnerLogin,
		repository.Identity.Name, repository.Lifecycle, repository.LastVerifiedAt, repository.RemovedAt, repository.CreatedAt, repository.UpdatedAt)
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return tx.Commit(ctx)
}

func (s *PostgreSQLStore) PutDefinition(ctx context.Context, definition BuildDefinition) error {
	if err := definition.validate(); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var applicationProject string
	if err = tx.QueryRow(ctx, `SELECT project_id::text FROM applications WHERE id=$1 FOR SHARE`, definition.ServiceID).Scan(&applicationProject); err != nil {
		return classifyPostgres(err)
	}
	if applicationProject != definition.ProjectID {
		return ErrUnauthorized
	}
	if definition.SourceKind == SourceGitHub {
		var repositoryInstallation, repositoryLifecycle string
		if err = tx.QueryRow(ctx, `SELECT installation_id::text,lifecycle FROM github_repositories WHERE id=$1 FOR SHARE`, definition.RepositoryID).Scan(&repositoryInstallation, &repositoryLifecycle); err != nil {
			return classifyPostgres(err)
		}
		if repositoryInstallation != definition.InstallationID || repositoryLifecycle != string(RepositoryActive) {
			return ErrUnauthorized
		}
	} else {
		var revision uint64
		var status string
		if err = tx.QueryRow(ctx, `SELECT revision,status FROM git_ssh_key_revisions WHERE scope=$1 AND owner_id=$2 AND revision=$3 FOR SHARE`,
			definition.GitSSH.KeyScope, definition.GitSSH.KeyOwnerID, definition.GitSSH.KeyRevision).Scan(&revision, &status); err != nil {
			return classifyPostgres(err)
		}
		if revision != definition.GitSSH.KeyRevision || status != "active" {
			return ErrUnauthorized
		}
	}
	var mode, endpoint, prefix, pushCredential, cacheCredential string
	if err = tx.QueryRow(ctx, `SELECT mode,endpoint,repository_prefix,push_credential_ref,cache_credential_ref FROM registry_targets WHERE id=$1 FOR SHARE`, definition.Spec.Registry.TargetID).Scan(&mode, &endpoint, &prefix, &pushCredential, &cacheCredential); err != nil {
		return classifyPostgres(err)
	}
	server, serverErr := registryServer(endpoint)
	if serverErr != nil || mode != string(definition.Spec.Registry.Mode) || server != definition.Spec.Registry.Server || prefix != definition.Spec.Registry.RepositoryPrefix ||
		pushCredential == "" || cacheCredential == "" || pushCredential == cacheCredential ||
		pushCredential != definition.Spec.Registry.PushCredentialSecret || cacheCredential != definition.Spec.Registry.CacheCredentialSecret {
		return ErrUnauthorized
	}
	if definition.Enabled {
		if _, err = tx.Exec(ctx, `UPDATE build_definitions SET enabled=false,updated_at=$1
			WHERE project_id=$2 AND service_id=$3 AND source_kind=$4 AND trigger_ref=$5 AND id<>$6 AND enabled=true
			AND ($4='git_ssh' OR repository_id=$7)`, definition.UpdatedAt, definition.ProjectID, definition.ServiceID,
			definition.SourceKind, definition.TriggerRef, definition.ID, nullableUUID(definition.RepositoryID)); err != nil {
			return classifyPostgres(err)
		}
	}
	spec, _ := json.Marshal(definition.Spec)
	var gitSSHSource any
	if definition.GitSSH != nil {
		gitSSHSource, _ = json.Marshal(definition.GitSSH)
	}
	command, err := tx.Exec(ctx, `INSERT INTO build_definitions(id,project_id,service_id,source_kind,installation_id,repository_id,git_ssh_source,registry_target_id,trigger_ref,spec,definition_digest,generation,enabled,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT(id) DO UPDATE SET trigger_ref=EXCLUDED.trigger_ref,spec=EXCLUDED.spec,definition_digest=EXCLUDED.definition_digest,generation=EXCLUDED.generation,enabled=EXCLUDED.enabled,updated_at=EXCLUDED.updated_at
		WHERE build_definitions.project_id=EXCLUDED.project_id AND build_definitions.service_id=EXCLUDED.service_id AND build_definitions.source_kind=EXCLUDED.source_kind
		AND build_definitions.installation_id IS NOT DISTINCT FROM EXCLUDED.installation_id AND build_definitions.repository_id IS NOT DISTINCT FROM EXCLUDED.repository_id
		AND build_definitions.git_ssh_source IS NOT DISTINCT FROM EXCLUDED.git_ssh_source AND build_definitions.registry_target_id=EXCLUDED.registry_target_id`,
		definition.ID, definition.ProjectID, definition.ServiceID, definition.SourceKind, nullableUUID(definition.InstallationID), nullableUUID(definition.RepositoryID), gitSSHSource,
		definition.Spec.Registry.TargetID, definition.TriggerRef, spec, definition.DefinitionDigest, definition.DefinitionGeneration, definition.Enabled, definition.CreatedAt, definition.UpdatedAt)
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return tx.Commit(ctx)
}

func (s *PostgreSQLStore) ApplyInstallationEvent(ctx context.Context, appID int64, event githubapp.InstallationEvent, now time.Time) error {
	if !validInstallationEvent(event) {
		return ErrInvalid
	}
	permissions, _ := json.Marshal(event.Permissions)
	lifecycle := InstallationActive
	var suspendedAt, deletedAt *time.Time
	at := now.UTC()
	switch event.Action {
	case "suspend":
		lifecycle, suspendedAt = InstallationSuspended, &at
	case "deleted":
		lifecycle, deletedAt = InstallationDeleted, &at
	case "created", "unsuspend", "new_permissions_accepted":
	default:
		return ErrInvalid
	}
	var command pgconn.CommandTag
	var err error
	if event.Action == "new_permissions_accepted" {
		command, err = s.pool.Exec(ctx, `UPDATE github_installations SET repository_selection=$6,permissions=$7,updated_at=$8 WHERE github_app_id=$1 AND github_installation_id=$2 AND github_account_id=$3 AND lower(account_login)=lower($4) AND account_type=$5 AND lifecycle<>'deleted'`, appID, event.InstallationID, event.Account.ID, event.Account.Login, event.Account.Type, event.RepositorySelection, permissions, at)
	} else {
		command, err = s.pool.Exec(ctx, `UPDATE github_installations SET repository_selection=$6,permissions=$7,lifecycle=$8,suspended_at=$9,deleted_at=$10,updated_at=$11
			WHERE github_app_id=$1 AND github_installation_id=$2 AND github_account_id=$3 AND lower(account_login)=lower($4) AND account_type=$5 AND (lifecycle<>'deleted' OR $8='deleted')`,
			appID, event.InstallationID, event.Account.ID, event.Account.Login, event.Account.Type, event.RepositorySelection, permissions, lifecycle, suspendedAt, deletedAt, at)
	}
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgreSQLStore) ApplyRepositoryEvent(ctx context.Context, appID int64, event githubapp.InstallationRepositoriesEvent, now time.Time) error {
	if !validRepositoryEvent(event) {
		return ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var installationID string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM github_installations WHERE github_app_id=$1 AND github_installation_id=$2 AND github_account_id=$3 AND lower(account_login)=lower($4) AND account_type=$5 AND lifecycle<>'deleted' FOR UPDATE`, appID, event.InstallationID, event.Account.ID, event.Account.Login, event.Account.Type).Scan(&installationID); err != nil {
		return classifyPostgres(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE github_installations SET repository_selection=$2,updated_at=$3 WHERE id=$1`, installationID, event.RepositorySelection, now.UTC()); err != nil {
		return err
	}
	if event.Action == "added" {
		for _, identity := range event.Added {
			if !validRepository(identity) || identity.OwnerID != event.Account.ID || !strings.EqualFold(identity.OwnerLogin, event.Account.Login) {
				return ErrUnauthorized
			}
			id := deterministicUUID("github-repository-v1", installationID, strconv.FormatInt(identity.ID, 10))
			_, err = tx.Exec(ctx, `INSERT INTO github_repositories(id,installation_id,github_repository_id,github_owner_id,owner_login,name,lifecycle,last_verified_at,created_at,updated_at)
				VALUES($1,$2,$3,$4,$5,$6,'active',$7,$7,$7)
				ON CONFLICT(installation_id,github_repository_id) DO UPDATE SET github_owner_id=EXCLUDED.github_owner_id,owner_login=EXCLUDED.owner_login,name=EXCLUDED.name,lifecycle='active',last_verified_at=EXCLUDED.last_verified_at,removed_at=NULL,updated_at=EXCLUDED.updated_at`,
				id, installationID, identity.ID, identity.OwnerID, identity.OwnerLogin, identity.Name, now.UTC())
			if err != nil {
				return classifyPostgres(err)
			}
		}
	} else if event.Action == "removed" {
		for _, identity := range event.Removed {
			command, updateErr := tx.Exec(ctx, `UPDATE github_repositories SET lifecycle='removed',removed_at=$6,updated_at=$6 WHERE installation_id=$1 AND github_repository_id=$2 AND github_owner_id=$3 AND lower(owner_login)=lower($4) AND name=$5`, installationID, identity.ID, identity.OwnerID, identity.OwnerLogin, identity.Name, now.UTC())
			if updateErr != nil {
				return classifyPostgres(updateErr)
			}
			if command.RowsAffected() != 1 {
				return ErrNotFound
			}
		}
	} else {
		return ErrInvalid
	}
	return tx.Commit(ctx)
}

func (s *PostgreSQLStore) ClaimDelivery(ctx context.Context, claim githubapp.OneTimeClaim, receipt DeliveryReceipt) (bool, error) {
	if err := validateClaim(claim); err != nil || claim.Kind != "github-delivery" || !claim.Permanent {
		return false, ErrInvalid
	}
	receipt.ClaimKey = claim.ClaimKey
	if err := validateReceipt(receipt); err != nil {
		return false, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	command, err := tx.Exec(ctx, `INSERT INTO github_one_time_claims(kind,claim_key,retain_until,permanent,created_at) VALUES($1,$2,$3,true,$4) ON CONFLICT DO NOTHING`, claim.Kind, claim.ClaimKey, claim.RetainUntil, receipt.ReceivedAt)
	if err != nil {
		return false, classifyPostgres(err)
	}
	if command.RowsAffected() == 0 {
		existing, getErr := deliveryByQuery(ctx, tx, receipt.ClaimKey, false)
		if getErr != nil {
			return false, getErr
		}
		if !sameReceiptIdentity(existing, receipt) {
			return false, ErrConflict
		}
		return false, tx.Commit(ctx)
	}
	_, err = tx.Exec(ctx, `INSERT INTO github_webhook_receipts(claim_key,github_app_id,github_installation_id,delivery_id,event,body_sha256,typed_event,repository_id,git_ref,state,available_at,received_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8::bigint,0::bigint),$9,'claimed',$10,$11,$11)`, receipt.ClaimKey, receipt.AppID, receipt.GitHubInstallationID, receipt.DeliveryID, receipt.Event, receipt.BodySHA256, receipt.TypedEvent, receipt.RepositoryID, receipt.GitRef, receipt.AvailableAt, receipt.ReceivedAt)
	if err != nil {
		return false, classifyPostgres(err)
	}
	return true, tx.Commit(ctx)
}

func (s *PostgreSQLStore) Delivery(ctx context.Context, claimKey string) (DeliveryReceipt, error) {
	return deliveryByQuery(ctx, s.pool, claimKey, false)
}

func (s *PostgreSQLStore) PendingDeliveries(ctx context.Context, now time.Time, limit int) ([]string, error) {
	if limit < 1 || limit > 1000 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT claim_key FROM github_webhook_receipts WHERE state IN ('claimed','processing') AND available_at<=$1 AND (lease_until IS NULL OR lease_until<=$1) ORDER BY received_at,claim_key LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		result = append(result, key)
	}
	return result, rows.Err()
}

// PurgeExpiredDeliveryPayloads preserves permanent claims while removing
// terminal typed payloads whose explicit replay-retention period elapsed.
func (s *PostgreSQLStore) PurgeExpiredDeliveryPayloads(ctx context.Context, now time.Time) (int64, error) {
	command, err := s.pool.Exec(ctx, `UPDATE github_webhook_receipts AS receipt
		SET typed_event=NULL,updated_at=$1
		FROM github_one_time_claims AS claim
		WHERE claim.kind='github-delivery' AND claim.claim_key=receipt.claim_key AND claim.permanent
			AND claim.retain_until<=$1 AND receipt.state IN ('enqueued','ignored','failed') AND receipt.typed_event IS NOT NULL`, now.UTC())
	if err != nil {
		return 0, classifyPostgres(err)
	}
	return command.RowsAffected(), nil
}

func (s *PostgreSQLStore) AcquireDelivery(ctx context.Context, claimKey, owner string, now time.Time, duration time.Duration) (DeliveryReceipt, bool, error) {
	if !validOwnerLease(owner, duration) {
		return DeliveryReceipt{}, false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return DeliveryReceipt{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	receipt, err := deliveryByQuery(ctx, tx, claimKey, true)
	if err != nil {
		return DeliveryReceipt{}, false, err
	}
	if terminalDelivery(receipt.State) || receipt.AvailableAt.After(now.UTC()) || receipt.LeaseOwner != "" && receipt.LeaseUntil.After(now.UTC()) {
		return receipt, false, tx.Commit(ctx)
	}
	receipt.State, receipt.LeaseOwner, receipt.LeaseUntil, receipt.UpdatedAt = DeliveryProcessing, owner, now.UTC().Add(duration), now.UTC()
	_, err = tx.Exec(ctx, `UPDATE github_webhook_receipts SET state='processing',lease_owner=$2,lease_until=$3,updated_at=$4 WHERE claim_key=$1`, claimKey, owner, receipt.LeaseUntil, receipt.UpdatedAt)
	if err != nil {
		return DeliveryReceipt{}, false, err
	}
	return receipt, true, tx.Commit(ctx)
}

func (s *PostgreSQLStore) HeartbeatDelivery(ctx context.Context, claimKey, owner string, now time.Time, duration time.Duration) error {
	if !validOwnerLease(owner, duration) {
		return ErrInvalid
	}
	command, err := s.pool.Exec(ctx, `UPDATE github_webhook_receipts SET lease_until=$4,updated_at=$3 WHERE claim_key=$1 AND state='processing' AND lease_owner=$2 AND lease_until>$3`, claimKey, owner, now.UTC(), now.UTC().Add(duration))
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgreSQLStore) RetryDelivery(ctx context.Context, claimKey, owner, code string, now, availableAt time.Time) error {
	if validateFailureCode(code) != nil || availableAt.Before(now.UTC()) {
		return ErrInvalid
	}
	command, err := s.pool.Exec(ctx, `UPDATE github_webhook_receipts SET state='claimed',failure_code=$3,available_at=$5,lease_owner=NULL,lease_until=NULL,updated_at=$4 WHERE claim_key=$1 AND state='processing' AND lease_owner=$2 AND lease_until>$4`, claimKey, owner, code, now.UTC(), availableAt.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgreSQLStore) FinishDelivery(ctx context.Context, claimKey, owner string, state DeliveryState, code string, now time.Time) error {
	if state != DeliveryIgnored && state != DeliveryFailed || state == DeliveryFailed && validateFailureCode(code) != nil || state == DeliveryIgnored && code != "" {
		return ErrInvalid
	}
	command, err := s.pool.Exec(ctx, `UPDATE github_webhook_receipts SET state=$3,failure_code=$4,completed_at=$5,lease_owner=NULL,lease_until=NULL,updated_at=$5 WHERE claim_key=$1 AND state='processing' AND lease_owner=$2 AND lease_until>$5`, claimKey, owner, state, code, now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgreSQLStore) AuthorizePush(ctx context.Context, appID, providerInstallationID int64, identity githubapp.RepositoryIdentity, ref string) (AuthorizedPush, error) {
	if !validRepository(identity) || !validGitRef(ref) {
		return AuthorizedPush{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return AuthorizedPush{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	installation, err := installationByProviderQuery(ctx, tx, appID, providerInstallationID)
	if err != nil || installation.Lifecycle != InstallationActive {
		if err == nil {
			err = ErrUnauthorized
		}
		return AuthorizedPush{}, err
	}
	repository, err := repositoryByProviderQuery(ctx, tx, installation.ID, identity.ID)
	if err != nil {
		return AuthorizedPush{}, err
	}
	if repository.Lifecycle != RepositoryActive || repository.Identity.OwnerID != identity.OwnerID || repository.Identity.Name != identity.Name || !strings.EqualFold(repository.Identity.OwnerLogin, identity.OwnerLogin) {
		return AuthorizedPush{}, ErrUnauthorized
	}
	rows, err := tx.Query(ctx, `SELECT id::text,project_id::text,service_id::text,source_kind,COALESCE(installation_id::text,''),COALESCE(repository_id::text,''),git_ssh_source,trigger_ref,spec,definition_digest,generation,enabled,created_at,updated_at FROM build_definitions WHERE source_kind='github' AND installation_id=$1 AND repository_id=$2 AND trigger_ref=$3 AND enabled=true ORDER BY id`, installation.ID, repository.ID, ref)
	if err != nil {
		return AuthorizedPush{}, err
	}
	definitions := make([]BuildDefinition, 0)
	for rows.Next() {
		definition, scanErr := scanDefinition(rows)
		if scanErr != nil {
			rows.Close()
			return AuthorizedPush{}, scanErr
		}
		definitions = append(definitions, definition)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return AuthorizedPush{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return AuthorizedPush{}, err
	}
	return AuthorizedPush{Installation: installation, Repository: repository, Definitions: definitions}, nil
}

func (s *PostgreSQLStore) EnqueuePushBuilds(ctx context.Context, input EnqueuePush, owner string, definitions []AttemptDefinition, now time.Time) ([]BuildAttempt, error) {
	if !commitRE.MatchString(input.CommitSHA) || !validGitRef(input.GitRef) || input.ResolvedAt.IsZero() || owner == "" || len(definitions) == 0 {
		return nil, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	receipt, err := deliveryByQuery(ctx, tx, input.ClaimKey, true)
	if err != nil {
		return nil, err
	}
	if receipt.State == DeliveryEnqueued {
		attempts, getErr := attemptsForDeliveryQuery(ctx, tx, input.ClaimKey)
		return attempts, joinCommit(tx, ctx, getErr)
	}
	if receipt.State != DeliveryProcessing || receipt.LeaseOwner != owner || !receipt.LeaseUntil.After(now.UTC()) || receipt.GitRef != input.GitRef {
		return nil, ErrLeaseLost
	}
	staged := make([]BuildAttempt, 0, len(definitions))
	for _, requested := range definitions {
		current, getErr := definitionByIDQuery(ctx, tx, requested.Definition.ID, true)
		if getErr != nil {
			return nil, getErr
		}
		if !sameDefinition(current, requested.Definition) || !current.Enabled {
			return nil, ErrUnauthorized
		}
		var installationLifecycle InstallationLifecycle
		if getErr = tx.QueryRow(ctx, `SELECT lifecycle FROM github_installations WHERE id=$1 FOR SHARE`, current.InstallationID).Scan(&installationLifecycle); getErr != nil {
			return nil, classifyPostgres(getErr)
		}
		if installationLifecycle != InstallationActive {
			return nil, ErrUnauthorized
		}
		repository, getErr := repositoryByIDQuery(ctx, tx, current.RepositoryID)
		if getErr != nil {
			return nil, getErr
		}
		if repository.Lifecycle != RepositoryActive || repository.InstallationID != current.InstallationID {
			return nil, ErrUnauthorized
		}
		existing, getErr := attemptByIDQuery(ctx, tx, deterministicUUID("build-attempt-v1", input.ClaimKey, current.ID), false)
		if getErr == nil {
			staged = append(staged, existing)
			continue
		}
		if !errors.Is(getErr, ErrNotFound) {
			return nil, getErr
		}
		// GitHub may deliver the same authoritative push more than once with
		// different delivery IDs (for example after webhook reconfiguration).
		// Coalesce by the independently resolved source tuple. Manual retries
		// remain distinct because their synthetic receipts have no typed event.
		existing, getErr = pushAttemptBySourceQuery(ctx, tx, current.ID, input.CommitSHA, input.GitRef)
		if getErr == nil {
			staged = append(staged, existing)
			continue
		}
		if !errors.Is(getErr, ErrNotFound) {
			return nil, getErr
		}
		var generation int64
		if err = tx.QueryRow(ctx, `UPDATE applications SET build_generation=build_generation+1 WHERE project_id=$1 AND id=$2 RETURNING build_generation`, current.ProjectID, current.ServiceID).Scan(&generation); err != nil {
			return nil, classifyPostgres(err)
		}
		imports, importErr := cacheImportsQuery(ctx, tx, current, generation)
		if importErr != nil {
			return nil, importErr
		}
		attempt, createErr := newAttemptWithExecution(current, requested.Execution, repository, input, generation, imports, now)
		if createErr != nil {
			return nil, createErr
		}
		planJSON, _ := json.Marshal(attempt.PlanRequest)
		checkoutJSON, _ := json.Marshal(attempt.CheckoutRequest)
		_, err = tx.Exec(ctx, `INSERT INTO build_attempts(id,definition_id,delivery_claim_key,trigger_kind,trigger_key,project_id,service_id,commit_sha,git_ref,generation,definition_digest,plan_request,checkout_request,input_digest,registry_mode,state,execution_attempts,max_attempts,available_at,job_namespace,job_name,cache_candidate,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'queued',0,$16,$17,$18,$19,$20,$21,$21)`,
			attempt.ID, attempt.DefinitionID, attempt.DeliveryClaimKey, attempt.TriggerKind, attempt.TriggerKey, attempt.ProjectID, attempt.ServiceID,
			attempt.CommitSHA, attempt.GitRef, attempt.Generation, attempt.DefinitionDigest, planJSON, checkoutJSON, attempt.InputDigest, attempt.RegistryMode,
			attempt.MaxAttempts, attempt.AvailableAt, attempt.JobNamespace, attempt.JobName, attempt.CacheCandidate, attempt.CreatedAt)
		if err != nil {
			return nil, classifyPostgres(err)
		}
		staged = append(staged, attempt)
	}
	command, err := tx.Exec(ctx, `UPDATE github_webhook_receipts SET state='enqueued',failure_code='',completed_at=$4,lease_owner=NULL,lease_until=NULL,updated_at=$4 WHERE claim_key=$1 AND state='processing' AND lease_owner=$2 AND git_ref=$3`, input.ClaimKey, owner, input.GitRef, now.UTC())
	if err != nil {
		return nil, err
	}
	if command.RowsAffected() != 1 {
		return nil, ErrLeaseLost
	}
	return staged, tx.Commit(ctx)
}

func (s *PostgreSQLStore) Attempt(ctx context.Context, attemptID string) (BuildAttempt, error) {
	return attemptByIDQuery(ctx, s.pool, attemptID, false)
}

func (s *PostgreSQLStore) HistoricalAttempt(ctx context.Context, attemptID string) (BuildAttempt, error) {
	if !uuidRE.MatchString(attemptID) {
		return BuildAttempt{}, ErrInvalid
	}
	return historicalAttemptByIDQuery(ctx, s.pool, attemptID)
}

func (s *PostgreSQLStore) AttemptAuthorization(ctx context.Context, attemptID string) (Installation, Repository, error) {
	var installationID, repositoryID, attemptDigest, definitionDigest string
	var enabled bool
	err := s.pool.QueryRow(ctx, `SELECT d.installation_id::text,d.repository_id::text,a.definition_digest,d.definition_digest,d.enabled FROM build_attempts a JOIN build_definitions d ON d.id=a.definition_id WHERE a.id=$1`, attemptID).Scan(&installationID, &repositoryID, &attemptDigest, &definitionDigest, &enabled)
	if err != nil {
		return Installation{}, Repository{}, classifyPostgres(err)
	}
	installation, err := installationByIDQuery(ctx, s.pool, installationID)
	if err != nil {
		return Installation{}, Repository{}, err
	}
	repository, err := repositoryByIDQuery(ctx, s.pool, repositoryID)
	if err != nil {
		return Installation{}, Repository{}, err
	}
	if !enabled || attemptDigest != definitionDigest || installation.Lifecycle != InstallationActive || repository.Lifecycle != RepositoryActive || repository.InstallationID != installation.ID {
		return Installation{}, Repository{}, ErrUnauthorized
	}
	return installation, repository, nil
}

func (s *PostgreSQLStore) ClaimNextAttempt(ctx context.Context, owner string, now time.Time, duration time.Duration, maxConcurrent int) (BuildAttempt, error) {
	if !validOwnerLease(owner, duration) || maxConcurrent < 1 || maxConcurrent > MaximumConcurrentBuilders {
		return BuildAttempt{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return BuildAttempt{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kuberploy-build-attempt-capacity'))`); err != nil {
		return BuildAttempt{}, classifyPostgres(err)
	}
	row := tx.QueryRow(ctx, `SELECT id::text FROM build_attempts
		WHERE state IN ('queued','preparing','running','cancelling') AND available_at<=$1 AND (lease_until IS NULL OR lease_until<=$1)
		AND (state<>'queued' OR (SELECT count(*) FROM build_attempts WHERE state IN ('preparing','running','cancelling'))<$2)
		ORDER BY CASE state WHEN 'cancelling' THEN 0 WHEN 'running' THEN 1 WHEN 'preparing' THEN 2 ELSE 3 END,created_at,id
		FOR UPDATE SKIP LOCKED LIMIT 1`, now.UTC(), maxConcurrent)
	var attemptID string
	if err = row.Scan(&attemptID); err != nil {
		return BuildAttempt{}, classifyPostgres(err)
	}
	attempt, err := attemptByIDQuery(ctx, tx, attemptID, false)
	if err != nil {
		return BuildAttempt{}, err
	}
	state := attempt.State
	executionAttempts := attempt.ExecutionAttempts
	startedAt := attempt.StartedAt
	if state == AttemptQueued {
		state = AttemptPreparing
		executionAttempts++
		if startedAt == nil {
			at := now.UTC()
			startedAt = &at
		}
	}
	if attempt.CancelRequestedAt != nil {
		state = AttemptCancelling
	}
	leaseUntil := now.UTC().Add(duration)
	_, err = tx.Exec(ctx, `UPDATE build_attempts SET state=$2,execution_attempts=$3,started_at=$4,lease_owner=$5,lease_until=$6,updated_at=$7 WHERE id=$1`, attempt.ID, state, executionAttempts, startedAt, owner, leaseUntil, now.UTC())
	if err != nil {
		return BuildAttempt{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return BuildAttempt{}, err
	}
	attempt.State, attempt.ExecutionAttempts, attempt.StartedAt, attempt.LeaseOwner, attempt.LeaseUntil, attempt.UpdatedAt = state, executionAttempts, startedAt, owner, leaseUntil, now.UTC()
	return attempt, nil
}

func (s *PostgreSQLStore) HeartbeatAttempt(ctx context.Context, attemptID, owner string, now time.Time, duration time.Duration) error {
	if !validOwnerLease(owner, duration) {
		return ErrInvalid
	}
	command, err := s.pool.Exec(ctx, `UPDATE build_attempts SET lease_until=$4,updated_at=$3 WHERE id=$1 AND lease_owner=$2 AND lease_until>$3 AND state NOT IN ('succeeded','failed','cancelled')`, attemptID, owner, now.UTC(), now.UTC().Add(duration))
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgreSQLStore) MarkAttemptRunning(ctx context.Context, attemptID, owner string, now time.Time) error {
	command, err := s.pool.Exec(ctx, `UPDATE build_attempts SET state='running',updated_at=$3 WHERE id=$1 AND lease_owner=$2 AND lease_until>$3 AND state IN ('preparing','running')`, attemptID, owner, now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgreSQLStore) DeferAttempt(ctx context.Context, attemptID, owner, code string, now, availableAt time.Time) error {
	if validateFailureCode(code) != nil || availableAt.Before(now.UTC()) {
		return ErrInvalid
	}
	command, err := s.pool.Exec(ctx, `UPDATE build_attempts SET failure_code=$3,available_at=$5,lease_owner=NULL,lease_until=NULL,updated_at=$4 WHERE id=$1 AND lease_owner=$2 AND lease_until>$4 AND cancel_requested_at IS NULL AND state IN ('preparing','running')`, attemptID, owner, code, now.UTC(), availableAt.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgreSQLStore) ScheduleAttemptRetry(ctx context.Context, attemptID, owner, code string, now, availableAt time.Time) (bool, error) {
	if validateFailureCode(code) != nil || availableAt.Before(now.UTC()) {
		return false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	attempt, err := attemptByIDQuery(ctx, tx, attemptID, true)
	if err != nil {
		return false, err
	}
	if attempt.LeaseOwner != owner || !attempt.LeaseUntil.After(now.UTC()) || terminalAttempt(attempt.State) {
		return false, ErrLeaseLost
	}
	if attempt.CancelRequestedAt != nil {
		_, err = tx.Exec(ctx, `UPDATE build_attempts SET state='cancelling',failure_code=$3,available_at=$5,lease_owner=NULL,lease_until=NULL,updated_at=$4 WHERE id=$1 AND lease_owner=$2 AND lease_until>$4`, attemptID, owner, code, now.UTC(), availableAt.UTC())
		if err != nil {
			return false, err
		}
		if err = tx.Commit(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	if attempt.ExecutionAttempts >= attempt.MaxAttempts {
		_, err = tx.Exec(ctx, `UPDATE build_attempts SET state='failed',failure_code=$3,completed_at=$4,lease_owner=NULL,lease_until=NULL,updated_at=$4 WHERE id=$1 AND lease_owner=$2 AND lease_until>$4`, attemptID, owner, code, now.UTC())
		return false, joinCommit(tx, ctx, err)
	}
	_, err = tx.Exec(ctx, `UPDATE build_attempts SET state='queued',failure_code=$3,available_at=$5,lease_owner=NULL,lease_until=NULL,updated_at=$4 WHERE id=$1 AND lease_owner=$2 AND lease_until>$4`, attemptID, owner, code, now.UTC(), availableAt.UTC())
	return true, joinCommit(tx, ctx, err)
}

func (s *PostgreSQLStore) FailAttempt(ctx context.Context, attemptID, owner, code string, now time.Time) error {
	if validateFailureCode(code) != nil {
		return ErrInvalid
	}
	command, err := s.pool.Exec(ctx, `UPDATE build_attempts SET state='failed',failure_code=$3,completed_at=$4,lease_owner=NULL,lease_until=NULL,updated_at=$4 WHERE id=$1 AND lease_owner=$2 AND lease_until>$4 AND cancel_requested_at IS NULL AND state NOT IN ('succeeded','failed','cancelled','cancelling')`, attemptID, owner, code, now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgreSQLStore) CompleteAttempt(ctx context.Context, attemptID, owner string, completion BuildCompletion, now time.Time) error {
	if validateBuildResult(completion.Result, completion.CacheReference, completion.LogReference) != nil {
		return ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	attempt, err := attemptByIDQuery(ctx, tx, attemptID, true)
	if err != nil {
		return err
	}
	result := completion.Result
	if attempt.LeaseOwner != owner || !attempt.LeaseUntil.After(now.UTC()) || attempt.State != AttemptRunning || attempt.CancelRequestedAt != nil || result.OperationID != attempt.ID || result.Generation != attempt.Generation ||
		result.Image.Reference != attempt.PlanRequest.Build.Destination.Repository+"@"+result.Image.Digest || !slices.Equal(result.Image.Platforms, attempt.PlanRequest.Build.Platforms) ||
		(completion.CacheReference != "" && completion.CacheReference != cacheReference(attempt)) {
		return ErrLeaseLost
	}
	resultJSON, _ := json.Marshal(result)
	_, err = tx.Exec(ctx, `UPDATE build_attempts SET state='succeeded',result=$3,cache_reference=$4,log_reference=$5,failure_code='',completed_at=$6,lease_owner=NULL,lease_until=NULL,updated_at=$6 WHERE id=$1 AND lease_owner=$2 AND lease_until>$6 AND state='running' AND cancel_requested_at IS NULL`, attemptID, owner, resultJSON, completion.CacheReference, completion.LogReference, now.UTC())
	return joinCommit(tx, ctx, err)
}

func (s *PostgreSQLStore) RequestCancel(ctx context.Context, attemptID string, now time.Time) (BuildAttempt, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return BuildAttempt{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	attempt, err := attemptByIDQuery(ctx, tx, attemptID, true)
	if err != nil {
		return BuildAttempt{}, err
	}
	if terminalAttempt(attempt.State) {
		return attempt, tx.Commit(ctx)
	}
	state := AttemptCancelling
	var completedAt *time.Time
	if attempt.State == AttemptQueued && attempt.LeaseOwner == "" {
		state = AttemptCancelled
		at := now.UTC()
		completedAt = &at
	}
	_, err = tx.Exec(ctx, `UPDATE build_attempts SET state=$2,cancel_requested_at=$3,available_at=$3,completed_at=$4,updated_at=$3 WHERE id=$1`, attemptID, state, now.UTC(), completedAt)
	if err != nil {
		return BuildAttempt{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return BuildAttempt{}, err
	}
	attempt.State, attempt.CancelRequestedAt, attempt.CompletedAt, attempt.AvailableAt, attempt.UpdatedAt = state, timePointer(now.UTC()), completedAt, now.UTC(), now.UTC()
	return attempt, nil
}

func (s *PostgreSQLStore) CompleteCancellation(ctx context.Context, attemptID, owner string, now time.Time) error {
	command, err := s.pool.Exec(ctx, `UPDATE build_attempts SET state='cancelled',failure_code='',completed_at=$3,lease_owner=NULL,lease_until=NULL,updated_at=$3 WHERE id=$1 AND lease_owner=$2 AND lease_until>$3 AND state='cancelling'`, attemptID, owner, now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

type rowQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type rowsQuery interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func deliveryByQuery(ctx context.Context, query rowQuery, claimKey string, forUpdate bool) (DeliveryReceipt, error) {
	suffix := ""
	if forUpdate {
		suffix = " FOR UPDATE"
	}
	var receipt DeliveryReceipt
	var leaseOwner *string
	var leaseUntil *time.Time
	err := query.QueryRow(ctx, `SELECT claim_key,github_app_id,github_installation_id,delivery_id::text,event,body_sha256,typed_event,COALESCE(repository_id,0),git_ref,state,failure_code,lease_owner,lease_until,available_at,received_at,completed_at,updated_at FROM github_webhook_receipts WHERE claim_key=$1`+suffix, claimKey).
		Scan(&receipt.ClaimKey, &receipt.AppID, &receipt.GitHubInstallationID, &receipt.DeliveryID, &receipt.Event, &receipt.BodySHA256, &receipt.TypedEvent, &receipt.RepositoryID, &receipt.GitRef,
			&receipt.State, &receipt.FailureCode, &leaseOwner, &leaseUntil, &receipt.AvailableAt, &receipt.ReceivedAt, &receipt.CompletedAt, &receipt.UpdatedAt)
	if leaseOwner != nil {
		receipt.LeaseOwner = *leaseOwner
	}
	if leaseUntil != nil {
		receipt.LeaseUntil = *leaseUntil
	}
	return receipt, classifyPostgres(err)
}

func installationByProviderQuery(ctx context.Context, query rowQuery, appID, providerID int64) (Installation, error) {
	return scanInstallation(query.QueryRow(ctx, `SELECT id::text,github_app_id,github_installation_id,github_account_id,account_login,account_type,repository_selection,permissions,lifecycle,suspended_at,deleted_at,last_verified_at,updated_at FROM github_installations WHERE github_app_id=$1 AND github_installation_id=$2`, appID, providerID))
}

func installationByIDQuery(ctx context.Context, query rowQuery, installationID string) (Installation, error) {
	return scanInstallation(query.QueryRow(ctx, `SELECT id::text,github_app_id,github_installation_id,github_account_id,account_login,account_type,repository_selection,permissions,lifecycle,suspended_at,deleted_at,last_verified_at,updated_at FROM github_installations WHERE id=$1`, installationID))
}

func scanInstallation(row pgx.Row) (Installation, error) {
	var installation Installation
	var permissions []byte
	err := row.Scan(&installation.ID, &installation.AppID, &installation.GitHubInstallationID, &installation.Account.ID, &installation.Account.Login, &installation.Account.Type,
		&installation.RepositorySelection, &permissions, &installation.Lifecycle, &installation.SuspendedAt, &installation.DeletedAt, &installation.LastVerifiedAt, &installation.UpdatedAt)
	if err != nil {
		return Installation{}, classifyPostgres(err)
	}
	if err = decodeClosedJSON(permissions, &installation.Permissions); err != nil || installation.validate() != nil {
		return Installation{}, ErrInvalid
	}
	return installation, nil
}

func repositoryByProviderQuery(ctx context.Context, query rowQuery, installationID string, providerID int64) (Repository, error) {
	return scanRepository(query.QueryRow(ctx, `SELECT id::text,installation_id::text,github_repository_id,github_owner_id,owner_login,name,lifecycle,last_verified_at,removed_at,created_at,updated_at FROM github_repositories WHERE installation_id=$1 AND github_repository_id=$2`, installationID, providerID))
}

func repositoryByIDQuery(ctx context.Context, query rowQuery, repositoryID string) (Repository, error) {
	return scanRepository(query.QueryRow(ctx, `SELECT id::text,installation_id::text,github_repository_id,github_owner_id,owner_login,name,lifecycle,last_verified_at,removed_at,created_at,updated_at FROM github_repositories WHERE id=$1`, repositoryID))
}

func scanRepository(row pgx.Row) (Repository, error) {
	var repository Repository
	err := row.Scan(&repository.ID, &repository.InstallationID, &repository.Identity.ID, &repository.Identity.OwnerID, &repository.Identity.OwnerLogin, &repository.Identity.Name,
		&repository.Lifecycle, &repository.LastVerifiedAt, &repository.RemovedAt, &repository.CreatedAt, &repository.UpdatedAt)
	if err != nil {
		return Repository{}, classifyPostgres(err)
	}
	if repository.validate() != nil {
		return Repository{}, ErrInvalid
	}
	return repository, nil
}

func definitionByIDQuery(ctx context.Context, query rowQuery, definitionID string, forUpdate bool) (BuildDefinition, error) {
	suffix := ""
	if forUpdate {
		suffix = " FOR SHARE"
	}
	return scanDefinition(query.QueryRow(ctx, `SELECT id::text,project_id::text,service_id::text,source_kind,COALESCE(installation_id::text,''),COALESCE(repository_id::text,''),git_ssh_source,trigger_ref,spec,definition_digest,generation,enabled,created_at,updated_at FROM build_definitions WHERE id=$1`+suffix, definitionID))
}

type scanner interface{ Scan(...any) error }

func scanDefinition(row scanner) (BuildDefinition, error) {
	var definition BuildDefinition
	var spec, gitSSHSource []byte
	err := row.Scan(&definition.ID, &definition.ProjectID, &definition.ServiceID, &definition.SourceKind, &definition.InstallationID, &definition.RepositoryID, &gitSSHSource, &definition.TriggerRef, &spec,
		&definition.DefinitionDigest, &definition.DefinitionGeneration, &definition.Enabled, &definition.CreatedAt, &definition.UpdatedAt)
	if err != nil {
		return BuildDefinition{}, classifyPostgres(err)
	}
	if len(gitSSHSource) != 0 {
		definition.GitSSH = &GitSSHSource{}
		if err = decodeClosedJSON(gitSSHSource, definition.GitSSH); err != nil {
			return BuildDefinition{}, ErrInvalid
		}
	}
	if err = decodeClosedJSON(spec, &definition.Spec); err != nil || definition.validate() != nil {
		return BuildDefinition{}, ErrInvalid
	}
	return definition, nil
}

func nullableUUID(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func attemptByIDQuery(ctx context.Context, query rowQuery, attemptID string, forUpdate bool) (BuildAttempt, error) {
	suffix := ""
	if forUpdate {
		suffix = " FOR UPDATE"
	}
	return scanAttempt(query.QueryRow(ctx, `SELECT id::text,definition_id::text,COALESCE(delivery_claim_key,''),trigger_kind,trigger_key,project_id::text,service_id::text,commit_sha,git_ref,generation,definition_digest,plan_request,checkout_request,input_digest,registry_mode,state,execution_attempts,max_attempts,available_at,lease_owner,lease_until,job_namespace,job_name,cache_candidate,cache_reference,result,log_reference,failure_code,cancel_requested_at,started_at,completed_at,created_at,updated_at FROM build_attempts WHERE id=$1`+suffix, attemptID))
}

func historicalAttemptByIDQuery(ctx context.Context, query rowQuery, attemptID string) (BuildAttempt, error) {
	return scanAttemptHistory(query.QueryRow(ctx, `SELECT id::text,definition_id::text,project_id::text,service_id::text,commit_sha,git_ref,generation,state,execution_attempts,max_attempts,cache_reference,result,failure_code,cancel_requested_at,started_at,completed_at,created_at,updated_at FROM build_attempts WHERE id=$1`, attemptID))
}

func pushAttemptBySourceQuery(ctx context.Context, query rowQuery, definitionID, commitSHA, gitRef string) (BuildAttempt, error) {
	return scanAttempt(query.QueryRow(ctx, `SELECT a.id::text,a.definition_id::text,COALESCE(a.delivery_claim_key,''),a.trigger_kind,a.trigger_key,a.project_id::text,a.service_id::text,a.commit_sha,a.git_ref,a.generation,a.definition_digest,a.plan_request,a.checkout_request,a.input_digest,a.registry_mode,a.state,a.execution_attempts,a.max_attempts,a.available_at,a.lease_owner,a.lease_until,a.job_namespace,a.job_name,a.cache_candidate,a.cache_reference,a.result,a.log_reference,a.failure_code,a.cancel_requested_at,a.started_at,a.completed_at,a.created_at,a.updated_at
		FROM build_attempts a JOIN github_webhook_receipts r ON r.claim_key=a.delivery_claim_key
		WHERE a.definition_id=$1 AND a.commit_sha=$2 AND a.git_ref=$3 AND r.typed_event IS NOT NULL
		ORDER BY a.generation,a.id LIMIT 1`, definitionID, commitSHA, gitRef))
}

func scanAttempt(row scanner) (BuildAttempt, error) {
	var attempt BuildAttempt
	var planJSON, checkoutJSON []byte
	var resultJSON []byte
	var leaseOwner *string
	var leaseUntil *time.Time
	err := row.Scan(&attempt.ID, &attempt.DefinitionID, &attempt.DeliveryClaimKey, &attempt.TriggerKind, &attempt.TriggerKey, &attempt.ProjectID, &attempt.ServiceID, &attempt.CommitSHA, &attempt.GitRef,
		&attempt.Generation, &attempt.DefinitionDigest, &planJSON, &checkoutJSON, &attempt.InputDigest, &attempt.RegistryMode, &attempt.State,
		&attempt.ExecutionAttempts, &attempt.MaxAttempts, &attempt.AvailableAt, &leaseOwner, &leaseUntil, &attempt.JobNamespace, &attempt.JobName,
		&attempt.CacheCandidate, &attempt.CacheReference, &resultJSON, &attempt.LogReference, &attempt.FailureCode, &attempt.CancelRequestedAt,
		&attempt.StartedAt, &attempt.CompletedAt, &attempt.CreatedAt, &attempt.UpdatedAt)
	if err != nil {
		return BuildAttempt{}, classifyPostgres(err)
	}
	if leaseOwner != nil {
		attempt.LeaseOwner = *leaseOwner
	}
	if leaseUntil != nil {
		attempt.LeaseUntil = *leaseUntil
	}
	if err = decodeClosedJSON(planJSON, &attempt.PlanRequest); err != nil {
		return BuildAttempt{}, ErrInvalid
	}
	if err = decodeClosedJSON(checkoutJSON, &attempt.CheckoutRequest); err != nil {
		return BuildAttempt{}, ErrInvalid
	}
	if len(resultJSON) > 0 {
		var result builder.BuildResult
		if err = decodeClosedJSON(resultJSON, &result); err != nil {
			return BuildAttempt{}, ErrInvalid
		}
		normalizeLegacyCacheReuse(&result)
		attempt.Result = &result
	}
	if err = validateStoredAttempt(attempt); err != nil {
		return BuildAttempt{}, err
	}
	return attempt, nil
}

func validateStoredAttempt(attempt BuildAttempt) error {
	if !uuidRE.MatchString(attempt.ID) || !uuidRE.MatchString(attempt.DefinitionID) || !validAttemptTrigger(attempt) ||
		!uuidRE.MatchString(attempt.ProjectID) || !uuidRE.MatchString(attempt.ServiceID) || !commitRE.MatchString(attempt.CommitSHA) || !validGitRef(attempt.GitRef) ||
		attempt.Generation < 1 || !digestRE.MatchString(attempt.DefinitionDigest) || !digestRE.MatchString(attempt.InputDigest) ||
		(attempt.RegistryMode != RegistryManaged && attempt.RegistryMode != RegistryExternal) || attempt.ExecutionAttempts < 0 ||
		attempt.MaxAttempts < 1 || attempt.MaxAttempts > 5 || !kubeNameRE.MatchString(attempt.JobNamespace) || !kubeNameRE.MatchString(attempt.JobName) ||
		attempt.CreatedAt.IsZero() || attempt.UpdatedAt.IsZero() || attempt.PlanRequest.Build.OperationID != attempt.ID || attempt.CheckoutRequest.OperationID != attempt.ID ||
		attempt.PlanRequest.Build.Generation != attempt.Generation || attempt.CheckoutRequest.Generation != attempt.Generation {
		return ErrInvalid
	}
	inputDigest, err := attemptInputDigest(attempt.PlanRequest, attempt.CheckoutRequest)
	if err != nil || inputDigest != attempt.InputDigest || attempt.PlanRequest.Build.Validate() != nil || attempt.CheckoutRequest.Validate() != nil {
		return ErrInvalid
	}
	if _, err = builder.PlanJob(attempt.PlanRequest); err != nil {
		return ErrInvalid
	}
	if terminalAttempt(attempt.State) != (attempt.CompletedAt != nil) || (attempt.LeaseOwner == "") != attempt.LeaseUntil.IsZero() {
		return ErrInvalid
	}
	if attempt.State == AttemptSucceeded {
		if attempt.Result == nil || validateResultForAttempt(*attempt.Result, attempt) != nil {
			return ErrInvalid
		}
	}
	return nil
}

func validAttemptTrigger(attempt BuildAttempt) bool {
	switch attempt.TriggerKind {
	case "github_push":
		return regexpHex64(attempt.DeliveryClaimKey) && attempt.TriggerKey == attempt.DeliveryClaimKey
	case "manual", "retry":
		return attempt.TriggerKey != "" && len(attempt.TriggerKey) <= 256 && !strings.ContainsAny(attempt.TriggerKey, "\x00\r\n")
	default:
		return false
	}
}

func attemptsForDeliveryQuery(ctx context.Context, query rowsQuery, claimKey string) ([]BuildAttempt, error) {
	rows, err := query.Query(ctx, `SELECT id::text,definition_id::text,COALESCE(delivery_claim_key,''),trigger_kind,trigger_key,project_id::text,service_id::text,commit_sha,git_ref,generation,definition_digest,plan_request,checkout_request,input_digest,registry_mode,state,execution_attempts,max_attempts,available_at,lease_owner,lease_until,job_namespace,job_name,cache_candidate,cache_reference,result,log_reference,failure_code,cancel_requested_at,started_at,completed_at,created_at,updated_at FROM build_attempts WHERE delivery_claim_key=$1 ORDER BY id`, claimKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]BuildAttempt, 0)
	for rows.Next() {
		attempt, scanErr := scanAttempt(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, attempt)
	}
	return result, rows.Err()
}

func cacheImportsQuery(ctx context.Context, query rowsQuery, definition BuildDefinition, generation int64) ([]string, error) {
	rows, err := query.Query(ctx, `SELECT cache_reference FROM build_attempts WHERE project_id=$1 AND service_id=$2 AND definition_digest=$3 AND state='succeeded' AND cache_reference<>'' AND generation<$4 ORDER BY generation DESC LIMIT $5`, definition.ProjectID, definition.ServiceID, definition.DefinitionDigest, generation, definition.Spec.CacheImports)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := make([]string, 0)
	for rows.Next() {
		var ref string
		if err = rows.Scan(&ref); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(refs)
	return refs, nil
}

func decodeClosedJSON(encoded []byte, destination any) error {
	if len(encoded) == 0 || len(encoded) > builder.MaxRequestBytes {
		return ErrInvalid
	}
	decoder := json.NewDecoder(io.LimitReader(bytes.NewReader(encoded), builder.MaxRequestBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return ErrInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ErrInvalid
	}
	return nil
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
		case "23505", "23503", "23514", "40001", "40P01":
			return ErrConflict
		}
	}
	return err
}

func joinCommit(tx pgx.Tx, ctx context.Context, err error) error {
	if err != nil {
		return classifyPostgres(err)
	}
	return classifyPostgres(tx.Commit(ctx))
}

func timePointer(value time.Time) *time.Time { return &value }

var _ Store = (*PostgreSQLStore)(nil)
var _ Store = (*MemoryStore)(nil)
