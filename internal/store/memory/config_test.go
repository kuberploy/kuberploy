package memory

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
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

func TestDeploymentConfigSaveAdvancesMaterializedImage(t *testing.T) {
	ctx := t.Context()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	project, _ := store.CreateProject(ctx, admin.ID, "image-project", "image-project", domain.CreateProject{Name: "Image", Slug: "image"})
	environment, _ := store.CreateEnvironment(ctx, admin.ID, "image-environment", "image-environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Dev", Slug: "dev"})
	application, _ := store.CreateApplication(ctx, admin.ID, "image-application", "image-application", domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	initialImage := "registry.example/api@sha256:" + strings.Repeat("a", 64)
	deployment, _, err := store.CreateDeployment(ctx, admin.ID, "image-deployment", "image-deployment", "request",
		domain.CreateDeployment{EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID, Image: initialImage, Runtime: domain.DefaultWorkloadRuntime(8080, nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	config, err := store.GetDeploymentConfigForActor(ctx, admin.ID, deployment.Value.ID)
	if err != nil {
		t.Fatal(err)
	}
	intent, intentDigest, diagnostics := appconfig.AutoDeployIntentTemplate(config.RawYAML)
	if len(diagnostics) != 0 {
		t.Fatalf("intent diagnostics=%#v", diagnostics)
	}
	updatedImage := "registry.example/api@sha256:" + strings.Repeat("b", 64)
	candidate := appconfig.ApplyAutoDeployImage(config.RawYAML, intent, intentDigest, updatedImage)
	if len(candidate.Diagnostics) != 0 {
		t.Fatalf("candidate diagnostics=%#v", candidate.Diagnostics)
	}
	tokenHash := sha256.Sum256([]byte("image-preview-token"))
	if err = store.CreateDeploymentConfigPreview(ctx, admin.ID, domain.CreateConfigPreview{DeploymentID: deployment.Value.ID,
		BaseETag: config.ETag, TokenHash: tokenHash[:], CandidateHash: candidate.Hash, CandidateRaw: candidate.Raw,
		Runtime: candidate.Runtime, ExpiresAt: time.Now().Add(time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	saved, operation, err := store.SaveDeploymentConfig(ctx, admin.ID, "image-save", "image-save", "request", domain.SaveDeploymentConfig{
		DeploymentID: deployment.Value.ID, BaseETag: config.ETag, TokenHash: tokenHash[:], CandidateHash: candidate.Hash,
		RawYAML: candidate.Raw, Runtime: domain.WorkloadRuntime{}}, nil)
	if err != nil || saved.Value.Image != updatedImage {
		t.Fatalf("saved image=%q err=%v", saved.Value.Image, err)
	}
	snapshot, err := store.GetDeploymentForOperation(ctx, operation.ID)
	if err != nil || snapshot.Image != updatedImage {
		t.Fatalf("operation image=%q err=%v", snapshot.Image, err)
	}
}
