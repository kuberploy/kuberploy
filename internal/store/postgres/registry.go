package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

type registryDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func databaseTime(value time.Time) time.Time { return value.UTC().Truncate(time.Microsecond) }

func (s *Store) PutRegistryTarget(ctx context.Context, target domain.RegistryTarget) (domain.RegistryTarget, error) {
	if err := registry.ValidateTarget(target); err != nil {
		return domain.RegistryTarget{}, err
	}
	now := databaseTime(time.Now())
	if target.CreatedAt.IsZero() {
		target.CreatedAt = now
	} else {
		target.CreatedAt = databaseTime(target.CreatedAt)
	}
	if target.UpdatedAt.IsZero() {
		target.UpdatedAt = target.CreatedAt
	} else {
		target.UpdatedAt = databaseTime(target.UpdatedAt)
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO registry_targets(
			id,name,mode,endpoint,repository_prefix,pull_credential_ref,
			push_credential_ref,cache_credential_ref,created_at,updated_at
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT(id) DO UPDATE SET
			name=EXCLUDED.name,endpoint=EXCLUDED.endpoint,
			repository_prefix=EXCLUDED.repository_prefix,
			pull_credential_ref=EXCLUDED.pull_credential_ref,
			push_credential_ref=EXCLUDED.push_credential_ref,
			cache_credential_ref=EXCLUDED.cache_credential_ref,
			updated_at=EXCLUDED.updated_at
		WHERE registry_targets.mode=EXCLUDED.mode AND
		      (registry_targets.repository_prefix=EXCLUDED.repository_prefix OR NOT EXISTS (
		          SELECT 1 FROM service_registry_policies p WHERE p.registry_target_id=registry_targets.id
		            AND p.repository<>EXCLUDED.repository_prefix
		            AND p.repository NOT LIKE EXCLUDED.repository_prefix || '/%'
		      ))
		RETURNING id,name,mode,endpoint,repository_prefix,pull_credential_ref,
			push_credential_ref,cache_credential_ref,created_at,updated_at`,
		target.ID, target.Name, target.Mode, target.Endpoint, target.RepositoryPrefix,
		target.PullCredentialRef, target.PushCredentialRef, target.CacheCredentialRef,
		target.CreatedAt, target.UpdatedAt,
	).Scan(&target.ID, &target.Name, &target.Mode, &target.Endpoint, &target.RepositoryPrefix,
		&target.PullCredentialRef, &target.PushCredentialRef, &target.CacheCredentialRef,
		&target.CreatedAt, &target.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RegistryTarget{}, base.ErrConflict
	}
	return target, classify(err)
}

func registryTarget(ctx context.Context, q registryDB, targetID string) (domain.RegistryTarget, error) {
	var target domain.RegistryTarget
	err := q.QueryRow(ctx, `SELECT id,name,mode,endpoint,repository_prefix,
		pull_credential_ref,push_credential_ref,cache_credential_ref,created_at,updated_at
		FROM registry_targets WHERE id=$1`, targetID).
		Scan(&target.ID, &target.Name, &target.Mode, &target.Endpoint, &target.RepositoryPrefix,
			&target.PullCredentialRef, &target.PushCredentialRef, &target.CacheCredentialRef,
			&target.CreatedAt, &target.UpdatedAt)
	return target, classify(err)
}

func (s *Store) RegistryTarget(ctx context.Context, targetID string) (domain.RegistryTarget, error) {
	return registryTarget(ctx, s.pool, targetID)
}

func (s *Store) PutServiceRegistryPolicy(ctx context.Context, policy domain.ServiceRegistryPolicy) (domain.ServiceRegistryPolicy, error) {
	policy = registry.NormalizePolicy(policy, time.Now())
	if err := registry.ValidatePolicy(policy); err != nil {
		return domain.ServiceRegistryPolicy{}, err
	}
	policy.CreatedAt = databaseTime(policy.CreatedAt)
	policy.UpdatedAt = databaseTime(policy.UpdatedAt)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.ServiceRegistryPolicy{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	target, err := registryTarget(ctx, tx, policy.RegistryTargetID)
	if err != nil {
		return domain.ServiceRegistryPolicy{}, err
	}
	if err = registry.ValidatePolicyForTarget(target, policy); err != nil {
		return domain.ServiceRegistryPolicy{}, err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO service_registry_policies(
			registry_target_id,service_id,repository,keep_last_successful,
			minimum_safety_age_seconds,
			cache_unused_expiry_seconds,cache_byte_quota,created_at,updated_at
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT(registry_target_id,service_id) DO UPDATE SET
			repository=EXCLUDED.repository,
			keep_last_successful=EXCLUDED.keep_last_successful,
			minimum_safety_age_seconds=EXCLUDED.minimum_safety_age_seconds,
			cache_unused_expiry_seconds=EXCLUDED.cache_unused_expiry_seconds,
			cache_byte_quota=EXCLUDED.cache_byte_quota,
			updated_at=EXCLUDED.updated_at
		RETURNING registry_target_id,service_id,repository,keep_last_successful,
			minimum_safety_age_seconds,
			cache_unused_expiry_seconds,cache_byte_quota,created_at,updated_at`,
		policy.RegistryTargetID, policy.ServiceID, policy.Repository, policy.KeepLastSuccessful,
		int64(policy.MinimumSafetyAge/time.Second),
		int64(policy.CacheUnusedExpiry/time.Second), policy.CacheByteQuota,
		policy.CreatedAt, policy.UpdatedAt,
	).Scan(&policy.RegistryTargetID, &policy.ServiceID, &policy.Repository, &policy.KeepLastSuccessful,
		newDurationScanner(&policy.MinimumSafetyAge),
		newDurationScanner(&policy.CacheUnusedExpiry), &policy.CacheByteQuota,
		&policy.CreatedAt, &policy.UpdatedAt)
	if err != nil {
		return domain.ServiceRegistryPolicy{}, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.ServiceRegistryPolicy{}, classify(err)
	}
	return policy, nil
}

type durationScanner struct{ target *time.Duration }

func newDurationScanner(target *time.Duration) *durationScanner {
	return &durationScanner{target: target}
}

func (s *durationScanner) ScanInt64(value int64, valid bool) error {
	if valid {
		*s.target = time.Duration(value) * time.Second
	}
	return nil
}

// Scan implements pgx.Scanner through the database/sql-compatible scalar
// values pgx exposes for bigint.
func (s *durationScanner) Scan(src any) error {
	value, ok := src.(int64)
	if !ok {
		return errors.New("duration column is not bigint")
	}
	*s.target = time.Duration(value) * time.Second
	return nil
}

func serviceRegistryPolicy(ctx context.Context, q registryDB, targetID, serviceID string) (domain.ServiceRegistryPolicy, error) {
	var policy domain.ServiceRegistryPolicy
	var minimumSafetyAge, cacheUnusedExpiry int64
	err := q.QueryRow(ctx, `SELECT registry_target_id,service_id,repository,
		keep_last_successful,minimum_safety_age_seconds,
		cache_unused_expiry_seconds,cache_byte_quota,created_at,updated_at
		FROM service_registry_policies WHERE registry_target_id=$1 AND service_id=$2`, targetID, serviceID).
		Scan(&policy.RegistryTargetID, &policy.ServiceID, &policy.Repository,
			&policy.KeepLastSuccessful, &minimumSafetyAge,
			&cacheUnusedExpiry, &policy.CacheByteQuota, &policy.CreatedAt, &policy.UpdatedAt)
	policy.MinimumSafetyAge = time.Duration(minimumSafetyAge) * time.Second
	policy.CacheUnusedExpiry = time.Duration(cacheUnusedExpiry) * time.Second
	return policy, classify(err)
}

func (s *Store) ServiceRegistryPolicy(ctx context.Context, targetID, serviceID string) (domain.ServiceRegistryPolicy, error) {
	return serviceRegistryPolicy(ctx, s.pool, targetID, serviceID)
}

func (s *Store) RecordRegistryInventory(ctx context.Context, observation domain.RegistryInventoryObservation) error {
	observation, err := normalizeRegistryInventory(observation)
	if err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = recordRegistryInventoryTx(ctx, tx, observation); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func normalizeRegistryInventory(observation domain.RegistryInventoryObservation) (domain.RegistryInventoryObservation, error) {
	if observation.RegistryTargetID == "" || observation.Revision == "" || observation.ObservedAt.IsZero() {
		return domain.RegistryInventoryObservation{}, base.ErrRegistryObservationIncomplete
	}
	// Canonicalize an empty complete inventory to JSON [] rather than null;
	// PostgreSQL deliberately requires the persisted value to remain an array.
	repositories := append([]string{}, observation.Repositories...)
	sort.Strings(repositories)
	for index, repository := range repositories {
		if repository == "" || repository == "*" || (index > 0 && repositories[index-1] == repository) {
			return domain.RegistryInventoryObservation{}, base.ErrRegistryObservationIncomplete
		}
	}
	observation.Repositories = repositories
	observation.ObservedAt = databaseTime(observation.ObservedAt)
	return observation, nil
}

func recordRegistryInventoryTx(ctx context.Context, tx pgx.Tx, observation domain.RegistryInventoryObservation) error {
	repositories := observation.Repositories
	body, _ := json.Marshal(repositories)
	if err := registryAdvisoryLock(ctx, tx, observation.RegistryTargetID, "inventory"); err != nil {
		return err
	}
	var currentRevision string
	var currentComplete bool
	var currentRepositories []byte
	var currentObserved time.Time
	err := tx.QueryRow(ctx, `SELECT revision,complete,repositories,observed_at
		FROM registry_inventory_observations WHERE registry_target_id=$1 FOR UPDATE`, observation.RegistryTargetID).
		Scan(&currentRevision, &currentComplete, &currentRepositories, &currentObserved)
	if err == nil && currentRevision == observation.Revision {
		var current []string
		_ = json.Unmarshal(currentRepositories, &current)
		if currentComplete == observation.Complete && currentObserved.Equal(observation.ObservedAt) && reflect.DeepEqual(current, repositories) {
			return nil
		}
		return base.ErrConflict
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO registry_inventory_observations(
		registry_target_id,revision,complete,repositories,observed_at
	) VALUES($1,$2,$3,$4,$5)
	ON CONFLICT(registry_target_id) DO UPDATE SET revision=EXCLUDED.revision,
		complete=EXCLUDED.complete,repositories=EXCLUDED.repositories,observed_at=EXCLUDED.observed_at`,
		observation.RegistryTargetID, observation.Revision, observation.Complete, body, observation.ObservedAt)
	if err != nil {
		return classify(err)
	}
	return nil
}

func (s *Store) ReplaceRegistryCatalog(ctx context.Context, snapshot domain.RegistryCatalogSnapshot) error {
	var err error
	snapshot, err = normalizeRegistryCatalog(snapshot)
	if err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = replaceRegistryCatalogTx(ctx, tx, snapshot); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func normalizeRegistryCatalog(snapshot domain.RegistryCatalogSnapshot) (domain.RegistryCatalogSnapshot, error) {
	snapshot.Observation.ObservedAt = databaseTime(snapshot.Observation.ObservedAt)
	snapshot.Observation.SnapshotDigest = base.RegistryCatalogSnapshotDigest(snapshot)
	if err := registry.ValidateCatalog(snapshot); err != nil {
		return domain.RegistryCatalogSnapshot{}, err
	}
	if snapshot.Observation.ID == "" {
		snapshot.Observation.ID = id.New()
	}
	return snapshot, nil
}

func replaceRegistryCatalogTx(ctx context.Context, tx pgx.Tx, snapshot domain.RegistryCatalogSnapshot) error {
	observation := snapshot.Observation
	if err := registryAdvisoryLock(ctx, tx, observation.RegistryTargetID, "catalog", observation.Repository); err != nil {
		return err
	}
	var currentRevision int64
	var currentDigest string
	err := tx.QueryRow(ctx, `SELECT revision,snapshot_digest FROM registry_catalog_observations
		WHERE registry_target_id=$1 AND repository=$2 ORDER BY revision DESC LIMIT 1 FOR UPDATE`,
		observation.RegistryTargetID, observation.Repository).Scan(&currentRevision, &currentDigest)
	if err == nil {
		if observation.Revision < currentRevision {
			return base.ErrConflict
		}
		if observation.Revision == currentRevision {
			if observation.SnapshotDigest == currentDigest {
				return nil
			}
			return base.ErrConflict
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if !observation.Complete {
		if err = insertCatalogObservation(ctx, tx, observation); err != nil {
			return err
		}
		return nil
	}
	if _, err = tx.Exec(ctx, `DELETE FROM registry_manifest_blobs WHERE registry_target_id=$1 AND repository=$2`, observation.RegistryTargetID, observation.Repository); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM registry_manifest_children WHERE registry_target_id=$1 AND repository=$2`, observation.RegistryTargetID, observation.Repository); err != nil {
		return err
	}
	for _, manifest := range snapshot.Manifests {
		firstObserved := databaseTime(manifest.FirstObservedAt)
		if firstObserved.IsZero() {
			firstObserved = observation.ObservedAt
		}
		_, err = tx.Exec(ctx, `INSERT INTO registry_manifests(
			registry_target_id,repository,digest,kind,media_type,size_bytes,
			platform_os,platform_architecture,platform_variant,present,
			first_observed_at,last_observed_at,last_observation_revision,deleted_at
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,true,$10,$11,$12,NULL)
		ON CONFLICT(registry_target_id,repository,digest) DO UPDATE SET
			kind=EXCLUDED.kind,media_type=EXCLUDED.media_type,size_bytes=EXCLUDED.size_bytes,
			platform_os=EXCLUDED.platform_os,
			platform_architecture=EXCLUDED.platform_architecture,
			platform_variant=EXCLUDED.platform_variant,present=true,
			last_observed_at=EXCLUDED.last_observed_at,
			last_observation_revision=EXCLUDED.last_observation_revision,deleted_at=NULL`,
			observation.RegistryTargetID, observation.Repository, manifest.Digest, manifest.Kind,
			manifest.MediaType, manifest.SizeBytes, manifest.PlatformOS,
			manifest.PlatformArchitecture, manifest.PlatformVariant, firstObserved,
			observation.ObservedAt, observation.Revision)
		if err != nil {
			return classify(err)
		}
	}
	for _, blob := range snapshot.Blobs {
		firstObserved := databaseTime(blob.FirstObservedAt)
		if firstObserved.IsZero() {
			firstObserved = observation.ObservedAt
		}
		_, err = tx.Exec(ctx, `INSERT INTO registry_blobs(
			registry_target_id,repository,digest,media_type,size_bytes,present,
			first_observed_at,last_observed_at,last_observation_revision,deleted_at
		) VALUES($1,$2,$3,$4,$5,true,$6,$7,$8,NULL)
		ON CONFLICT(registry_target_id,repository,digest) DO UPDATE SET
			media_type=EXCLUDED.media_type,size_bytes=EXCLUDED.size_bytes,present=true,
			last_observed_at=EXCLUDED.last_observed_at,
			last_observation_revision=EXCLUDED.last_observation_revision,deleted_at=NULL`,
			observation.RegistryTargetID, observation.Repository, blob.Digest, blob.MediaType,
			blob.SizeBytes, firstObserved, observation.ObservedAt, observation.Revision)
		if err != nil {
			return classify(err)
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE registry_manifests SET present=false,deleted_at=$3
		WHERE registry_target_id=$1 AND repository=$2 AND present=true
			AND last_observation_revision<>$4`, observation.RegistryTargetID,
		observation.Repository, observation.ObservedAt, observation.Revision); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE registry_blobs SET present=false,deleted_at=$3
		WHERE registry_target_id=$1 AND repository=$2 AND present=true
			AND last_observation_revision<>$4`, observation.RegistryTargetID,
		observation.Repository, observation.ObservedAt, observation.Revision); err != nil {
		return err
	}
	for _, link := range snapshot.Children {
		if _, err = tx.Exec(ctx, `INSERT INTO registry_manifest_children(
			registry_target_id,repository,parent_digest,child_digest) VALUES($1,$2,$3,$4)`,
			observation.RegistryTargetID, observation.Repository, link.ParentDigest, link.ChildDigest); err != nil {
			return classify(err)
		}
	}
	for _, link := range snapshot.BlobLinks {
		if _, err = tx.Exec(ctx, `INSERT INTO registry_manifest_blobs(
			registry_target_id,repository,manifest_digest,blob_digest) VALUES($1,$2,$3,$4)`,
			observation.RegistryTargetID, observation.Repository, link.ManifestDigest, link.BlobDigest); err != nil {
			return classify(err)
		}
	}
	if err = reconcileRegistryAvailability(ctx, tx, observation); err != nil {
		return err
	}
	if err = insertCatalogObservation(ctx, tx, observation); err != nil {
		return err
	}
	return nil
}

func insertCatalogObservation(ctx context.Context, tx pgx.Tx, observation domain.RegistryCatalogObservation) error {
	_, err := tx.Exec(ctx, `INSERT INTO registry_catalog_observations(
		id,registry_target_id,repository,revision,complete,snapshot_digest,
		observed_at,manifest_count,blob_count
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, observation.ID,
		observation.RegistryTargetID, observation.Repository, observation.Revision,
		observation.Complete, observation.SnapshotDigest, observation.ObservedAt,
		observation.ManifestCount, observation.BlobCount)
	return classify(err)
}

func reconcileRegistryAvailability(ctx context.Context, tx pgx.Tx, observation domain.RegistryCatalogObservation) error {
	_, err := tx.Exec(ctx, `UPDATE registry_releases r SET
		availability=CASE WHEN t.mode='managed' THEN 'expired' ELSE 'missing' END,
		availability_observed_at=$3
	FROM registry_targets t
	WHERE r.registry_target_id=$1 AND r.repository=$2
		AND t.id=r.registry_target_id
		AND NOT EXISTS (SELECT 1 FROM registry_manifests m
			WHERE m.registry_target_id=r.registry_target_id AND m.repository=r.repository
				AND m.digest=r.root_digest AND m.present=true)`, observation.RegistryTargetID,
		observation.Repository, observation.ObservedAt)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE registry_releases r SET availability='present',availability_observed_at=NULL
		WHERE r.registry_target_id=$1 AND r.repository=$2
			AND EXISTS (SELECT 1 FROM registry_manifests m
				WHERE m.registry_target_id=r.registry_target_id AND m.repository=r.repository
					AND m.digest=r.root_digest AND m.present=true)`, observation.RegistryTargetID, observation.Repository)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE registry_cache_generations c SET state='missing'
		WHERE c.registry_target_id=$1 AND c.repository=$2 AND c.state<>'deleted'
			AND NOT EXISTS (SELECT 1 FROM registry_manifests m
				WHERE m.registry_target_id=c.registry_target_id AND m.repository=c.repository
					AND m.digest=c.root_digest AND m.present=true)`, observation.RegistryTargetID, observation.Repository)
	return err
}

func registryAdvisoryLock(ctx context.Context, tx pgx.Tx, parts ...string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, advisoryIdentity(parts...))
	return err
}

func (s *Store) ReplaceRegistryProtectionSnapshot(ctx context.Context, snapshot domain.RegistryProtectionSnapshot) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = replaceRegistryProtectionSnapshotTx(ctx, tx, snapshot); err != nil {
		return err
	}
	return classify(tx.Commit(ctx))
}

func replaceRegistryProtectionSnapshotTx(ctx context.Context, tx pgx.Tx, snapshot domain.RegistryProtectionSnapshot) error {
	snapshot.Observation.ObservedAt = databaseTime(snapshot.Observation.ObservedAt)
	snapshot.Observation.SnapshotDigest = base.RegistryProtectionSnapshotDigest(snapshot)
	observation := snapshot.Observation
	expectedKind := map[domain.RegistryAuthority]domain.RegistryArtifactReferenceKind{
		domain.RegistryAuthorityGitIntent:  domain.RegistryReferenceCurrentGitIntent,
		domain.RegistryAuthorityRuntime:    domain.RegistryReferenceObservedRunning,
		domain.RegistryAuthorityOperations: domain.RegistryReferenceActiveOperation,
	}[observation.Authority]
	if observation.RegistryTargetID == "" || observation.ServiceID == "" || observation.Revision == "" || observation.ObservedAt.IsZero() || expectedKind == "" {
		return base.ErrRegistryObservationIncomplete
	}
	if !observation.Complete && len(snapshot.References) != 0 {
		return base.ErrRegistryObservationIncomplete
	}
	seen := make(map[string]struct{})
	for index := range snapshot.References {
		reference := &snapshot.References[index]
		if reference.RegistryTargetID != observation.RegistryTargetID || reference.ServiceID != observation.ServiceID || reference.Kind != expectedKind || reference.Repository == "" || reference.ReferenceKey == "" {
			return base.ErrRegistryObservationIncomplete
		}
		if _, duplicate := seen[reference.ReferenceKey]; duplicate {
			return base.ErrConflict
		}
		seen[reference.ReferenceKey] = struct{}{}
		if reference.CreatedAt.IsZero() {
			reference.CreatedAt = observation.ObservedAt
		} else {
			reference.CreatedAt = databaseTime(reference.CreatedAt)
		}
		if reference.ObservedAt.IsZero() {
			reference.ObservedAt = observation.ObservedAt
		} else {
			reference.ObservedAt = databaseTime(reference.ObservedAt)
		}
	}
	if err := registryAdvisoryLock(ctx, tx, observation.RegistryTargetID, observation.ServiceID, string(observation.Authority)); err != nil {
		return err
	}
	var currentRevision, currentDigest string
	err := tx.QueryRow(ctx, `SELECT revision,snapshot_digest FROM registry_authority_observations
		WHERE registry_target_id=$1 AND service_id=$2 AND authority=$3 FOR UPDATE`,
		observation.RegistryTargetID, observation.ServiceID, observation.Authority).
		Scan(&currentRevision, &currentDigest)
	if err == nil && currentRevision == observation.Revision {
		if currentDigest == observation.SnapshotDigest {
			return nil
		}
		return base.ErrConflict
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if observation.Complete {
		if _, err = tx.Exec(ctx, `DELETE FROM registry_artifact_references
			WHERE registry_target_id=$1 AND service_id=$2 AND kind=$3`, observation.RegistryTargetID, observation.ServiceID, expectedKind); err != nil {
			return err
		}
		for _, reference := range snapshot.References {
			_, err = tx.Exec(ctx, `INSERT INTO registry_artifact_references(
				registry_target_id,service_id,repository,digest,kind,reference_key,
				source_revision,created_at,observed_at
			) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, reference.RegistryTargetID,
				reference.ServiceID, reference.Repository, reference.Digest, reference.Kind,
				reference.ReferenceKey, reference.SourceRevision, reference.CreatedAt, reference.ObservedAt)
			if err != nil {
				return classify(err)
			}
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO registry_authority_observations(
		registry_target_id,service_id,authority,revision,complete,snapshot_digest,observed_at
	) VALUES($1,$2,$3,$4,$5,$6,$7)
	ON CONFLICT(registry_target_id,service_id,authority) DO UPDATE SET
		revision=EXCLUDED.revision,complete=EXCLUDED.complete,
		snapshot_digest=EXCLUDED.snapshot_digest,observed_at=EXCLUDED.observed_at`,
		observation.RegistryTargetID, observation.ServiceID, observation.Authority,
		observation.Revision, observation.Complete, observation.SnapshotDigest, observation.ObservedAt)
	if err != nil {
		return classify(err)
	}
	return nil
}

func (s *Store) PutRegistryPin(ctx context.Context, reference domain.RegistryArtifactReference) error {
	if reference.Kind != domain.RegistryReferencePin || reference.RegistryTargetID == "" || reference.ServiceID == "" || reference.Repository == "" || reference.ReferenceKey == "" {
		return base.ErrRegistryPolicyInvalid
	}
	if reference.CreatedAt.IsZero() {
		reference.CreatedAt = databaseTime(time.Now())
	} else {
		reference.CreatedAt = databaseTime(reference.CreatedAt)
	}
	if reference.ObservedAt.IsZero() {
		reference.ObservedAt = reference.CreatedAt
	} else {
		reference.ObservedAt = databaseTime(reference.ObservedAt)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO registry_artifact_references(
		registry_target_id,service_id,repository,digest,kind,reference_key,
		source_revision,created_at,observed_at
	) VALUES($1,$2,$3,$4,'pin',$5,$6,$7,$8)
	ON CONFLICT(registry_target_id,service_id,kind,reference_key) DO UPDATE SET
		repository=EXCLUDED.repository,digest=EXCLUDED.digest,
		source_revision=EXCLUDED.source_revision,observed_at=EXCLUDED.observed_at`,
		reference.RegistryTargetID, reference.ServiceID, reference.Repository, reference.Digest,
		reference.ReferenceKey, reference.SourceRevision, reference.CreatedAt, reference.ObservedAt)
	return classify(err)
}

func (s *Store) DeleteRegistryPin(ctx context.Context, targetID, serviceID, referenceKey string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM registry_artifact_references
		WHERE registry_target_id=$1 AND service_id=$2 AND kind='pin' AND reference_key=$3`, targetID, serviceID, referenceKey)
	return err
}

func (s *Store) PutRegistryRelease(ctx context.Context, release domain.RegistryRelease) (domain.RegistryRelease, bool, error) {
	if release.ID == "" || release.RegistryTargetID == "" || release.ServiceID == "" || release.Repository == "" || release.RootDigest == "" {
		return domain.RegistryRelease{}, false, base.ErrRegistryPolicyInvalid
	}
	if release.Availability == "" {
		release.Availability = domain.RegistryArtifactPresent
	}
	if release.Availability != domain.RegistryArtifactPresent || release.AvailabilityObservedAt != nil {
		return domain.RegistryRelease{}, false, base.ErrRegistryPolicyInvalid
	}
	if release.CreatedAt.IsZero() {
		release.CreatedAt = databaseTime(time.Now())
	} else {
		release.CreatedAt = databaseTime(release.CreatedAt)
	}
	if release.SucceededAt != nil {
		value := databaseTime(*release.SucceededAt)
		release.SucceededAt = &value
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO registry_releases(
		id,registry_target_id,service_id,repository,root_digest,created_at,
		succeeded_at,availability,availability_observed_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULL) ON CONFLICT(id) DO NOTHING`,
		release.ID, release.RegistryTargetID, release.ServiceID, release.Repository,
		release.RootDigest, release.CreatedAt, release.SucceededAt, release.Availability)
	if err != nil {
		return domain.RegistryRelease{}, false, classify(err)
	}
	if tag.RowsAffected() == 1 {
		return release, false, nil
	}
	current, err := registryRelease(ctx, s.pool, release.ID)
	if err != nil {
		return domain.RegistryRelease{}, false, err
	}
	if !registryReleasesEqual(current, release) {
		return domain.RegistryRelease{}, false, base.ErrConflict
	}
	return current, true, nil
}

func registryRelease(ctx context.Context, q registryDB, releaseID string) (domain.RegistryRelease, error) {
	var release domain.RegistryRelease
	err := q.QueryRow(ctx, `SELECT id,registry_target_id,service_id,repository,
		root_digest,created_at,succeeded_at,availability,availability_observed_at
		FROM registry_releases WHERE id=$1`, releaseID).
		Scan(&release.ID, &release.RegistryTargetID, &release.ServiceID, &release.Repository,
			&release.RootDigest, &release.CreatedAt, &release.SucceededAt, &release.Availability,
			&release.AvailabilityObservedAt)
	return release, classify(err)
}

func (s *Store) RegistryRelease(ctx context.Context, releaseID string) (domain.RegistryRelease, error) {
	return registryRelease(ctx, s.pool, releaseID)
}

func (s *Store) PutRegistryCacheGeneration(ctx context.Context, generation domain.RegistryCacheGeneration) (domain.RegistryCacheGeneration, bool, error) {
	if generation.ID == "" || generation.RegistryTargetID == "" || generation.ServiceID == "" || generation.Repository == "" || generation.PlatformSet == "" || generation.TrustLane == "" || generation.CacheSchema == "" || generation.BuildDefinitionHash == "" || generation.Generation <= 0 || generation.SizeBytes < 0 {
		return domain.RegistryCacheGeneration{}, false, base.ErrRegistryPolicyInvalid
	}
	if generation.CreatedAt.IsZero() {
		generation.CreatedAt = databaseTime(time.Now())
	} else {
		generation.CreatedAt = databaseTime(generation.CreatedAt)
	}
	if generation.LastUsedAt.IsZero() {
		generation.LastUsedAt = generation.CreatedAt
	} else {
		generation.LastUsedAt = databaseTime(generation.LastUsedAt)
	}
	if generation.CompletedAt != nil {
		value := databaseTime(*generation.CompletedAt)
		generation.CompletedAt = &value
	}
	if generation.State == "" {
		generation.State = "succeeded"
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO registry_cache_generations(
		id,registry_target_id,service_id,repository,platform_set,trust_lane,
		cache_schema,build_definition_hash,generation,root_digest,size_bytes,state,
		active_imports,active_exports,created_at,completed_at,last_used_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
	ON CONFLICT(id) DO NOTHING`, generation.ID, generation.RegistryTargetID,
		generation.ServiceID, generation.Repository, generation.PlatformSet, generation.TrustLane,
		generation.CacheSchema, generation.BuildDefinitionHash, generation.Generation,
		generation.RootDigest, generation.SizeBytes, generation.State, generation.ActiveImports,
		generation.ActiveExports, generation.CreatedAt, generation.CompletedAt, generation.LastUsedAt)
	if err != nil {
		return domain.RegistryCacheGeneration{}, false, classify(err)
	}
	if tag.RowsAffected() == 1 {
		return generation, false, nil
	}
	current, err := registryCacheGeneration(ctx, s.pool, generation.ID)
	if err != nil {
		return domain.RegistryCacheGeneration{}, false, err
	}
	if !registryCachesEqual(current, generation) {
		return domain.RegistryCacheGeneration{}, false, base.ErrConflict
	}
	return current, true, nil
}

func registryCacheGeneration(ctx context.Context, q registryDB, cacheID string) (domain.RegistryCacheGeneration, error) {
	var generation domain.RegistryCacheGeneration
	err := q.QueryRow(ctx, `SELECT id,registry_target_id,service_id,repository,
		platform_set,trust_lane,cache_schema,build_definition_hash,generation,
		root_digest,size_bytes,state,active_imports,active_exports,created_at,
		completed_at,last_used_at FROM registry_cache_generations WHERE id=$1`, cacheID).
		Scan(&generation.ID, &generation.RegistryTargetID, &generation.ServiceID,
			&generation.Repository, &generation.PlatformSet, &generation.TrustLane,
			&generation.CacheSchema, &generation.BuildDefinitionHash, &generation.Generation,
			&generation.RootDigest, &generation.SizeBytes, &generation.State,
			&generation.ActiveImports, &generation.ActiveExports, &generation.CreatedAt,
			&generation.CompletedAt, &generation.LastUsedAt)
	return generation, classify(err)
}

func registryReleasesEqual(left, right domain.RegistryRelease) bool {
	return left.ID == right.ID && left.RegistryTargetID == right.RegistryTargetID &&
		left.ServiceID == right.ServiceID && left.Repository == right.Repository &&
		left.RootDigest == right.RootDigest && left.CreatedAt.Equal(right.CreatedAt) &&
		timePointersEqual(left.SucceededAt, right.SucceededAt) &&
		left.Availability == right.Availability &&
		timePointersEqual(left.AvailabilityObservedAt, right.AvailabilityObservedAt)
}

func registryCachesEqual(left, right domain.RegistryCacheGeneration) bool {
	return left.ID == right.ID && left.RegistryTargetID == right.RegistryTargetID &&
		left.ServiceID == right.ServiceID && left.Repository == right.Repository &&
		left.PlatformSet == right.PlatformSet && left.TrustLane == right.TrustLane &&
		left.CacheSchema == right.CacheSchema && left.BuildDefinitionHash == right.BuildDefinitionHash &&
		left.Generation == right.Generation && left.RootDigest == right.RootDigest &&
		left.SizeBytes == right.SizeBytes && left.State == right.State &&
		left.ActiveImports == right.ActiveImports && left.ActiveExports == right.ActiveExports &&
		left.CreatedAt.Equal(right.CreatedAt) && timePointersEqual(left.CompletedAt, right.CompletedAt) &&
		left.LastUsedAt.Equal(right.LastUsedAt)
}

func timePointersEqual(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func (s *Store) RegistryLifecycleSnapshot(ctx context.Context, targetID, serviceID string, now time.Time) (domain.RegistryLifecycleSnapshot, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.RegistryLifecycleSnapshot{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	snapshot, err := registryLifecycleSnapshot(ctx, tx, targetID, serviceID, now)
	if err != nil {
		return domain.RegistryLifecycleSnapshot{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.RegistryLifecycleSnapshot{}, err
	}
	return snapshot, nil
}

func registryLifecycleSnapshot(ctx context.Context, q registryDB, targetID, serviceID string, now time.Time) (domain.RegistryLifecycleSnapshot, error) {
	target, err := registryTarget(ctx, q, targetID)
	if err != nil {
		return domain.RegistryLifecycleSnapshot{}, err
	}
	policy, err := serviceRegistryPolicy(ctx, q, targetID, serviceID)
	if err != nil {
		return domain.RegistryLifecycleSnapshot{}, err
	}
	snapshot := domain.RegistryLifecycleSnapshot{Target: target, Policy: policy, AsOf: now.UTC()}
	var repositoriesJSON []byte
	err = q.QueryRow(ctx, `SELECT registry_target_id,revision,complete,repositories,observed_at
		FROM registry_inventory_observations WHERE registry_target_id=$1`, targetID).
		Scan(&snapshot.Inventory.RegistryTargetID, &snapshot.Inventory.Revision,
			&snapshot.Inventory.Complete, &repositoriesJSON, &snapshot.Inventory.ObservedAt)
	if err == nil {
		if err = json.Unmarshal(repositoriesJSON, &snapshot.Inventory.Repositories); err != nil {
			return domain.RegistryLifecycleSnapshot{}, err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return domain.RegistryLifecycleSnapshot{}, err
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT ON(repository)
		id,registry_target_id,repository,revision,complete,snapshot_digest,
		observed_at,manifest_count,blob_count
		FROM registry_catalog_observations WHERE registry_target_id=$1
		ORDER BY repository,revision DESC`, targetID)
	if err != nil {
		return domain.RegistryLifecycleSnapshot{}, err
	}
	for rows.Next() {
		var observation domain.RegistryCatalogObservation
		if err = rows.Scan(&observation.ID, &observation.RegistryTargetID, &observation.Repository,
			&observation.Revision, &observation.Complete, &observation.SnapshotDigest,
			&observation.ObservedAt, &observation.ManifestCount, &observation.BlobCount); err != nil {
			rows.Close()
			return domain.RegistryLifecycleSnapshot{}, err
		}
		snapshot.CatalogObservations = append(snapshot.CatalogObservations, observation)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return domain.RegistryLifecycleSnapshot{}, err
	}
	if err = loadRegistryGraph(ctx, q, targetID, &snapshot); err != nil {
		return domain.RegistryLifecycleSnapshot{}, err
	}
	rows, err = q.Query(ctx, `SELECT registry_target_id,service_id,authority,
		revision,complete,snapshot_digest,observed_at FROM registry_authority_observations
		WHERE registry_target_id=$1 AND service_id=$2 ORDER BY authority`, targetID, serviceID)
	if err != nil {
		return domain.RegistryLifecycleSnapshot{}, err
	}
	for rows.Next() {
		var observation domain.RegistryAuthorityObservation
		if err = rows.Scan(&observation.RegistryTargetID, &observation.ServiceID,
			&observation.Authority, &observation.Revision, &observation.Complete,
			&observation.SnapshotDigest, &observation.ObservedAt); err != nil {
			rows.Close()
			return domain.RegistryLifecycleSnapshot{}, err
		}
		snapshot.AuthorityObservations = append(snapshot.AuthorityObservations, observation)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return domain.RegistryLifecycleSnapshot{}, err
	}
	rows, err = q.Query(ctx, `SELECT registry_target_id,service_id,repository,digest,
		kind,reference_key,source_revision,created_at,observed_at
		FROM registry_artifact_references WHERE registry_target_id=$1 AND service_id=$2
		ORDER BY kind,reference_key`, targetID, serviceID)
	if err != nil {
		return domain.RegistryLifecycleSnapshot{}, err
	}
	for rows.Next() {
		var reference domain.RegistryArtifactReference
		if err = rows.Scan(&reference.RegistryTargetID, &reference.ServiceID, &reference.Repository,
			&reference.Digest, &reference.Kind, &reference.ReferenceKey,
			&reference.SourceRevision, &reference.CreatedAt, &reference.ObservedAt); err != nil {
			rows.Close()
			return domain.RegistryLifecycleSnapshot{}, err
		}
		snapshot.References = append(snapshot.References, reference)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return domain.RegistryLifecycleSnapshot{}, err
	}
	rows, err = q.Query(ctx, `SELECT id,registry_target_id,service_id,repository,
		root_digest,created_at,succeeded_at,availability,availability_observed_at
		FROM registry_releases WHERE registry_target_id=$1 AND service_id=$2 ORDER BY id`, targetID, serviceID)
	if err != nil {
		return domain.RegistryLifecycleSnapshot{}, err
	}
	for rows.Next() {
		var release domain.RegistryRelease
		if err = rows.Scan(&release.ID, &release.RegistryTargetID, &release.ServiceID,
			&release.Repository, &release.RootDigest, &release.CreatedAt,
			&release.SucceededAt, &release.Availability, &release.AvailabilityObservedAt); err != nil {
			rows.Close()
			return domain.RegistryLifecycleSnapshot{}, err
		}
		snapshot.Releases = append(snapshot.Releases, release)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return domain.RegistryLifecycleSnapshot{}, err
	}
	rows, err = q.Query(ctx, `SELECT id,registry_target_id,service_id,repository,
		platform_set,trust_lane,cache_schema,build_definition_hash,generation,
		root_digest,size_bytes,state,active_imports,active_exports,created_at,
		completed_at,last_used_at FROM registry_cache_generations
		WHERE registry_target_id=$1 AND service_id=$2 ORDER BY id`, targetID, serviceID)
	if err != nil {
		return domain.RegistryLifecycleSnapshot{}, err
	}
	for rows.Next() {
		var cache domain.RegistryCacheGeneration
		if err = rows.Scan(&cache.ID, &cache.RegistryTargetID, &cache.ServiceID,
			&cache.Repository, &cache.PlatformSet, &cache.TrustLane, &cache.CacheSchema,
			&cache.BuildDefinitionHash, &cache.Generation, &cache.RootDigest,
			&cache.SizeBytes, &cache.State, &cache.ActiveImports, &cache.ActiveExports,
			&cache.CreatedAt, &cache.CompletedAt, &cache.LastUsedAt); err != nil {
			rows.Close()
			return domain.RegistryLifecycleSnapshot{}, err
		}
		snapshot.CacheGenerations = append(snapshot.CacheGenerations, cache)
	}
	rows.Close()
	return snapshot, rows.Err()
}

func loadRegistryGraph(ctx context.Context, q registryDB, targetID string, snapshot *domain.RegistryLifecycleSnapshot) error {
	rows, err := q.Query(ctx, `SELECT registry_target_id,repository,digest,kind,media_type,
		size_bytes,platform_os,platform_architecture,platform_variant,present,
		first_observed_at,last_observed_at,last_observation_revision,deleted_at
		FROM registry_manifests WHERE registry_target_id=$1 AND present=true
		ORDER BY repository,digest`, targetID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var manifest domain.RegistryManifest
		if err = rows.Scan(&manifest.RegistryTargetID, &manifest.Repository, &manifest.Digest,
			&manifest.Kind, &manifest.MediaType, &manifest.SizeBytes, &manifest.PlatformOS,
			&manifest.PlatformArchitecture, &manifest.PlatformVariant, &manifest.Present,
			&manifest.FirstObservedAt, &manifest.LastObservedAt,
			&manifest.LastObservationRevision, &manifest.DeletedAt); err != nil {
			rows.Close()
			return err
		}
		snapshot.Manifests = append(snapshot.Manifests, manifest)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	rows, err = q.Query(ctx, `SELECT registry_target_id,repository,digest,media_type,
		size_bytes,present,first_observed_at,last_observed_at,
		last_observation_revision,deleted_at FROM registry_blobs
		WHERE registry_target_id=$1 AND present=true ORDER BY repository,digest`, targetID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var blob domain.RegistryBlob
		if err = rows.Scan(&blob.RegistryTargetID, &blob.Repository, &blob.Digest,
			&blob.MediaType, &blob.SizeBytes, &blob.Present, &blob.FirstObservedAt,
			&blob.LastObservedAt, &blob.LastObservationRevision, &blob.DeletedAt); err != nil {
			rows.Close()
			return err
		}
		snapshot.Blobs = append(snapshot.Blobs, blob)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	rows, err = q.Query(ctx, `SELECT c.repository,c.parent_digest,c.child_digest
		FROM registry_manifest_children c
		JOIN registry_manifests p ON p.registry_target_id=c.registry_target_id
			AND p.repository=c.repository AND p.digest=c.parent_digest AND p.present=true
		JOIN registry_manifests ch ON ch.registry_target_id=c.registry_target_id
			AND ch.repository=c.repository AND ch.digest=c.child_digest AND ch.present=true
		WHERE c.registry_target_id=$1 ORDER BY c.repository,c.parent_digest,c.child_digest`, targetID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var link domain.RegistryManifestLink
		if err = rows.Scan(&link.Repository, &link.ParentDigest, &link.ChildDigest); err != nil {
			rows.Close()
			return err
		}
		snapshot.Children = append(snapshot.Children, link)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	rows, err = q.Query(ctx, `SELECT l.repository,l.manifest_digest,l.blob_digest
		FROM registry_manifest_blobs l
		JOIN registry_manifests m ON m.registry_target_id=l.registry_target_id
			AND m.repository=l.repository AND m.digest=l.manifest_digest AND m.present=true
		JOIN registry_blobs b ON b.registry_target_id=l.registry_target_id
			AND b.repository=l.repository AND b.digest=l.blob_digest AND b.present=true
		WHERE l.registry_target_id=$1 ORDER BY l.repository,l.manifest_digest,l.blob_digest`, targetID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var link domain.RegistryManifestBlobLink
		if err = rows.Scan(&link.Repository, &link.ManifestDigest, &link.BlobDigest); err != nil {
			rows.Close()
			return err
		}
		snapshot.BlobLinks = append(snapshot.BlobLinks, link)
	}
	rows.Close()
	return rows.Err()
}

var _ base.RegistryStore = (*Store)(nil)
