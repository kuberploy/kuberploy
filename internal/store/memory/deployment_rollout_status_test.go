package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func rolloutStatusFixture(t *testing.T) (*Store, domain.User, domain.Deployment, domain.Environment, domain.Application) {
	t.Helper()
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	project, err := store.CreateProject(ctx, admin.ID, "rollout-project", "rollout-project", domain.CreateProject{Name: "Rollout", Slug: "rollout"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.CreateEnvironment(ctx, admin.ID, "rollout-environment", "rollout-environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := store.CreateApplication(ctx, admin.ID, "rollout-application", "rollout-application", domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := store.CreateDeployment(ctx, admin.ID, "rollout-deployment", "rollout-deployment", "request", domain.CreateDeployment{EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID, Image: "registry.test/api@sha256:" + strings.Repeat("a", 64), Runtime: domain.DefaultWorkloadRuntime(8080, nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	deployment := store.deployments[created.Value.ID]
	deployment.DesiredRevision = strings.Repeat("b", 40)
	store.deployments[deployment.ID] = deployment
	store.mu.Unlock()
	return store, admin, deployment, environment.Value, application.Value
}

func TestDeploymentStatusProjectsOnlyExactFreshArgoObservation(t *testing.T) {
	ctx := context.Background()
	store, admin, deployment, environment, application := rolloutStatusFixture(t)
	status, err := store.DeploymentStatusForActor(ctx, admin.ID, deployment.ID)
	if err != nil || status.ArgoSyncStatus != "unknown" || status.RolloutHealth != "unknown" || status.ArgoObservedAt != nil {
		t.Fatalf("missing observation status=%#v err=%v", status, err)
	}
	now := time.Now().UTC()
	exact := domain.ArgoRolloutObservation{DeploymentID: deployment.ID, ApplicationID: application.ID, ProjectID: application.ProjectID,
		EnvironmentID: environment.ID, DestinationNamespace: environment.Namespace, DesiredRevision: deployment.DesiredRevision,
		ObservedRevision: deployment.DesiredRevision, SyncStatus: "synced", HealthStatus: "healthy", ObservedAt: now}
	store.PutArgoRolloutObservation(ctx, exact)
	status, err = store.DeploymentStatusForActor(ctx, admin.ID, deployment.ID)
	if err != nil || status.ArgoSyncStatus != "synced" || status.RolloutHealth != "healthy" || status.ArgoObservedRevision != deployment.DesiredRevision || status.ArgoObservedAt == nil {
		t.Fatalf("healthy observation status=%#v err=%v", status, err)
	}
	contradictory := exact
	contradictory.ObservedRevision = strings.Repeat("e", 40)
	store.PutArgoRolloutObservation(ctx, contradictory)
	status, err = store.DeploymentStatusForActor(ctx, admin.ID, deployment.ID)
	if err != nil || status.ArgoSyncStatus != "unknown" || status.RolloutHealth != "unknown" || status.ArgoObservedRevision != "" || status.ArgoObservedAt != nil {
		t.Fatalf("contradictory synced/healthy revision leaked=%#v err=%v", status, err)
	}
	exact.SyncStatus, exact.HealthStatus, exact.ObservedRevision = "out-of-sync", "degraded", strings.Repeat("c", 40)
	store.PutArgoRolloutObservation(ctx, exact)
	status, _ = store.DeploymentStatus(ctx, deployment.ID)
	if status.ArgoSyncStatus != "out-of-sync" || status.RolloutHealth != "degraded" || status.ArgoObservedRevision != exact.ObservedRevision {
		t.Fatalf("degraded observation status=%#v", status)
	}
	for name, mutate := range map[string]func(*domain.ArgoRolloutObservation){
		"application substituted": func(value *domain.ArgoRolloutObservation) {
			value.ApplicationID = "11111111-1111-4111-8111-111111111111"
		},
		"environment substituted": func(value *domain.ArgoRolloutObservation) {
			value.EnvironmentID = "22222222-2222-4222-8222-222222222222"
		},
		"project substituted":    func(value *domain.ArgoRolloutObservation) { value.ProjectID = "33333333-3333-4333-8333-333333333333" },
		"namespace substituted":  func(value *domain.ArgoRolloutObservation) { value.DestinationNamespace = "attacker" },
		"desired revision stale": func(value *domain.ArgoRolloutObservation) { value.DesiredRevision = strings.Repeat("d", 40) },
		"observation stale": func(value *domain.ArgoRolloutObservation) {
			value.ObservedAt = now.Add(-argoRolloutObservationMaxAge - time.Second)
		},
	} {
		t.Run(name, func(t *testing.T) {
			forged := exact
			mutate(&forged)
			store.PutArgoRolloutObservation(ctx, forged)
			got, getErr := store.DeploymentStatusForActor(ctx, admin.ID, deployment.ID)
			if getErr != nil || got.ArgoSyncStatus != "unknown" || got.RolloutHealth != "unknown" || got.ArgoObservedRevision != "" || got.ArgoObservedAt != nil {
				t.Fatalf("forged observation status=%#v err=%v", got, getErr)
			}
		})
	}
}
