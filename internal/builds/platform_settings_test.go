package builds

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
)

func TestBuilderPlatformSettingsDriveNewExecutionAndReplay(t *testing.T) {
	store := NewMemoryStore()
	config := testWorkerRuntimeConfig()
	service := &BuilderPlatformSettingsService{Store: store, Defaults: DefaultBuilderPlatformSettings(config), Now: func() time.Time {
		return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	}}
	current, err := service.Current(t.Context())
	if err != nil || current.Revision != 0 || current.MaxConcurrentBuilders != 1 {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	input := current.Input()
	input.NodeIsolation = true
	input.MaxConcurrentBuilders = 4
	input.DinDResources.CPULimit = "6"
	actor := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	updated, replay, err := service.Update(t.Context(), actor, "settings-1", "sha256:"+string(make([]byte, 64)), 0, input)
	if err == nil {
		t.Fatal("nonhex fingerprint accepted")
	}
	fingerprint := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	updated, replay, err = service.Update(t.Context(), actor, "settings-1", fingerprint, 0, input)
	if err != nil || replay || updated.Revision != 1 {
		t.Fatalf("update=%+v replay=%v err=%v", updated, replay, err)
	}
	replayed, replay, err := service.Update(t.Context(), actor, "settings-1", fingerprint, 0, input)
	if err != nil || !replay || replayed.Revision != 1 {
		t.Fatalf("replay=%+v replay=%v err=%v", replayed, replay, err)
	}
	execution, err := config.ExecutionSettingsForPlatform(5000, updated)
	if err != nil || execution.NodeSelector["kuberploy.io/node-class"] != "dind-builder" || execution.DinDResources.CPULimit != "6" {
		t.Fatalf("execution=%+v err=%v", execution, err)
	}
}

func TestBuilderNodeIsolationCanCreateDefinition(t *testing.T) {
	config := testWorkerRuntimeConfig()
	settings := DefaultBuilderPlatformSettings(config)
	settings.NodeIsolation = true
	execution, err := config.ExecutionSettingsForPlatform(5000, settings)
	if err != nil {
		t.Fatal(err)
	}
	_, err = PrepareDefinition(BuildDefinition{
		ID: testDefinitionID, ProjectID: testProjectID, ServiceID: testServiceID,
		InstallationID: testInstallationID, RepositoryID: testRepositoryID,
		TriggerRef: "refs/heads/main", Enabled: true,
		Spec: DefinitionSpec{
			ContextPath: ".", DockerfilePath: "Dockerfile", Platforms: []string{"linux/amd64"},
			Registry: RegistryBinding{TargetID: testRegistryID, Mode: RegistryManaged, Server: "registry.test:5000", RepositoryPrefix: "kuberploy",
				PushCredentialSecret: "registry-push", CacheCredentialSecret: "registry-cache"},
			CacheTrustLane: "protected", CacheImports: 2,
			Profile:   builder.BuildProfile{Resource: "standard", TimeoutSeconds: 900, Egress: "registry-and-source"},
			Execution: execution, MaxAttempts: 3,
		},
	}, testNow)
	if err != nil {
		t.Fatalf("node-isolated definition rejected: %v", err)
	}
}

func TestClaimNextAttemptHonorsPlatformConcurrency(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	store.attempts["aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"] = BuildAttempt{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", State: AttemptQueued, AvailableAt: now, CreatedAt: now}
	store.attempts["bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"] = BuildAttempt{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", State: AttemptQueued, AvailableAt: now, CreatedAt: now.Add(time.Second)}
	first, err := store.ClaimNextAttempt(context.Background(), "worker-a", now, time.Minute, 1)
	if err != nil || first.State != AttemptPreparing {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err = store.ClaimNextAttempt(context.Background(), "worker-b", now, time.Minute, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("queue exceeded max concurrency: %v", err)
	}
	second, err := store.ClaimNextAttempt(context.Background(), "worker-b", now, time.Minute, 2)
	if err != nil || second.ID == first.ID || second.State != AttemptPreparing {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}

func TestBuilderPlatformSettingsRejectIncompleteResources(t *testing.T) {
	settings := BuilderPlatformSettings{MaxConcurrentBuilders: 1, CheckoutResources: builder.ContainerResources{},
		DinDResources: builder.ContainerResources{}, AgentResources: builder.ContainerResources{}}
	if !errors.Is(settings.Validate(), ErrInvalid) {
		t.Fatal("incomplete builder resources accepted")
	}
}
