package builds

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/githubapp"
)

func (s *PostgreSQLStore) PutSetupAuthorization(ctx context.Context, authorization SetupAuthorization) (SetupAuthorization, bool, error) {
	if err := validateSetupAuthorization(authorization); err != nil {
		return SetupAuthorization{}, false, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return SetupAuthorization{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "github-setup-authorization|"+authorization.ActorID+"|"+authorization.IdempotencyKey); err != nil {
		return SetupAuthorization{}, false, err
	}
	command, err := tx.Exec(ctx, `INSERT INTO github_setup_authorizations(actor_id,idempotency_key,request_fingerprint,state_value,expires_at,created_at)
		VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(actor_id,idempotency_key) DO NOTHING`, authorization.ActorID, authorization.IdempotencyKey,
		authorization.RequestFingerprint, authorization.State, authorization.ExpiresAt, authorization.CreatedAt)
	if err != nil {
		return SetupAuthorization{}, false, classifyPostgres(err)
	}
	if command.RowsAffected() == 1 {
		return authorization, false, tx.Commit(ctx)
	}
	var stored SetupAuthorization
	err = tx.QueryRow(ctx, `SELECT actor_id::text,idempotency_key,request_fingerprint,state_value,expires_at,created_at
		FROM github_setup_authorizations WHERE actor_id=$1 AND idempotency_key=$2 FOR UPDATE`, authorization.ActorID, authorization.IdempotencyKey).
		Scan(&stored.ActorID, &stored.IdempotencyKey, &stored.RequestFingerprint, &stored.State, &stored.ExpiresAt, &stored.CreatedAt)
	if err != nil {
		return SetupAuthorization{}, false, classifyPostgres(err)
	}
	if stored.RequestFingerprint != authorization.RequestFingerprint {
		return SetupAuthorization{}, false, ErrConflict
	}
	return stored, true, tx.Commit(ctx)
}

func (s *PostgreSQLStore) GitHubUserBinding(ctx context.Context, actorID string) (githubapp.AccountIdentity, error) {
	if !uuidRE.MatchString(actorID) {
		return githubapp.AccountIdentity{}, ErrInvalid
	}
	var identity githubapp.AccountIdentity
	identity.Type = "User"
	err := s.pool.QueryRow(ctx, `SELECT github_user_id,github_login FROM github_user_bindings WHERE user_id=$1`, actorID).Scan(&identity.ID, &identity.Login)
	return identity, classifyPostgres(err)
}

func (s *PostgreSQLStore) BindGitHubUser(ctx context.Context, actorID string, identity githubapp.AccountIdentity, now time.Time) error {
	if !uuidRE.MatchString(actorID) || identity.ID <= 0 || identity.Type != "User" || !loginRE.MatchString(identity.Login) || now.IsZero() {
		return ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "github-user-actor|"+actorID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "github-user-id|"+strconv.FormatInt(identity.ID, 10)); err != nil {
		return err
	}
	var currentID int64
	var currentLogin string
	err = tx.QueryRow(ctx, `SELECT github_user_id,github_login FROM github_user_bindings WHERE user_id=$1 FOR UPDATE`, actorID).Scan(&currentID, &currentLogin)
	if err == nil && currentID != identity.ID {
		return ErrUnauthorized
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return classifyPostgres(err)
	}
	var otherActor string
	err = tx.QueryRow(ctx, `SELECT user_id::text FROM github_user_bindings WHERE github_user_id=$1 FOR UPDATE`, identity.ID).Scan(&otherActor)
	if err == nil && otherActor != actorID {
		return ErrUnauthorized
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return classifyPostgres(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO github_user_bindings(user_id,github_user_id,github_login,bound_at,updated_at)
		VALUES($1,$2,$3,$4,$4) ON CONFLICT(user_id) DO UPDATE SET github_login=EXCLUDED.github_login,updated_at=EXCLUDED.updated_at
		WHERE github_user_bindings.github_user_id=EXCLUDED.github_user_id`, actorID, identity.ID, identity.Login, now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	return tx.Commit(ctx)
}

func (s *PostgreSQLStore) PutSetupHandoff(ctx context.Context, handoff SetupHandoff) error {
	if err := validateSetupHandoff(handoff); err != nil {
		return err
	}
	installation, err := json.Marshal(handoff.Installation)
	if err != nil {
		return ErrInvalid
	}
	repositories, err := json.Marshal(handoff.Repositories)
	if err != nil {
		return ErrInvalid
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO github_setup_handoffs(digest,actor_id,github_user_id,github_user_login,installation,repositories,expires_at,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, handoff.Digest[:], handoff.ActorID, handoff.GitHubUser.ID, handoff.GitHubUser.Login,
		installation, repositories, handoff.ExpiresAt, handoff.CreatedAt)
	return classifyPostgres(err)
}

func (s *PostgreSQLStore) ConsumeSetupHandoff(ctx context.Context, digest [sha256.Size]byte, actorID, idempotencyKey, fingerprint string, now time.Time) (ConsumedSetupHandoff, bool, error) {
	if !uuidRE.MatchString(actorID) || !setupIdempotencyRE.MatchString(idempotencyKey) || !setupFingerprintRE.MatchString(fingerprint) || now.IsZero() {
		return ConsumedSetupHandoff{}, false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ConsumedSetupHandoff{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var stored ConsumedSetupHandoff
	var installationJSON, repositoriesJSON []byte
	var consumedAt *time.Time
	var storedKey, storedFingerprint *string
	err = tx.QueryRow(ctx, `SELECT actor_id::text,github_user_id,github_user_login,installation,repositories,expires_at,created_at,
		consumed_at,link_idempotency_key,link_request_fingerprint,COALESCE(linked_installation_id::text,'')
		FROM github_setup_handoffs WHERE digest=$1 FOR UPDATE`, digest[:]).Scan(&stored.ActorID, &stored.GitHubUser.ID, &stored.GitHubUser.Login,
		&installationJSON, &repositoriesJSON, &stored.ExpiresAt, &stored.CreatedAt, &consumedAt, &storedKey, &storedFingerprint, &stored.LinkedInstallationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ConsumedSetupHandoff{}, false, ErrUnauthorized
		}
		return ConsumedSetupHandoff{}, false, classifyPostgres(err)
	}
	stored.Digest, stored.GitHubUser.Type = digest, "User"
	if stored.ActorID != actorID || !now.UTC().Before(stored.ExpiresAt) || decodeClosedJSON(installationJSON, &stored.Installation) != nil ||
		decodeClosedJSON(repositoriesJSON, &stored.Repositories) != nil || validateSetupHandoff(stored.SetupHandoff) != nil {
		return ConsumedSetupHandoff{}, false, ErrUnauthorized
	}
	if consumedAt != nil {
		if storedKey == nil || storedFingerprint == nil || *storedKey != idempotencyKey || *storedFingerprint != fingerprint {
			return ConsumedSetupHandoff{}, false, ErrConflict
		}
		stored.Replay = true
		return stored, true, tx.Commit(ctx)
	}
	command, err := tx.Exec(ctx, `UPDATE github_setup_handoffs SET consumed_at=$2,link_idempotency_key=$3,link_request_fingerprint=$4
		WHERE digest=$1 AND consumed_at IS NULL`, digest[:], now.UTC(), idempotencyKey, fingerprint)
	if err != nil {
		return ConsumedSetupHandoff{}, false, classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ConsumedSetupHandoff{}, false, ErrConflict
	}
	return stored, false, tx.Commit(ctx)
}

func (s *PostgreSQLStore) CompleteSetupHandoff(ctx context.Context, digest [sha256.Size]byte, installationID string, now time.Time) error {
	if !uuidRE.MatchString(installationID) || now.IsZero() {
		return ErrInvalid
	}
	command, err := s.pool.Exec(ctx, `UPDATE github_setup_handoffs SET linked_installation_id=$2
		WHERE digest=$1 AND consumed_at IS NOT NULL AND (linked_installation_id IS NULL OR linked_installation_id=$2)`, digest[:], installationID)
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}
