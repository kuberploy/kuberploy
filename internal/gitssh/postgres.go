package gitssh

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresRepository struct {
	pool *pgxpool.Pool
}

func OpenPostgresRepository(ctx context.Context, databaseURL string) (*PostgresRepository, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Git SSH database URL: %w", err)
	}
	config.ConnConfig.RuntimeParams["application_name"] = "kuberploy-api-git-ssh"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open Git SSH database: %w", err)
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping Git SSH database: %w", err)
	}
	return &PostgresRepository{pool: pool}, nil
}

func NewPostgresRepository(pool *pgxpool.Pool) (*PostgresRepository, error) {
	if pool == nil {
		return nil, errors.New("Git SSH PostgreSQL pool is required")
	}
	return &PostgresRepository{pool: pool}, nil
}

func (r *PostgresRepository) Close() {
	if r != nil && r.pool != nil {
		r.pool.Close()
	}
}

func (r *PostgresRepository) create(ctx context.Context, record keyRecord) (KeyMetadata, error) {
	return r.mutate(ctx, record, false)
}

func (r *PostgresRepository) rotate(ctx context.Context, record keyRecord) (KeyMetadata, error) {
	return r.mutate(ctx, record, true)
}

func (r *PostgresRepository) mutate(ctx context.Context, record keyRecord, rotate bool) (KeyMetadata, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return KeyMetadata{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	metadata, err := mutateKeyTx(ctx, tx, record, rotate)
	if err != nil {
		return KeyMetadata{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return KeyMetadata{}, err
	}
	return metadata, nil
}

func mutateKeyTx(ctx context.Context, tx pgx.Tx, record keyRecord, rotate bool) (KeyMetadata, error) {
	lockKey := string(record.metadata.Scope) + ":" + record.metadata.OwnerID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		return KeyMetadata{}, err
	}
	var activeRevision uint64
	err := tx.QueryRow(ctx, `SELECT revision FROM git_ssh_key_revisions
		WHERE scope=$1 AND owner_id=$2 AND status='active' FOR UPDATE`, record.metadata.Scope, record.metadata.OwnerID).Scan(&activeRevision)
	if rotate {
		if errors.Is(err, pgx.ErrNoRows) {
			return KeyMetadata{}, ErrActiveKeyNotFound
		}
		if err != nil {
			return KeyMetadata{}, err
		}
		if _, err = tx.Exec(ctx, `UPDATE git_ssh_key_revisions SET status='revoked',revoked_at=now()
			WHERE scope=$1 AND owner_id=$2 AND revision=$3 AND status='active'`, record.metadata.Scope, record.metadata.OwnerID, activeRevision); err != nil {
			return KeyMetadata{}, err
		}
	} else if err == nil {
		return KeyMetadata{}, ErrActiveKeyExists
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return KeyMetadata{}, err
	}

	if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM git_ssh_key_revisions
		WHERE scope=$1 AND owner_id=$2`, record.metadata.Scope, record.metadata.OwnerID).Scan(&record.metadata.Revision); err != nil {
		return KeyMetadata{}, err
	}
	record.metadata.Status = StatusActive
	_, err = tx.Exec(ctx, `INSERT INTO git_ssh_key_revisions(
		id,scope,owner_id,revision,status,public_key,fingerprint,encryption_key_version,private_key_ciphertext,created_at
	) VALUES(gen_random_uuid(),$1,$2,$3,'active',$4,$5,$6,$7,now())`, record.metadata.Scope, record.metadata.OwnerID,
		record.metadata.Revision, record.metadata.PublicKey, record.metadata.Fingerprint, record.envelope.KeyVersion, record.envelope.Ciphertext)
	if err != nil {
		return KeyMetadata{}, err
	}
	return record.metadata, nil
}

func (r *PostgresRepository) revoke(ctx context.Context, scope Scope, ownerID string) (KeyMetadata, error) {
	if err := validateIdentity(scope, ownerID); err != nil {
		return KeyMetadata{}, err
	}
	return revokeKey(ctx, r.pool, scope, ownerID)
}

func revokeKey(ctx context.Context, query interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, scope Scope, ownerID string) (KeyMetadata, error) {
	var metadata KeyMetadata
	err := query.QueryRow(ctx, `UPDATE git_ssh_key_revisions SET status='revoked',revoked_at=now()
		WHERE id=(SELECT id FROM git_ssh_key_revisions WHERE scope=$1 AND owner_id=$2 AND status='active' FOR UPDATE)
		RETURNING scope,owner_id::text,revision,status,public_key,fingerprint`, scope, ownerID).Scan(
		&metadata.Scope, &metadata.OwnerID, &metadata.Revision, &metadata.Status, &metadata.PublicKey, &metadata.Fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return KeyMetadata{}, ErrActiveKeyNotFound
	}
	return metadata, err
}

func (r *PostgresRepository) mutateIdempotent(ctx context.Context, request MutationRequest, record *keyRecord) (MutationResult, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return MutationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "git-ssh-idempotency:"+request.ActorID+":"+request.IdempotencyKey); err != nil {
		return MutationResult{}, err
	}
	var storedOperation MutationOperation
	var storedFingerprint string
	var storedScope Scope
	var storedOwnerID string
	var storedRevision uint64
	err = tx.QueryRow(ctx, `SELECT operation,request_fingerprint,scope,owner_id::text,key_revision
		FROM git_ssh_key_mutation_receipts WHERE actor_id=$1 AND idempotency_key=$2`, request.ActorID, request.IdempotencyKey).Scan(
		&storedOperation, &storedFingerprint, &storedScope, &storedOwnerID, &storedRevision)
	if err == nil {
		if storedOperation != request.Operation || storedFingerprint != request.RequestFingerprint || storedScope != request.Scope || storedOwnerID != request.OwnerID {
			return MutationResult{}, ErrIdempotencyConflict
		}
		metadata, loadErr := metadataRevision(ctx, tx, storedScope, storedOwnerID, storedRevision)
		if loadErr != nil {
			return MutationResult{}, loadErr
		}
		return MutationResult{Value: metadata, Replay: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return MutationResult{}, err
	}

	var metadata KeyMetadata
	switch request.Operation {
	case OperationCreate:
		if record == nil {
			return MutationResult{}, ErrInvalidEnvelope
		}
		metadata, err = mutateKeyTx(ctx, tx, *record, false)
	case OperationRotate:
		if record == nil {
			return MutationResult{}, ErrInvalidEnvelope
		}
		metadata, err = mutateKeyTx(ctx, tx, *record, true)
	case OperationRevoke:
		metadata, err = revokeKey(ctx, tx, request.Scope, request.OwnerID)
	default:
		err = errors.New("unsupported Git SSH mutation")
	}
	if err != nil {
		return MutationResult{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO git_ssh_key_mutation_receipts(
		actor_id,idempotency_key,operation,request_fingerprint,scope,owner_id,key_revision,created_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,now())`, request.ActorID, request.IdempotencyKey, request.Operation,
		request.RequestFingerprint, request.Scope, request.OwnerID, metadata.Revision)
	if err != nil {
		return MutationResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Value: metadata}, nil
}

func metadataRevision(ctx context.Context, query interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, scope Scope, ownerID string, revision uint64) (KeyMetadata, error) {
	var metadata KeyMetadata
	err := query.QueryRow(ctx, `SELECT scope,owner_id::text,revision,status,public_key,fingerprint
		FROM git_ssh_key_revisions WHERE scope=$1 AND owner_id=$2 AND revision=$3`, scope, ownerID, revision).Scan(
		&metadata.Scope, &metadata.OwnerID, &metadata.Revision, &metadata.Status, &metadata.PublicKey, &metadata.Fingerprint)
	return metadata, err
}

func (r *PostgresRepository) active(ctx context.Context, scope Scope, ownerID string) (keyRecord, error) {
	if err := validateIdentity(scope, ownerID); err != nil {
		return keyRecord{}, err
	}
	var record keyRecord
	err := r.pool.QueryRow(ctx, `SELECT scope,owner_id::text,revision,status,public_key,fingerprint,
		encryption_key_version,private_key_ciphertext FROM git_ssh_key_revisions
		WHERE scope=$1 AND owner_id=$2 AND status='active'`, scope, ownerID).Scan(
		&record.metadata.Scope, &record.metadata.OwnerID, &record.metadata.Revision, &record.metadata.Status,
		&record.metadata.PublicKey, &record.metadata.Fingerprint, &record.envelope.KeyVersion, &record.envelope.Ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return keyRecord{}, ErrActiveKeyNotFound
	}
	return record, err
}

func (r *PostgresRepository) List(ctx context.Context, scope Scope, ownerID string) ([]KeyMetadata, error) {
	if err := validateIdentity(scope, ownerID); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `SELECT scope,owner_id::text,revision,status,public_key,fingerprint
		FROM git_ssh_key_revisions WHERE scope=$1 AND owner_id=$2 ORDER BY revision`, scope, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]KeyMetadata, 0)
	for rows.Next() {
		var item KeyMetadata
		if err = rows.Scan(&item.Scope, &item.OwnerID, &item.Revision, &item.Status, &item.PublicKey, &item.Fingerprint); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
