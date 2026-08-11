package postgres

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestPostgreSQLPlatformGitBindingIsAuthorizedCatalogBoundIdempotentAndConcurrent(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	st, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = testdb.ApplyMigrations(ctx, st.pool); err != nil {
		t.Fatal(err)
	}

	adminID, viewerID, installationID, repositoryID := id.New(), id.New(), id.New(), id.New()
	clusterID, concurrentClusterID, rejectedClusterID := id.New(), id.New(), id.New()
	now := databaseTime(time.Now())
	suffix := adminID[:8]
	cleanup := func(cleanupContext context.Context) {
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM audit_events WHERE actor_id=$1`, adminID)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM mutation_receipts WHERE actor_id IN ($1,$2)`, adminID, viewerID)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM git_repository_bindings WHERE cluster_id IN ($1,$2,$3)`, clusterID, concurrentClusterID, rejectedClusterID)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM github_repositories WHERE id=$1`, repositoryID)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM github_installations WHERE id=$1`, installationID)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM access_grants WHERE subject_user_id IN ($1,$2) OR created_by IN ($1,$2)`, adminID, viewerID)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM users WHERE id IN ($1,$2)`, adminID, viewerID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())

	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id,login,role,issuer,subject,grant_revision,created_at)
			VALUES($1,$2,'platform-admin','platform-binding-test',$2,1,$3),
			      ($4,$5,'developer','platform-binding-test',$5,1,$3)`, []any{adminID, "platform-admin-" + suffix, now, viewerID, "platform-viewer-" + suffix}},
		{`INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at)
			VALUES($1,$2,'platform-admin','platform','platform','bootstrap',$2,$3)`, []any{id.New(), adminID, now}},
		{`INSERT INTO github_installations(
			id,github_installation_id,account_login,account_type,owner_user_id,visibility,team_id,
			repository_selection,repository_count,github_app_id,github_account_id,lifecycle,permissions,
			last_verified_at,created_at,updated_at)
			VALUES($1,184242,'kuberploy','Organization',$2,'private',NULL,'selected',1,177,1888,
			'active','{"metadata":"read","contents":"write"}'::jsonb,$3,$3,$3)`, []any{installationID, adminID, now}},
		{`INSERT INTO github_repositories(
			id,installation_id,github_repository_id,github_owner_id,owner_login,name,lifecycle,last_verified_at,created_at,updated_at)
			VALUES($1,$2,189001,1888,'kuberploy','platform-gitops','active',$3,$3,$3)`, []any{repositoryID, installationID, now}},
	} {
		if _, err = st.pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	input := gitprojection.CreatePlatformBindingInput{ClusterID: clusterID, LinkedInstallationID: installationID,
		LinkedRepositoryID: repositoryID, GitHubAppID: 177,
		Repository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 184242,
			RepositoryID: 189001, Owner: "kuberploy", Name: "platform-gitops"}, TargetRef: "refs/heads/platform"}
	created, err := st.CreatePlatformGitBinding(ctx, adminID, "platform-binding", "platform-binding", "platform-binding-request", input)
	if err != nil || created.Replay || created.Value.Kind != gitprojection.BindingPlatform || created.Value.ClusterID != clusterID ||
		created.Value.Prefix != gitprojection.PlatformPrefix(clusterID) || created.Value.CredentialMode != gitprojection.CredentialGitHubApp ||
		created.Value.CredentialSecretName != "" {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	replay, err := st.CreatePlatformGitBinding(ctx, adminID, "platform-binding", "platform-binding", "platform-binding-replay", input)
	if err != nil || !replay.Replay || replay.Value.ID != created.Value.ID {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}
	if _, err = st.CreatePlatformGitBinding(ctx, adminID, "platform-binding", "changed", "platform-binding-conflict", input); !errors.Is(err, base.ErrIdempotencyConflict) {
		t.Fatalf("changed fingerprint accepted: %v", err)
	}
	if _, err = st.CreatePlatformGitBinding(ctx, viewerID, "platform-binding-viewer", "platform-binding-viewer", "platform-binding-viewer", input); !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("non-platform-admin create error=%v", err)
	}
	if _, err = st.GetPlatformGitBindingForActor(ctx, viewerID, clusterID); !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("non-platform-admin read error=%v", err)
	}
	read, err := st.GetPlatformGitBindingForActor(ctx, adminID, clusterID)
	if err != nil || read.ID != created.Value.ID {
		t.Fatalf("read=%#v err=%v", read, err)
	}
	var auditCount int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events
		WHERE actor_id=$1 AND action='argo-platform-git-binding.create' AND target_type='cluster' AND target_id=$2`, adminID, clusterID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("audit count=%d err=%v", auditCount, err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE git_repository_bindings SET target_ref='refs/heads/attacker' WHERE id=$1`, created.Value.ID); err == nil {
		t.Fatal("platform binding immutable target ref was mutable")
	}

	rejected := input
	rejected.ClusterID = rejectedClusterID
	rejected.GitHubAppID++
	if _, err = st.CreatePlatformGitBinding(ctx, adminID, "platform-binding-app-mismatch", "platform-binding-app-mismatch", "platform-binding-app-mismatch", rejected); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("operator/catalog App ID mismatch error=%v", err)
	}
	rejected.GitHubAppID = input.GitHubAppID
	rejected.LinkedRepositoryID = id.New()
	if _, err = st.CreatePlatformGitBinding(ctx, adminID, "platform-binding-repo-mismatch", "platform-binding-repo-mismatch", "platform-binding-repo-mismatch", rejected); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("unresolved repository catalog error=%v", err)
	}

	concurrent := input
	concurrent.ClusterID = concurrentClusterID
	results := make(chan error, 2)
	var start sync.WaitGroup
	start.Add(1)
	for index, key := range []string{"platform-pg-concurrent-a", "platform-pg-concurrent-b"} {
		go func(index int, key string) {
			start.Wait()
			candidate := concurrent
			if index == 1 {
				candidate.TargetRef = "refs/heads/platform-other"
			}
			_, createErr := st.CreatePlatformGitBinding(ctx, adminID, key, key, key, candidate)
			results <- createErr
		}(index, key)
	}
	start.Done()
	succeeded, conflicted := 0, 0
	for range 2 {
		switch createErr := <-results; {
		case createErr == nil:
			succeeded++
		case errors.Is(createErr, base.ErrConflict):
			conflicted++
		default:
			t.Fatalf("unexpected concurrent result: %v", createErr)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d", succeeded, conflicted)
	}
}
