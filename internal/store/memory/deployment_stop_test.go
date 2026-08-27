package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

func TestStopDeploymentPublishesDeleteAndAllowsAppDeletion(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	project, err := store.CreateProject(ctx, admin.ID, "stop-project", "stop-project", domain.CreateProject{Name: "Stop", Slug: "stop"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.CreateEnvironment(ctx, admin.ID, "stop-environment", "stop-environment", domain.CreateEnvironment{
		ProjectID: project.Value.ID, Name: "Development", Slug: "development", ProtectionPolicy: domain.EnvironmentDevelopment,
	})
	if err != nil {
		t.Fatal(err)
	}
	application, err := store.CreateApplication(ctx, admin.ID, "stop-application", "stop-application", domain.CreateApplication{
		ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID, Name: "API", Slug: "api",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	binding, err := gitprojection.NewGitHubEnvironmentBinding("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", project.Value.ID, environment.Value.ID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 11, RepositoryID: 12, Owner: "kuberploy", Name: "desired-state"}, "refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	binding.State = gitprojection.BindingReady
	binding.TargetHeadRevision, binding.IndexedRevision = strings.Repeat("c", 40), strings.Repeat("c", 40)
	binding.ProjectionGeneration = 1
	binding.TargetHeadObservedAt, binding.IndexedAt, binding.UpdatedAt = now.Add(time.Second), now.Add(time.Second), now.Add(time.Second)
	if err = store.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	chartDigest := "sha256:" + strings.Repeat("d", 64)
	createPlan := gitprojection.WritePlan{BindingID: binding.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID,
		ApplicationID: application.Value.ID, BaseRevision: binding.IndexedRevision, Precondition: gitprojection.MutationCreateIfAbsent,
		ChartDigest: chartDigest, PolicyVersion: "appconfig-v1alpha1"}
	created, operation, err := store.CreateDeployment(ctx, admin.ID, "deploy", "deploy", "request-deploy", domain.CreateDeployment{
		EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID,
		Image: "registry.example.test/api@sha256:" + strings.Repeat("a", 64), Replicas: 1, Port: 8080,
	}, &createPlan)
	if err != nil {
		t.Fatal(err)
	}
	started, execute, err := store.StartOperation(ctx, operation.ID, operation.Generation, "stop-test-worker", time.Minute)
	if err != nil || !execute {
		t.Fatalf("start deploy=%#v execute=%t err=%v", started, execute, err)
	}
	if err = store.CompleteGitOperation(ctx, operation.ID, operation.Generation, "stop-test-worker", domain.GitPublicationResult{Mode: "direct", Revision: binding.IndexedRevision}); err != nil {
		t.Fatal(err)
	}
	document, err := gitprojection.NewDocument(binding, 1, application.Value.ID, binding.IndexedRevision, binding.IndexedRevision,
		strings.Repeat("e", 40), created.Value.ConfigRaw, nil, nil, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutProjectionDocument(ctx, document); err != nil {
		t.Fatal(err)
	}
	dependencyPaths, err := gitprojection.DependencyPaths(binding)
	if err != nil {
		t.Fatal(err)
	}
	etag, err := gitprojection.StrongETagWithDependencies(binding, []gitprojection.Document{document}, dependencyPaths, nil, chartDigest, "appconfig-v1alpha1")
	if err != nil {
		t.Fatal(err)
	}
	stopPlan := createPlan
	stopPlan.Precondition, stopPlan.ExpectedETag = gitprojection.MutationMatchETag, etag
	stopped, err := store.StopDeployment(ctx, admin.ID, created.Value.ID, "stop", "stop", "request-stop", &stopPlan)
	if err != nil {
		t.Fatal(err)
	}
	command, err := store.AcceptedGitWriteCommand(stopped.Value.ID)
	if err != nil || command.Action != gitprojection.MutationDelete {
		t.Fatalf("command=%#v err=%v", command, err)
	}
	started, execute, err = store.StartOperation(ctx, stopped.Value.ID, stopped.Value.Generation, "stop-test-worker", time.Minute)
	if err != nil || !execute {
		t.Fatalf("start stop=%#v execute=%t err=%v", started, execute, err)
	}
	stopRevision := strings.Repeat("f", 40)
	if err = store.CompleteGitOperation(ctx, stopped.Value.ID, stopped.Value.Generation, "stop-test-worker", domain.GitPublicationResult{Mode: "direct", Revision: stopRevision}); err != nil {
		t.Fatal(err)
	}
	deployment, err := store.GetDeployment(ctx, created.Value.ID)
	if err != nil || deployment.State != "stopped" || deployment.DesiredRevision != stopRevision {
		t.Fatalf("deployment=%#v err=%v", deployment, err)
	}
	placements, err := store.ListEnvironmentAppPlacementsForActor(ctx, admin.ID, environment.Value.ID)
	if err != nil || len(placements) != 1 || placements[0].DesiredState != domain.EnvironmentAppPlacementStopped {
		t.Fatalf("placements=%#v err=%v", placements, err)
	}
	if replay, err := store.DeleteApplication(ctx, admin.ID, application.Value.ID, application.Value.Name, "delete", "delete", "request-delete"); err != nil || replay {
		t.Fatalf("delete replay=%t err=%v", replay, err)
	}
}
