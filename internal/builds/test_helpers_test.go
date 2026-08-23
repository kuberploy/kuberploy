package builds

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/githubapp"
)

const (
	testInstallationID  = "11111111-1111-4111-8111-111111111111"
	testProjectID       = "22222222-2222-4222-8222-222222222222"
	testServiceID       = "33333333-3333-4333-8333-333333333333"
	testRepositoryID    = "44444444-4444-4444-8444-444444444444"
	testRegistryID      = "55555555-5555-4555-8555-555555555555"
	testDefinitionID    = "66666666-6666-4666-8666-666666666666"
	testAppID           = int64(99)
	testProviderInstall = int64(77)
	testAccountID       = int64(10)
	testProviderRepo    = int64(20)
)

var testNow = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

func validInstallation(now time.Time) Installation {
	return Installation{
		ID: testInstallationID, AppID: testAppID, GitHubInstallationID: testProviderInstall,
		Account:             githubapp.AccountIdentity{ID: testAccountID, Login: "kuberploy", Type: "Organization"},
		RepositorySelection: "selected", Permissions: githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead},
		Lifecycle: InstallationActive, LastVerifiedAt: now, UpdatedAt: now,
	}
}

func repositoryFixture(now time.Time) Repository {
	return Repository{
		ID: testRepositoryID, InstallationID: testInstallationID,
		Identity:  githubapp.RepositoryIdentity{ID: testProviderRepo, Name: "demo", OwnerID: testAccountID, OwnerLogin: "kuberploy"},
		Lifecycle: RepositoryActive, LastVerifiedAt: now, CreatedAt: now, UpdatedAt: now,
	}
}

func testResources() builder.ContainerResources {
	return builder.ContainerResources{
		CPURequest: "100m", MemoryRequest: "128Mi", EphemeralStorageRequest: "256Mi",
		CPULimit: "1000m", MemoryLimit: "1Gi", EphemeralStorageLimit: "2Gi",
	}
}

func validDefinition(t *testing.T, now time.Time, mode RegistryMode) BuildDefinition {
	return definitionWithIDs(t, now, mode, testDefinitionID, testProjectID, testServiceID, testInstallationID, testRepositoryID, testRegistryID)
}

func definitionWithIDs(t *testing.T, now time.Time, mode RegistryMode, definitionID, projectID, serviceID, installationID, repositoryID, registryID string) BuildDefinition {
	t.Helper()
	definition, err := PrepareDefinition(BuildDefinition{
		ID: definitionID, ProjectID: projectID, ServiceID: serviceID,
		InstallationID: installationID, RepositoryID: repositoryID, TriggerRef: "refs/heads/main", Enabled: true,
		Spec: DefinitionSpec{
			ContextPath: ".", DockerfilePath: "Dockerfile", Platforms: []string{"linux/arm64", "linux/amd64"},
			Registry: RegistryBinding{TargetID: registryID, Mode: mode, Server: "registry.test", RepositoryPrefix: "kuberploy",
				PushCredentialSecret: "registry-push", CacheCredentialSecret: "registry-cache"},
			BuildArgs: []builder.BuildArg{{Name: "APP_ENV", Value: "production"}}, CacheTrustLane: "trusted", CacheImports: 2,
			Profile: builder.BuildProfile{Resource: "standard", TimeoutSeconds: 900, Egress: "registry-and-source"}, MaxAttempts: 3,
			Execution: ExecutionSettings{
				Namespace: "kuberploy-build-dind", PodServiceAccount: "kuberploy-build-pod",
				BuilderAgentImage: "registry.test/system/builder-agent@sha256:" + strings.Repeat("1", 64),
				BuildKitImage:     builder.DefaultBuildKitImage,
				NodeSelector:      map[string]string{"kuberploy.io/node-class": "dind-builder", "kubernetes.io/arch": "amd64"},
				Toleration:        builder.TaintToleration{Key: "kuberploy.io/dind-builder", Value: "true", Effect: "NoSchedule"},
				CheckoutResources: testResources(), DinDResources: testResources(), AgentResources: testResources(),
				WorkspaceSizeLimit: "10Gi", SocketSizeLimit: "16Mi", ResultSizeLimit: "1Mi", DockerDataSizeLimit: "20Gi",
				ActiveDeadlineSeconds: 1800, TTLSecondsAfterFinished: 3600,
				Egress: []builder.EgressEndpoint{{CIDR: "198.51.100.10/32", Ports: []int{5000}}, {CIDR: "192.0.2.10/32", Ports: []int{443}}},
			},
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

func seedMemory(t *testing.T, mode RegistryMode) (*MemoryStore, BuildDefinition) {
	t.Helper()
	store := NewMemoryStore()
	definition := validDefinition(t, testNow, mode)
	if err := store.PutInstallation(context.Background(), validInstallation(testNow)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRepository(context.Background(), repositoryFixture(testNow)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutDefinition(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	return store, definition
}

func pushBody(t *testing.T, after string) []byte {
	t.Helper()
	payload := map[string]any{
		"ref": "refs/heads/main", "after": after, "created": false, "deleted": false, "forced": false,
		"repository":   map[string]any{"id": testProviderRepo, "name": "demo", "full_name": "kuberploy/demo", "owner": map[string]any{"id": testAccountID, "login": "kuberploy", "type": "Organization"}},
		"installation": map[string]any{"id": testProviderInstall},
		"sender":       map[string]any{"id": int64(30), "login": "builder-user", "type": "User"},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type staticVerifier struct{ envelope githubapp.WebhookEnvelope }

func (v staticVerifier) Verify(context.Context, http.Header, io.Reader) (githubapp.WebhookEnvelope, error) {
	envelope := v.envelope
	envelope.Body = append([]byte(nil), v.envelope.Body...)
	return envelope, nil
}

func testEnvelope(t *testing.T, deliveryID, after string, now time.Time) githubapp.WebhookEnvelope {
	return githubapp.WebhookEnvelope{AppID: testAppID, Event: "push", DeliveryID: deliveryID, Body: pushBody(t, after), ReceivedAt: now, ReplayUntil: now.Add(24 * time.Hour)}
}

type fakeProvider struct {
	mu             sync.Mutex
	resolvedCommit string
	now            time.Time
	transient      int
	resolveHook    func()
	verifyCalls    int
	mintCalls      int
	resolveCalls   int
}

func (p *fakeProvider) VerifyInstallation(context.Context, int64, githubapp.AccountIdentity, githubapp.Permissions) (githubapp.Installation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verifyCalls++
	if p.transient > 0 {
		p.transient--
		return githubapp.Installation{}, githubapp.ErrTransport
	}
	return githubapp.Installation{}, nil
}
func (p *fakeProvider) MintInstallationToken(context.Context, githubapp.TokenRequest) (githubapp.InstallationToken, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mintCalls++
	return githubapp.InstallationToken{}, nil
}
func (p *fakeProvider) ResolveEventRef(_ context.Context, _ githubapp.InstallationToken, event githubapp.PushEvent) (githubapp.ResolvedRef, error) {
	p.mu.Lock()
	p.resolveCalls++
	hook := p.resolveHook
	p.resolveHook = nil
	resolvedCommit, now := p.resolvedCommit, p.now
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	if event.UntrustedAfter == resolvedCommit {
		return githubapp.ResolvedRef{}, errors.New("test did not distinguish webhook SHA")
	}
	return githubapp.ResolvedRef{Ref: event.Ref, CommitSHA: resolvedCommit, ResolvedAt: now}, nil
}

func webhookService(store Store, provider *fakeProvider, envelope githubapp.WebhookEnvelope, clock *time.Time) *WebhookService {
	return &WebhookService{Verifier: staticVerifier{envelope}, Provider: provider, Store: store, Owner: "webhook-worker", LeaseDuration: time.Minute, Runtime: testWorkerRuntimeConfig(), Now: func() time.Time { return *clock }}
}

func testWorkerRuntimeConfig() WorkerRuntimeConfig {
	config, err := WorkerRuntimeConfigFromLookup(mapLookup(map[string]string{
		GitHubBuildsEnabledEnv: "true", GitHubAppIDEnv: "99", GitHubAppClientIDEnv: "Iv1_TestClient",
		BuilderNamespaceEnv: "kuberploy-build-dind", BuilderPodServiceAccountEnv: "kuberploy-build-pod",
		BuilderAgentImageEnv:        "registry.test/system/builder-agent@sha256:" + strings.Repeat("2", 64),
		BuilderBuildKitImageEnv:     builder.DefaultBuildKitImage,
		BuilderSourceEgressCIDRsEnv: "192.0.2.10/32", BuilderRegistryEgressCIDRsEnv: "198.51.100.10/32",
		"KUBERPLOY_KUBE_API_SERVER_CIDRS": "10.43.0.1/32",
	}))
	if err != nil {
		panic(err)
	}
	return config
}

func storedAttemptDefinitions(definitions []BuildDefinition) []AttemptDefinition {
	result := make([]AttemptDefinition, len(definitions))
	for index := range definitions {
		result[index] = AttemptDefinition{Definition: definitions[index], Execution: definitions[index].Spec.Execution}
	}
	return result
}

type fakeKubernetes struct {
	mu           sync.Mutex
	errorCount   int
	capacityErr  error
	cancelErrors int
	mismatch     bool
	state        WorkloadState
	promoted     bool
	workloads    []BuildWorkload
	cancelled    []string
}

func (k *fakeKubernetes) BuilderCapacityReady(context.Context) error { return k.capacityErr }

func (k *fakeKubernetes) Ensure(_ context.Context, workload BuildWorkload) (WorkloadObservation, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.workloads = append(k.workloads, workload)
	if k.errorCount > 0 {
		k.errorCount--
		return WorkloadObservation{}, errors.New("infrastructure unavailable")
	}
	job, policy, digest := workload.Plan.Job, workload.Plan.NetworkPolicy, workload.InputDigest
	if k.mismatch {
		job = map[string]any{"kind": "Job"}
	}
	observation := WorkloadObservation{State: k.state, Job: job, NetworkPolicy: policy, InputDigest: digest}
	if k.state == WorkloadSucceeded {
		result := builder.BuildResult{
			APIVersion: builder.ProtocolVersion, OperationID: workload.Attempt.ID, Generation: workload.Attempt.Generation, Status: "Succeeded",
			Image:      builder.Image{Reference: workload.Attempt.PlanRequest.Build.Destination.Repository + "@sha256:" + strings.Repeat("c", 64), Digest: "sha256:" + strings.Repeat("c", 64), Platforms: workload.Attempt.PlanRequest.Build.Platforms},
			CacheReuse: builder.CacheReuseHit,
			StartedAt:  testNow, CompletedAt: testNow.Add(time.Minute),
		}
		observation.Result, _ = json.Marshal(result)
		observation.LogReference = "k8s://kuberploy-build-dind/pods/build-pod/containers/agent"
	}
	return observation, nil
}
func (k *fakeKubernetes) Cancel(_ context.Context, attempt BuildAttempt) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.cancelled = append(k.cancelled, attempt.ID)
	if k.cancelErrors > 0 {
		k.cancelErrors--
		return errors.New("cancellation unavailable")
	}
	return nil
}
func (k *fakeKubernetes) PromoteCache(context.Context, BuildAttempt, string) (bool, error) {
	return k.promoted, nil
}

func createAttempt(t *testing.T, store *MemoryStore, mode RegistryMode, clock *time.Time) BuildAttempt {
	t.Helper()
	provider := &fakeProvider{resolvedCommit: strings.Repeat("b", 40), now: *clock}
	envelope := testEnvelope(t, "11111111-2222-4333-8444-555555555555", strings.Repeat("a", 40), *clock)
	outcome, err := webhookService(store, provider, envelope, clock).Handle(context.Background(), http.Header{}, strings.NewReader("ignored"))
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.AttemptIDs) != 1 {
		t.Fatalf("attempts=%v", outcome.AttemptIDs)
	}
	attempt, err := store.Attempt(context.Background(), outcome.AttemptIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if attempt.RegistryMode != mode {
		t.Fatalf("mode=%s", attempt.RegistryMode)
	}
	return attempt
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
