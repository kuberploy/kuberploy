package postgres

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

// RecoverLegacyLocalAdmin upgrades one pre-017 local administrator whose
// display name was previously used as the password credential identity. The
// caller must provide the exact user ID and a freshly hashed password. A retry
// is allowed only when the same recovered email and credential identity are
// already present. This is intentionally an offline store operation; the HTTP
// API never accepts a display name as a login identifier.
func (s *Store) RecoverLegacyLocalAdmin(ctx context.Context, userID, email, passwordHash, requestID string) error {
	if s == nil || s.pool == nil || strings.TrimSpace(userID) == "" ||
		strings.TrimSpace(email) == "" || strings.TrimSpace(passwordHash) == "" ||
		strings.TrimSpace(requestID) == "" {
		return base.ErrPreconditionFailed
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var currentEmail *string
	var role, issuer, currentCredential string
	err = tx.QueryRow(ctx, `
		SELECT u.email,u.role,u.issuer,c.email_normalized
		FROM users u
		JOIN user_password_credentials c ON c.user_id=u.id
		WHERE u.id=$1
		FOR UPDATE OF u,c`, userID).
		Scan(&currentEmail, &role, &issuer, &currentCredential)
	if err != nil {
		return classify(err)
	}
	if role != "platform-admin" || issuer == "kuberploy:service-account" || strings.TrimSpace(currentCredential) == "" {
		return base.ErrConflict
	}

	if currentEmail != nil {
		if strings.TrimSpace(*currentEmail) != email || normalizeCredential(currentCredential) != email {
			return base.ErrConflict
		}
	} else {
		result, updateErr := tx.Exec(ctx, `UPDATE users SET email=$2 WHERE id=$1 AND email IS NULL`, userID, email)
		if updateErr != nil {
			return classify(updateErr)
		}
		if result.RowsAffected() != 1 {
			return base.ErrConflict
		}
	}
	if _, err = tx.Exec(ctx, `
		UPDATE user_password_credentials
		SET email_normalized=$2,password_hash=$3,updated_at=now()
		WHERE user_id=$1`, userID, email, passwordHash); err != nil {
		return classify(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, userID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO audit_events(id,actor_id,action,target_type,target_id,request_id,detail)
		VALUES($1,$2,'auth.admin.email-recovered','user',$2,$3,'{"method":"offline-recovery"}'::jsonb)`,
		id.New(), userID, requestID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
