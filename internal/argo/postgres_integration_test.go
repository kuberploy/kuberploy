package argo_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

func TestPostgreSQLArgoObservationAndRollbackContract(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
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
	const (
		pgProject     = "a2000000-0000-4000-8000-000000000001"
		pgEnvironment = "a2000000-0000-4000-8000-000000000002"
		pgApplication = "a2000000-0000-4000-8000-000000000003"
		pgBinding     = "a2000000-0000-4000-8000-000000000004"
		pgCommand     = "a2000000-0000-4000-8000-000000000005"
		pgOperation   = "a2000000-0000-4000-8000-000000000006"
		pgDeployment  = "a2000000-0000-4000-8000-000000000009"
		pgDeployOp    = "a2000000-0000-4000-8000-000000000010"
		pgBinding2    = "a2000000-0000-4000-8000-000000000011"
	)
	cleanup := func(cleanupContext context.Context) {
		_, _ = pool.Exec(cleanupContext, `DELETE FROM argo_rollback_commands WHERE id=$1`, pgCommand)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM argo_application_observations WHERE application_id=$1`, pgApplication)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM argo_observation_runtime WHERE argo_namespace='argocd'`)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_verified_head_observations WHERE binding_id=$1`, pgBinding)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_repository_bindings WHERE id=$1`, pgBinding2)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_repository_bindings WHERE id=$1`, pgBinding)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM deployments WHERE id=$1`, pgDeployment)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM operations WHERE id=$1`, pgDeployOp)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM applications WHERE id=$1`, pgApplication)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM environments WHERE id=$1`, pgEnvironment)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM projects WHERE id=$1`, pgProject)
	}
	cleanup(ctx)
	defer cleanup(context.Background())
	now := time.Now().UTC().Truncate(time.Microsecond)
	project := domain.Project{ID: pgProject, Name: "Argo integration", Slug: "argo-integration", CreatedAt: now}
	namespace, argoProject := domain.DeriveEnvironmentDestination(project, "argo")
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,created_at) VALUES($1,'Argo integration','argo-integration',$2)`, pgProject, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Argo','argo',$3,$4,$5)`, pgEnvironment, pgProject, namespace, argoProject, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'Argo app','argo-app',$3)`, pgApplication, pgProject, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,created_at,updated_at) VALUES($1,'deployment.git-write','succeeded','deployment',$2,'argo-observation-integration',1,$3,$3)`, pgDeployOp, pgDeployment, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO deployments(id,environment_id,application_id,image,replicas,port,environment,state,operation_id,generation,runtime,created_at,updated_at) VALUES($1,$2,$3,'registry.example/app@sha256:`+strings.Repeat("f", 64)+`',1,8080,'{}','ready',$4,1,'{}',$5,$5)`, pgDeployment, pgEnvironment, pgApplication, pgDeployOp, now); err != nil {
		t.Fatal(err)
	}
	binding, err := gitprojection.NewEnvironmentBinding(pgBinding, pgProject, pgEnvironment, gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 201, RepositoryID: 202, Owner: "kuberploy", Name: "argo-integration"}, "refs/heads/main", "argo-git-writer", now)
	if err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("a", 40)
	binding.TargetHeadRevision, binding.TargetHeadObservedAt, binding.State, binding.UpdatedAt = revision, now, gitprojection.BindingIndexing, now
	gitStore, err := gitprojection.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = gitStore.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	store, err := argo.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := argo.NewPostgreSQLObservationTargetResolver(pool)
	if err != nil {
		t.Fatal(err)
	}
	target, err := resolver.ResolveArgoObservationTarget(ctx, pgDeployment)
	if err != nil || target.DeploymentID != pgDeployment || target.ApplicationID != pgApplication || target.EnvironmentID != pgEnvironment || target.DesiredRevision != revision {
		t.Fatalf("target=%#v err=%v", target, err)
	}
	ambiguous, err := gitprojection.NewEnvironmentBinding(pgBinding2, pgProject, pgEnvironment, gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 201, RepositoryID: 203, Owner: "kuberploy", Name: "argo-integration-other"}, "refs/heads/main", "argo-git-writer", now)
	if err != nil {
		t.Fatal(err)
	}
	ambiguous.TargetHeadRevision, ambiguous.TargetHeadObservedAt, ambiguous.State, ambiguous.UpdatedAt = revision, now, gitprojection.BindingIndexing, now
	if err = gitStore.PutBinding(ctx, ambiguous); !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("second environment Git authority was accepted: %v", err)
	}
	if target, err = resolver.ResolveArgoObservationTarget(ctx, pgDeployment); err != nil || target.DesiredRevision != revision {
		t.Fatalf("authoritative target after rejected ambiguity=%#v err=%v", target, err)
	}
	observation := argo.Observation{DeploymentID: pgDeployment, ApplicationID: pgApplication, ProjectID: pgProject, EnvironmentID: pgEnvironment, ArgoUID: "a2000000-0000-4000-8000-000000000007", ArgoNamespace: "argocd", ArgoName: argo.ApplicationName(pgDeployment), DestinationNamespace: namespace, DesiredRevision: revision, ObservedRevision: revision, Sync: argo.SyncSynced, Health: argo.HealthHealthy, OperationPhase: "succeeded", Resources: []argo.ResourceIdentity{{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: namespace, Name: "kp-a-integration", UID: "a2000000-0000-4000-8000-000000000008", Health: argo.HealthHealthy}}, ObservedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second)}
	if err = store.PutObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	storedObservation, err := store.Observation(ctx, pgDeployment)
	if err != nil || !storedObservation.Reconciled() || storedObservation.ProjectID != pgProject {
		t.Fatalf("observation=%#v err=%v", storedObservation, err)
	}
	work, err := store.ClaimObservation(ctx, "argocd", "integration-observer-a", now.Add(2*time.Second), 30*time.Second)
	if err != nil || work.Lease.Epoch != 1 {
		t.Fatalf("observation work=%#v err=%v", work, err)
	}
	fenced := observation
	fenced.ObservedAt, fenced.UpdatedAt = now.Add(3*time.Second), now.Add(3*time.Second)
	if err = store.PutObservationFenced(ctx, work.Lease, fenced, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimObservation(ctx, "argocd", "integration-observer-b", now.Add(33*time.Second), 30*time.Second)
	if err != nil || second.Lease.Epoch != 2 {
		t.Fatalf("reclaimed observation work=%#v err=%v", second, err)
	}
	if err = store.PutObservationFenced(ctx, work.Lease, fenced, now.Add(34*time.Second)); !errors.Is(err, argo.ErrLeaseLost) {
		t.Fatalf("stale PostgreSQL observer wrote: %v", err)
	}
	if err = store.FinishObservation(ctx, second.Lease, argo.ObservationOutcome{SnapshotVersion: "900", NextPollAt: now.Add(time.Minute)}, now.Add(35*time.Second)); err != nil {
		t.Fatal(err)
	}
	path, err := gitprojection.ApplicationPath(binding, pgApplication)
	if err != nil {
		t.Fatal(err)
	}
	candidate := []byte("kind: AppConfig\n")
	sum := sha256.Sum256(candidate)
	command := argo.RollbackCommand{ID: pgCommand, ApplicationID: pgApplication, ProjectID: pgProject, EnvironmentID: pgEnvironment, BindingID: pgBinding, OperationID: pgOperation, BaseRevision: revision, ExpectedETag: `"sha256:` + strings.Repeat("b", 64) + `"`, ReleaseRepository: "registry.example/argo-app", ReleaseDigest: "sha256:" + strings.Repeat("c", 64), CandidateSHA256: "sha256:" + hex.EncodeToString(sum[:]), State: argo.RollbackPendingGit, CreatedAt: now.Add(2 * time.Second), UpdatedAt: now.Add(2 * time.Second)}
	mutation := gitprojection.Mutation{BindingID: pgBinding, OperationID: pgOperation, Path: path, BaseRevision: revision, ExpectedETag: command.ExpectedETag, Content: candidate, Message: "rollback integration"}
	created, err := store.CreateRollback(ctx, command, mutation)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if err = store.CompleteRollback(ctx, pgCommand, strings.Repeat("d", 40), now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	stored, storedMutation, err := store.Rollback(ctx, pgCommand)
	if err != nil || stored.State != argo.RollbackGitCommitted || stored.ProjectID != pgProject || string(storedMutation.Content) != string(candidate) {
		t.Fatalf("rollback=%#v mutation=%#v err=%v", stored, storedMutation, err)
	}
}
