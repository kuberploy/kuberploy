package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/passwordauth"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestRecoverLegacyLocalAdmin(t *testing.T) {
	url := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	st, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	userID := id.New()
	oldCredential := "legacy display name"
	oldHash, err := passwordauth.Hash("old legacy password 123")
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := passwordauth.Hash("new recovered password 123")
	if err != nil {
		t.Fatal(err)
	}
	sessionHash := sha256.Sum256([]byte("legacy-admin-session-" + userID))
	createdAt := time.Now().UTC()
	if _, err = st.pool.Exec(ctx, `
		INSERT INTO users(id,email,display_name,role,issuer,subject,grant_revision,created_at)
		VALUES($1::uuid,NULL,'Legacy Admin','platform-admin','kuberploy:bootstrap',$1::text,1,$2)`, userID, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `
		INSERT INTO user_password_credentials(user_id,email_normalized,password_hash)
		VALUES($1,$2,$3)`, userID, oldCredential, oldHash); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `
		INSERT INTO sessions(token_hash,user_id,grant_revision,expires_at)
		VALUES($1,$2,1,$3)`, sessionHash[:], userID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		cleanupCtx := context.Background()
		_, _ = st.pool.Exec(cleanupCtx, `DELETE FROM audit_events WHERE target_id=$1`, userID)
		_, _ = st.pool.Exec(cleanupCtx, `DELETE FROM sessions WHERE user_id=$1`, userID)
		_, _ = st.pool.Exec(cleanupCtx, `DELETE FROM user_password_credentials WHERE user_id=$1`, userID)
		_, _ = st.pool.Exec(cleanupCtx, `DELETE FROM users WHERE id=$1`, userID)
	}
	defer cleanup()

	if err = st.RecoverLegacyLocalAdmin(ctx, userID, "recovered@example.test", newHash, "offline-recovery-test"); err != nil {
		t.Fatal(err)
	}
	var email, normalized, persistedHash string
	if err = st.pool.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, userID).Scan(&email); err != nil || email != "recovered@example.test" {
		t.Fatalf("recovered email=%q err=%v", email, err)
	}
	if err = st.pool.QueryRow(ctx, `SELECT email_normalized,password_hash FROM user_password_credentials WHERE user_id=$1`, userID).Scan(&normalized, &persistedHash); err != nil || normalized != email || persistedHash != newHash {
		t.Fatalf("credential normalized=%q hashMatch=%t err=%v", normalized, persistedHash == newHash, err)
	}
	var sessions, auditCount int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id=$1`, userID).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("sessions=%d err=%v", sessions, err)
	}
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE target_id=$1 AND action='auth.admin.email-recovered'`, userID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("audit count=%d err=%v", auditCount, err)
	}
	if err = st.RecoverLegacyLocalAdmin(ctx, userID, "other@example.test", newHash, "offline-recovery-replay"); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("recovery replay err=%v", err)
	}
	retryHash, err := passwordauth.Hash("retry recovered password 123")
	if err != nil {
		t.Fatal(err)
	}
	if err = st.RecoverLegacyLocalAdmin(ctx, userID, "recovered@example.test", retryHash, "offline-recovery-retry"); err != nil {
		t.Fatalf("same-email recovery retry err=%v", err)
	}
	if err = st.pool.QueryRow(ctx, `SELECT password_hash FROM user_password_credentials WHERE user_id=$1`, userID).Scan(&persistedHash); err != nil || persistedHash != retryHash {
		t.Fatalf("retry hashMatch=%t err=%v", persistedHash == retryHash, err)
	}
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE target_id=$1 AND action='auth.admin.email-recovered'`, userID).Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatalf("retry audit count=%d err=%v", auditCount, err)
	}
}
