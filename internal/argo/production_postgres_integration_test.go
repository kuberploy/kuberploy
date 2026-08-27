package argo

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitops"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

type toggledRegistryEligibility struct {
	resolved bool
	calls    int
}

func (r *toggledRegistryEligibility) ResolveRegistryReferences(_ context.Context, target DesiredStateTarget, approval DesiredStateProjectionApproval, _ time.Time) (bool, error) {
	r.calls++
	if target.Validate() != nil || approval.validateFor(target, false) != nil || approval.RegistryReferencesResolved {
		return false, ErrInvalid
	}
	return r.resolved, nil
}

func TestPostgreSQLProductionProjectionMaterializerAndClaimGate(t *testing.T) {
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
		userID                    = "81111111-1111-4111-8111-111111111111"
		projectID                 = "82111111-1111-4111-8111-111111111111"
		environmentID             = "83111111-1111-4111-8111-111111111111"
		applicationID             = "84111111-1111-4111-8111-111111111111"
		deploymentID              = "85111111-1111-4111-8111-111111111111"
		operationID               = "86111111-1111-4111-8111-111111111111"
		stoppedApplicationID      = "84111111-1111-4111-8111-222222222222"
		stoppedDeploymentID       = "85111111-1111-4111-8111-222222222222"
		stoppedOperationID        = "86111111-1111-4111-8111-222222222222"
		platformID                = "87111111-1111-4111-8111-111111111111"
		bindingID                 = "88111111-1111-4111-8111-111111111111"
		installationPlatformID    = "8a111111-1111-4111-8111-111111111111"
		installationEnvironmentID = "8b111111-1111-4111-8111-111111111111"
		repositoryPlatformID      = "8c111111-1111-4111-8111-111111111111"
		repositoryEnvironmentID   = "8d111111-1111-4111-8111-111111111111"
		foundationEnvironmentID   = "8f111111-1111-4111-8111-111111111111"
		foundationBindingID       = "80111111-1111-4111-8111-111111111111"
	)
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, statement := range []string{
			`DELETE FROM runtime_readiness WHERE runtime_kind='argo-desired-state' AND platform_binding_id='` + platformID + `'`,
			`ALTER TABLE argo_desired_state_materialization_receipts DISABLE TRIGGER USER`,
			`DELETE FROM argo_desired_state_materialization_receipts WHERE platform_binding_id IN ('` + bindingID + `','` + foundationBindingID + `','` + platformID + `') OR environment_binding_id IN ('` + bindingID + `','` + foundationBindingID + `','` + platformID + `')`,
			`ALTER TABLE argo_desired_state_materialization_receipts ENABLE TRIGGER USER`,
			`DELETE FROM argo_desired_state_commands WHERE platform_binding_id='` + platformID + `'`,
			`DELETE FROM git_projected_documents WHERE binding_id IN ('` + bindingID + `','` + foundationBindingID + `','` + platformID + `')`,
			`DELETE FROM git_projection_generations WHERE binding_id IN ('` + bindingID + `','` + foundationBindingID + `','` + platformID + `')`,
			`DELETE FROM git_repository_bindings WHERE id IN ('` + bindingID + `','` + foundationBindingID + `','` + platformID + `')`,
			`DELETE FROM deployments WHERE id IN ('` + deploymentID + `','` + stoppedDeploymentID + `')`,
			`DELETE FROM operations WHERE id IN ('` + operationID + `','` + stoppedOperationID + `')`,
			`DELETE FROM applications WHERE id IN ('` + applicationID + `','` + stoppedApplicationID + `')`,
			`DELETE FROM environments WHERE id IN ('` + environmentID + `','` + foundationEnvironmentID + `')`,
			`DELETE FROM projects WHERE id='` + projectID + `'`,
			`DELETE FROM github_repositories WHERE id IN ('` + repositoryPlatformID + `','` + repositoryEnvironmentID + `')`,
			`DELETE FROM github_installations WHERE id IN ('` + installationPlatformID + `','` + installationEnvironmentID + `')`,
			`DELETE FROM users WHERE id='` + userID + `'`,
		} {
			_, _ = pool.Exec(cleanupCtx, statement)
		}
	}
	cleanup()
	defer cleanup()

	now := time.Now().UTC().Truncate(time.Microsecond)
	project := domain.Project{ID: projectID, Name: "Production Argo", Slug: "production-argo", CreatedAt: now}
	namespace, argoProject := domain.DeriveEnvironmentDestination(project, "production")
	environment := domain.Environment{ID: environmentID, ProjectID: projectID, Name: "Production", Slug: "production",
		Namespace: namespace, ArgoProject: argoProject, CreatedAt: now}
	application := domain.Application{ID: applicationID, ProjectID: projectID, Name: "API", Slug: "api", CreatedAt: now}
	runtime := domain.DefaultWorkloadRuntime(8080, nil)
	deployment := domain.Deployment{ID: deploymentID, EnvironmentID: environmentID, ApplicationID: applicationID,
		Image: "registry.example.test/api@sha256:" + strings.Repeat("1", 64), Replicas: 1, Port: 8080,
		Runtime: runtime, State: "pending-git", OperationID: operationID, Generation: 1, CreatedAt: now, UpdatedAt: now}
	runtimeJSON, _ := json.Marshal(runtime)
	permissions := `{"metadata":"read","contents":"write"}`
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id,display_name,role,issuer,subject,grant_revision,created_at) VALUES($1,'argo-runtime','platform-admin','test','argo-runtime',1,$2)`, []any{userID, now}},
		{`INSERT INTO projects(id,name,slug,created_at) VALUES($1,$2,$3,$4)`, []any{project.ID, project.Name, project.Slug, now}},
		{`INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, []any{environment.ID, projectID, environment.Name, environment.Slug, environment.Namespace, environment.ArgoProject, now}},
		{`INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,$3,$4,$5)`, []any{application.ID, projectID, application.Name, application.Slug, now}},
		{`INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'Stopped App','stopped-app',$3)`, []any{stoppedApplicationID, projectID, now}},
		{`INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,progress,git_revision,created_at,updated_at) VALUES($1,'deployment.apply','queued','deployment',$2,'argo-runtime-test',1,'[]','',$3,$3)`, []any{operationID, deploymentID, now}},
		{`INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,progress,git_revision,created_at,updated_at) VALUES($1,'deployment.stop','succeeded','deployment',$2,'argo-runtime-test-stopped',1,'[]','',$3,$3)`, []any{stoppedOperationID, stoppedDeploymentID, now}},
		{`INSERT INTO deployments(id,environment_id,application_id,image,replicas,port,environment,route,runtime,state,operation_id,generation,created_at,updated_at) VALUES($1,$2,$3,$4,1,8080,'{}',NULL,$5,'pending-git',$6,1,$7,$7)`, []any{deploymentID, environmentID, applicationID, deployment.Image, runtimeJSON, operationID, now}},
		{`INSERT INTO deployments(id,environment_id,application_id,image,replicas,port,environment,route,runtime,state,operation_id,generation,created_at,updated_at) VALUES($1,$2,$3,$4,1,8080,'{}',NULL,$5,'stopped',$6,1,$7,$7)`, []any{stoppedDeploymentID, environmentID, stoppedApplicationID, "registry.example.test/stopped@sha256:" + strings.Repeat("2", 64), runtimeJSON, stoppedOperationID, now}},
		{`INSERT INTO github_installations(id,github_installation_id,account_login,account_type,owner_user_id,visibility,repository_selection,repository_count,created_at,updated_at,github_app_id,github_account_id,lifecycle,permissions,last_verified_at) VALUES($1,$2,'kuberploy','Organization',$3,'private','selected',1,$4,$4,1001,$5,'active',$6,$4)`, []any{installationPlatformID, int64(501), userID, now, int64(7001), permissions}},
		{`INSERT INTO github_installations(id,github_installation_id,account_login,account_type,owner_user_id,visibility,repository_selection,repository_count,created_at,updated_at,github_app_id,github_account_id,lifecycle,permissions,last_verified_at) VALUES($1,$2,'kuberploy','Organization',$3,'private','selected',1,$4,$4,1001,$5,'active',$6,$4)`, []any{installationEnvironmentID, int64(502), userID, now, int64(7002), permissions}},
		{`INSERT INTO github_repositories(id,installation_id,github_repository_id,github_owner_id,owner_login,name,lifecycle,last_verified_at,created_at,updated_at) VALUES($1,$2,$3,$4,'kuberploy','platform','active',$5,$5,$5)`, []any{repositoryPlatformID, installationPlatformID, int64(601), int64(7001), now}},
		{`INSERT INTO github_repositories(id,installation_id,github_repository_id,github_owner_id,owner_login,name,lifecycle,last_verified_at,created_at,updated_at) VALUES($1,$2,$3,$4,'kuberploy','environment','active',$5,$5,$5)`, []any{repositoryEnvironmentID, installationEnvironmentID, int64(602), int64(7002), now}},
	}
	for _, statement := range statements {
		if _, err = pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	projectionStore, err := gitprojection.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	platform, err := gitprojection.NewGitHubPlatformBinding(platformID, gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 501, RepositoryID: 601, Owner: "kuberploy", Name: "platform"},
		"refs/heads/platform", now)
	if err != nil {
		t.Fatal(err)
	}
	platform.TargetHeadRevision, platform.TargetHeadObservedAt = strings.Repeat("a", 40), now
	platform.State, platform.UpdatedAt = gitprojection.BindingIndexing, now
	if err = projectionStore.PutBinding(ctx, platform); err != nil {
		t.Fatal(err)
	}
	binding, err := gitprojection.NewGitHubEnvironmentBinding(bindingID, projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 502, RepositoryID: 602, Owner: "kuberploy", Name: "environment"},
		"refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("b", 40)
	binding.TargetHeadRevision, binding.IndexedRevision = revision, revision
	binding.TargetHeadObservedAt, binding.IndexedAt = now, now
	binding.ProjectionGeneration, binding.State, binding.UpdatedAt = 1, gitprojection.BindingReady, now
	if err = projectionStore.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	activatedAt := now.Add(time.Second)
	if _, err = pool.Exec(ctx, `INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at) VALUES($1,1,$2,$3,'active',$4,$5)`,
		binding.ID, revision, binding.ParserVersion, now, activatedAt); err != nil {
		t.Fatal(err)
	}
	raw, err := gitops.RenderAppConfig(project, environment, application, deployment)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, diagnostics := appconfig.ParseAndValidate(raw)
	if len(diagnostics) != 0 {
		t.Fatalf("invalid AppConfig fixture: %#v", diagnostics)
	}
	document, err := gitprojection.NewDocument(binding, 1, applicationID, revision, revision, strings.Repeat("c", 40), raw, parsed, nil, activatedAt)
	if err != nil {
		t.Fatal(err)
	}
	parsedJSON, _ := json.Marshal(document.Parsed)
	diagnosticsJSON := []byte(`[]`)
	if _, err = pool.Exec(ctx, `INSERT INTO git_projected_documents(binding_id,generation,path,application_id,source_revision,config_revision,blob_id,content_sha256,raw,parsed,valid,diagnostics,schema_version,parser_version,indexed_at)
		VALUES($1,1,$2,$3,$4,$5,$6,$7,$8,$9,true,$10,$11,$12,$13)`, binding.ID, document.Path, applicationID,
		document.SourceRevision, document.ConfigRevision, document.BlobID, document.ContentSHA256, document.Raw, parsedJSON,
		diagnosticsJSON, document.SchemaVersion, document.ParserVersion, document.IndexedAt); err != nil {
		t.Fatal(err)
	}

	registry := &toggledRegistryEligibility{resolved: true}
	policyDigest := "sha256:" + strings.Repeat("d", 64)
	gate, err := NewPostgreSQLDesiredStateProjectionGate(pool, gitprojection.SchemaOnlyAppConfigPolicyValidator{}, registry, policyDigest)
	if err != nil {
		t.Fatal(err)
	}
	credentialName, _ := RepositoryCredentialName(platform.ID)
	identity, err := DesiredStateRuntimeIdentityForConfig(DesiredStateRuntimeConfig{Enabled: true, GitHubAppID: 1001,
		PlatformBindingID: platform.ID, ArgoNamespace: "argocd", RootApplicationName: PlatformRootApplicationName,
		RepositorySecretName: credentialName, Runtime: RuntimeLock{ChartRepository: "oci://ghcr.io/kuberploy/charts", ChartName: "kuberploy-runtime",
			ChartVersion: "1.2.3", ChartDigest: "sha256:" + strings.Repeat("e", 64), RendererImage: "ghcr.io/kuberploy/renderer@sha256:" + strings.Repeat("f", 64)},
		DigestEnforcement: ChartDigestNativeOCI})
	if err != nil {
		t.Fatal(err)
	}
	argoStore, err := NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	materializer, err := NewPostgreSQLDesiredStateMaterializer(pool, argoStore, projectionStore, gate, registry, identity)
	if err != nil {
		t.Fatal(err)
	}
	materializer.newID = func() string { return "8e111111-1111-4111-8111-111111111111" }
	created, err := materializer.MaterializeDesiredStateOnce(ctx, activatedAt.Add(time.Second))
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	status, err := argoStore.LatestDesiredState(ctx, projectID, environmentID)
	if err != nil || status.CatalogDigest == "" || status.EnvironmentRevision != revision {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	if created, err = materializer.MaterializeDesiredStateOnce(ctx, activatedAt.Add(2*time.Second)); err != nil || created {
		t.Fatalf("duplicate materialization created=%v err=%v", created, err)
	}

	work, err := argoStore.ClaimDesiredState(ctx, "production-argo-worker", identity.DesiredStateWorkerIdentity,
		activatedAt.Add(3*time.Second), minimumDesiredStateLease)
	if err != nil {
		t.Fatal(err)
	}
	if err = gate.ValidateDesiredStateClaim(ctx, work.Command, DesiredStateClaimActive); err != nil {
		t.Fatalf("exact active claim rejected: %v", err)
	}
	registry.resolved = false
	if err = gate.ValidateDesiredStateClaim(ctx, work.Command, DesiredStateClaimActive); !errors.Is(err, ErrRegistryReferencesNotReady) {
		t.Fatalf("unready registry claim accepted: %v", err)
	}
	registry.resolved = true
	tampered := work.Command
	tampered.CatalogDigest = "sha256:" + strings.Repeat("0", 64)
	if err = gate.ValidateDesiredStateClaim(ctx, tampered, DesiredStateClaimActive); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tampered approval receipt accepted: %v", err)
	}
	tamperedProjection := work.Command
	tamperedProjection.EnvironmentRevision = strings.Repeat("0", 40)
	if err = gate.ValidateDesiredStateClaim(ctx, tamperedProjection, DesiredStateClaimActive); !errors.Is(err, ErrInvalid) ||
		errors.Is(err, ErrDesiredStateProjectionSuperseded) {
		t.Fatalf("tampered projection receipt was mistaken for supersession: %v", err)
	}
	wrongPolicyGate, err := NewPostgreSQLDesiredStateProjectionGate(pool, gitprojection.SchemaOnlyAppConfigPolicyValidator{}, registry,
		"sha256:"+strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err = wrongPolicyGate.ValidateDesiredStateClaim(ctx, work.Command, DesiredStateClaimActive); !errors.Is(err, ErrInvalid) {
		t.Fatalf("different projection policy accepted claim: %v", err)
	}

	supersededAt := activatedAt.Add(5 * time.Second)
	retired, err := argoStore.SupersedeDesiredState(ctx, work.Lease, supersededAt)
	if err != nil || retired.State != DesiredStateSuperseded || retired.WriteBaseRevision != "" {
		t.Fatalf("pre-write projection race was not superseded: command=%#v err=%v", retired, err)
	}
	materializer.newID = func() string { return "8e555555-5555-4555-8555-555555555555" }
	if created, err = materializer.MaterializeDesiredStateOnce(ctx, supersededAt.Add(time.Second)); err != nil || !created {
		t.Fatalf("superseded pre-write command was not rematerialized: created=%v err=%v", created, err)
	}
	replacement, err := argoStore.LatestDesiredState(ctx, projectID, environmentID)
	if err != nil || replacement.CommandID == retired.ID || replacement.Generation != retired.Generation+1 ||
		replacement.State != DesiredStatePending || replacement.EnvironmentRevision != retired.EnvironmentRevision {
		t.Fatalf("superseded replacement status=%#v retired=%#v err=%v", replacement, retired.Status(), err)
	}
	work, err = argoStore.ClaimDesiredState(ctx, "production-argo-worker", identity.DesiredStateWorkerIdentity,
		supersededAt.Add(2*time.Second), minimumDesiredStateLease)
	if err != nil || work.Command.ID != replacement.CommandID || work.Command.ContentSHA256 != retired.ContentSHA256 {
		t.Fatalf("replacement command work=%#v status=%#v err=%v", work, replacement, err)
	}
	if err = gate.ValidateDesiredStateClaim(ctx, work.Command, DesiredStateClaimActive); err != nil {
		t.Fatalf("replacement active claim rejected: %v", err)
	}

	receiptAt := activatedAt.Add(8 * time.Second)
	bound, err := argoStore.BindDesiredStateWriteBase(ctx, work.Lease, platform.TargetHeadRevision, receiptAt, receiptAt)
	if err != nil {
		t.Fatal(err)
	}
	advancedRevision := strings.Repeat("3", 40)
	if _, err = pool.Exec(ctx, `INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at)
		VALUES($1,2,$2,$3,'active',$4,$4)`, binding.ID, advancedRevision, binding.ParserVersion, receiptAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE git_repository_bindings SET target_head_revision=$2,indexed_revision=$2,
		target_head_observed_at=$3,indexed_at=$3,projection_generation=2,updated_at=$3 WHERE id=$1`,
		binding.ID, advancedRevision, receiptAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = gate.ValidateDesiredStateClaim(ctx, work.Command, DesiredStateClaimActive); !errors.Is(err, ErrDesiredStateProjectionSuperseded) {
		t.Fatalf("stale active projection was not classified as superseded: %v", err)
	}
	registry.resolved = false
	beforeCalls := registry.calls
	if err = gate.ValidateDesiredStateClaim(ctx, bound, DesiredStateClaimRecovery); err != nil {
		t.Fatalf("durable write-base recovery was stranded by a newer projection: %v", err)
	}
	if registry.calls != beforeCalls {
		t.Fatal("mutable registry freshness was consulted after durable write-base receipt")
	}
	tamperedRecovery := bound
	tamperedRecovery.CatalogDigest = "sha256:" + strings.Repeat("0", 64)
	if err = gate.ValidateDesiredStateClaim(ctx, tamperedRecovery, DesiredStateClaimRecovery); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tampered durable recovery receipt accepted: %v", err)
	}
	committedRevision := strings.Repeat("2", 40)
	committed, err := argoStore.MarkDesiredStateGitCommitted(ctx, *bound.Lease, committedRevision, receiptAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = gate.ValidateDesiredStateClaim(ctx, committed, DesiredStateClaimRecovery); err != nil {
		t.Fatalf("durable operation recovery was stranded: %v", err)
	}
	if registry.calls != beforeCalls {
		t.Fatal("mutable registry freshness was consulted after durable Git commit")
	}
	verified, err := argoStore.CompleteDesiredStateVerified(ctx, *committed.Lease, committedRevision, receiptAt.Add(2*time.Second))
	if err != nil || verified.State != DesiredStateVerified {
		t.Fatalf("complete verified command=%#v err=%v", verified, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO git_projected_documents(binding_id,generation,path,application_id,source_revision,config_revision,blob_id,content_sha256,raw,parsed,valid,diagnostics,schema_version,parser_version,indexed_at)
		VALUES($1,2,$2,$3,$4,$4,$5,$6,$7,$8,true,$9,$10,$11,$12)`, binding.ID, document.Path, applicationID,
		advancedRevision, strings.Repeat("4", 40), document.ContentSHA256, document.Raw, parsedJSON,
		diagnosticsJSON, document.SchemaVersion, document.ParserVersion, receiptAt.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	rotatedIdentity, err := DesiredStateRuntimeIdentityForConfig(DesiredStateRuntimeConfig{Enabled: true, GitHubAppID: 1001,
		PlatformBindingID: platform.ID, ArgoNamespace: "argocd", RootApplicationName: PlatformRootApplicationName,
		RepositorySecretName: credentialName, Runtime: RuntimeLock{ChartRepository: "oci://registry.example.test/kuberploy/charts", ChartName: "kuberploy-runtime",
			ChartVersion: identity.Runtime.ChartVersion, ChartDigest: identity.Runtime.ChartDigest, RendererImage: identity.Runtime.RendererImage},
		DigestEnforcement: ChartDigestNativeOCI})
	if err != nil {
		t.Fatal(err)
	}
	rotatedMaterializer, err := NewPostgreSQLDesiredStateMaterializer(pool, argoStore, projectionStore, gate, registry, rotatedIdentity)
	if err != nil {
		t.Fatal(err)
	}
	rotatedMaterializer.newID = func() string { return "8e222222-2222-4222-8222-222222222222" }
	registry.resolved = true
	created, err = rotatedMaterializer.MaterializeDesiredStateOnce(ctx, receiptAt.Add(4*time.Second))
	if err != nil || !created {
		t.Fatalf("verified desired state did not rotate with runtime lock: created=%v err=%v", created, err)
	}
	rotated, err := argoStore.LatestDesiredState(ctx, projectID, environmentID)
	if err != nil || rotated.ChartVersion != rotatedIdentity.Runtime.ChartVersion || rotated.ChartDigest != rotatedIdentity.Runtime.ChartDigest ||
		rotated.RendererImage != rotatedIdentity.Runtime.RendererImage {
		t.Fatalf("rotated runtime lock command=%#v err=%v", rotated, err)
	}
	rotatedCommand, err := argoStore.DesiredStateCommand(ctx, rotated.CommandID)
	if err != nil || rotatedCommand.Runtime.ChartRepository != rotatedIdentity.Runtime.ChartRepository {
		t.Fatalf("rotated runtime repository command=%#v err=%v", rotatedCommand, err)
	}
	if created, err = rotatedMaterializer.MaterializeDesiredStateOnce(ctx, receiptAt.Add(5*time.Second)); err != nil || created {
		t.Fatalf("duplicate runtime-lock rotation created=%v err=%v", created, err)
	}

	// An environment foundation is useful before the environment owns any
	// applications or deployments. Its ready, exact empty projection must still
	// publish the environment-owned AppProject and empty ApplicationSet so Helm-
	// only Applications never require a dummy Deployment to make their project.
	foundationNamespace, foundationArgoProject := domain.DeriveEnvironmentDestination(project, "helm-only")
	foundationEnvironment := domain.Environment{ID: foundationEnvironmentID, ProjectID: projectID, Name: "Helm only",
		Slug: "helm-only", Namespace: foundationNamespace, ArgoProject: foundationArgoProject, CreatedAt: now}
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, foundationEnvironment.ID, projectID, foundationEnvironment.Name,
		foundationEnvironment.Slug, foundationEnvironment.Namespace, foundationEnvironment.ArgoProject, now); err != nil {
		t.Fatal(err)
	}
	// Keep the independent foundation command logically older than the pending
	// runtime-lock rotation so the platform scheduler proves FIFO selection.
	foundationCreatedAt := receiptAt.Add(2 * time.Second)
	foundationBinding, err := gitprojection.NewGitHubEnvironmentBinding(foundationBindingID, projectID, foundationEnvironmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 502, RepositoryID: 602, Owner: "kuberploy", Name: "environment"},
		"refs/heads/main", foundationCreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	foundationRevision := strings.Repeat("5", 40)
	foundationBinding.TargetHeadRevision, foundationBinding.IndexedRevision = foundationRevision, foundationRevision
	foundationBinding.TargetHeadObservedAt, foundationBinding.IndexedAt = foundationCreatedAt, foundationCreatedAt
	foundationBinding.ProjectionGeneration, foundationBinding.State, foundationBinding.UpdatedAt = 1, gitprojection.BindingReady, foundationCreatedAt
	if err = projectionStore.PutBinding(ctx, foundationBinding); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at)
		VALUES($1,1,$2,$3,'active',$4,$4)`, foundationBinding.ID, foundationRevision, foundationBinding.ParserVersion,
		foundationCreatedAt); err != nil {
		t.Fatal(err)
	}
	materializer.newID = func() string { return "8e333333-3333-4333-8333-333333333333" }
	created, err = materializer.MaterializeDesiredStateOnce(ctx, foundationCreatedAt.Add(time.Second))
	if err != nil || !created {
		t.Fatalf("foundation-only environment was not materialized: created=%v err=%v", created, err)
	}
	foundationStatus, err := argoStore.LatestDesiredState(ctx, projectID, foundationEnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	foundationCommand, err := argoStore.DesiredStateCommand(ctx, foundationStatus.CommandID)
	if err != nil || foundationCommand.ArgoProject != foundationArgoProject ||
		strings.Count(string(foundationCommand.Content), "\nkind: ") != 2 ||
		!strings.Contains(string(foundationCommand.Content), "kind: AppProject") ||
		!strings.Contains(string(foundationCommand.Content), "kind: ApplicationSet") ||
		!strings.Contains(string(foundationCommand.Content), "elements: []") {
		t.Fatalf("foundation-only desired state is not AppProject plus empty ApplicationSet: command=%#v err=%v", foundationCommand, err)
	}
	foundationWork, err := argoStore.ClaimDesiredState(ctx, "foundation-argo-worker", identity.DesiredStateWorkerIdentity,
		foundationCreatedAt.Add(2*time.Second), minimumDesiredStateLease)
	if err != nil || foundationWork.Command.EnvironmentID != foundationEnvironmentID {
		t.Fatalf("foundation-only command was not independently claimable: work=%#v err=%v", foundationWork, err)
	}
	if err = gate.ValidateDesiredStateClaim(ctx, foundationWork.Command, DesiredStateClaimActive); err != nil {
		t.Fatalf("foundation-only active claim rejected: %v", err)
	}
	foundationReceiptAt := foundationCreatedAt.Add(3 * time.Second)
	foundationBound, err := argoStore.BindDesiredStateWriteBase(ctx, foundationWork.Lease,
		platform.TargetHeadRevision, foundationReceiptAt, foundationReceiptAt)
	if err != nil {
		t.Fatal(err)
	}
	if err = gate.ValidateDesiredStateClaim(ctx, foundationBound, DesiredStateClaimRecovery); err != nil {
		t.Fatalf("foundation-only durable recovery receipt rejected: %v", err)
	}
	foundationCommit := strings.Repeat("6", 40)
	foundationCommitted, err := argoStore.MarkDesiredStateGitCommitted(ctx, *foundationBound.Lease,
		foundationCommit, foundationReceiptAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = gate.ValidateDesiredStateClaim(ctx, foundationCommitted, DesiredStateClaimRecovery); err != nil {
		t.Fatalf("foundation-only Git-committed recovery rejected: %v", err)
	}
	if completed, completeErr := argoStore.CompleteDesiredStateVerified(ctx, *foundationCommitted.Lease,
		foundationCommit, foundationReceiptAt.Add(2*time.Second)); completeErr != nil || completed.State != DesiredStateVerified {
		t.Fatalf("foundation-only command not verifiable: command=%#v err=%v", completed, completeErr)
	}

	var verifiedReceiptCommand string
	if err = pool.QueryRow(ctx, `SELECT desired_state_command_id::text
		FROM argo_desired_state_materialization_receipts
		WHERE environment_binding_id=$1 AND environment_revision=$2 AND environment_generation=1`,
		foundationBinding.ID, foundationRevision).Scan(&verifiedReceiptCommand); err != nil ||
		verifiedReceiptCommand != foundationCommand.ID {
		t.Fatalf("verified command receipt=%s err=%v", verifiedReceiptCommand, err)
	}

	// An unrelated shared-branch advance changes the exact projection receipt
	// but not the empty environment's rendered AppProject/ApplicationSet bytes.
	// The materializer must durably bind generation 2 to the verified generation
	// 1 command before Helm may treat that older commit as current authority.
	foundationAdvancedRevision := strings.Repeat("7", 40)
	if _, err = pool.Exec(ctx, `INSERT INTO git_projection_generations(
		binding_id,generation,head_revision,parser_version,state,started_at,activated_at
	) VALUES($1,2,$2,$3,'active',$4,$4)`, foundationBinding.ID,
		foundationAdvancedRevision, foundationBinding.ParserVersion, foundationReceiptAt.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE git_repository_bindings SET
		target_head_revision=$2,indexed_revision=$2,projection_generation=2,
		target_head_observed_at=$3,indexed_at=$3,updated_at=$3 WHERE id=$1`,
		foundationBinding.ID, foundationAdvancedRevision, foundationReceiptAt.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	materializer.newID = func() string { return "8e444444-4444-4444-8444-444444444444" }
	if created, err = materializer.MaterializeDesiredStateOnce(ctx, foundationReceiptAt.Add(4*time.Second)); err != nil || !created {
		t.Fatalf("unchanged generation receipt not materialized: created=%v err=%v", created, err)
	}
	var unchangedReceiptCommand, unchangedReceiptRevision, unchangedReceiptDigest string
	if err = pool.QueryRow(ctx, `SELECT desired_state_command_id::text,desired_state_revision,
		desired_state_content_sha256 FROM argo_desired_state_materialization_receipts
		WHERE environment_binding_id=$1 AND environment_revision=$2 AND environment_generation=2`,
		foundationBinding.ID, foundationAdvancedRevision).Scan(&unchangedReceiptCommand,
		&unchangedReceiptRevision, &unchangedReceiptDigest); err != nil ||
		unchangedReceiptCommand != foundationCommand.ID || unchangedReceiptRevision != foundationCommit ||
		unchangedReceiptDigest != foundationCommand.ContentSHA256 {
		t.Fatalf("unchanged receipt command=%s revision=%s digest=%s err=%v",
			unchangedReceiptCommand, unchangedReceiptRevision, unchangedReceiptDigest, err)
	}
	if created, err = materializer.MaterializeDesiredStateOnce(ctx, foundationReceiptAt.Add(5*time.Second)); err != nil || created {
		t.Fatalf("unchanged receipt replay created=%v err=%v", created, err)
	}

	catalog, err := NewPostgreSQLRuntimeBindingCatalog(pool)
	if err != nil {
		t.Fatal(err)
	}
	authorities, err := catalog.ArgoRepositoryBindings(ctx, 1001, platform.ID, now, time.Minute)
	if err != nil || len(authorities) != 3 {
		t.Fatalf("authorities=%#v err=%v", authorities, err)
	}
	for _, authority := range authorities {
		if !authority.Authorized || authority.RevocationRequired {
			t.Fatalf("ready binding was not authorized: %#v", authority)
		}
	}
	staleCatalogAt := now.Add(-2 * time.Minute)
	if _, err = pool.Exec(ctx, `UPDATE github_installations SET last_verified_at=$1 WHERE github_app_id=$2`, staleCatalogAt, int64(1001)); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE github_repositories SET last_verified_at=$1 WHERE installation_id IN ($2,$3)`, staleCatalogAt, installationPlatformID, installationEnvironmentID); err != nil {
		t.Fatal(err)
	}
	authorities, err = catalog.ArgoRepositoryBindings(ctx, 1001, platform.ID, now, time.Minute)
	if err != nil || len(authorities) != 3 {
		t.Fatalf("stale authorities=%#v err=%v", authorities, err)
	}
	for _, authority := range authorities {
		if authority.Authorized || authority.RevocationRequired {
			t.Fatalf("stale catalog proof was treated as authorized: %#v", authority)
		}
	}
	verifiedBindings := make([]gitprojection.Binding, 0, len(authorities))
	for _, authority := range authorities {
		verifiedBindings = append(verifiedBindings, authority.Binding)
	}
	if err = catalog.MarkArgoRepositoryBindingsVerified(ctx, 1001, verifiedBindings, now); err != nil {
		t.Fatalf("renew provider-verified catalog: %v", err)
	}
	authorities, err = catalog.ArgoRepositoryBindings(ctx, 1001, platform.ID, now.Add(time.Second), time.Minute)
	if err != nil || len(authorities) != 3 {
		t.Fatalf("renewed authorities=%#v err=%v", authorities, err)
	}
	for _, authority := range authorities {
		if !authority.Authorized || authority.RevocationRequired {
			t.Fatalf("provider-verified catalog was not renewed: %#v", authority)
		}
	}
	if _, err = pool.Exec(ctx, `UPDATE github_repositories SET lifecycle='removed',removed_at=$2,updated_at=$2 WHERE id=$1`, repositoryEnvironmentID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	authorities, err = catalog.ArgoRepositoryBindings(ctx, 1001, platform.ID, now.Add(time.Second), 2*time.Minute)
	if err != nil || len(authorities) != 3 || !authorities[0].Authorized || authorities[0].RevocationRequired ||
		authorities[1].Authorized || !authorities[1].RevocationRequired || authorities[2].Authorized || !authorities[2].RevocationRequired {
		t.Fatalf("removed repository was not retained for revocation: %#v err=%v", authorities, err)
	}
}
