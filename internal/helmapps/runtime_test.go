package helmapps

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

func validRuntimeConfig(t *testing.T) RuntimeConfig {
	t.Helper()
	_, payload, runtime := protectedApplicationFixture(t)
	return RuntimeConfig{Enabled: true,
		Renderer: KubernetesRendererConfig{Namespace: "kuberploy-helm-renderer",
			ServiceAccount: "kuberploy-helm-renderer", PollInterval: 100 * time.Millisecond},
		WorkPollInterval: time.Second, RenderLeaseDuration: time.Minute, PublishLeaseDuration: time.Minute,
		ReadinessLeaseDuration: 30 * time.Second, OCIRequestTimeout: 15 * time.Second,
		OCIRegistryHosts:  []string{"ghcr.io", "registry.example.com"},
		OCIAuthHosts:      []string{"auth.example.com", "ghcr.io"},
		OCIRedirectHosts:  []string{"pkg-containers.githubusercontent.com"},
		PackageCacheBytes: 64 << 20, Application: runtime, Publisher: payload.Publisher}
}

func TestRuntimeConfigDisabledIsStrictlyDormant(t *testing.T) {
	config := RuntimeConfig{Enabled: false,
		OCIRegistryHosts: []string{"HTTPS://UNSAFE"}, PackageCacheBytes: -1}
	if err := config.Validate(); err != nil {
		t.Fatalf("disabled configuration should be dormant: %v", err)
	}
	runtime, err := NewRuntime(config, RuntimeDependencies{})
	if err != nil || runtime.Enabled || runtime.Store != nil || runtime.Releases != nil || runtime.Publications != nil || runtime.Publisher != nil {
		t.Fatalf("disabled runtime constructed dependencies: runtime=%+v err=%v", runtime, err)
	}
	apiRuntime, err := NewAPIRuntime(config, APIRuntimeDependencies{})
	if err != nil || apiRuntime.Enabled || apiRuntime.Releases != nil || apiRuntime.Values != nil {
		t.Fatalf("disabled API runtime constructed dependencies: runtime=%+v err=%v", apiRuntime, err)
	}
	capabilities, err := apiRuntime.Capabilities(context.Background())
	if err != nil || capabilities != (Capabilities{}) {
		t.Fatalf("disabled capabilities=%+v err=%v", capabilities, err)
	}
}

func TestRuntimeConfigEnabledFailsClosedOnEveryPartialPrerequisite(t *testing.T) {
	valid := validRuntimeConfig(t)
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	mutations := []RuntimeConfig{
		func() RuntimeConfig { value := valid; value.Renderer.Namespace = ""; return value }(),
		func() RuntimeConfig { value := valid; value.Renderer.ServiceAccount = "UPPER"; return value }(),
		func() RuntimeConfig { value := valid; value.Renderer.PollInterval = time.Second * 3; return value }(),
		func() RuntimeConfig { value := valid; value.WorkPollInterval = time.Millisecond; return value }(),
		func() RuntimeConfig { value := valid; value.RenderLeaseDuration = RenderTimeout; return value }(),
		func() RuntimeConfig { value := valid; value.PublishLeaseDuration = 5 * time.Second; return value }(),
		func() RuntimeConfig { value := valid; value.ReadinessLeaseDuration = time.Second; return value }(),
		func() RuntimeConfig {
			value := valid
			value.WorkPollInterval = 20 * time.Second
			value.ReadinessLeaseDuration = 30 * time.Second
			return value
		}(),
		func() RuntimeConfig { value := valid; value.OCIRequestTimeout = 100 * time.Millisecond; return value }(),
		func() RuntimeConfig { value := valid; value.PackageCacheBytes = MaximumChartSize - 1; return value }(),
		func() RuntimeConfig { value := valid; value.OCIRegistryHosts = nil; return value }(),
		func() RuntimeConfig {
			value := valid
			value.OCIRegistryHosts = []string{"registry.example.com", "ghcr.io"}
			return value
		}(),
		func() RuntimeConfig {
			value := valid
			value.OCIRegistryHosts = []string{"ghcr.io", "ghcr.io"}
			return value
		}(),
		func() RuntimeConfig {
			value := valid
			value.OCIAuthHosts = []string{"HTTPS://auth.example.com"}
			return value
		}(),
		func() RuntimeConfig {
			value := valid
			value.OCIRedirectHosts = []string{"z.example.com", "a.example.com"}
			return value
		}(),
		func() RuntimeConfig {
			value := valid
			value.OCICredentialProfiles = []OCIRegistryCredentialProfile{{RegistryHost: "unknown.example.com",
				AuthHost: "auth.example.com", Name: "private-main", Mode: OCICredentialModeBasic,
				ProjectionDigest: digestBytes([]byte("profile"))}}
			return value
		}(),
		func() RuntimeConfig {
			value := valid
			value.OCICredentialProfiles = []OCIRegistryCredentialProfile{
				{RegistryHost: "ghcr.io", AuthHost: "ghcr.io", Name: "same", Mode: OCICredentialModeBasic, ProjectionDigest: digestBytes([]byte("one"))},
				{RegistryHost: "registry.example.com", AuthHost: "auth.example.com", Name: "same", Mode: OCICredentialModeBearer, ProjectionDigest: digestBytes([]byte("two"))},
			}
			return value
		}(),
		func() RuntimeConfig { value := valid; value.Application.ArgoNamespace = ""; return value }(),
		func() RuntimeConfig {
			value := valid
			value.Publisher.ConfigDigest = digestBytes([]byte("different"))
			value.Publisher.Contract = "caller"
			return value
		}(),
	}
	for index, mutation := range mutations {
		if mutation.Validate() == nil {
			t.Fatalf("mutation %d unexpectedly validated: %+v", index, mutation)
		}
	}
}

func TestRuntimeConfigIdentityDigestIsDeterministicAndExcludesReplicaIdentity(t *testing.T) {
	config := validRuntimeConfig(t)
	first, err := config.IdentityDigest()
	if err != nil {
		t.Fatal(err)
	}
	second, err := config.IdentityDigest()
	if err != nil || first != second {
		t.Fatalf("replica identity changed runtime contract: first=%s second=%s err=%v", first, second, err)
	}
	changed := config
	changed.OCIRequestTimeout = 20 * time.Second
	third, err := changed.IdentityDigest()
	if err != nil || third == first {
		t.Fatalf("runtime policy change was not digested: first=%s third=%s err=%v", first, third, err)
	}
}

func TestEnabledRuntimeRequiresAndWiresExactProtectedPublisherDependencies(t *testing.T) {
	config := validRuntimeConfig(t)
	fixture := newProtectedPublisherFixture(t)
	_, _, _, argoObservation := protectedArgoReadinessFixture(t, testTime)
	dependencies := RuntimeDependencies{Pool: &pgxpool.Pool{}, OCIClient: &http.Client{},
		RendererAPI: newFakeRendererKubernetesAPI(nil), Bindings: &bindingResolverStub{},
		ArgoMaterialization: ArgoMaterializationAuthority{PolicyDigest: digestBytes([]byte("argo-policy")),
			Runtime: argo.RuntimeLock{ChartRepository: "oci://ghcr.io/kuberploy/charts",
				ChartName: argo.RuntimeChartName, ChartVersion: "1.2.3",
				ChartDigest:   digestBytes([]byte("runtime-chart")),
				RendererImage: "ghcr.io/kuberploy/renderer@" + digestBytes([]byte("renderer"))},
			DigestEnforcement: argo.ChartDigestNativeOCI},
		GitBindings: fixture.bindings, GitProvider: fixture.headVerifier(t), GitManager: fixture.manager,
		ArgoObservation: argoObservation,
		CascadeRoots:    cascadeRootSourceStub{}, CascadeApplications: cascadeApplicationSourceStub{},
		WorkerID: "helm-runtime-worker-0001", WorkerEpoch: 1, StartedAt: testTime,
		Now: func() time.Time { return testTime }, ReportError: func(string, error) {}}
	runtime, err := NewRuntime(config, dependencies)
	if err != nil || runtime == nil || !runtime.Enabled || runtime.Publisher == nil || runtime.Publisher.Validate() != nil {
		t.Fatalf("runtime=%#v err=%v", runtime, err)
	}
	mutations := []RuntimeDependencies{
		func() RuntimeDependencies { value := dependencies; value.GitBindings = nil; return value }(),
		func() RuntimeDependencies { value := dependencies; value.GitProvider = nil; return value }(),
		func() RuntimeDependencies { value := dependencies; value.GitManager = nil; return value }(),
		func() RuntimeDependencies {
			value := dependencies
			value.ArgoObservation = argo.DesiredStateRuntimeWorkerObservation{}
			return value
		}(),
		func() RuntimeDependencies { value := dependencies; value.CascadeRoots = nil; return value }(),
		func() RuntimeDependencies { value := dependencies; value.CascadeApplications = nil; return value }(),
	}
	invalidManager := *fixture.manager
	invalidManager.Root = "/"
	invalid := dependencies
	invalid.GitManager = &invalidManager
	mutations = append(mutations, invalid)
	for index, mutation := range mutations {
		if runtime, runtimeErr := NewRuntime(config, mutation); runtimeErr == nil || runtime != nil {
			t.Fatalf("partial protected publisher dependency %d was accepted: runtime=%#v err=%v", index, runtime, runtimeErr)
		}
	}
}

type capabilityReadinessStub struct {
	renderer, publisher, argo                bool
	rendererErr, publisherErr, argoErr       error
	rendererCalls, publisherCalls, argoCalls int
}

type exactRuntimeConfigReadinessStub struct {
	publisher ProtectedPublisherIdentity
}

func (s exactRuntimeConfigReadinessStub) RuntimeReady(context.Context, time.Time) (bool, error) {
	return true, nil
}

func (s exactRuntimeConfigReadinessStub) PublisherReady(_ context.Context,
	publisher ProtectedPublisherIdentity, _ time.Time) (bool, error) {
	return publisher == s.publisher, nil
}

func (s exactRuntimeConfigReadinessStub) ProtectedHelmApplicationsReady(_ context.Context,
	publisher ProtectedPublisherIdentity, _ time.Time) (bool, error) {
	return publisher.Validate() == nil, nil
}

type protectedPublisherProcessorStub struct {
	validateErr                    error
	payloadErr, applicationErr     error
	payloadCalls, applicationCalls atomic.Int64
}

type protectedLaneProcessorStub struct {
	validateErr error
	err         error
	calls       atomic.Int64
}

func (s *protectedLaneProcessorStub) Validate() error { return s.validateErr }
func (s *protectedLaneProcessorStub) ProcessOne(context.Context) error {
	s.calls.Add(1)
	return s.err
}

type protectedCascadeObserverProcessorStub struct{}

func (*protectedCascadeObserverProcessorStub) Validate() error { return nil }
func (*protectedCascadeObserverProcessorStub) ProcessOne(context.Context) (ProtectedApplicationCascadeReceipt, error) {
	return ProtectedApplicationCascadeReceipt{}, ErrNotFound
}

type cascadeRootSourceStub struct{}

func (cascadeRootSourceStub) ObservePlatformRootApplicationForCascade(context.Context,
	argo.PlatformRootApplicationExpectation, time.Time) (argo.PlatformRootApplicationObservation, error) {
	return argo.PlatformRootApplicationObservation{}, argo.ErrPlatformRootNotReady
}

type cascadeApplicationSourceStub struct{}

func (cascadeApplicationSourceStub) ObserveProtectedApplication(context.Context,
	argo.ProtectedApplicationExpectation, time.Time) (argo.ProtectedApplicationObservation, error) {
	return argo.ProtectedApplicationObservation{}, argo.ErrProtectedApplicationNotReady
}

type ociCredentialReadinessStub struct {
	err   error
	calls atomic.Int64
}

func (s *ociCredentialReadinessStub) Probe(context.Context) error {
	s.calls.Add(1)
	return s.err
}

func (s *protectedPublisherProcessorStub) Validate() error { return s.validateErr }
func (s *protectedPublisherProcessorStub) ProcessPayloadOne(context.Context) (ProtectedPayloadIntent, error) {
	s.payloadCalls.Add(1)
	return ProtectedPayloadIntent{}, s.payloadErr
}
func (s *protectedPublisherProcessorStub) ProcessApplicationOne(context.Context) (ProtectedApplicationIntent, error) {
	s.applicationCalls.Add(1)
	return ProtectedApplicationIntent{}, s.applicationErr
}

func (s *protectedPublisherProcessorStub) ProcessCascadePreflightOne(context.Context) (ProtectedApplicationCascadePreflight, error) {
	return ProtectedApplicationCascadePreflight{}, ErrNotFound
}

func TestRendererReadinessRequiresEveryConfiguredOCICredentialProfile(t *testing.T) {
	config := validRuntimeConfig(t)
	config.OCICredentialProfiles = []OCIRegistryCredentialProfile{{RegistryHost: "ghcr.io", AuthHost: "ghcr.io",
		Name: "private-main", Mode: OCICredentialModeBearer, ProjectionDigest: digestBytes([]byte("profile"))}}
	store := NewMemoryStore(config.Publisher.ConfigDigest)
	probe := &ociCredentialReadinessStub{err: ErrOCICredentialUnavailable}
	runtime := &Runtime{Enabled: true, Config: config, Store: store, credentials: probe,
		Worker: Worker{Store: store, Packages: stubPackages{}, Renderer: &stubRenderer{},
			LeaseDuration: config.RenderLeaseDuration, Now: func() time.Time { return testTime },
			OperatorConfigDigest: config.Publisher.ConfigDigest},
		workerID: "helm-renderer-worker-0001", workerEpoch: 1, startedAt: testTime}
	if err := runtime.ObserveRendererReadiness(t.Context()); !errors.Is(err, ErrOCICredentialUnavailable) {
		t.Fatalf("unavailable credential readiness error=%v", err)
	}
	if ready, err := store.RuntimeReady(t.Context(), testTime.Add(time.Second)); err != nil || ready {
		t.Fatalf("unavailable credential advertised ready=%v err=%v", ready, err)
	}
	probe.err = nil
	if err := runtime.ObserveRendererReadiness(t.Context()); err != nil {
		t.Fatalf("credentialed readiness: %v", err)
	}
	if ready, err := store.RuntimeReady(t.Context(), testTime.Add(time.Second)); err != nil || !ready || probe.calls.Load() != 2 {
		t.Fatalf("credentialed ready=%v calls=%d err=%v", ready, probe.calls.Load(), err)
	}
}

func (s *capabilityReadinessStub) RuntimeReady(context.Context, time.Time) (bool, error) {
	s.rendererCalls++
	return s.renderer, s.rendererErr
}

func (s *capabilityReadinessStub) PublisherReady(context.Context, ProtectedPublisherIdentity, time.Time) (bool, error) {
	s.publisherCalls++
	return s.publisher, s.publisherErr
}

func (s *capabilityReadinessStub) ProtectedHelmApplicationsReady(context.Context, ProtectedPublisherIdentity, time.Time) (bool, error) {
	s.argoCalls++
	return s.argo, s.argoErr
}

func TestCapabilityGateRequiresExactCombinedRendererPublisherAndArgoReadiness(t *testing.T) {
	config := validRuntimeConfig(t)
	tests := []struct {
		name                      string
		renderer, publisher, argo bool
		want                      bool
	}{
		{name: "all ready", renderer: true, publisher: true, argo: true, want: true},
		{name: "renderer stale", publisher: true, argo: true},
		{name: "publisher stale", renderer: true, argo: true},
		{name: "argo stale", renderer: true, publisher: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe := &capabilityReadinessStub{renderer: test.renderer, publisher: test.publisher, argo: test.argo}
			gate := CapabilityGate{Enabled: true, Renderer: probe, Publisher: probe, Argo: probe, PublisherID: config.Publisher}
			capabilities, err := gate.Evaluate(context.Background(), testTime)
			if err != nil || capabilities.HelmDeployments != test.want || capabilities.HelmRollbacks != test.want {
				t.Fatalf("capabilities=%+v err=%v", capabilities, err)
			}
			if probe.rendererCalls != 1 || probe.publisherCalls != 1 || probe.argoCalls != 1 {
				t.Fatalf("gate did not evaluate every dependency: %+v", probe)
			}
		})
	}
}

func TestCapabilityGateReturnsNoPartialCapabilityOnProbeError(t *testing.T) {
	config := validRuntimeConfig(t)
	probe := &capabilityReadinessStub{renderer: true, publisher: true, argo: true,
		publisherErr: errors.New("publisher unavailable")}
	gate := CapabilityGate{Enabled: true, Renderer: probe, Publisher: probe, Argo: probe, PublisherID: config.Publisher}
	capabilities, err := gate.Evaluate(context.Background(), testTime)
	if err == nil || capabilities != (Capabilities{}) {
		t.Fatalf("capabilities=%+v err=%v", capabilities, err)
	}
	if probe.rendererCalls != 1 || probe.publisherCalls != 1 || probe.argoCalls != 1 {
		t.Fatalf("probe error short-circuited exact evaluation: %+v", probe)
	}
}

func TestFullRuntimePublisherIdentityRejectsFreshOldWorkerAfterConfigChange(t *testing.T) {
	baseGitIdentity := gitprojection.RuntimeIdentity{ContractVersion: gitprojection.RuntimeContract,
		ConfigDigest: digestBytes([]byte("git-projection-runtime")), GitHubAppID: 12345}
	oldConfig := validRuntimeConfig(t)
	oldPublisher, err := ProtectedPublisherIdentityForRuntime(baseGitIdentity, oldConfig)
	if err != nil {
		t.Fatal(err)
	}
	oldConfig.Publisher = oldPublisher
	newConfig := oldConfig
	newConfig.PackageCacheBytes += MaximumChartSize
	newPublisher, err := ProtectedPublisherIdentityForRuntime(baseGitIdentity, newConfig)
	if err != nil || newPublisher == oldPublisher {
		t.Fatalf("old=%#v new=%#v err=%v", oldPublisher, newPublisher, err)
	}
	newConfig.Publisher = newPublisher
	oldWorker := exactRuntimeConfigReadinessStub{publisher: oldPublisher}
	gate := CapabilityGate{Enabled: true, Renderer: oldWorker, Publisher: oldWorker,
		Argo: oldWorker, PublisherID: newPublisher}
	capabilities, err := gate.Evaluate(t.Context(), testTime)
	if err != nil || capabilities != (Capabilities{}) {
		t.Fatalf("old worker satisfied new config: capabilities=%#v err=%v", capabilities, err)
	}
	newWorker := exactRuntimeConfigReadinessStub{publisher: newPublisher}
	gate.Renderer, gate.Publisher, gate.Argo = newWorker, newWorker, newWorker
	capabilities, err = gate.Evaluate(t.Context(), testTime)
	if err != nil || !capabilities.HelmDeployments || !capabilities.HelmRollbacks {
		t.Fatalf("exact new worker rejected: capabilities=%#v err=%v", capabilities, err)
	}
}

func TestProtectedPublisherRuntimeIdentityCoversEveryOperatorControlledRuntimeField(t *testing.T) {
	baseGitIdentity := gitprojection.RuntimeIdentity{ContractVersion: gitprojection.RuntimeContract,
		ConfigDigest: digestBytes([]byte("git-projection-runtime")), GitHubAppID: 12345}
	config := validRuntimeConfig(t)
	baseline, err := ProtectedPublisherIdentityForRuntime(baseGitIdentity, config)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		base   gitprojection.RuntimeIdentity
		mutate func(*RuntimeConfig)
	}{
		{name: "base Git projection", base: gitprojection.RuntimeIdentity{ContractVersion: gitprojection.RuntimeContract,
			ConfigDigest: digestBytes([]byte("new-git-runtime")), GitHubAppID: 12345}, mutate: func(*RuntimeConfig) {}},
		{name: "renderer namespace", base: baseGitIdentity, mutate: func(c *RuntimeConfig) { c.Renderer.Namespace = "other-renderer" }},
		{name: "renderer service account", base: baseGitIdentity, mutate: func(c *RuntimeConfig) { c.Renderer.ServiceAccount = "other-renderer" }},
		{name: "renderer poll", base: baseGitIdentity, mutate: func(c *RuntimeConfig) { c.Renderer.PollInterval = 200 * time.Millisecond }},
		{name: "work poll", base: baseGitIdentity, mutate: func(c *RuntimeConfig) { c.WorkPollInterval = 2 * time.Second }},
		{name: "render lease", base: baseGitIdentity, mutate: func(c *RuntimeConfig) { c.RenderLeaseDuration = 2 * time.Minute }},
		{name: "publish lease", base: baseGitIdentity, mutate: func(c *RuntimeConfig) { c.PublishLeaseDuration = 2 * time.Minute }},
		{name: "readiness lease", base: baseGitIdentity, mutate: func(c *RuntimeConfig) { c.ReadinessLeaseDuration = 45 * time.Second }},
		{name: "OCI timeout", base: baseGitIdentity, mutate: func(c *RuntimeConfig) { c.OCIRequestTimeout = 20 * time.Second }},
		{name: "registry allowlist", base: baseGitIdentity, mutate: func(c *RuntimeConfig) {
			c.OCIRegistryHosts = []string{"docker.io", "ghcr.io", "registry.example.com"}
		}},
		{name: "auth allowlist", base: baseGitIdentity, mutate: func(c *RuntimeConfig) { c.OCIAuthHosts = nil }},
		{name: "OCI credential profile", base: baseGitIdentity, mutate: func(c *RuntimeConfig) {
			c.OCICredentialProfiles = []OCIRegistryCredentialProfile{{RegistryHost: "ghcr.io", AuthHost: "ghcr.io",
				Name: "private-main", Mode: OCICredentialModeBearer, ProjectionDigest: digestBytes([]byte("profile"))}}
		}},
		{name: "package cache", base: baseGitIdentity, mutate: func(c *RuntimeConfig) { c.PackageCacheBytes += MaximumChartSize }},
		{name: "Argo application runtime", base: baseGitIdentity, mutate: func(c *RuntimeConfig) { c.Application.ArgoNamespace = "other-argocd" }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			candidate := config
			candidate.OCIRegistryHosts = append([]string(nil), config.OCIRegistryHosts...)
			candidate.OCIAuthHosts = append([]string(nil), config.OCIAuthHosts...)
			test.mutate(&candidate)
			identity, identityErr := ProtectedPublisherIdentityForRuntime(test.base, candidate)
			if identityErr != nil || identity == baseline {
				t.Fatalf("identity=%#v baseline=%#v err=%v", identity, baseline, identityErr)
			}
		})
	}
}

func TestRuntimeRunKeepsReadinessIndependentAndTreatsEmptyQueuesAsIdle(t *testing.T) {
	config := validRuntimeConfig(t)
	config.WorkPollInterval = 100 * time.Millisecond
	config.ReadinessLeaseDuration = 5 * time.Second
	store := NewMemoryStore(config.Publisher.ConfigDigest)
	planningStore := &planningStoreStub{applicationErr: ErrNotFound, payloadErr: ErrNotFound}
	publisher := &protectedPublisherProcessorStub{payloadErr: ErrNotFound, applicationErr: ErrNotFound}
	lane := &protectedLaneProcessorStub{err: ErrNotFound}
	resolver := &bindingResolverStub{}
	var reported atomic.Int64
	runtime := &Runtime{Enabled: true, Config: config, Store: store, Publications: planningStore,
		Worker: Worker{Store: store, Packages: stubPackages{}, Renderer: &stubRenderer{},
			LeaseDuration: config.RenderLeaseDuration, Now: func() time.Time { return testTime },
			OperatorConfigDigest: config.Publisher.ConfigDigest},
		Planner: PublicationPlanner{Store: planningStore, Bindings: resolver,
			Publisher: config.Publisher, Application: config.Application,
			NewID: func() string { return testCommandID }, Now: func() time.Time { return testTime }}, Publisher: publisher,
		Cascade: &protectedCascadeObserverProcessorStub{}, ProtectedLane: lane,
		workerID: "helm-renderer-worker-0001", workerEpoch: 1, startedAt: testTime,
		reportError: func(string, error) { reported.Add(1) }}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := runtime.Run(ctx); err != nil {
		t.Fatal(err)
	}
	ready, err := store.RuntimeReady(context.Background(), testTime.Add(time.Second))
	if err != nil || !ready {
		t.Fatalf("renderer readiness=%v err=%v", ready, err)
	}
	if reported.Load() != 0 {
		t.Fatalf("empty durable queues were reported as failures %d times", reported.Load())
	}
	if len(planningStore.publisherReadiness) == 0 || lane.calls.Load() == 0 {
		t.Fatalf("publisher runtime lane did not start: readiness=%d lane=%d",
			len(planningStore.publisherReadiness), lane.calls.Load())
	}
}
