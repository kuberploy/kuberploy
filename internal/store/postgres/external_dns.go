package postgres

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/externaldns"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func (s *Store) ListExternalDNSIntegrationsForActor(ctx context.Context, actor string) ([]domain.ExternalDNSIntegration, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionExternalDNSManage, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return nil, err
	}
	items, err := externalDNSIntegrations(ctx, tx, "", "", false)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) ListExternalDNSIntegrationsForRuntime(ctx context.Context, limit int) ([]domain.ExternalDNSIntegration, error) {
	if limit < 1 || limit > 100 {
		return nil, base.ErrConflict
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	items, err := externalDNSIntegrations(ctx, tx, "", "", true)
	if err != nil {
		return nil, err
	}
	if len(items) > limit {
		return nil, base.ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) RecordExternalDNSPublication(ctx context.Context, integrationID string, revision int64, deleted bool, contentDigest, commit string, observedAt time.Time) error {
	if revision < 1 || observedAt.IsZero() {
		return base.ErrConflict
	}
	state := "materialized"
	if deleted {
		state = "dematerialized"
		contentDigest = ""
	}
	result, err := s.pool.Exec(ctx, `UPDATE external_dns_integrations SET protected_git_state=$3,protected_git_revision=$2,
		protected_git_content_digest=$4,protected_git_commit=$5,protected_git_observed_at=$6,updated_at=GREATEST(updated_at,$6)
		WHERE id=$1 AND runtime_revision=$2 AND (($3='materialized' AND lifecycle='active') OR ($3='dematerialized' AND lifecycle='deactivated'))`, integrationID, revision, state, contentDigest, commit, observedAt.UTC())
	if err != nil {
		return classify(err)
	}
	if result.RowsAffected() != 1 {
		return base.ErrConflict
	}
	return nil
}

func (s *Store) AdvanceExternalDNSRuntimeRevision(ctx context.Context, integrationID string, revision int64, contentDigest string, changedAt time.Time) error {
	if revision < 1 || contentDigest == "" || changedAt.IsZero() {
		return base.ErrConflict
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	item, err := externalDNSIntegration(ctx, tx, integrationID, true)
	if err != nil {
		return err
	}
	if item.Lifecycle != "active" || item.RuntimeRevision != revision || item.ProtectedGitState != "materialized" ||
		item.ProtectedGitRevision != revision || item.ProtectedGitContentDigest != contentDigest {
		return base.ErrConflict
	}
	now := databaseTime(changedAt)
	result, err := tx.Exec(ctx, `UPDATE external_dns_integrations SET runtime_revision=runtime_revision+1,updated_at=$4,
		protected_git_state='pending',protected_git_revision=NULL,protected_git_content_digest='',protected_git_commit='',protected_git_observed_at=NULL
		WHERE id=$1 AND runtime_revision=$2 AND lifecycle='active' AND protected_git_state='materialized'
		  AND protected_git_revision=$2 AND protected_git_content_digest=$3`, integrationID, revision, contentDigest, now)
	if err != nil {
		return classify(err)
	}
	if result.RowsAffected() != 1 {
		return base.ErrConflict
	}
	if err = invalidateExternalDNSProjectionBindings(ctx, tx, item.EnvironmentIDs, now); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return classify(err)
	}
	return nil
}

func (s *Store) CreateExternalDNSIntegrationForActor(ctx context.Context, actor, key, fingerprint, requestID string, integration domain.ExternalDNSIntegration) (base.Result[domain.ExternalDNSIntegration], error) {
	if err := externaldns.Validate(integration); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionExternalDNSManage, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	if replay, ok, replayErr := externalDNSReplay(ctx, tx, actor, "external-dns-integrations.create", key, fingerprint); replayErr != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, replayErr
	} else if ok {
		if err = tx.Commit(ctx); err != nil {
			return base.Result[domain.ExternalDNSIntegration]{}, err
		}
		return base.Result[domain.ExternalDNSIntegration]{Value: replay, Replay: true}, nil
	}
	if err = externalDNSEnvironmentsExist(ctx, tx, integration.EnvironmentIDs); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	now := databaseTime(time.Now())
	integration.CreatedBy, integration.CreatedAt, integration.UpdatedAt = actor, now, now
	integration.RuntimeRevision, integration.Lifecycle, integration.ProtectedGitState = 1, "active", "pending"
	suffixes, _ := json.Marshal(integration.AllowedDomainSuffixes)
	_, err = tx.Exec(ctx, `INSERT INTO external_dns_integrations(
		id,slug,name,mode,provider_kind,txt_owner_id,allowed_domain_suffixes,sync_policy,
		destructive_sync_confirmed,credential_secret_ref,provider_config_ref,egress_config_ref,
		operator_profile_ref,created_by,created_at,updated_at,runtime_revision,lifecycle
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),NULLIF($11,''),NULLIF($12,''),NULLIF($13,''),$14,$15,$16,$17,$18)`,
		integration.ID, integration.Slug, integration.Name, integration.Mode, integration.ProviderKind,
		integration.TXTOwnerID, suffixes, integration.SyncPolicy, integration.DestructiveSyncConfirmed,
		integration.CredentialSecretRef, integration.ProviderConfigRef, integration.EgressConfigRef,
		integration.OperatorProfileRef, integration.CreatedBy, integration.CreatedAt, integration.UpdatedAt,
		integration.RuntimeRevision, integration.Lifecycle)
	if err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, classify(err)
	}
	if err = replaceExternalDNSEnvironments(ctx, tx, integration.ID, integration.EnvironmentIDs, now); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, classify(err)
	}
	if err = invalidateExternalDNSProjectionBindings(ctx, tx, integration.EnvironmentIDs, now); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	if err = putIdem(ctx, tx, actor, "external-dns-integrations.create", key, fingerprint, "external-dns-integration", integration.ID, nil); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "external-dns-integration.create", "external-dns-integration", integration.ID, requestID, externalDNSAuditDetail(integration)); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	return base.Result[domain.ExternalDNSIntegration]{Value: integration}, nil
}

func (s *Store) UpdateExternalDNSIntegrationForActor(ctx context.Context, actor, key, fingerprint, requestID string, integration domain.ExternalDNSIntegration) (base.Result[domain.ExternalDNSIntegration], error) {
	if err := externaldns.Validate(integration); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionExternalDNSManage, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	idemScope := "external-dns-integrations.update:" + integration.ID
	if replay, ok, replayErr := externalDNSReplay(ctx, tx, actor, idemScope, key, fingerprint); replayErr != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, replayErr
	} else if ok {
		if err = tx.Commit(ctx); err != nil {
			return base.Result[domain.ExternalDNSIntegration]{}, err
		}
		return base.Result[domain.ExternalDNSIntegration]{Value: replay, Replay: true}, nil
	}
	current, err := externalDNSIntegration(ctx, tx, integration.ID, true)
	if err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	if current.Lifecycle == "deactivated" || current.Slug != integration.Slug || current.TXTOwnerID != integration.TXTOwnerID {
		return base.Result[domain.ExternalDNSIntegration]{}, base.ErrConflict
	}
	if err = externalDNSEnvironmentsExist(ctx, tx, integration.EnvironmentIDs); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	integration.CreatedBy, integration.CreatedAt, integration.UpdatedAt = current.CreatedBy, current.CreatedAt, databaseTime(time.Now())
	changed := current.Name != integration.Name || current.Mode != integration.Mode || current.ProviderKind != integration.ProviderKind || !slices.Equal(current.AllowedDomainSuffixes, integration.AllowedDomainSuffixes) || current.SyncPolicy != integration.SyncPolicy || current.DestructiveSyncConfirmed != integration.DestructiveSyncConfirmed || current.CredentialSecretRef != integration.CredentialSecretRef || current.ProviderConfigRef != integration.ProviderConfigRef || current.EgressConfigRef != integration.EgressConfigRef || current.OperatorProfileRef != integration.OperatorProfileRef
	integration.RuntimeRevision, integration.Lifecycle = current.RuntimeRevision, "active"
	if changed {
		integration.RuntimeRevision++
		integration.ProtectedGitState = "pending"
		integration.ProtectedGitRevision = 0
		integration.ProtectedGitContentDigest = ""
		integration.ProtectedGitCommit = ""
		integration.ProtectedGitObservedAt = nil
	} else {
		integration.ProtectedGitState = current.ProtectedGitState
		integration.ProtectedGitRevision = current.ProtectedGitRevision
		integration.ProtectedGitContentDigest = current.ProtectedGitContentDigest
		integration.ProtectedGitCommit = current.ProtectedGitCommit
		integration.ProtectedGitObservedAt = current.ProtectedGitObservedAt
	}
	suffixes, _ := json.Marshal(integration.AllowedDomainSuffixes)
	result, err := tx.Exec(ctx, `UPDATE external_dns_integrations SET
		name=$2,mode=$3,provider_kind=$4,allowed_domain_suffixes=$5,sync_policy=$6,
		destructive_sync_confirmed=$7,credential_secret_ref=NULLIF($8,''),provider_config_ref=NULLIF($9,''),
		egress_config_ref=NULLIF($10,''),operator_profile_ref=NULLIF($11,''),updated_at=$12,runtime_revision=$13,
		protected_git_state=CASE WHEN $14 THEN 'pending' ELSE protected_git_state END,
		protected_git_revision=CASE WHEN $14 THEN NULL ELSE protected_git_revision END,
		protected_git_content_digest=CASE WHEN $14 THEN '' ELSE protected_git_content_digest END,
		protected_git_commit=CASE WHEN $14 THEN '' ELSE protected_git_commit END,
		protected_git_observed_at=CASE WHEN $14 THEN NULL ELSE protected_git_observed_at END
		WHERE id=$1 AND lifecycle='active'`,
		integration.ID, integration.Name, integration.Mode, integration.ProviderKind, suffixes,
		integration.SyncPolicy, integration.DestructiveSyncConfirmed, integration.CredentialSecretRef,
		integration.ProviderConfigRef, integration.EgressConfigRef, integration.OperatorProfileRef, integration.UpdatedAt, integration.RuntimeRevision, changed)
	if err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, classify(err)
	}
	if result.RowsAffected() != 1 {
		return base.Result[domain.ExternalDNSIntegration]{}, base.ErrConflict
	}
	if err = replaceExternalDNSEnvironments(ctx, tx, integration.ID, integration.EnvironmentIDs, integration.UpdatedAt); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, classify(err)
	}
	affectedEnvironments := append(append([]string(nil), current.EnvironmentIDs...), integration.EnvironmentIDs...)
	if err = invalidateExternalDNSProjectionBindings(ctx, tx, affectedEnvironments, integration.UpdatedAt); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	if err = putIdem(ctx, tx, actor, idemScope, key, fingerprint, "external-dns-integration", integration.ID, nil); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "external-dns-integration.update", "external-dns-integration", integration.ID, requestID, externalDNSAuditDetail(integration)); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	return base.Result[domain.ExternalDNSIntegration]{Value: integration}, nil
}

func (s *Store) DeactivateExternalDNSIntegrationForActor(ctx context.Context, actor, key, fingerprint, requestID, integrationID string) (base.Result[domain.ExternalDNSIntegration], error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionExternalDNSManage, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	scope := "external-dns-integrations.deactivate:" + integrationID
	if replay, ok, replayErr := externalDNSReplay(ctx, tx, actor, scope, key, fingerprint); replayErr != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, replayErr
	} else if ok {
		if err = tx.Commit(ctx); err != nil {
			return base.Result[domain.ExternalDNSIntegration]{}, err
		}
		return base.Result[domain.ExternalDNSIntegration]{Value: replay, Replay: true}, nil
	}
	item, err := externalDNSIntegration(ctx, tx, integrationID, true)
	if err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	if item.Lifecycle != "active" {
		return base.Result[domain.ExternalDNSIntegration]{}, base.ErrConflict
	}
	now := databaseTime(time.Now())
	result, err := tx.Exec(ctx, `UPDATE external_dns_integrations SET lifecycle='deactivated',deactivated_by=$2,deactivated_at=$3,updated_at=$3,
		protected_git_state='pending',protected_git_revision=NULL,protected_git_content_digest='',protected_git_commit='',protected_git_observed_at=NULL WHERE id=$1 AND lifecycle='active'`, integrationID, actor, now)
	if err != nil || result.RowsAffected() != 1 {
		if err != nil {
			return base.Result[domain.ExternalDNSIntegration]{}, classify(err)
		}
		return base.Result[domain.ExternalDNSIntegration]{}, base.ErrConflict
	}
	item.Lifecycle, item.DeactivatedBy, item.DeactivatedAt, item.UpdatedAt = "deactivated", actor, &now, now
	item.ProtectedGitState, item.ProtectedGitRevision, item.ProtectedGitContentDigest, item.ProtectedGitCommit, item.ProtectedGitObservedAt = "pending", 0, "", "", nil
	if err = invalidateExternalDNSProjectionBindings(ctx, tx, item.EnvironmentIDs, now); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	if err = putIdem(ctx, tx, actor, scope, key, fingerprint, "external-dns-integration", integrationID, nil); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "external-dns-integration.deactivate", "external-dns-integration", integrationID, requestID, externalDNSAuditDetail(item)); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	return base.Result[domain.ExternalDNSIntegration]{Value: item}, nil
}

// invalidateExternalDNSProjectionBindings closes the direct-Git policy
// revalidation wakeup. State changes to indexing prevent a concurrent
// reconciliation from recording a metadata update as already validated.
func invalidateExternalDNSProjectionBindings(ctx context.Context, tx pgx.Tx, environmentIDs []string, changedAt time.Time) error {
	if len(environmentIDs) == 0 || changedAt.IsZero() {
		return base.ErrConflict
	}
	seen := map[string]struct{}{}
	unique := make([]string, 0, len(environmentIDs))
	for _, environmentID := range environmentIDs {
		if _, exists := seen[environmentID]; exists {
			continue
		}
		seen[environmentID] = struct{}{}
		unique = append(unique, environmentID)
	}
	_, err := tx.Exec(ctx, `UPDATE git_repository_bindings
		SET state=CASE WHEN target_head_revision IS NULL THEN state ELSE 'indexing' END,
			updated_at=GREATEST(updated_at+interval '1 microsecond',$2)
		WHERE kind='environment' AND environment_id=ANY($1::uuid[])`, unique, changedAt.UTC())
	return classify(err)
}

func (s *Store) ExternalDNSIntegrationsForEnvironmentActor(ctx context.Context, actor, environmentID string) ([]domain.ExternalDNSIntegration, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionExternalDNSRead, domain.AccessTarget{Type: "environment", ID: environmentID}); err != nil {
		return nil, err
	}
	items, err := externalDNSIntegrations(ctx, tx, environmentID, "", false)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) ExternalDNSIntegrationsForApplicationActor(ctx context.Context, actor, applicationID, environmentID string) ([]domain.ExternalDNSIntegration, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionExternalDNSRead, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return nil, err
	}
	var valid bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM applications a JOIN environments e ON e.project_id=a.project_id WHERE a.id=$1 AND e.id=$2)`, applicationID, environmentID).Scan(&valid); err != nil {
		return nil, err
	}
	if !valid {
		return nil, base.ErrNotFound
	}
	items, err := externalDNSIntegrations(ctx, tx, environmentID, "", false)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func externalDNSReplay(ctx context.Context, tx pgx.Tx, actor, scope, key, fingerprint string) (domain.ExternalDNSIntegration, bool, error) {
	old, ok, err := findIdem(ctx, tx, actor, scope, key)
	if err != nil || !ok {
		return domain.ExternalDNSIntegration{}, false, err
	}
	if old.fingerprint != fingerprint {
		return domain.ExternalDNSIntegration{}, false, base.ErrIdempotencyConflict
	}
	item, err := externalDNSIntegration(ctx, tx, old.resourceID, false)
	return item, err == nil, err
}

func externalDNSIntegration(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, integrationID string, forUpdate bool) (domain.ExternalDNSIntegration, error) {
	query := externalDNSSelect + ` WHERE i.id=$1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var item domain.ExternalDNSIntegration
	var suffixes []byte
	err := q.QueryRow(ctx, query, integrationID).Scan(&item.ID, &item.Slug, &item.Name, &item.Mode, &item.ProviderKind,
		&item.TXTOwnerID, &suffixes, &item.SyncPolicy, &item.DestructiveSyncConfirmed, &item.CredentialSecretRef,
		&item.ProviderConfigRef, &item.EgressConfigRef, &item.OperatorProfileRef, &item.CreatedBy, &item.CreatedAt, &item.UpdatedAt,
		&item.RuntimeRevision, &item.Lifecycle, &item.DeactivatedBy, &item.DeactivatedAt,
		&item.ProtectedGitState, &item.ProtectedGitRevision, &item.ProtectedGitContentDigest, &item.ProtectedGitCommit, &item.ProtectedGitObservedAt)

	if err != nil {
		return item, classify(err)
	}
	if err = json.Unmarshal(suffixes, &item.AllowedDomainSuffixes); err != nil {
		return item, err
	}
	item.EnvironmentIDs, err = externalDNSEnvironmentIDs(ctx, q, item.ID)
	return item, err
}

const externalDNSSelect = `SELECT i.id,i.slug,i.name,i.mode,i.provider_kind,i.txt_owner_id,i.allowed_domain_suffixes,
	i.sync_policy,i.destructive_sync_confirmed,COALESCE(i.credential_secret_ref,''),COALESCE(i.provider_config_ref,''),
	COALESCE(i.egress_config_ref,''),COALESCE(i.operator_profile_ref,''),i.created_by,i.created_at,i.updated_at,
	i.runtime_revision,i.lifecycle,COALESCE(i.deactivated_by::text,''),i.deactivated_at,
	i.protected_git_state,COALESCE(i.protected_git_revision,0),i.protected_git_content_digest,i.protected_git_commit,i.protected_git_observed_at
	FROM external_dns_integrations i`

func externalDNSIntegrations(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, environmentID, integrationID string, runtimeOnly bool) ([]domain.ExternalDNSIntegration, error) {
	query, args := externalDNSSelect, []any{}
	if environmentID != "" {
		query += ` JOIN external_dns_integration_environments x ON x.integration_id=i.id WHERE x.environment_id=$1 AND i.lifecycle='active'`
		args = append(args, environmentID)
	} else if integrationID != "" {
		query += ` WHERE i.id=$1`
		args = append(args, integrationID)
	}
	if runtimeOnly {
		query += ` WHERE i.lifecycle='active' OR i.protected_git_state<>'dematerialized'`
	}
	// API pages expose at most 100 records. Read one sentinel row so callers can
	// report truncation without allowing an unbounded integration scan.
	query += ` ORDER BY i.name,i.id LIMIT 101`
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.ExternalDNSIntegration, 0)
	for rows.Next() {
		var item domain.ExternalDNSIntegration
		var suffixes []byte
		if err = rows.Scan(&item.ID, &item.Slug, &item.Name, &item.Mode, &item.ProviderKind, &item.TXTOwnerID,
			&suffixes, &item.SyncPolicy, &item.DestructiveSyncConfirmed, &item.CredentialSecretRef,
			&item.ProviderConfigRef, &item.EgressConfigRef, &item.OperatorProfileRef, &item.CreatedBy,
			&item.CreatedAt, &item.UpdatedAt, &item.RuntimeRevision, &item.Lifecycle, &item.DeactivatedBy, &item.DeactivatedAt,
			&item.ProtectedGitState, &item.ProtectedGitRevision, &item.ProtectedGitContentDigest, &item.ProtectedGitCommit, &item.ProtectedGitObservedAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(suffixes, &item.AllowedDomainSuffixes); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for index := range items {
		items[index].EnvironmentIDs, err = externalDNSEnvironmentIDs(ctx, q, items[index].ID)
		if err != nil {
			return nil, err
		}
	}
	return items, nil
}

func externalDNSEnvironmentIDs(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, integrationID string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT environment_id::text FROM external_dns_integration_environments WHERE integration_id=$1 ORDER BY environment_id`, integrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var environmentID string
		if err = rows.Scan(&environmentID); err != nil {
			return nil, err
		}
		ids = append(ids, environmentID)
	}
	return ids, rows.Err()
}

func externalDNSEnvironmentsExist(ctx context.Context, tx pgx.Tx, environmentIDs []string) error {
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM environments WHERE id=ANY($1::uuid[])`, environmentIDs).Scan(&count); err != nil {
		return err
	}
	if count != len(environmentIDs) {
		return base.ErrNotFound
	}
	return nil
}

func replaceExternalDNSEnvironments(ctx context.Context, tx pgx.Tx, integrationID string, environmentIDs []string, createdAt time.Time) error {
	if _, err := tx.Exec(ctx, `DELETE FROM external_dns_integration_environments WHERE integration_id=$1`, integrationID); err != nil {
		return err
	}
	for _, environmentID := range environmentIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO external_dns_integration_environments(integration_id,environment_id,created_at) VALUES($1,$2,$3)`, integrationID, environmentID, createdAt); err != nil {
			return err
		}
	}
	return nil
}

func externalDNSAuditDetail(item domain.ExternalDNSIntegration) map[string]any {
	environmentIDs := append([]string(nil), item.EnvironmentIDs...)
	sort.Strings(environmentIDs)
	return map[string]any{
		"slug": item.Slug, "name": item.Name, "mode": item.Mode, "providerKind": item.ProviderKind,
		"txtOwnerId": item.TXTOwnerID, "allowedDomainSuffixes": item.AllowedDomainSuffixes,
		"syncPolicy": item.SyncPolicy, "destructiveSyncConfirmed": item.DestructiveSyncConfirmed,
		"credentialSecretRef": item.CredentialSecretRef, "providerConfigRef": item.ProviderConfigRef,
		"egressConfigRef": item.EgressConfigRef, "operatorProfileRef": item.OperatorProfileRef,
		"environmentIds": environmentIDs,
	}
}
