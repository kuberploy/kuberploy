package argo_test

import (
	"context"
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

func TestPostgreSQLDesiredStateFencingSaturationAndExactReadiness(t *testing.T) {
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
		pgProjectID            = "b2400000-0000-4000-8000-000000000001"
		pgEnvironmentID        = "b2400000-0000-4000-8000-000000000002"
		pgEnvironmentBindingID = "b2400000-0000-4000-8000-000000000003"
		pgPlatformBindingID    = "b2400000-0000-4000-8000-000000000004"
		pgClusterID            = "b2400000-0000-4000-8000-000000000005"
		pgApplicationID        = "b2400000-0000-4000-8000-000000000006"
		pgDeploymentID         = "b2400000-0000-4000-8000-000000000007"
		pgCommandID            = "b2400000-0000-4000-8000-000000000008"
		pgSupersededCommandID  = "b2400000-0000-4000-8000-000000000009"
		pgSecondApplicationID  = "b2400000-0000-4000-8000-000000000010"
		pgSecondDeploymentID   = "b2400000-0000-4000-8000-000000000011"
	)
	cleanup := func(cleanupContext context.Context) {
		_, _ = pool.Exec(cleanupContext, `DELETE FROM argo_desired_state_runtime_readiness WHERE platform_binding_id=$1`, pgPlatformBindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM argo_desired_state_commands WHERE environment_id=$1`, pgEnvironmentID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_repository_bindings WHERE id IN ($1,$2)`, pgEnvironmentBindingID, pgPlatformBindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM environments WHERE id=$1`, pgEnvironmentID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM projects WHERE id=$1`, pgProjectID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())
	now := time.Now().UTC().Truncate(time.Microsecond)
	project := domain.Project{ID: pgProjectID, Name: "Argo desired state", Slug: "argo-desired-state", CreatedAt: now}
	namespace, argoProject := domain.DeriveEnvironmentDestination(project, "production")
	environment := domain.Environment{ID: pgEnvironmentID, ProjectID: pgProjectID, Name: "Production", Slug: "production", Namespace: namespace, ArgoProject: argoProject, CreatedAt: now}
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,created_at) VALUES($1,$2,$3,$4)`, project.ID, project.Name, project.Slug, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`,
		environment.ID, environment.ProjectID, environment.Name, environment.Slug, environment.Namespace, environment.ArgoProject, now); err != nil {
		t.Fatal(err)
	}
	repository := gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 2401, RepositoryID: 2402, Owner: "kuberploy", Name: "argo-desired-state"}
	environmentBinding, err := gitprojection.NewGitHubEnvironmentBinding(pgEnvironmentBindingID, pgProjectID, pgEnvironmentID, repository, "refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	platformBinding, err := gitprojection.NewGitHubPlatformBinding(pgPlatformBindingID, pgClusterID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 2401, RepositoryID: 2403, Owner: "kuberploy", Name: "platform-desired-state"},
		"refs/heads/platform", now)
	if err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("a", 40)
	environmentBinding.TargetHeadRevision, environmentBinding.TargetHeadObservedAt = revision, now
	environmentBinding.IndexedRevision, environmentBinding.IndexedAt, environmentBinding.ProjectionGeneration = revision, now, 1
	environmentBinding.State, environmentBinding.UpdatedAt = gitprojection.BindingReady, now
	platformBinding.TargetHeadRevision, platformBinding.TargetHeadObservedAt, platformBinding.State, platformBinding.UpdatedAt = revision, now, gitprojection.BindingIndexing, now
	gitStore, err := gitprojection.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = gitStore.PutBinding(ctx, environmentBinding); err != nil {
		t.Fatal(err)
	}
	if err = gitStore.PutBinding(ctx, platformBinding); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO git_projection_generations(
		binding_id,generation,head_revision,parser_version,state,started_at,activated_at
	) VALUES($1,$2,$3,$4,'active',$5,$5)`, environmentBinding.ID, environmentBinding.ProjectionGeneration,
		environmentBinding.IndexedRevision, environmentBinding.ParserVersion, now); err != nil {
		t.Fatal(err)
	}
	runtime := argo.RuntimeLock{ChartRepository: "oci://ghcr.io/kuberploy/charts", ChartName: "kuberploy-runtime", ChartVersion: "1.2.3",
		ChartDigest: "sha256:" + strings.Repeat("b", 64), RendererImage: "ghcr.io/kuberploy/runtime-renderer@sha256:" + strings.Repeat("c", 64)}
	target := argo.DesiredStateTarget{Environment: argo.EnvironmentTarget{Project: project, Environment: environment, Binding: environmentBinding, ArgoNamespace: "argocd", Runtime: runtime}, PlatformBinding: platformBinding}
	command, err := planDesiredStateCommand(t, pgCommandID, target,
		[]domain.Application{{ID: pgApplicationID, ProjectID: pgProjectID}},
		[]domain.Deployment{{ID: pgDeploymentID, ApplicationID: pgApplicationID, EnvironmentID: pgEnvironmentID}}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	store, err := argo.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE git_projection_generations SET state='failed',activated_at=NULL
		WHERE binding_id=$1 AND generation=$2`, environmentBinding.ID, environmentBinding.ProjectionGeneration); err != nil {
		t.Fatal(err)
	}
	if _, createErr := store.CreateDesiredState(ctx, command); createErr == nil {
		t.Fatal("command was created without its exact active projection receipt")
	}
	if _, err = pool.Exec(ctx, `UPDATE git_projection_generations SET state='active',activated_at=$3
		WHERE binding_id=$1 AND generation=$2`, environmentBinding.ID, environmentBinding.ProjectionGeneration, now); err != nil {
		t.Fatal(err)
	}
	tamperedBase := command
	tamperedBase.BaseRevision = strings.Repeat("f", 40)
	if _, createErr := store.CreateDesiredState(ctx, tamperedBase); createErr == nil {
		t.Fatal("PostgreSQL trigger accepted a planned base other than the exact platform binding head")
	}
	prepopulatedReceipt := command
	writeBaseObservedAt := now
	prepopulatedReceipt.WriteBaseRevision, prepopulatedReceipt.WriteBaseObservedAt = command.BaseRevision, &writeBaseObservedAt
	if _, createErr := store.CreateDesiredState(ctx, prepopulatedReceipt); !errors.Is(createErr, argo.ErrInvalid) {
		t.Fatalf("PostgreSQL Create accepted a caller-prepopulated write-base receipt: %v", createErr)
	}
	if created, createErr := store.CreateDesiredState(ctx, command); createErr != nil || !created {
		t.Fatalf("created=%v err=%v", created, createErr)
	}
	if _, err = pool.Exec(ctx, `UPDATE argo_desired_state_commands SET write_base_revision=base_revision,
		write_base_observed_at=created_at WHERE id=$1`, command.ID); err == nil {
		t.Fatal("PostgreSQL trigger allowed an unfenced pending command to prepopulate its write-base receipt")
	}
	identity, err := argo.DesiredStateRuntimeIdentityForConfig(argo.DesiredStateRuntimeConfig{
		Enabled: true, GitHubAppID: 2400, PlatformBindingID: pgPlatformBindingID, ClusterID: pgClusterID,
		ArgoNamespace: "argocd", RootApplicationName: "kuberploy-root", RepositorySecretName: "kuberploy-platform-repository",
		Runtime: runtime, DigestEnforcement: argo.ChartDigestNativeOCI,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimDesiredState(ctx, "argo-pg-worker-owner-a", identity.DesiredStateWorkerIdentity, now, 30*time.Second)
	if err != nil || first.Lease.Epoch != 1 {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := store.ClaimDesiredState(ctx, "argo-pg-worker-owner-b", identity.DesiredStateWorkerIdentity, now.Add(31*time.Second), 30*time.Second)
	if err != nil || second.Lease.Epoch != 2 {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	writeBaseObservedAt = now.Add(31 * time.Second)
	if _, err = store.BindDesiredStateWriteBase(ctx, first.Lease, command.BaseRevision, writeBaseObservedAt, writeBaseObservedAt); !errors.Is(err, argo.ErrLeaseLost) {
		t.Fatalf("stale PostgreSQL worker bound write base: %v", err)
	}
	if _, err = store.BindDesiredStateWriteBase(ctx, second.Lease, command.BaseRevision, writeBaseObservedAt, writeBaseObservedAt); err != nil {
		t.Fatalf("bind PostgreSQL write base: %v", err)
	}
	if _, err = store.BindDesiredStateWriteBase(ctx, second.Lease, strings.Repeat("f", 40), writeBaseObservedAt, writeBaseObservedAt); !errors.Is(err, argo.ErrConflict) {
		t.Fatalf("PostgreSQL write-base receipt was mutable: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE argo_desired_state_commands SET write_base_revision=$2 WHERE id=$1`, command.ID, strings.Repeat("f", 40)); err == nil {
		t.Fatal("PostgreSQL trigger allowed write-base receipt mutation")
	}
	if _, err = store.MarkDesiredStateGitCommitted(ctx, first.Lease, strings.Repeat("d", 40), now.Add(32*time.Second)); !errors.Is(err, argo.ErrLeaseLost) {
		t.Fatalf("stale PostgreSQL worker wrote: %v", err)
	}
	if _, err = store.MarkDesiredStateGitCommitted(ctx, second.Lease, strings.Repeat("d", 40), now.Add(32*time.Second)); err != nil {
		t.Fatal(err)
	}
	lease := second.Lease
	clock := now.Add(32 * time.Second)
	for index := 0; index < 32; index++ {
		clock = clock.Add(time.Second)
		if _, err = store.RetryDesiredState(ctx, lease, argo.DesiredStateRetry{FailureCode: "provider-timeout", NextAttemptAt: clock}, clock); err != nil {
			t.Fatalf("retry %d: %v", index, err)
		}
		work, claimErr := store.ClaimDesiredState(ctx, "argo-pg-worker-owner-b", identity.DesiredStateWorkerIdentity, clock, 30*time.Second)
		if claimErr != nil {
			t.Fatalf("claim %d: %v", index, claimErr)
		}
		lease = work.Lease
	}
	current, err := store.DesiredStateCommand(ctx, command.ID)
	if err != nil || current.ConsecutiveFailures != 30 || current.State != argo.DesiredStateGitCommitted {
		t.Fatalf("saturated=%#v err=%v", current, err)
	}
	clock = clock.Add(time.Second)
	if _, err = store.CompleteDesiredStateVerified(ctx, lease, strings.Repeat("d", 40), clock); err != nil {
		t.Fatalf("saturated committed recovery: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE argo_desired_state_commands SET last_failure_code='tampered',updated_at=updated_at+interval '1 second' WHERE id=$1`, command.ID); err == nil {
		t.Fatal("terminal PostgreSQL command was mutable")
	}

	observation := argo.DesiredStateRuntimeWorkerObservation{WorkerID: "argo-pg-readiness-worker", DesiredStateRuntimeIdentity: identity, StartedAt: now, ObservedAt: now}
	readyLease, err := store.AcquireDesiredStateReadiness(ctx, observation, argo.DesiredStateReadinessLease)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.DesiredStateRuntimeReady(ctx, identity, now.Add(20*time.Second), argo.DesiredStateHeartbeatMaxAge); err != nil {
		t.Fatalf("exact PostgreSQL readiness rejected: %v", err)
	}
	mismatch := identity
	mismatch.RepositorySecretName = "different-repository-secret"
	if err = store.DesiredStateRuntimeReady(ctx, mismatch, now.Add(20*time.Second), argo.DesiredStateHeartbeatMaxAge); !errors.Is(err, argo.ErrDesiredStateNotReady) {
		t.Fatalf("mismatched PostgreSQL readiness accepted: %v", err)
	}
	replacement, err := store.AcquireDesiredStateReadiness(ctx, observation, argo.DesiredStateReadinessLease)
	if err != nil || replacement.Epoch != readyLease.Epoch+1 {
		t.Fatalf("replacement=%#v err=%v", replacement, err)
	}
	if _, err = store.HeartbeatDesiredStateReadiness(ctx, readyLease, now.Add(time.Second), argo.DesiredStateReadinessLease); !errors.Is(err, argo.ErrLeaseLost) {
		t.Fatalf("stale PostgreSQL readiness heartbeat accepted: %v", err)
	}

	verifiedCommand, err := store.DesiredStateCommand(ctx, command.ID)
	if err != nil {
		t.Fatal(err)
	}
	superseded, err := planDesiredStateCommand(t, pgSupersededCommandID, target,
		[]domain.Application{{ID: pgApplicationID, ProjectID: pgProjectID}, {ID: pgSecondApplicationID, ProjectID: pgProjectID}},
		[]domain.Deployment{{ID: pgDeploymentID, ApplicationID: pgApplicationID, EnvironmentID: pgEnvironmentID},
			{ID: pgSecondDeploymentID, ApplicationID: pgSecondApplicationID, EnvironmentID: pgEnvironmentID}},
		&verifiedCommand, clock.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if created, createErr := store.CreateDesiredState(ctx, superseded); createErr != nil || !created {
		t.Fatalf("stale-claim fixture created=%v err=%v", created, createErr)
	}
	advancedAt := clock.Add(2 * time.Second)
	if _, err = pool.Exec(ctx, `UPDATE git_repository_bindings SET state='indexing',target_head_revision=$2,
		target_head_observed_at=$3,updated_at=$3 WHERE id=$1`, environmentBinding.ID, strings.Repeat("9", 40), advancedAt); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ClaimDesiredState(ctx, "argo-pg-worker-owner-c", identity.DesiredStateWorkerIdentity, advancedAt, 30*time.Second); !errors.Is(err, argo.ErrNotFound) {
		t.Fatalf("never-attempted stale projection was claimed: %v", err)
	}
	staleStatus, err := store.LatestDesiredState(ctx, pgProjectID, pgEnvironmentID)
	if err != nil || staleStatus.CommandID != superseded.ID || staleStatus.State != argo.DesiredStateSuperseded || staleStatus.LastFailureCode != "projection-superseded" {
		t.Fatalf("stale projection was not durably retired: %#v err=%v", staleStatus, err)
	}
}
