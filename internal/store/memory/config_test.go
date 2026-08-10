package memory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestDeploymentAcceptanceSnapshotsConfigAndGETIsReadOnly(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	project, err := store.CreateProject(ctx, admin.ID, "project", "project", domain.CreateProject{Name: "Project", Slug: "project"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.CreateEnvironment(ctx, admin.ID, "environment", "environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Development", Slug: "development"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := store.CreateApplication(ctx, admin.ID, "application", "application", domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	if err != nil {
		t.Fatal(err)
	}
	deployment, operation, err := store.CreateDeployment(ctx, admin.ID, "deployment", "deployment", "request", domain.CreateDeployment{EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID, Image: "registry.example/api@sha256:" + strings.Repeat("a", 64), Runtime: domain.DefaultWorkloadRuntime(8080, nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.GetDeploymentConfigForActor(ctx, admin.ID, deployment.Value.ID)
	if err != nil || len(state.RawYAML) == 0 {
		t.Fatalf("config state=%#v err=%v", state, err)
	}
	snapshot, err := store.GetDeploymentForOperation(ctx, operation.ID)
	if err != nil || string(snapshot.ConfigRaw) != string(state.RawYAML) {
		t.Fatalf("operation snapshot mismatch: %q err=%v", snapshot.ConfigRaw, err)
	}
	release, releaseOperation, err := store.CreateDeployment(ctx, admin.ID, "deployment-v2", "deployment-v2", "request-v2", domain.CreateDeployment{EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID, Image: "registry.example/api@sha256:" + strings.Repeat("b", 64), Runtime: domain.DefaultWorkloadRuntime(8080, nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	releaseState, err := store.GetDeploymentConfigForActor(ctx, admin.ID, release.Value.ID)
	if err != nil || releaseState.ETag == state.ETag || !strings.Contains(string(releaseState.RawYAML), strings.Repeat("b", 64)) {
		t.Fatalf("release did not atomically advance config projection: %#v err=%v", releaseState, err)
	}
	releaseSnapshot, err := store.GetDeploymentForOperation(ctx, releaseOperation.ID)
	if err != nil || string(releaseSnapshot.ConfigRaw) != string(releaseState.RawYAML) {
		t.Fatalf("release operation snapshot mismatch: %q err=%v", releaseSnapshot.ConfigRaw, err)
	}

	store.mu.Lock()
	legacy := store.deployments[release.Value.ID]
	legacy.ConfigRaw, legacy.ConfigVersion = nil, 0
	store.deployments[legacy.ID] = legacy
	store.mu.Unlock()
	if _, err = store.GetDeploymentConfigForActor(ctx, admin.ID, legacy.ID); !errors.Is(err, base.ErrConfigProjectionMissing) {
		t.Fatalf("GET silently repaired missing projection: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.deployments[legacy.ID].ConfigRaw) != 0 {
		t.Fatal("read-only GET mutated missing projection")
	}
}
