package gitprojection_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
)

func TestPostgreSQLProjectionContract(t *testing.T) {
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
		pgProject              = "a1000000-0000-4000-8000-000000000001"
		pgEnvironment          = "a1000000-0000-4000-8000-000000000002"
		pgApplication          = "a1000000-0000-4000-8000-000000000003"
		pgBinding              = "a1000000-0000-4000-8000-000000000004"
		pgOperation            = "a1000000-0000-4000-8000-000000000005"
		pgUser                 = "a1000000-0000-4000-8000-000000000006"
		pgInstallation         = "a1000000-0000-4000-8000-000000000007"
		pgRepository           = "a1000000-0000-4000-8000-000000000008"
		pgWriteOperation       = "a1000000-0000-4000-8000-000000000009"
		pgDeployment           = "a1000000-0000-4000-8000-000000000010"
		pgDeleteOperation      = "a1000000-0000-4000-8000-000000000011"
		pgProviderInstallation = int64(9_100_000_000_000_101)
		pgProviderRepository   = int64(9_100_000_000_000_102)
		pgProviderAccount      = int64(9_100_000_000_000_103)
		pgProviderApp          = int64(9_100_000_000_000_104)
	)
	cleanup := func(cleanupContext context.Context) {
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_pull_request_publications WHERE operation_id=$1`, pgDeleteOperation)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_path_reservations WHERE binding_id=$1`, pgBinding)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_write_commands WHERE operation_id IN ($1,$2) AND command_kind='deployment'`, pgWriteOperation, pgDeleteOperation)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM deployments WHERE id=$1`, pgDeployment)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM operations WHERE id IN ($1,$2)`, pgWriteOperation, pgDeleteOperation)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_verified_head_observations WHERE binding_id=$1`, pgBinding)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_repository_bindings WHERE id=$1`, pgBinding)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM github_repositories WHERE id=$1`, pgRepository)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM github_installations WHERE id=$1`, pgInstallation)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM applications WHERE id=$1`, pgApplication)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM environments WHERE id=$1`, pgEnvironment)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM projects WHERE id=$1`, pgProject)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM users WHERE id=$1`, pgUser)
	}
	cleanup(ctx)
	defer cleanup(context.Background())
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,created_at) VALUES($1,'Projection integration','projection-integration',$2) ON CONFLICT(id) DO NOTHING`, pgProject, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Integration','integration','kp-projection-integration','kp-projection-integration',$3) ON CONFLICT(id) DO NOTHING`, pgEnvironment, pgProject, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'Projection app','projection-app',$3) ON CONFLICT(id) DO NOTHING`, pgApplication, pgProject, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject,grant_revision,created_at) VALUES($1,'Projection integration','platform-admin','projection-integration','projection-integration',1,$2)`, pgUser, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO github_installations(id,github_installation_id,account_login,account_type,owner_user_id,visibility,repository_selection,repository_count,github_app_id,github_account_id,lifecycle,permissions,last_verified_at,created_at,updated_at)
		VALUES($1,$2,'kuberploy','Organization',$3,'private','selected',1,$4,$5,'active','{"metadata":"read","contents":"read"}'::jsonb,$6,$6,$6)`, pgInstallation, pgProviderInstallation, pgUser, pgProviderApp, pgProviderAccount, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO github_repositories(id,installation_id,github_repository_id,github_owner_id,owner_login,name,lifecycle,last_verified_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,'kuberploy','projection-integration','active',$5,$5,$5)`, pgRepository, pgInstallation, pgProviderRepository, pgProviderAccount, now); err != nil {
		t.Fatal(err)
	}
	binding, err := gitprojection.NewGitHubEnvironmentBinding(pgBinding, pgProject, pgEnvironment, gitprojection.RepositoryIdentity{Provider: "github", InstallationID: pgProviderInstallation, RepositoryID: pgProviderRepository, Owner: "kuberploy", Name: "projection-integration"}, "refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	store, err := gitprojection.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	authorization, err := store.GitHubAuthorization(ctx, binding, pgProviderApp)
	if err != nil || authorization.Account.ID != pgProviderAccount || authorization.Repository.ID != pgProviderRepository || authorization.Repository.OwnerID != pgProviderAccount {
		t.Fatalf("authorization=%#v err=%v", authorization, err)
	}
	if _, err = store.GitHubAuthorization(ctx, binding, pgProviderApp+1); !errors.Is(err, gitprojection.ErrNotFound) {
		t.Fatalf("wrong GitHub App authorized: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE github_repositories SET lifecycle='removed',removed_at=$2,updated_at=$2 WHERE id=$1`, pgRepository, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.GitHubAuthorization(ctx, binding, pgProviderApp); !errors.Is(err, gitprojection.ErrNotFound) {
		t.Fatalf("removed repository authorized: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE github_repositories SET lifecycle='active',removed_at=NULL,updated_at=$2 WHERE id=$1`, pgRepository, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	work, err := store.ClaimReconciliation(ctx, "postgres-projection-worker", now, 2*time.Minute)
	if err != nil || !work.BindingChanged {
		t.Fatalf("work=%#v err=%v", work, err)
	}
	head := strings.Repeat("a", 40)
	binding, replay, err := store.RecordVerifiedHead(ctx, verified(binding, head, "postgres-provider-1", now.Add(time.Second)))
	if err != nil || replay {
		t.Fatalf("record head replay=%v err=%v", replay, err)
	}
	if _, replay, err = store.RecordVerifiedHead(ctx, verified(binding, head, "postgres-provider-1", now.Add(time.Second))); err != nil || !replay {
		t.Fatalf("head replay=%v err=%v", replay, err)
	}
	generation, err := store.BeginGeneration(ctx, work.Lease, head, binding.ParserVersion, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	document, err := gitprojection.NewDocument(binding, generation.Number, pgApplication, head, strings.Repeat("9", 40), strings.Repeat("b", 40), []byte("kind: AppConfig\n"), map[string]any{"nested": map[string]any{"value": "stable"}}, nil, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutDocuments(ctx, generation, []gitprojection.Document{document}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateGeneration(ctx, work.Lease, generation, gitprojection.SchemaOnlyAppConfigPolicyValidator{}, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	effectiveDocument, err := store.Document(ctx, binding.ID, document.Path)
	if err != nil || effectiveDocument.ConfigRevision != head {
		t.Fatalf("PostgreSQL activation did not persist the effective dependency-bundle revision: %#v err=%v", effectiveDocument, err)
	}
	if err = store.FinishReconciliation(ctx, work.Lease, gitprojection.ReconciliationOutcome{LastCommit: head, NextPollAt: now.Add(time.Hour)}, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	bundle, err := store.Bundle(ctx, binding.ID, document.Path, nil, "sha256:"+strings.Repeat("c", 64), "policy-v1")
	if err != nil || bundle.Stale || bundle.ETag == "" {
		t.Fatalf("bundle=%#v err=%v", bundle, err)
	}
	if _, _, err = store.RecordVerifiedHead(ctx, verified(binding, head, "postgres-provider-2", now.Add(5*time.Second))); err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimReconciliation(ctx, work.Lease.Owner, now.Add(6*time.Second), 2*time.Minute)
	if err != nil || second.Lease.Epoch != work.Lease.Epoch+1 || second.Reclaimed || !second.BindingChanged {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	staleOutcome := gitprojection.ReconciliationOutcome{LastCommit: head, ConsecutiveFailure: 1, NextPollAt: now.Add(time.Hour), FailureCode: "stale-worker"}
	if _, err = store.HeartbeatReconciliation(ctx, work.Lease, now.Add(7*time.Second), 2*time.Minute); !errors.Is(err, gitprojection.ErrLeaseLost) {
		t.Fatalf("stale heartbeat=%v", err)
	}
	if err = store.FinishReconciliation(ctx, work.Lease, staleOutcome, now.Add(7*time.Second)); !errors.Is(err, gitprojection.ErrLeaseLost) {
		t.Fatalf("stale finish=%v", err)
	}
	if _, err = store.ActivateGeneration(ctx, work.Lease, generation, gitprojection.SchemaOnlyAppConfigPolicyValidator{}, now.Add(7*time.Second)); !errors.Is(err, gitprojection.ErrLeaseLost) {
		t.Fatalf("stale activation=%v", err)
	}
	if err = store.FailGeneration(ctx, work.Lease, generation, now.Add(7*time.Second)); !errors.Is(err, gitprojection.ErrLeaseLost) {
		t.Fatalf("stale failure=%v", err)
	}
	second.Lease, err = store.HeartbeatReconciliation(ctx, second.Lease, now.Add(7*time.Second), 2*time.Minute)
	if err != nil || !second.Lease.Until.Equal(now.Add(127*time.Second)) {
		t.Fatalf("second heartbeat=%#v err=%v", second.Lease, err)
	}
	reclaimedAt := second.Lease.Until
	third, err := store.ClaimReconciliation(ctx, second.Lease.Owner, reclaimedAt, 2*time.Minute)
	if err != nil || !third.Reclaimed || third.Lease.Epoch != second.Lease.Epoch+1 {
		t.Fatalf("third=%#v err=%v", third, err)
	}
	if err = store.FinishReconciliation(ctx, second.Lease, staleOutcome, reclaimedAt); !errors.Is(err, gitprojection.ErrLeaseLost) {
		t.Fatalf("expired second lease finalized: %v", err)
	}
	if err = store.ReleaseReconciliation(ctx, third.Lease, reclaimedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	leaseNow := reclaimedAt.Add(2 * time.Second)
	lease := 30 * time.Second
	leaseUntil := leaseNow.Add(lease)
	reservation := gitprojection.PathReservation{BindingID: binding.ID, TargetRef: binding.TargetRef, Path: document.Path, OperationID: pgOperation, Owner: "postgres-worker", BaseRevision: head, State: gitprojection.ReservationCandidate, LeaseUntil: &leaseUntil, CreatedAt: leaseNow, UpdatedAt: leaseNow}
	if _, replay, err = store.AcquirePath(ctx, reservation, leaseNow, lease); err != nil || replay {
		t.Fatalf("reserve replay=%v err=%v", replay, err)
	}
	if _, replay, err = store.AcquirePath(ctx, reservation, leaseNow, lease); err != nil || !replay {
		t.Fatalf("reserve replay=%v err=%v", replay, err)
	}
	if _, err = store.FinalizePath(ctx, binding.ID, binding.TargetRef, document.Path, pgOperation, strings.Repeat("d", 40), leaseNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	// A successful operation commit can be followed by another normal
	// fast-forward before the projection worker activates a generation. The
	// later exact generation must converge the linked command and release its
	// path fence rather than waiting forever for equality with the earlier OID.
	if _, err = pool.Exec(ctx, `DELETE FROM git_path_reservations WHERE binding_id=$1`, pgBinding); err != nil {
		t.Fatal(err)
	}
	testStart := leaseNow.Add(3 * time.Second)
	if _, err = pool.Exec(ctx, `INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,created_at,updated_at)
		VALUES($1,'deployment.git-write','queued','deployment',$2,'postgres-descendant-convergence',1,$3,$3)`, pgWriteOperation, pgDeployment, testStart); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO deployments(id,environment_id,application_id,image,replicas,port,environment,runtime,state,operation_id,created_at,updated_at)
		VALUES($1,$2,$3,$4,1,8080,'{}'::jsonb,'{}'::jsonb,'pending-git',$5,$6,$6)`, pgDeployment, pgEnvironment, pgApplication,
		"registry.example/projection@sha256:"+strings.Repeat("e", 64), pgWriteOperation, testStart); err != nil {
		t.Fatal(err)
	}
	current, err := store.Binding(ctx, pgBinding)
	if err != nil || current.State != gitprojection.BindingReady || current.TargetHeadRevision != head || current.IndexedRevision != head {
		t.Fatalf("current=%#v err=%v", current, err)
	}
	plan := gitprojection.WritePlan{BindingID: current.ID, ProjectID: current.ProjectID, EnvironmentID: current.EnvironmentID,
		ApplicationID: pgApplication, BaseRevision: head, Precondition: gitprojection.MutationMatchETag,
		ExpectedETag: `"sha256:` + strings.Repeat("f", 64) + `"`, ChartDigest: "sha256:" + strings.Repeat("a", 64), PolicyVersion: "runtime-policy-v1"}
	command, err := gitprojection.NewWriteCommand(pgWriteOperation, pgDeployment, pgUser, plan, current, []byte("kind: AppConfig\n"), "update app config", testStart)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutWriteCommand(ctx, command); err != nil {
		t.Fatal(err)
	}
	writeLease := 30 * time.Second
	writeLeaseStart := testStart.Add(time.Second)
	writeLeaseUntil := writeLeaseStart.Add(writeLease)
	writeReservation := gitprojection.PathReservation{BindingID: current.ID, TargetRef: current.TargetRef, Path: command.Path,
		OperationID: command.OperationID, Owner: "postgres-projection-writer", BaseRevision: head, State: gitprojection.ReservationCandidate,
		LeaseUntil: &writeLeaseUntil, CreatedAt: writeLeaseStart, UpdatedAt: writeLeaseStart}
	if _, replay, err = store.AcquirePath(ctx, writeReservation, writeLeaseStart, writeLease); err != nil || replay {
		t.Fatalf("write reservation replay=%v err=%v", replay, err)
	}
	operationCommit := strings.Repeat("b", 40)
	operationHead := verified(current, operationCommit, "postgres-operation-commit", testStart.Add(2*time.Second))
	operationHead.Source = gitprojection.ObservationWrite
	if _, err = store.FinalizeVerifiedPath(ctx, current.ID, current.TargetRef, command.Path, command.OperationID, operationCommit, operationHead, testStart.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	laterHead := strings.Repeat("c", 40)
	current, _, err = store.RecordVerifiedHead(ctx, verified(current, laterHead, "postgres-later-fast-forward", testStart.Add(4*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	convergence, err := store.ClaimReconciliation(ctx, "postgres-descendant-indexer", testStart.Add(5*time.Second), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	convergedGeneration, err := store.BeginGeneration(ctx, convergence.Lease, laterHead, current.ParserVersion, testStart.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	convergedDocument, err := gitprojection.NewDocument(current, convergedGeneration.Number, pgApplication, laterHead,
		operationCommit, effectiveDocument.BlobID, command.Content, effectiveDocument.Parsed, nil, testStart.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutDocuments(ctx, convergedGeneration, []gitprojection.Document{convergedDocument}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateGeneration(ctx, convergence.Lease, convergedGeneration, gitprojection.SchemaOnlyAppConfigPolicyValidator{}, testStart.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.PathReservation(ctx, current.ID, current.TargetRef, command.Path); !errors.Is(err, gitprojection.ErrNotFound) {
		t.Fatalf("descendant activation left PostgreSQL reservation: %v", err)
	}
	storedCommand, err := store.WriteCommand(ctx, command.OperationID)
	if err != nil || storedCommand.State != gitprojection.WriteCommandIndexed || storedCommand.CommittedRevision != operationCommit ||
		storedCommand.IndexedGeneration != convergedGeneration.Number {
		t.Fatalf("stored command=%#v err=%v", storedCommand, err)
	}
	var deploymentState, desiredRevision string
	if err = pool.QueryRow(ctx, `SELECT state,desired_revision FROM deployments WHERE id=$1`, pgDeployment).Scan(&deploymentState, &desiredRevision); err != nil {
		t.Fatal(err)
	}
	if deploymentState != "git-committed" || desiredRevision != effectiveDocument.ConfigRevision || desiredRevision == operationCommit {
		t.Fatalf("no-change direct deployment state=%q desired=%q effective=%q operation=%q",
			deploymentState, desiredRevision, effectiveDocument.ConfigRevision, operationCommit)
	}

	// A parent VariableSet changes the effective values revision for every app
	// even when app.yaml is byte-identical. Activation must advance the central
	// deployment fence to the same revision rendered into Argo.
	if err = store.FinishReconciliation(ctx, convergence.Lease, gitprojection.ReconciliationOutcome{LastCommit: laterHead,
		NextPollAt: testStart.Add(time.Hour)}, testStart.Add(8*time.Second)); err != nil {
		t.Fatal(err)
	}
	parentHead := strings.Repeat("f", 40)
	current, _, err = store.RecordVerifiedHead(ctx, verified(current, parentHead, "postgres-parent-variable", testStart.Add(9*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	parentWork, err := store.ClaimReconciliation(ctx, "postgres-parent-indexer", testStart.Add(10*time.Second), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parentGeneration, err := store.BeginGeneration(ctx, parentWork.Lease, parentHead, current.ParserVersion, testStart.Add(11*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	parentApp, err := gitprojection.NewDocument(current, parentGeneration.Number, pgApplication, parentHead,
		convergedDocument.ConfigRevision, convergedDocument.BlobID, convergedDocument.Raw, convergedDocument.Parsed, nil, testStart.Add(11*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	dependencyPaths, err := gitprojection.DependencyPaths(current)
	if err != nil {
		t.Fatal(err)
	}
	parentRaw := []byte("apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  FEATURE: \"enabled\"\n")
	parentDocument, err := gitprojection.NewDependencyDocument(current, parentGeneration.Number, dependencyPaths[0], parentHead, parentHead,
		strings.Repeat("1", 40), parentRaw, map[string]any{"apiVersion": "variables.kuberploy.io/v1alpha1", "kind": "VariableSet",
			"values": map[string]any{"FEATURE": "enabled"}}, nil, testStart.Add(11*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutDocuments(ctx, parentGeneration, []gitprojection.Document{parentApp, parentDocument}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateGeneration(ctx, parentWork.Lease, parentGeneration, gitprojection.SchemaOnlyAppConfigPolicyValidator{}, testStart.Add(12*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT desired_revision FROM deployments WHERE id=$1`, pgDeployment).Scan(&desiredRevision); err != nil {
		t.Fatal(err)
	}
	if desiredRevision != parentHead {
		t.Fatalf("parent VariableSet activation left deployment desired revision=%q want=%q", desiredRevision, parentHead)
	}
	if err = store.FinishReconciliation(ctx, parentWork.Lease, gitprojection.ReconciliationOutcome{LastCommit: parentHead,
		NextPollAt: testStart.Add(time.Hour)}, testStart.Add(13*time.Second)); err != nil {
		t.Fatal(err)
	}

	// The provider can verify a protected delete after the exact deletion head
	// is already active. Receipt convergence must stop the deployment without
	// waiting for a different repository head to trigger another generation.
	stopAt := testStart.Add(14 * time.Second)
	if _, err = pool.Exec(ctx, `INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,created_at,updated_at)
		VALUES($1,'deployment.git-write','succeeded','deployment',$2,'postgres-protected-delete',2,$3,$3)`, pgDeleteOperation, pgDeployment, stopAt); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE deployments SET state='merge-pending-index',operation_id=$2,generation=2,updated_at=$3 WHERE id=$1`, pgDeployment, pgDeleteOperation, stopAt); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO environment_app_placements(project_id,environment_id,application_id,state,desired_state,created_at,updated_at)
		VALUES($1,$2,$3,'active','running',$4,$4)
		ON CONFLICT(environment_id,application_id) DO UPDATE SET state='active',desired_state='running',updated_at=EXCLUDED.updated_at`,
		pgProject, pgEnvironment, pgApplication, stopAt); err != nil {
		t.Fatal(err)
	}
	current, err = store.Binding(ctx, pgBinding)
	if err != nil {
		t.Fatal(err)
	}
	deletePlan := gitprojection.WritePlan{BindingID: current.ID, ProjectID: current.ProjectID, EnvironmentID: current.EnvironmentID,
		ApplicationID: pgApplication, BaseRevision: current.IndexedRevision, Precondition: gitprojection.MutationMatchETag,
		ExpectedETag: `"sha256:` + strings.Repeat("2", 64) + `"`, ChartDigest: "sha256:" + strings.Repeat("3", 64), PolicyVersion: "runtime-policy-v1"}
	deleteCommand, err := gitprojection.NewDeleteWriteCommand(pgDeleteOperation, pgDeployment, pgUser, deletePlan, current,
		parentApp.Raw, "stop protected app", stopAt)
	if err != nil {
		t.Fatal(err)
	}
	deleteCommand.PublicationMode = gitprojection.PublicationPullRequest
	if err = store.PutWriteCommand(ctx, deleteCommand); err != nil {
		t.Fatal(err)
	}
	repository := gitpublication.Repository{InstallationID: current.Repository.InstallationID, ID: current.Repository.RepositoryID,
		Owner: current.Repository.Owner, Name: current.Repository.Name}
	publication, err := gitpublication.NewPublication(pgDeleteOperation, current.ID, repository, current.TargetRef, current.IndexedRevision, stopAt)
	if err == nil {
		err = store.CreatePublication(ctx, publication)
	}
	if err != nil {
		t.Fatalf("delete publication=%#v err=%v", publication, err)
	}
	writeBase, err := publication.WithWriteBase(current.IndexedRevision, stopAt.Add(time.Second))
	if err != nil || store.CompareAndSwapPublication(ctx, publication, writeBase) != nil {
		t.Fatalf("delete write base=%#v err=%v", writeBase, err)
	}
	candidate, err := writeBase.WithCandidate(strings.Repeat("4", 40), stopAt.Add(2*time.Second))
	if err != nil || store.CompareAndSwapPublication(ctx, writeBase, candidate) != nil {
		t.Fatalf("delete candidate=%#v err=%v", candidate, err)
	}
	prURL := "https://github.com/kuberploy/projection-integration/pull/11"
	opened, err := candidate.WithPullRequest(gitpublication.PullRequestObservation{Repository: repository, Number: 11, URL: prURL,
		TargetRef: current.TargetRef, HeadRef: candidate.CandidateRef, HeadRevision: candidate.CandidateRevision,
		State: gitpublication.PullRequestOpen, ObservedAt: stopAt.Add(3 * time.Second)}, stopAt.Add(3*time.Second))
	if err != nil || store.CompareAndSwapPublication(ctx, candidate, opened) != nil {
		t.Fatalf("delete open=%#v err=%v", opened, err)
	}
	mergePending, err := opened.WithPullRequest(gitpublication.PullRequestObservation{Repository: repository, Number: 11, URL: prURL,
		TargetRef: current.TargetRef, HeadRef: opened.CandidateRef, HeadRevision: opened.CandidateRevision,
		State: gitpublication.PullRequestClosed, Merged: true, MergeRevision: strings.Repeat("5", 40), ObservedAt: stopAt.Add(4 * time.Second)}, stopAt.Add(4*time.Second))
	if err != nil || store.CompareAndSwapPublication(ctx, opened, mergePending) != nil {
		t.Fatalf("delete merge pending=%#v err=%v", mergePending, err)
	}
	deleteHead := strings.Repeat("6", 40)
	current, _, err = store.RecordVerifiedHead(ctx, verified(current, deleteHead, "postgres-delete-head", stopAt.Add(5*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	deleteWork, err := store.ClaimReconciliation(ctx, "postgres-delete-indexer", stopAt.Add(6*time.Second), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	deleteGeneration, err := store.BeginGeneration(ctx, deleteWork.Lease, deleteHead, current.ParserVersion, stopAt.Add(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutDocuments(ctx, deleteGeneration, []gitprojection.Document{}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateGeneration(ctx, deleteWork.Lease, deleteGeneration, gitprojection.SchemaOnlyAppConfigPolicyValidator{}, stopAt.Add(8*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = store.FinishReconciliation(ctx, deleteWork.Lease, gitprojection.ReconciliationOutcome{LastCommit: deleteHead,
		NextPollAt: stopAt.Add(time.Hour)}, stopAt.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	mergeVerified, err := mergePending.WithVerifiedMerge(deleteHead, stopAt.Add(10*time.Second))
	if err != nil || store.CompareAndSwapPublication(ctx, mergePending, mergeVerified) != nil {
		t.Fatalf("late delete verification=%#v err=%v", mergeVerified, err)
	}
	deleteCommand, err = store.WriteCommand(ctx, pgDeleteOperation)
	if err != nil || deleteCommand.State != gitprojection.WriteCommandIndexed || deleteCommand.IndexedGeneration != deleteGeneration.Number {
		t.Fatalf("late delete command=%#v err=%v", deleteCommand, err)
	}
	var placementState, desiredState string
	if err = pool.QueryRow(ctx, `SELECT d.state,d.desired_revision,p.state,p.desired_state FROM deployments d
		JOIN environment_app_placements p ON p.environment_id=d.environment_id AND p.application_id=d.application_id WHERE d.id=$1`, pgDeployment).
		Scan(&deploymentState, &desiredRevision, &placementState, &desiredState); err != nil {
		t.Fatal(err)
	}
	if deploymentState != "stopped" || desiredRevision != deleteHead || placementState != "draft" || desiredState != "stopped" {
		t.Fatalf("late protected delete deployment=%q desired=%q placement=%q/%q", deploymentState, desiredRevision, placementState, desiredState)
	}
}

func TestPostgreSQLDependencyInvalidationSchedulesSameHeadReindex(t *testing.T) {
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
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	const (
		projectID     = "a2000000-0000-4000-8000-000000000001"
		environmentID = "a2000000-0000-4000-8000-000000000002"
		bindingID     = "a2000000-0000-4000-8000-000000000003"
		applicationID = "a2000000-0000-4000-8000-000000000005"
	)
	cleanup := func(cleanupContext context.Context) {
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_repository_bindings WHERE id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM applications WHERE id=$1`, applicationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM environments WHERE id=$1`, environmentID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM projects WHERE id=$1`, projectID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,created_at) VALUES($1,'Dependency refresh','dependency-refresh',$2)`, projectID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at)
		VALUES($1,$2,'Dependency refresh','dependency-refresh','kp-dependency-refresh','kp-dependency-refresh',$3)`, environmentID, projectID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug,created_at)
		VALUES($1,$2,'Dependency refresh','dependency-refresh',$3)`, applicationID, projectID, now); err != nil {
		t.Fatal(err)
	}
	binding, err := gitprojection.NewGitHubEnvironmentBinding(bindingID, projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 9_200_000_000_000_101, RepositoryID: 9_200_000_000_000_102, Owner: "kuberploy", Name: "dependency-refresh"}, "refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	store, err := gitprojection.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	work, err := store.ClaimReconciliation(ctx, "dependency-refresh-worker", now, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("d", 40)
	binding, _, err = store.RecordVerifiedHead(ctx, verified(binding, head, "dependency-refresh-head", now.Add(time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	generation, err := store.BeginGeneration(ctx, work.Lease, head, binding.ParserVersion, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	profileTarget := "a2000000-0000-4000-8000-000000000004"
	document, err := gitprojection.NewDocument(binding, generation.Number, applicationID, head, head,
		strings.Repeat("e", 40), []byte("kind: AppConfig\n"), map[string]any{"spec": map[string]any{"delivery": map[string]any{
			"registryPull": map[string]any{"targetId": profileTarget, "profileName": "managed-registry", "profileRevision": float64(5)},
		}}}, nil, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutDocuments(ctx, generation, []gitprojection.Document{document}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateGeneration(ctx, work.Lease, generation, gitprojection.SchemaOnlyAppConfigPolicyValidator{}, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = store.FinishReconciliation(ctx, work.Lease, gitprojection.ReconciliationOutcome{LastCommit: head, NextPollAt: now.Add(time.Hour)}, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE git_projected_documents
		SET valid=false,diagnostics='[{"code":"RegistryPullProfileMismatch","detail":"profile changed","pointer":"/spec/delivery/registryPull"}]'::jsonb
		WHERE binding_id=$1 AND generation=$2 AND path=$3`, bindingID, generation.Number, document.Path); err != nil {
		t.Fatal(err)
	}
	invalidateAt := now.Add(5 * time.Second)
	invalidatedMatch, err := store.InvalidateMatchingProfileMismatch(ctx, environmentID, profileTarget, "managed-registry", 5, invalidateAt)
	if err != nil || !invalidatedMatch {
		t.Fatalf("invalidated=%t err=%v", invalidatedMatch, err)
	}
	invalidated, err := store.Binding(ctx, bindingID)
	if err != nil || invalidated.State != gitprojection.BindingIndexing || invalidated.TargetHeadRevision != head || invalidated.IndexedRevision != head {
		t.Fatalf("invalidated=%#v err=%v", invalidated, err)
	}
	reindex, err := store.ClaimReconciliation(ctx, "dependency-refresh-worker", now.Add(6*time.Second), 2*time.Minute)
	if err != nil || !reindex.BindingChanged || reindex.Binding.State != gitprojection.BindingIndexing || reindex.Binding.TargetHeadRevision != head {
		t.Fatalf("reindex=%#v err=%v", reindex, err)
	}
	if invalidatedMatch, err = store.InvalidateMatchingProfileMismatch(ctx, environmentID, profileTarget, "managed-registry", 5, now.Add(7*time.Second)); err != nil || invalidatedMatch {
		t.Fatalf("non-ready projection invalidated=%t err=%v", invalidatedMatch, err)
	}
	if err = store.ReleaseReconciliation(ctx, reindex.Lease, now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
}
