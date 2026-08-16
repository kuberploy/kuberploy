package gitprojection_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
)

func TestPostgreSQLGitHubPushWakeIsExactAndLostUpdateSafe(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	userID, projectID, environmentID, bindingID := id.New(), id.New(), id.New(), id.New()
	installationRow, repositoryRow := id.New(), id.New()
	seed := now.UnixNano() & 0x3fffffffffffffff
	appID, installationID, repositoryID, accountID := seed+1, seed+2, seed+3, seed+4
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject,created_at) VALUES($1,$2,'platform-admin',$2,$2,$3)`, userID, "wake-"+strings.ReplaceAll(userID, "-", "")[:8], now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,created_at) VALUES($1,'Wake project',$2,$3)`, projectID, "wake-"+strings.ReplaceAll(projectID, "-", "")[:8], now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Wake','wake',$3,$3,$4)`, environmentID, projectID, "wake-"+strings.ReplaceAll(environmentID, "-", "")[:8], now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO github_installations(id,github_installation_id,account_login,account_type,owner_user_id,visibility,repository_selection,repository_count,github_app_id,github_account_id,lifecycle,permissions,last_verified_at,created_at,updated_at)
		VALUES($1,$2,'kuberploy','Organization',$3,'private','selected',1,$4,$5,'active','{"metadata":"read","contents":"read"}'::jsonb,$6,$6,$6)`, installationRow, installationID, userID, appID, accountID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO github_repositories(id,installation_id,github_repository_id,github_owner_id,owner_login,name,lifecycle,last_verified_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,'kuberploy','wake-repo','active',$5,$5,$5)`, repositoryRow, installationRow, repositoryID, accountID, now); err != nil {
		t.Fatal(err)
	}
	binding, err := gitprojection.NewGitHubEnvironmentBinding(bindingID, projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: installationID, RepositoryID: repositoryID, Owner: "kuberploy", Name: "wake-repo"}, "refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := gitprojection.NewPostgreSQLStore(pool)
	if err = store.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	push := gitprojection.GitHubPushWake{GitHubAppID: appID, InstallationID: installationID, RepositoryID: repositoryID,
		TargetRef: binding.TargetRef, AfterCommit: strings.Repeat("a", 40), DeliveryHash: "sha256:" + strings.Repeat("a", 64), ReceivedAt: now.Add(time.Second)}
	wrong := push
	wrong.GitHubAppID++
	wrong.DeliveryHash = "sha256:" + strings.Repeat("b", 64)
	if result, wakeErr := store.WakeGitHubPush(ctx, wrong); wakeErr != nil || len(result.Bindings) != 0 {
		t.Fatalf("wrong app result=%#v err=%v", result, wakeErr)
	}
	result, err := store.WakeGitHubPush(ctx, push)
	if err != nil || len(result.Bindings) != 1 || result.Bindings[0].BindingID != bindingID || result.Bindings[0].WakeGeneration != 1 {
		t.Fatalf("exact result=%#v err=%v", result, err)
	}
	if replay, wakeErr := store.WakeGitHubPush(ctx, push); wakeErr != nil || !replay.Replay {
		t.Fatalf("replay=%#v err=%v", replay, wakeErr)
	}
	work, err := store.ClaimReconciliation(ctx, "postgres-wake-worker", now.Add(2*time.Second), time.Minute)
	if err != nil || work.Lease.WakeGeneration != 1 {
		t.Fatalf("work=%#v err=%v", work, err)
	}
	head := strings.Repeat("c", 40)
	if _, err = pool.Exec(ctx, `UPDATE git_repository_bindings SET target_head_revision=$2,indexed_revision=$2,target_head_observed_at=$3,indexed_at=$3,projection_generation=1,state='ready',updated_at=$3 WHERE id=$1`, bindingID, head, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	second := push
	second.AfterCommit = strings.Repeat("d", 40)
	second.DeliveryHash = "sha256:" + strings.Repeat("d", 64)
	second.ReceivedAt = now.Add(3500 * time.Millisecond)
	if result, err = store.WakeGitHubPush(ctx, second); err != nil || result.Bindings[0].WakeGeneration != 2 {
		t.Fatalf("second=%#v err=%v", result, err)
	}
	if err = store.FinishReconciliation(ctx, work.Lease, gitprojection.ReconciliationOutcome{LastCommit: head, NextPollAt: now.Add(time.Hour)}, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	var wakeGeneration, reconciled int64
	var next time.Time
	if err = pool.QueryRow(ctx, `SELECT wake_generation,reconciled_wake_generation,next_poll_at FROM git_safety_poll_cursors WHERE binding_id=$1`, bindingID).Scan(&wakeGeneration, &reconciled, &next); err != nil || wakeGeneration != 2 || reconciled != 1 || next.After(now.Add(4*time.Second)) {
		t.Fatalf("lost wake generation=%d reconciled=%d next=%s err=%v", wakeGeneration, reconciled, next, err)
	}
}
