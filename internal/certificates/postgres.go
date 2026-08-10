package certificates

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

type PostgreSQLStore struct{ pool *pgxpool.Pool }

func NewPostgreSQLStore(pool *pgxpool.Pool) (*PostgreSQLStore, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &PostgreSQLStore{pool: pool}, nil
}

func (s *PostgreSQLStore) Record(ctx context.Context, value Version, binding secrets.Binding, secretVersion secrets.Version) (Version, bool, error) {
	if s == nil || s.pool == nil || value.ValidateFor(binding, secretVersion) != nil {
		return Version{}, false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Version{}, false, ErrUnavailable
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if existing, found, readErr := readCertificateVersion(ctx, tx, value.SecretVersionID); found || readErr != nil {
		if readErr != nil {
			return Version{}, false, readErr
		}
		if !sameVersion(existing, value) {
			return Version{}, false, ErrConflict
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return Version{}, false, ErrUnavailable
		}
		return existing, true, nil
	}
	var storedPurpose, storedProvider, storedTargetType, storedVersionProvider, state string
	var storedVersionNumber int64
	var storedFingerprint []byte
	var artifactPresent bool
	err = tx.QueryRow(ctx, "SELECT b.purpose,b.provider,v.version_number,v.provider,v.target_secret_type,v.state,"+
		"v.content_fingerprint,(v.provider_object_name IS NOT NULL) FROM secret_bindings b "+
		"JOIN secret_binding_versions v ON v.binding_id=b.id WHERE b.id=$1 AND v.id=$2 FOR UPDATE OF b,v",
		value.BindingID, value.SecretVersionID).Scan(
		&storedPurpose, &storedProvider, &storedVersionNumber, &storedVersionProvider, &storedTargetType, &state,
		&storedFingerprint, &artifactPresent,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, false, ErrNotFound
	}
	if err != nil {
		return Version{}, false, ErrUnavailable
	}
	if storedPurpose != string(secrets.PurposeTLSCertificate) || storedProvider != string(secrets.ProviderSealedSecrets) ||
		storedVersionProvider != string(secrets.ProviderSealedSecrets) || storedTargetType != string(secrets.TargetSecretTLS) ||
		storedVersionNumber != value.Number || !artifactPresent ||
		(state != string(secrets.VersionAwaitingReadiness) && state != string(secrets.VersionActive) && state != string(secrets.VersionRetained)) ||
		len(storedFingerprint) != len(value.SecretContentFingerprint) ||
		subtle.ConstantTimeCompare(storedFingerprint, value.SecretContentFingerprint[:]) != 1 {
		return Version{}, false, ErrConflict
	}
	dnsNames, _ := json.Marshal(value.DNSNames)
	ipAddresses, _ := json.Marshal(value.IPAddresses)
	_, err = tx.Exec(ctx, "INSERT INTO tls_certificate_versions("+
		"version_id,binding_id,version_number,secret_content_fingerprint,leaf_fingerprint,public_key_fingerprint,"+
		"dns_names,ip_addresses,not_before,not_after,created_by,created_at) "+
		"VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb,$9,$10,$11,$12)",
		value.SecretVersionID, value.BindingID, value.Number, value.SecretContentFingerprint[:],
		value.LeafFingerprint, value.PublicKeyFingerprint, dnsNames, ipAddresses,
		value.NotBefore, value.NotAfter, value.CreatedBy, value.CreatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			existing, readErr := s.Version(ctx, value.SecretVersionID)
			if readErr == nil && sameVersion(existing, value) {
				return existing, true, nil
			}
			if readErr != nil && !errors.Is(readErr, ErrNotFound) {
				return Version{}, false, readErr
			}
			return Version{}, false, ErrConflict
		}
		return Version{}, false, ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return Version{}, false, ErrUnavailable
	}
	return cloneVersion(value), false, nil
}

func (s *PostgreSQLStore) Version(ctx context.Context, secretVersionID string) (Version, error) {
	if s == nil || s.pool == nil || !uuidRE.MatchString(secretVersionID) {
		return Version{}, ErrInvalid
	}
	value, found, err := readCertificateVersion(ctx, s.pool, secretVersionID)
	if err != nil {
		return Version{}, err
	}
	if !found {
		return Version{}, ErrNotFound
	}
	return value, nil
}

func (s *PostgreSQLStore) Versions(ctx context.Context, bindingID string) ([]Version, error) {
	if s == nil || s.pool == nil || !uuidRE.MatchString(bindingID) {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, "SELECT version_id::text FROM tls_certificate_versions WHERE binding_id=$1 ORDER BY version_number", bindingID)
	if err != nil {
		return nil, ErrUnavailable
	}
	ids := []string{}
	for rows.Next() {
		var versionID string
		if err = rows.Scan(&versionID); err != nil {
			rows.Close()
			return nil, ErrUnavailable
		}
		ids = append(ids, versionID)
	}
	if rows.Err() != nil {
		rows.Close()
		return nil, ErrUnavailable
	}
	rows.Close()
	result := make([]Version, 0, len(ids))
	for _, versionID := range ids {
		value, found, readErr := readCertificateVersion(ctx, s.pool, versionID)
		if readErr != nil {
			return nil, readErr
		}
		if !found {
			return nil, ErrConflict
		}
		result = append(result, value)
	}
	return result, nil
}

type certificateQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readCertificateVersion(ctx context.Context, query certificateQueryer, secretVersionID string) (Version, bool, error) {
	var value Version
	var fingerprint, dnsNames, ipAddresses []byte
	err := query.QueryRow(ctx, "SELECT binding_id::text,version_id::text,version_number,secret_content_fingerprint,"+
		"leaf_fingerprint,public_key_fingerprint,dns_names,ip_addresses,not_before,not_after,created_by::text,created_at "+
		"FROM tls_certificate_versions WHERE version_id=$1", secretVersionID).Scan(
		&value.BindingID, &value.SecretVersionID, &value.Number, &fingerprint,
		&value.LeafFingerprint, &value.PublicKeyFingerprint, &dnsNames, &ipAddresses,
		&value.NotBefore, &value.NotAfter, &value.CreatedBy, &value.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, false, nil
	}
	if err != nil {
		return Version{}, false, ErrUnavailable
	}
	if len(fingerprint) != len(value.SecretContentFingerprint) ||
		json.Unmarshal(dnsNames, &value.DNSNames) != nil || json.Unmarshal(ipAddresses, &value.IPAddresses) != nil {
		return Version{}, false, ErrConflict
	}
	copy(value.SecretContentFingerprint[:], fingerprint)
	value.NotBefore, value.NotAfter, value.CreatedAt = value.NotBefore.UTC(), value.NotAfter.UTC(), value.CreatedAt.UTC()
	if value.Validate() != nil {
		return Version{}, false, ErrConflict
	}
	return cloneVersion(value), true, nil
}

func isUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

var _ Store = (*PostgreSQLStore)(nil)
