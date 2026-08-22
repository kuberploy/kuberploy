package gitssh

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/testdb"
)

func TestPostgresRepositoryLifecycleKeepsPrivateKeyEncrypted(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	repository, err := NewPostgresRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	encryption, err := NewAESGCMEncryption("integration-v1", bytes.Repeat([]byte{0x62}, AES256KeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repository, encryption)
	if err != nil {
		t.Fatal(err)
	}
	ownerID := id.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM git_ssh_key_revisions WHERE owner_id=$1`, ownerID)
	})

	first, err := service.Create(ctx, CreateRequest{Scope: ScopeApp, OwnerID: ownerID})
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := service.PrivateKey(ctx, ScopeApp, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	defer zero(privateKey)
	var ciphertext []byte
	if err = pool.QueryRow(ctx, `SELECT private_key_ciphertext FROM git_ssh_key_revisions
		WHERE scope='app' AND owner_id=$1 AND revision=1`, ownerID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ciphertext, privateKey) || bytes.Contains(ciphertext, privateKey) {
		t.Fatal("database contains plaintext Git SSH private key")
	}

	second, err := service.Rotate(ctx, ScopeApp, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 || second.Revision != 2 || first.Fingerprint == second.Fingerprint {
		t.Fatalf("revisions: first=%#v second=%#v", first, second)
	}
	items, err := service.List(ctx, ScopeApp, ownerID)
	if err != nil || len(items) != 2 || items[0].Status != StatusRevoked || items[1].Status != StatusActive {
		t.Fatalf("history=%#v err=%v", items, err)
	}
	if _, err = service.Revoke(ctx, ScopeApp, ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err = service.PrivateKey(ctx, ScopeApp, ownerID); err != ErrActiveKeyNotFound {
		t.Fatalf("private key after revoke error = %v", err)
	}
}

func TestPostgresRepositoryMutationIdempotency(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	repository, err := NewPostgresRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	encryption, _ := NewAESGCMEncryption("integration-v1", bytes.Repeat([]byte{0x73}, AES256KeyBytes))
	service, err := NewService(repository, encryption)
	if err != nil {
		t.Fatal(err)
	}
	actorID, ownerID := id.New(), id.New()
	_, err = pool.Exec(ctx, `INSERT INTO users(id,email,display_name,role,issuer,subject,created_at)
		VALUES($1,$2,$3,'platform-admin','git-ssh-test',$4,now())`, actorID, actorID+"@example.test", "git-ssh-"+actorID, actorID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM git_ssh_key_mutation_receipts WHERE actor_id=$1`, actorID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM git_ssh_key_revisions WHERE owner_id=$1`, ownerID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, actorID)
	})
	request := MutationRequest{Operation: OperationCreate, ActorID: actorID, IdempotencyKey: "create-key",
		RequestFingerprint: strings.Repeat("a", 64), Scope: ScopeProject, OwnerID: ownerID}
	first, err := service.Mutate(ctx, request)
	if err != nil || first.Replay {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	replay, err := service.Mutate(ctx, request)
	if err != nil || !replay.Replay || replay.Value.Fingerprint != first.Value.Fingerprint || replay.Value.Revision != 1 {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}
	request.RequestFingerprint = strings.Repeat("b", 64)
	if _, err = service.Mutate(ctx, request); err != ErrIdempotencyConflict {
		t.Fatalf("conflicting replay error=%v", err)
	}
}
