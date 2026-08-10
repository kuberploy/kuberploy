package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/helmapps"
)

type helmArgoPrerequisiteStub struct{}

func (helmArgoPrerequisiteStub) ObserveProductionPrerequisites(context.Context, time.Time) (argo.ProductionPrerequisiteProof, error) {
	return argo.ProductionPrerequisiteProof{}, nil
}

type helmArgoMaterializerStub struct{}

func (helmArgoMaterializerStub) MaterializeDesiredStateOnce(context.Context, time.Time) (bool, error) {
	return false, nil
}

func helmWorkerAuthoritiesFixture(t *testing.T) (*gitProjectionRuntime, *argoDesiredStateRuntime,
	map[string]string, helmapps.RuntimeConfig) {
	t.Helper()
	pool := &pgxpool.Pool{}
	projectionStore, err := gitprojection.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	projectionIdentity := gitprojection.RuntimeIdentity{ContractVersion: gitprojection.RuntimeContract,
		ConfigDigest: "sha256:" + strings.Repeat("a", 64), GitHubAppID: 12345}
	client := &githubapp.Client{}
	projection := &gitProjectionRuntime{store: projectionStore, identity: projectionIdentity,
		headVerifier: gitprojection.GitHubHeadVerifier{AppID: projectionIdentity.GitHubAppID,
			Authorizations: projectionStore, Client: client},
		writeManager: &gitprojection.MirrorManager{Root: "/tmp/kuberploy-helm-worker-test",
			CredentialProvider: gitprojection.GitHubGitCredentialProvider{AppID: projectionIdentity.GitHubAppID,
				Authorizations: projectionStore, Client: gitprojection.GitHubGitClientAdapter{Client: client}}}}
	platformBindingID := "11111111-1111-4111-8111-111111111111"
	clusterID := "22222222-2222-4222-8222-222222222222"
	repositoryCredential, err := argo.RepositoryCredentialName(platformBindingID)
	if err != nil {
		t.Fatal(err)
	}
	argoIdentity, err := argo.DesiredStateRuntimeIdentityForConfig(argo.DesiredStateRuntimeConfig{
		Enabled: true, GitHubAppID: projectionIdentity.GitHubAppID, PlatformBindingID: platformBindingID,
		ClusterID: clusterID, ArgoNamespace: "argocd", RootApplicationName: argo.PlatformRootApplicationName,
		RepositorySecretName: repositoryCredential,
		Runtime: argo.RuntimeLock{ChartRepository: "oci://ghcr.io/kuberploy/charts", ChartName: argo.RuntimeChartName,
			ChartVersion: "1.2.3", ChartDigest: "sha256:" + strings.Repeat("b", 64),
			RendererImage: "ghcr.io/kuberploy/renderer@sha256:" + strings.Repeat("c", 64)},
		DigestEnforcement: argo.ChartDigestNativeOCI,
	})
	if err != nil {
		t.Fatal(err)
	}
	argoStore, err := argo.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	argoRuntime := &argoDesiredStateRuntime{store: argoStore, runtime: &argo.ProductionDesiredStateRuntime{
		Worker: &argo.DesiredStateRuntimeWorker{Observation: argo.DesiredStateRuntimeWorkerObservation{
			WorkerID: "helm-argo-worker-test", DesiredStateRuntimeIdentity: argoIdentity,
			StartedAt: time.Now().UTC(), ObservedAt: time.Now().UTC()}},
		Prerequisites: helmArgoPrerequisiteStub{}, Materializer: helmArgoMaterializerStub{},
	}}
	values := helmWorkerRuntimeEnvironment()
	config, err := protectedHelmRuntimeConfigForWorker(projection, helmWorkerLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	return projection, argoRuntime, values, config
}

func helmWorkerRuntimeEnvironment() map[string]string {
	return map[string]string{
		helmapps.RuntimeEnabledEnv: "true", helmapps.RuntimeRendererNamespaceEnv: "kuberploy-system",
		helmapps.RuntimeRendererServiceAccountEnv: "kuberploy-helm-renderer",
		helmapps.RuntimeRendererPollMillisEnv:     "100", helmapps.RuntimeWorkPollMillisEnv: "1000",
		helmapps.RuntimeRenderLeaseSecondsEnv: "60", helmapps.RuntimePublishLeaseSecondsEnv: "60",
		helmapps.RuntimeReadinessSecondsEnv: "30", helmapps.RuntimeOCIRequestSecondsEnv: "15",
		helmapps.RuntimeOCIRegistryHostsEnv: "ghcr.io", helmapps.RuntimePackageCacheBytesEnv: "67108864",
		helmapps.RuntimeArgoNamespaceEnv: "argocd",
	}
}

func helmWorkerLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { value, ok := values[name]; return value, ok }
}

func TestNewHelmApplicationsRuntimeIsStrictlyDefaultOff(t *testing.T) {
	runtime, err := newHelmApplicationsRuntimeFromLookup(t.Context(), "not-a-database-url", "worker", nil, nil,
		helmWorkerLookup(nil))
	if err != nil || runtime != nil {
		t.Fatalf("runtime=%#v err=%v", runtime, err)
	}
}

func TestNewHelmApplicationsRuntimeRejectsEnabledMissingAuthoritiesBeforeIO(t *testing.T) {
	_, _, values, _ := helmWorkerAuthoritiesFixture(t)
	if runtime, err := newHelmApplicationsRuntimeFromLookup(t.Context(), "not-a-database-url", "worker", nil, nil,
		helmWorkerLookup(values)); err == nil || runtime != nil {
		t.Fatalf("runtime=%#v err=%v", runtime, err)
	}
}

func TestValidateHelmWorkerAuthoritiesRejectsEveryMissingArgoOrGitDependency(t *testing.T) {
	projection, argoRuntime, _, config := helmWorkerAuthoritiesFixture(t)
	identity, err := validateHelmWorkerAuthorities(config, projection, argoRuntime)
	if err != nil || identity.PlatformBindingID == "" {
		t.Fatalf("identity=%#v err=%v", identity, err)
	}
	tests := []struct {
		name   string
		mutate func(**gitProjectionRuntime, **argoDesiredStateRuntime)
	}{
		{name: "Git runtime", mutate: func(p **gitProjectionRuntime, _ **argoDesiredStateRuntime) { *p = nil }},
		{name: "Git store", mutate: func(p **gitProjectionRuntime, _ **argoDesiredStateRuntime) { (*p).store = nil }},
		{name: "Git authorization", mutate: func(p **gitProjectionRuntime, _ **argoDesiredStateRuntime) { (*p).headVerifier.Authorizations = nil }},
		{name: "Git provider", mutate: func(p **gitProjectionRuntime, _ **argoDesiredStateRuntime) { (*p).headVerifier.Client = nil }},
		{name: "Git manager", mutate: func(p **gitProjectionRuntime, _ **argoDesiredStateRuntime) { (*p).writeManager = nil }},
		{name: "Git credentials", mutate: func(p **gitProjectionRuntime, _ **argoDesiredStateRuntime) {
			(*p).writeManager.CredentialProvider = nil
		}},
		{name: "Argo runtime", mutate: func(_ **gitProjectionRuntime, a **argoDesiredStateRuntime) { *a = nil }},
		{name: "Argo store", mutate: func(_ **gitProjectionRuntime, a **argoDesiredStateRuntime) { (*a).store = nil }},
		{name: "Argo production runtime", mutate: func(_ **gitProjectionRuntime, a **argoDesiredStateRuntime) { (*a).runtime = nil }},
		{name: "Argo worker", mutate: func(_ **gitProjectionRuntime, a **argoDesiredStateRuntime) { (*a).runtime.Worker = nil }},
		{name: "Argo prerequisites", mutate: func(_ **gitProjectionRuntime, a **argoDesiredStateRuntime) { (*a).runtime.Prerequisites = nil }},
		{name: "Argo materializer", mutate: func(_ **gitProjectionRuntime, a **argoDesiredStateRuntime) { (*a).runtime.Materializer = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projectionCopy, argoCopy := *projection, *argoRuntime
			managerCopy := *projection.writeManager
			projectionCopy.writeManager = &managerCopy
			runtimeCopy := *argoRuntime.runtime
			argoCopy.runtime = &runtimeCopy
			candidateProjection, candidateArgo := &projectionCopy, &argoCopy
			test.mutate(&candidateProjection, &candidateArgo)
			if got, authorityErr := validateHelmWorkerAuthorities(config, candidateProjection, candidateArgo); !errors.Is(authorityErr, helmapps.ErrInvalid) || got != (argo.DesiredStateRuntimeIdentity{}) {
				t.Fatalf("authority=%#v err=%v", got, authorityErr)
			}
		})
	}
}
