package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/githubapp"
)

type retryExecutionStore struct {
	builds.APIStore
	source            builds.BuildAttempt
	definition        builds.BuildDefinition
	existing          *builds.BuildAttempt
	commandReplay     bool
	capturedExecution builds.ExecutionSettings
	historicalReads   int
}

func (s *retryExecutionStore) Attempt(_ context.Context, attemptID string) (builds.BuildAttempt, error) {
	if attemptID == s.source.ID {
		return s.source, nil
	}
	if s.existing != nil && attemptID == s.existing.ID {
		return *s.existing, nil
	}
	return builds.BuildAttempt{}, builds.ErrNotFound
}

func (s *retryExecutionStore) HistoricalAttempt(_ context.Context, attemptID string) (builds.BuildAttempt, error) {
	s.historicalReads++
	if attemptID == s.source.ID {
		return s.source, nil
	}
	return builds.BuildAttempt{}, builds.ErrNotFound
}

func (s *retryExecutionStore) Definition(context.Context, string) (builds.BuildDefinition, error) {
	return s.definition, nil
}

func (s *retryExecutionStore) ClaimAPICommand(_ context.Context, _, _, _, _, _, resourceID string, _ time.Time) (string, bool, error) {
	return resourceID, s.commandReplay, nil
}

func (s *retryExecutionStore) RetryAttempt(_ context.Context, _, retryID, claimKey string, execution builds.ExecutionSettings, now time.Time) (builds.BuildAttempt, bool, error) {
	s.capturedExecution = execution
	return builds.BuildAttempt{ID: retryID, DefinitionID: s.definition.ID, DeliveryClaimKey: claimKey, TriggerKind: "github_push", TriggerKey: claimKey, State: builds.AttemptQueued, CreatedAt: now}, false, nil
}

type retryExecutionResolver struct {
	resolution BuildDefinitionResolution
	err        error
	calls      int
}

func (r *retryExecutionResolver) ResolveBuildDefinition(context.Context, string, string, string, string) (BuildDefinitionResolution, error) {
	r.calls++
	return r.resolution, r.err
}

func TestBuildRetryRefreshesTrustedExecutionAndReplayKeepsAcceptedAttempt(t *testing.T) {
	definitionID := "11111111-1111-4111-8111-111111111111"
	sourceID := "22222222-2222-4222-8222-222222222222"
	targetID := "33333333-3333-4333-8333-333333333333"
	definition := builds.BuildDefinition{ID: definitionID, ProjectID: "44444444-4444-4444-8444-444444444444", ServiceID: "55555555-5555-4555-8555-555555555555",
		Spec: builds.DefinitionSpec{Registry: builds.RegistryBinding{TargetID: targetID}}}
	source := builds.BuildAttempt{ID: sourceID, DefinitionID: definitionID, State: builds.AttemptFailed}
	current := builds.ExecutionSettings{BuilderAgentImage: "registry.test/builder@sha256:" + strings.Repeat("9", 64)}
	store := &retryExecutionStore{source: source, definition: definition}
	resolver := &retryExecutionResolver{resolution: BuildDefinitionResolution{Registry: builds.RegistryBinding{TargetID: targetID}, Execution: current}}
	backend, err := NewBuildBackendWithClock(store, resolver, func() time.Time { return time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}

	attempt, replay, err := backend.Retry(t.Context(), "66666666-6666-4666-8666-666666666666", sourceID, "retry-runtime-0001", "sha256:"+strings.Repeat("a", 64))
	if err != nil || replay || attempt.State != builds.AttemptQueued || store.capturedExecution.BuilderAgentImage != current.BuilderAgentImage || resolver.calls != 1 {
		t.Fatalf("attempt=%#v replay=%v captured=%#v resolverCalls=%d err=%v", attempt, replay, store.capturedExecution, resolver.calls, err)
	}

	store.commandReplay = true
	store.existing = &attempt
	resolver.err = errors.New("mutable resolver must not run for accepted replay")
	replayed, replay, err := backend.Retry(t.Context(), "66666666-6666-4666-8666-666666666666", sourceID, "retry-runtime-0001", "sha256:"+strings.Repeat("a", 64))
	if err != nil || !replay || replayed.ID != attempt.ID || resolver.calls != 1 {
		t.Fatalf("replayed=%#v replay=%v resolverCalls=%d err=%v", replayed, replay, resolver.calls, err)
	}
}

func TestBuildBackendReadsHistoricalAttemptProjection(t *testing.T) {
	store := &retryExecutionStore{source: builds.BuildAttempt{ID: "22222222-2222-4222-8222-222222222222"}}
	resolver := &retryExecutionResolver{}
	backend, err := NewBuildBackend(store, resolver)
	if err != nil {
		t.Fatal(err)
	}

	attempt, err := backend.Attempt(t.Context(), store.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ID != store.source.ID || store.historicalReads != 1 {
		t.Fatalf("attempt=%#v historicalReads=%d", attempt, store.historicalReads)
	}
}

func TestBuildBackendEditsOneStableAppSource(t *testing.T) {
	now := time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)
	actorID := "11111111-1111-4111-8111-111111111111"
	projectID := "22222222-2222-4222-8222-222222222222"
	applicationID := "33333333-3333-4333-8333-333333333333"
	installationID := "44444444-4444-4444-8444-444444444444"
	repositoryID := "55555555-5555-4555-8555-555555555555"
	registryID := "66666666-6666-4666-8666-666666666666"
	sourceID := "77777777-7777-4777-8777-777777777777"
	resources := builder.ContainerResources{CPURequest: "100m", MemoryRequest: "128Mi", EphemeralStorageRequest: "256Mi",
		CPULimit: "1", MemoryLimit: "1Gi", EphemeralStorageLimit: "2Gi"}
	execution := builds.ExecutionSettings{
		Namespace: "kuberploy-build-dind", PodServiceAccount: "kuberploy-build-pod",
		BuilderAgentImage: "registry.test/system/builder-agent@sha256:" + strings.Repeat("1", 64), BuildKitImage: builder.DefaultBuildKitImage,
		NodeSelector: map[string]string{}, CheckoutResources: resources, DinDResources: resources, AgentResources: resources,
		WorkspaceSizeLimit: "10Gi", SocketSizeLimit: "16Mi", ResultSizeLimit: "1Mi", DockerDataSizeLimit: "20Gi",
		ActiveDeadlineSeconds: 1800, TTLSecondsAfterFinished: 3600,
		Egress: []builder.EgressEndpoint{{CIDR: "192.0.2.10/32", Ports: []int{443}}},
	}
	registry := builds.RegistryBinding{TargetID: registryID, Mode: builds.RegistryManaged, Server: "registry.test", RepositoryPrefix: "kuberploy",
		PushCredentialSecret: "registry-push", CacheCredentialSecret: "registry-cache"}
	original, err := builds.PrepareDefinition(builds.BuildDefinition{
		ID: sourceID, ProjectID: projectID, ServiceID: applicationID, SourceKind: builds.SourceGitHub,
		InstallationID: installationID, RepositoryID: repositoryID, TriggerRef: "refs/heads/main", Enabled: true,
		Spec: builds.DefinitionSpec{ContextPath: ".", DockerfilePath: "Dockerfile", Platforms: []string{"linux/amd64"}, Registry: registry,
			BuildArgs: []builder.BuildArg{{Name: "APP_ENV", Value: "production"}}, CacheTrustLane: "trusted", CacheImports: 1,
			Profile: builder.BuildProfile{Resource: "standard", TimeoutSeconds: 900, Egress: "registry-and-source"}, Execution: execution, MaxAttempts: 3},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	store := builds.NewMemoryStore()
	installation := builds.Installation{ID: installationID, AppID: 12, GitHubInstallationID: 34,
		Account: githubapp.AccountIdentity{ID: 56, Login: "kuberploy", Type: "Organization"}, RepositorySelection: "selected",
		Permissions: githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead},
		Lifecycle:   builds.InstallationActive, LastVerifiedAt: now, UpdatedAt: now}
	repository := builds.Repository{ID: repositoryID, InstallationID: installationID,
		Identity:  githubapp.RepositoryIdentity{ID: 78, OwnerID: 56, OwnerLogin: "kuberploy", Name: "fixture"},
		Lifecycle: builds.RepositoryActive, LastVerifiedAt: now, CreatedAt: now, UpdatedAt: now}
	if err = store.PutInstallation(t.Context(), installation); err != nil {
		t.Fatal(err)
	}
	if err = store.PutRepository(t.Context(), repository); err != nil {
		t.Fatal(err)
	}
	if err = store.PutDefinition(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	resolver := &retryExecutionResolver{resolution: BuildDefinitionResolution{Registry: registry, Execution: execution}}
	backend, err := NewBuildBackendWithClock(store, resolver, func() time.Time { return now.Add(time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	mutation := BuildDefinitionMutation{ApplicationID: applicationID, ProjectID: projectID, InstallationID: installationID,
		RepositoryID: repositoryID, RegistryTargetID: registryID, TriggerRef: "refs/heads/main", ContextPath: ".", DockerfilePath: "Dockerfile.prod",
		Platforms: []string{"linux/amd64"}, CacheTrustLane: "trusted", CacheImports: 1,
		Profile: builder.BuildProfile{Resource: "standard", TimeoutSeconds: 900, Egress: "registry-and-source"}, MaxAttempts: 3,
		ActorID: actorID, IdempotencyKey: "edit-app-source-01", Fingerprint: "sha256:" + strings.Repeat("a", 64)}
	edited, replay, err := backend.CreateDefinition(t.Context(), mutation)
	if err != nil || replay || edited.ID != sourceID || edited.DefinitionGeneration != 2 || edited.Spec.DockerfilePath != "Dockerfile.prod" ||
		len(edited.Spec.BuildArgs) != 1 || edited.Spec.BuildArgs[0].Value != "production" {
		t.Fatalf("edited=%#v replay=%v err=%v", edited, replay, err)
	}
	sources, err := store.DefinitionsForService(t.Context(), applicationID)
	if err != nil || len(sources) != 1 || sources[0].ID != sourceID {
		t.Fatalf("sources=%#v err=%v", sources, err)
	}
	replayed, replay, err := backend.CreateDefinition(t.Context(), mutation)
	if err != nil || !replay || replayed.ID != sourceID || replayed.DefinitionGeneration != 2 {
		t.Fatalf("replayed=%#v replay=%v err=%v", replayed, replay, err)
	}
}
