package secrets

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
)

func TestRuntimeSecretMetadataWakeupIsAtomicAndSurvivesSameHeadObservation(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = migrateSecretTestDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	actorID, organizationID, projectID, environmentID, applicationID := id.New(), id.New(), id.New(), id.New(), id.New()
	suffix := strings.ReplaceAll(projectID, "-", "")[:12]
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id,display_name,role,issuer,subject,created_at) VALUES($1,$2,'platform-admin','secret-wakeup',$2,$3)`, []any{actorID, "wake-" + suffix, now}},
		{`INSERT INTO teams(id,name,slug,created_by,created_at) VALUES($1,'Wake team',$2,$3,$4)`, []any{organizationID, "wake-team-" + suffix, actorID, now}},
		{`INSERT INTO projects(id,name,slug,team_id,created_at) VALUES($1,'Wake project',$2,$3,$4)`, []any{projectID, "wake-project-" + suffix, organizationID, now}},
		{`INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Wake env',$3,$4,$4,$5)`, []any{environmentID, projectID, "wake-env-" + suffix, "wake-" + suffix, now}},
		{`INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'Wake app',$3,$4)`, []any{applicationID, projectID, "wake-app-" + suffix, now}},
	} {
		if _, err = pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	projectionStore, err := gitprojection.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := gitprojection.NewGitHubEnvironmentBinding(id.New(), projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 11, RepositoryID: 22, Owner: "kuberploy", Name: "desired-state"},
		"refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	baseline := now.Add(time.Second)
	binding.TargetHeadRevision, binding.IndexedRevision, binding.ProjectionGeneration = head, head, 1
	binding.TargetHeadObservedAt, binding.IndexedAt, binding.UpdatedAt, binding.State = baseline, baseline, baseline, gitprojection.BindingReady
	if err = projectionStore.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	secretStore, err := NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProviders{}
	service := Service{Store: secretStore, Keys: staticKeys{key: []byte("0123456789abcdef0123456789abcdef")}, SealedSecrets: provider, Now: func() time.Time { return baseline.Add(time.Second) }}
	request := createRequest(t, ProviderSealedSecrets, "wakeup-private-value", "wakeup-create-0001")
	request.ActorID = actorID
	request.Scope = Scope{OrganizationID: organizationID, ProjectID: projectID, EnvironmentID: environmentID,
		ApplicationID: applicationID, Namespace: "wake-" + suffix}
	request.Name, request.RequestID = "wakeup", "wakeup-create"
	created, err := service.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	invalidated, err := projectionStore.Binding(ctx, binding.ID)
	if err != nil || invalidated.State != gitprojection.BindingIndexing || !invalidated.UpdatedAt.After(baseline) || invalidated.TargetHeadRevision != head {
		t.Fatalf("metadata wakeup binding=%#v err=%v", invalidated, err)
	}
	observed, _, err := projectionStore.RecordVerifiedHead(ctx, gitprojection.VerifiedHead{BindingID: binding.ID,
		Repository: binding.Repository, TargetRef: binding.TargetRef, Commit: head, Source: gitprojection.ObservationPoll,
		ProviderRequest: "same-head-after-secret-wakeup", ObservedAt: invalidated.UpdatedAt.Add(time.Second)})
	if err != nil || observed.State != gitprojection.BindingIndexing {
		t.Fatalf("same-head observation erased wakeup: binding=%#v err=%v", observed, err)
	}

	// A caller transaction rollback must also roll back the wakeup.
	if _, err = pool.Exec(ctx, `UPDATE git_repository_bindings SET state='ready',updated_at=$2 WHERE id=$1`, binding.ID, observed.UpdatedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	beforeRollback, err := projectionStore.Binding(ctx, binding.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = invalidateRuntimeSecretProjectionTx(ctx, tx, created.Binding.ID, beforeRollback.UpdatedAt.Add(time.Second)); err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	afterRollback, err := projectionStore.Binding(ctx, binding.ID)
	if err != nil || afterRollback.State != gitprojection.BindingReady || !afterRollback.UpdatedAt.Equal(beforeRollback.UpdatedAt) {
		t.Fatalf("rolled-back wakeup leaked: before=%#v after=%#v err=%v", beforeRollback, afterRollback, err)
	}
}
