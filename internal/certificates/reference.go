package certificates

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

// referenceCandidate is the exact locked metadata needed to authorize a
// custom-certificate reference. It is deliberately package-private: callers
// may receive a ResolvedReference only after the continuous observation gate
// has also proved this candidate fresh.
type referenceCandidate struct {
	Binding     secrets.Binding
	Version     secrets.Version
	Certificate Version
	Resolved    ResolvedReference
}

// PostgreSQLReferenceResolver is the sole production authorization boundary
// for custom-certificate AppConfig references. It combines the exact locked
// metadata proof with both continuous per-certificate readiness and the exact
// observer-worker identity in the caller's transaction.
type PostgreSQLReferenceResolver struct {
	Store            *PostgreSQLStore
	Identity         ObservationIdentity
	MaximumAge       time.Duration
	WorkerMaximumAge time.Duration
}

func NewPostgreSQLReferenceResolver(store *PostgreSQLStore, config ObservationConfig) (*PostgreSQLReferenceResolver, error) {
	if store == nil || store.pool == nil || config.Validate() != nil {
		return nil, ErrObservationUnavailable
	}
	identity, err := ObservationIdentityForConfig(config)
	if err != nil {
		return nil, ErrObservationUnavailable
	}
	return &PostgreSQLReferenceResolver{
		Store: store, Identity: identity, MaximumAge: config.MaximumObservationAge,
		WorkerMaximumAge: CertificateObservationHeartbeatMaxAge,
	}, nil
}

// ResolveCertificateReferenceTx implements the projection policy seam. An
// unavailable observation is a safe semantic "not ready" result; PostgreSQL
// or identity-corruption errors remain infrastructure failures so generation
// activation rolls back and retries instead of persisting a false diagnostic.
func (r *PostgreSQLReferenceResolver) ResolveCertificateReferenceTx(
	ctx context.Context,
	tx pgx.Tx,
	expectedScope secrets.Scope,
	ref Reference,
	host string,
	now time.Time,
) (ResolvedReference, error) {
	if r == nil || r.Store == nil || tx == nil || r.Identity.Validate() != nil ||
		r.MaximumAge < 10*time.Second || r.MaximumAge > 30*time.Minute ||
		r.WorkerMaximumAge < 2*time.Second || r.WorkerMaximumAge > 5*time.Minute {
		return ResolvedReference{}, ErrInvalid
	}
	candidate, err := resolveReferenceMetadataTx(ctx, tx, expectedScope, ref, host, now)
	if err != nil {
		return ResolvedReference{}, err
	}
	if err = r.Store.ActiveCertificateReadyTx(ctx, tx, candidate.Binding.ID, candidate.Version.ID, r.Identity, now, r.MaximumAge); err != nil {
		if errors.Is(err, ErrObservationUnavailable) {
			return ResolvedReference{}, ErrNotReady
		}
		return ResolvedReference{}, err
	}
	if err = r.Store.CertificateObservationRuntimeReadyTx(ctx, tx, r.Identity, now, r.WorkerMaximumAge); err != nil {
		if errors.Is(err, ErrObservationUnavailable) {
			return ResolvedReference{}, ErrNotReady
		}
		return ResolvedReference{}, err
	}
	return candidate.Resolved, nil
}

func (c referenceCandidate) validate(expectedScope secrets.Scope, ref Reference, host string, now time.Time) error {
	if expectedScope.Validate() != nil || ref.Validate() != nil || now.IsZero() || c.validateIdentity() != nil {
		return ErrConflict
	}
	if c.Binding.Scope != expectedScope || c.Binding.ID != ref.BindingID || c.Binding.Name != ref.Name ||
		c.Binding.Purpose != secrets.PurposeTLSCertificate || c.Binding.Provider != secrets.ProviderSealedSecrets {
		return ErrNotFound
	}
	if c.Binding.ActiveVersion != ref.Version || c.Version.Number != ref.Version {
		return ErrNotReady
	}
	if now.UTC().Before(c.Certificate.NotBefore) || !now.UTC().Before(c.Certificate.NotAfter) {
		return ErrNotReady
	}
	if !c.Certificate.CoversHost(host) {
		return ErrHostMismatch
	}
	return nil
}

func (c referenceCandidate) validateIdentity() error {
	if validateActiveCertificateTarget(c.Binding, c.Version, c.Certificate) != nil || c.Resolved.Validate() != nil {
		return ErrConflict
	}
	targetName := secrets.TargetSecretName(c.Binding, c.Version.Number)
	if c.Version.Artifact.ObjectName != targetName || c.Version.Artifact.TargetSecretName != targetName ||
		c.Resolved.BindingID != c.Binding.ID || c.Resolved.SecretVersionID != c.Version.ID || c.Resolved.Name != c.Binding.Name ||
		c.Resolved.Version != c.Version.Number || c.Resolved.Namespace != c.Binding.Scope.Namespace || c.Resolved.TargetSecretName != targetName ||
		c.Resolved.LeafFingerprint != c.Certificate.LeafFingerprint || c.Resolved.PublicKeyFingerprint != c.Certificate.PublicKeyFingerprint ||
		!c.Resolved.NotBefore.Equal(c.Certificate.NotBefore) || !c.Resolved.NotAfter.Equal(c.Certificate.NotAfter) {
		return ErrConflict
	}
	return nil
}

// resolveReferenceMetadataTx locks and verifies the complete immutable
// binding/version/provider/X.509 identity. It intentionally does not by itself
// authorize rendering: the public resolver must additionally prove a fresh
// continuous provider observation and exact worker-readiness identity.
func resolveReferenceMetadataTx(
	ctx context.Context,
	tx pgx.Tx,
	expectedScope secrets.Scope,
	ref Reference,
	host string,
	now time.Time,
) (referenceCandidate, error) {
	if tx == nil || expectedScope.Validate() != nil || ref.Validate() != nil || now.IsZero() {
		return referenceCandidate{}, ErrInvalid
	}
	candidate, err := readActiveReferenceCandidateTx(ctx, tx, ref.BindingID, ref.Version)
	if err != nil {
		return referenceCandidate{}, err
	}
	if err = candidate.validate(expectedScope, ref, host, now); err != nil {
		return referenceCandidate{}, err
	}
	return candidate, nil
}

func readActiveReferenceCandidateTx(ctx context.Context, tx pgx.Tx, bindingID string, versionNumber int64) (referenceCandidate, error) {
	if tx == nil || !uuidRE.MatchString(bindingID) || versionNumber <= 0 {
		return referenceCandidate{}, ErrInvalid
	}
	var candidate referenceCandidate
	var bindingProvider, bindingPurpose, bindingState string
	var versionProvider, versionTargetType, versionState, fingerprintKeyID string
	var contentFingerprint, certificateFingerprint []byte
	var objectName, targetName, providerRevision, manifestDigest, sealingFingerprint, ciphertextDigest sql.NullString
	var stagedAt, readinessObservedAt, activatedAt, retainedAt, deleteStarted, deletedAt sql.NullTime
	err := tx.QueryRow(ctx, `SELECT
		b.id::text,COALESCE(b.organization_id::text,''),b.project_id::text,b.environment_id::text,b.application_id::text,
		b.target_namespace,b.name,b.provider,b.purpose,b.state,b.active_version,b.created_by::text,
		b.created_at,b.updated_at,b.delete_started_at,b.deleted_at,
		v.id::text,v.binding_id::text,v.version_number,v.provider,v.target_secret_type,v.state,
		v.fingerprint_key_id,v.content_fingerprint,v.provider_object_name,v.target_secret_name,
		v.provider_revision,v.manifest_digest,v.sealed_key_fingerprint,v.ciphertext_digest,v.failure_code,
		v.staged_at,v.readiness_observed_at,v.activated_at,v.retained_at,v.created_at,v.updated_at,
		c.secret_content_fingerprint,c.leaf_fingerprint,c.public_key_fingerprint,c.dns_names,c.ip_addresses,
		c.not_before,c.not_after,c.created_by::text,c.created_at
		FROM secret_bindings b
		JOIN secret_binding_versions v ON v.binding_id=b.id AND v.version_number=$2
		JOIN tls_certificate_versions c ON c.version_id=v.id AND c.binding_id=b.id AND c.version_number=v.version_number
		WHERE b.id=$1
		FOR SHARE OF b,v,c`, bindingID, versionNumber).Scan(
		&candidate.Binding.ID, &candidate.Binding.Scope.OrganizationID, &candidate.Binding.Scope.ProjectID,
		&candidate.Binding.Scope.EnvironmentID, &candidate.Binding.Scope.ApplicationID, &candidate.Binding.Scope.Namespace,
		&candidate.Binding.Name, &bindingProvider, &bindingPurpose, &bindingState, &candidate.Binding.ActiveVersion,
		&candidate.Binding.CreatedBy, &candidate.Binding.CreatedAt, &candidate.Binding.UpdatedAt, &deleteStarted, &deletedAt,
		&candidate.Version.ID, &candidate.Version.BindingID, &candidate.Version.Number, &versionProvider, &versionTargetType,
		&versionState, &fingerprintKeyID, &contentFingerprint, &objectName, &targetName, &providerRevision, &manifestDigest,
		&sealingFingerprint, &ciphertextDigest, &candidate.Version.FailureCode, &stagedAt, &readinessObservedAt,
		&activatedAt, &retainedAt, &candidate.Version.CreatedAt, &candidate.Version.UpdatedAt,
		&certificateFingerprint, &candidate.Certificate.LeafFingerprint, &candidate.Certificate.PublicKeyFingerprint,
		&candidate.Certificate.DNSNames, &candidate.Certificate.IPAddresses, &candidate.Certificate.NotBefore,
		&candidate.Certificate.NotAfter, &candidate.Certificate.CreatedBy, &candidate.Certificate.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return referenceCandidate{}, ErrNotFound
	}
	if err != nil {
		return referenceCandidate{}, ErrUnavailable
	}
	candidate.Binding.Provider = secrets.ProviderKind(bindingProvider)
	candidate.Binding.Purpose = secrets.BindingPurpose(bindingPurpose)
	candidate.Binding.State = secrets.BindingState(bindingState)
	candidate.Binding.DeleteStarted, candidate.Binding.DeletedAt = nullTime(deleteStarted), nullTime(deletedAt)
	candidate.Version.Provider = secrets.ProviderKind(versionProvider)
	candidate.Version.TargetSecretType = secrets.TargetSecretType(versionTargetType)
	candidate.Version.State = secrets.VersionState(versionState)
	candidate.Version.FingerprintKeyID = fingerprintKeyID
	candidate.Version.StagedAt, candidate.Version.ReadinessObservedAt = nullTime(stagedAt), nullTime(readinessObservedAt)
	candidate.Version.ActivatedAt, candidate.Version.RetainedAt = nullTime(activatedAt), nullTime(retainedAt)
	if len(contentFingerprint) != len(candidate.Version.ContentFingerprint) ||
		len(certificateFingerprint) != len(candidate.Certificate.SecretContentFingerprint) {
		return referenceCandidate{}, ErrConflict
	}
	copy(candidate.Version.ContentFingerprint[:], contentFingerprint)
	copy(candidate.Certificate.SecretContentFingerprint[:], certificateFingerprint)
	if subtle.ConstantTimeCompare(candidate.Version.ContentFingerprint[:], candidate.Certificate.SecretContentFingerprint[:]) != 1 ||
		!objectName.Valid || !targetName.Valid || !providerRevision.Valid || !manifestDigest.Valid ||
		!sealingFingerprint.Valid || !ciphertextDigest.Valid {
		return referenceCandidate{}, ErrConflict
	}
	candidate.Version.Artifact = &secrets.Artifact{
		Provider: secrets.ProviderKind(versionProvider), Namespace: candidate.Binding.Scope.Namespace,
		ObjectName: objectName.String, TargetSecretName: targetName.String, ProviderRevision: providerRevision.String,
		ManifestDigest: manifestDigest.String, SealedKeyFingerprint: sealingFingerprint.String,
		CiphertextDigest: ciphertextDigest.String, TargetSecretType: secrets.TargetSecretType(versionTargetType),
	}
	deliveries, deliveryErr := readCertificateDeliveriesTx(ctx, tx, candidate.Binding.ID, candidate.Version.ID)
	if deliveryErr != nil {
		return referenceCandidate{}, deliveryErr
	}
	candidate.Version.Deliveries = deliveries
	candidate.Certificate.BindingID = candidate.Binding.ID
	candidate.Certificate.SecretVersionID = candidate.Version.ID
	candidate.Certificate.Number = candidate.Version.Number
	candidate.Resolved = ResolvedReference{
		BindingID: candidate.Binding.ID, SecretVersionID: candidate.Version.ID, Name: candidate.Binding.Name,
		Version: candidate.Version.Number, Namespace: candidate.Binding.Scope.Namespace,
		TargetSecretName: secrets.TargetSecretName(candidate.Binding, candidate.Version.Number),
		LeafFingerprint:  candidate.Certificate.LeafFingerprint, PublicKeyFingerprint: candidate.Certificate.PublicKeyFingerprint,
		NotBefore: candidate.Certificate.NotBefore.UTC(), NotAfter: candidate.Certificate.NotAfter.UTC(),
	}
	if err = candidate.validateIdentity(); err != nil {
		return referenceCandidate{}, err
	}
	return candidate, nil
}

func readCertificateDeliveriesTx(ctx context.Context, tx pgx.Tx, bindingID, versionID string) ([]secrets.Delivery, error) {
	rows, err := tx.Query(ctx, `SELECT source_key,kind,COALESCE(environment_name,''),COALESCE(file_path,''),COALESCE(file_mode,0)
		FROM secret_binding_deliveries WHERE binding_id=$1 AND version_id=$2 ORDER BY ordinal`, bindingID, versionID)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	deliveries := []secrets.Delivery{}
	for rows.Next() {
		var delivery secrets.Delivery
		var kind string
		if err = rows.Scan(&delivery.SourceKey, &kind, &delivery.EnvironmentName, &delivery.FilePath, &delivery.FileMode); err != nil {
			return nil, ErrUnavailable
		}
		delivery.Kind = secrets.DeliveryKind(kind)
		deliveries = append(deliveries, delivery)
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	want := certificateDeliveries()
	if len(deliveries) != len(want) {
		return nil, ErrConflict
	}
	for index := range want {
		if deliveries[index] != want[index] {
			return nil, ErrConflict
		}
	}
	return deliveries, nil
}

func nullTime(value sql.NullTime) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time.UTC()
}
