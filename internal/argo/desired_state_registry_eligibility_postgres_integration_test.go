package argo_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitops"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/imagepull"
	"github.com/kuberploy/kuberploy/internal/projectionpolicy"
	storepostgres "github.com/kuberploy/kuberploy/internal/store/postgres"
)

func TestPostgreSQLDesiredStateRegistryEligibilityUsesExactIndexedPolicyDocuments(t *testing.T) {
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
	if err = storepostgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	projectID, environmentID, applicationID := id.New(), id.New(), id.New()
	bindingID, targetID, deploymentID := id.New(), id.New(), id.New()
	suffix := strings.ReplaceAll(projectID, "-", "")[:12]
	now := time.Now().UTC().Truncate(time.Microsecond)
	project := domain.Project{ID: projectID, Name: "Argo pull " + suffix, Slug: "argo-pull-" + suffix, CreatedAt: now}
	namespace, argoProject := domain.DeriveEnvironmentDestination(project, "production-"+suffix)
	environment := domain.Environment{ID: environmentID, ProjectID: projectID, Name: "Production", Slug: "production-" + suffix,
		Namespace: namespace, ArgoProject: argoProject, CreatedAt: now}
	application := domain.Application{ID: applicationID, ProjectID: projectID, Name: "API", Slug: "api-" + suffix, CreatedAt: now}
	repository := "tenant/" + projectID + "/" + applicationID
	server := "registry." + suffix + ".example.test:5000"
	pullRef := "runtime-pull/" + suffix

	cleanup := func(cleanupContext context.Context) {
		_, _ = pool.Exec(cleanupContext, `DELETE FROM runtime_registry_pull_artifacts WHERE environment_id=$1`, environmentID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_projected_documents WHERE binding_id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_projection_generations WHERE binding_id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_safety_poll_cursors WHERE binding_id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_verified_head_observations WHERE binding_id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_repository_bindings WHERE id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM service_registry_policies WHERE registry_target_id=$1`, targetID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM registry_targets WHERE id=$1`, targetID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM applications WHERE id=$1`, applicationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM environments WHERE id=$1`, environmentID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM projects WHERE id=$1`, projectID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())

	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO projects(id,name,slug,created_at) VALUES($1,$2,$3,$4)`, []any{project.ID, project.Name, project.Slug, now}},
		{`INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7)`, []any{environment.ID, environment.ProjectID, environment.Name, environment.Slug, environment.Namespace, environment.ArgoProject, now}},
		{`INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,$3,$4,$5)`, []any{application.ID, application.ProjectID, application.Name, application.Slug, now}},
		{`INSERT INTO registry_targets(id,name,mode,endpoint,repository_prefix,pull_credential_ref,created_at,updated_at)
			VALUES($1,$2,'external',$3,'tenant',$4,$5,$5)`, []any{targetID, "argo-pull-" + suffix, "https://" + server, pullRef, now}},
		{`INSERT INTO service_registry_policies(registry_target_id,service_id,repository,created_at,updated_at)
			VALUES($1,$2,$3,$4,$4)`, []any{targetID, applicationID, repository, now}},
	} {
		if _, err = pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	binding, err := gitprojection.NewGitHubEnvironmentBinding(bindingID, projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 8_100_000_000_000_001, RepositoryID: 8_100_000_000_000_002,
			Owner: "kuberploy", Name: "argo-private-pull"}, "refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	projectionStore, err := gitprojection.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = projectionStore.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	work, err := projectionStore.ClaimReconciliation(ctx, "argo-registry-policy-worker", now.Add(time.Second), 4*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	config := imagepull.DefaultRuntimeConfig()
	config.Enabled = true
	config.Namespaces = []string{namespace}
	config.Profiles = []imagepull.Profile{{Name: "external-main", TargetID: targetID, RegistryServer: server,
		CredentialRef: pullRef, Revision: 1, SourceSecretRef: "pull-source-" + suffix, SourceSecretKey: ".dockerconfigjson"}}
	if err = config.Validate(); err != nil {
		t.Fatal(err)
	}
	validator := &projectionpolicy.Validator{Registry: &projectionpolicy.RegistryPullReferencePolicy{Config: config}}
	rawDeployment := domain.Deployment{ID: deploymentID, ApplicationID: applicationID, EnvironmentID: environmentID,
		Image: server + "/" + repository + "@sha256:" + strings.Repeat("a", 64), Runtime: domain.DefaultWorkloadRuntime(8080, nil)}
	publicRaw, err := gitops.RenderAppConfig(project, environment, application, rawDeployment)
	if err != nil {
		t.Fatal(err)
	}
	privateRaw := argoRegistryPullDocument(t, publicRaw, targetID, "external-main", 1)

	sequence := 0
	stage := func(raw []byte) gitprojection.Binding {
		t.Helper()
		sequence++
		head := strings.Repeat(string("123456789abcdef"[sequence]), 40)
		observedAt := now.Add(time.Duration(sequence*10) * time.Second)
		observed, _, stageErr := projectionStore.RecordVerifiedHead(ctx, gitprojection.VerifiedHead{
			BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef, Commit: head,
			Source: gitprojection.ObservationPoll, ProviderRequest: "argo-registry-policy-" + strconv.Itoa(sequence), ObservedAt: observedAt,
		})
		if stageErr != nil {
			t.Fatal(stageErr)
		}
		binding = observed
		generation, stageErr := projectionStore.BeginGeneration(ctx, work.Lease, head, binding.ParserVersion, observedAt.Add(time.Second))
		if stageErr != nil {
			t.Fatal(stageErr)
		}
		parsed, _, diagnostics := appconfig.ParseAndValidate(raw)
		if len(diagnostics) != 0 {
			t.Fatalf("invalid AppConfig fixture: %#v\n%s", diagnostics, raw)
		}
		document, stageErr := gitprojection.NewDocument(binding, generation.Number, applicationID, head, head,
			strings.Repeat(string("abcdef0123456789"[sequence]), 40), raw, parsed, nil, observedAt.Add(2*time.Second))
		if stageErr != nil {
			t.Fatal(stageErr)
		}
		if stageErr = projectionStore.PutDocuments(ctx, generation, []gitprojection.Document{document}); stageErr != nil {
			t.Fatal(stageErr)
		}
		binding, stageErr = projectionStore.ActivateGeneration(ctx, work.Lease, generation, validator, observedAt.Add(3*time.Second))
		if stageErr != nil {
			t.Fatal(stageErr)
		}
		return binding
	}

	platform, err := gitprojection.NewGitHubPlatformBinding(id.New(), id.New(),
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 8_100_000_000_000_001,
			RepositoryID: 8_100_000_000_000_003, Owner: "kuberploy", Name: "argo-platform"}, "refs/heads/platform", now)
	if err != nil {
		t.Fatal(err)
	}
	platform.TargetHeadRevision, platform.TargetHeadObservedAt = strings.Repeat("f", 40), now
	platform.State, platform.UpdatedAt = gitprojection.BindingIndexing, now
	runtime := argo.RuntimeLock{ChartRepository: "oci://ghcr.io/kuberploy/charts", ChartName: "kuberploy-runtime", ChartVersion: "1.2.3",
		ChartDigest: "sha256:" + strings.Repeat("b", 64), RendererImage: "ghcr.io/kuberploy/runtime-renderer@sha256:" + strings.Repeat("c", 64)}
	applications := []domain.Application{application}
	deployments := []domain.Deployment{{ID: deploymentID, ApplicationID: applicationID, EnvironmentID: environmentID}}
	resolver, err := argo.NewPostgreSQLRegistryEligibilityResolver(pool, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	artifactStore, err := imagepull.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	plan := func(activeBinding gitprojection.Binding, at time.Time) error {
		t.Helper()
		target := argo.DesiredStateTarget{Environment: argo.EnvironmentTarget{Project: project, Environment: environment,
			Binding: activeBinding, ArgoNamespace: "argocd", Runtime: runtime}, PlatformBinding: platform}
		approval := desiredStateApproval(t, target, applications, deployments)
		_, planErr := (argo.DesiredStatePlanner{Projection: staticDesiredStateProjectionGate{approval: approval},
			RegistryEligibility: resolver}).Plan(ctx, id.New(), target, nil, at)
		return planErr
	}

	publicBinding := stage(publicRaw)
	if err = plan(publicBinding, publicBinding.UpdatedAt.Add(time.Second)); err != nil {
		t.Fatalf("public AppConfig was blocked by artifact eligibility: %v", err)
	}

	privateBinding := stage(privateRaw)
	readyAt := privateBinding.UpdatedAt.Add(time.Second)
	if err = plan(privateBinding, readyAt); !errors.Is(err, argo.ErrRegistryReferencesNotReady) {
		t.Fatalf("awaiting private artifact entered desired state: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE runtime_registry_pull_artifacts
		SET runtime_state='ready',last_observed_at=$4,observed_uid=$5,observed_resource_version='1',
		    next_observation_at=$6,updated_at=$4
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3`,
		environmentID, targetID, 1, readyAt, id.New(), readyAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = plan(privateBinding, readyAt.Add(time.Second)); err != nil {
		t.Fatalf("exact ready private artifact was blocked: %v", err)
	}
	if err = plan(privateBinding, readyAt.Add(2*time.Minute)); !errors.Is(err, argo.ErrRegistryReferencesNotReady) {
		t.Fatalf("stale private artifact entered desired state: %v", err)
	}

	if _, err = pool.Exec(ctx, `DELETE FROM runtime_registry_pull_artifacts
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=1`, environmentID, targetID); err != nil {
		t.Fatal(err)
	}
	if err = plan(privateBinding, readyAt.Add(2*time.Second)); !errors.Is(err, argo.ErrRegistryReferencesNotReady) {
		t.Fatalf("absent private artifact entered desired state: %v", err)
	}
	desiredV1, err := imagepull.Desired(config, environmentID, namespace, targetID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = artifactStore.EnsureArtifact(ctx, desiredV1, readyAt.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE runtime_registry_pull_artifacts
		SET runtime_state='ready',last_observed_at=$4,observed_uid=$5,observed_resource_version='2',
		    next_observation_at=$6,updated_at=$4
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3`,
		environmentID, targetID, 1, readyAt.Add(4*time.Second), id.New(), readyAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE runtime_registry_pull_artifacts SET active=false,updated_at=$4
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3`,
		environmentID, targetID, 1, readyAt.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = plan(privateBinding, readyAt.Add(6*time.Second)); !errors.Is(err, argo.ErrRegistryReferencesNotReady) {
		t.Fatalf("inactive private artifact entered desired state: %v", err)
	}

	rotatedConfig := config
	rotatedConfig.Profiles = append([]imagepull.Profile(nil), config.Profiles...)
	rotatedConfig.Profiles[0].Revision = 2
	desiredV2, err := imagepull.Desired(rotatedConfig, environmentID, namespace, targetID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = artifactStore.EnsureArtifact(ctx, desiredV2, readyAt.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE runtime_registry_pull_artifacts
		SET runtime_state='ready',last_observed_at=$4,observed_uid=$5,observed_resource_version='3',
		    next_observation_at=$6,updated_at=$4
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3`,
		environmentID, targetID, 2, readyAt.Add(8*time.Second), id.New(), readyAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = plan(privateBinding, readyAt.Add(9*time.Second)); !errors.Is(err, argo.ErrRegistryReferencesNotReady) {
		t.Fatalf("profile-revision-mismatched artifact entered desired state: %v", err)
	}

	target := argo.DesiredStateTarget{Environment: argo.EnvironmentTarget{Project: project, Environment: environment,
		Binding: privateBinding, ArgoNamespace: "argocd", Runtime: runtime}, PlatformBinding: platform}
	missingCatalog := desiredStateApproval(t, target, nil, nil)
	if _, err = (argo.DesiredStatePlanner{Projection: staticDesiredStateProjectionGate{approval: missingCatalog},
		RegistryEligibility: resolver}).Plan(ctx, id.New(), target, nil, readyAt.Add(10*time.Second)); !errors.Is(err, argo.ErrConflict) {
		t.Fatalf("catalog omitting an indexed AppConfig was accepted: %v", err)
	}
}

func argoRegistryPullDocument(t testing.TB, raw []byte, targetID, profileName string, revision int64) []byte {
	t.Helper()
	needle := "    release:\n"
	replacement := "    registryPull:\n" +
		"      targetId: " + targetID + "\n" +
		"      profileName: " + profileName + "\n" +
		"      profileRevision: " + strconv.FormatInt(revision, 10) + "\n" + needle
	if !strings.Contains(string(raw), needle) {
		t.Fatal("AppConfig fixture has no delivery release block")
	}
	return []byte(strings.Replace(string(raw), needle, replacement, 1))
}
