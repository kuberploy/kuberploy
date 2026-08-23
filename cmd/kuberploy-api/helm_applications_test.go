package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/helmapps"
)

func helmAPIAuthoritiesFixture(t *testing.T) (*gitProjectionAPI, *argoDesiredStateAPI,
	map[string]string, helmapps.RuntimeConfig) {
	t.Helper()
	pool := &pgxpool.Pool{}
	projectionStore, err := gitprojection.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	projectionIdentity := gitprojection.RuntimeIdentity{ContractVersion: gitprojection.RuntimeContract,
		ConfigDigest: "sha256:" + strings.Repeat("a", 64), GitHubAppID: 12345}
	projection := &gitProjectionAPI{store: projectionStore, backend: &gitprojection.ControlPlane{},
		readiness: &gitprojection.RuntimeReadinessProbe{Store: projectionStore, Identity: projectionIdentity}}
	platformBindingID := "11111111-1111-4111-8111-111111111111"
	repositoryCredential, err := argo.RepositoryCredentialName(platformBindingID)
	if err != nil {
		t.Fatal(err)
	}
	argoIdentity, err := argo.DesiredStateRuntimeIdentityForConfig(argo.DesiredStateRuntimeConfig{
		Enabled: true, GitHubAppID: projectionIdentity.GitHubAppID, PlatformBindingID: platformBindingID,
		ArgoNamespace: "argocd", RootApplicationName: argo.PlatformRootApplicationName,
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
	argoRuntime := &argoDesiredStateAPI{store: argoStore,
		readiness: &argo.ProductionDesiredStateReadinessProbe{Store: argoStore, Identity: argoIdentity}}
	values := helmAPIRuntimeEnvironment()
	config, err := protectedHelmRuntimeConfigForAPI(projection, helmAPILookup(values))
	if err != nil {
		t.Fatal(err)
	}
	return projection, argoRuntime, values, config
}

func helmAPIRuntimeEnvironment() map[string]string {
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

func helmAPILookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { value, ok := values[name]; return value, ok }
}

func TestNewHelmApplicationsAPIIsStrictlyDefaultOff(t *testing.T) {
	runtime, err := newHelmApplicationsAPIFromLookup(t.Context(), "not-a-database-url", nil, nil, helmAPILookup(nil))
	if err != nil || runtime != nil {
		t.Fatalf("runtime=%#v err=%v", runtime, err)
	}
}

func TestNewHelmApplicationsAPIRejectsEnabledMissingAuthoritiesBeforeIO(t *testing.T) {
	_, _, values, _ := helmAPIAuthoritiesFixture(t)
	if runtime, err := newHelmApplicationsAPIFromLookup(t.Context(), "not-a-database-url", nil, nil,
		helmAPILookup(values)); err == nil || runtime != nil {
		t.Fatalf("runtime=%#v err=%v", runtime, err)
	}
}

func TestValidateHelmAPIAuthoritiesRejectsEveryMissingArgoOrGitDependency(t *testing.T) {
	projection, argoRuntime, _, config := helmAPIAuthoritiesFixture(t)
	identity, err := validateHelmAPIAuthorities(config, projection, argoRuntime)
	if err != nil || identity.PlatformBindingID == "" {
		t.Fatalf("identity=%#v err=%v", identity, err)
	}
	tests := []struct {
		name   string
		mutate func(**gitProjectionAPI, **argoDesiredStateAPI)
	}{
		{name: "Git API", mutate: func(p **gitProjectionAPI, _ **argoDesiredStateAPI) { *p = nil }},
		{name: "Git store", mutate: func(p **gitProjectionAPI, _ **argoDesiredStateAPI) { (*p).store = nil }},
		{name: "Git backend", mutate: func(p **gitProjectionAPI, _ **argoDesiredStateAPI) { (*p).backend = nil }},
		{name: "Git readiness", mutate: func(p **gitProjectionAPI, _ **argoDesiredStateAPI) { (*p).readiness = nil }},
		{name: "Git readiness store", mutate: func(p **gitProjectionAPI, _ **argoDesiredStateAPI) { (*p).readiness.Store = nil }},
		{name: "Argo API", mutate: func(_ **gitProjectionAPI, a **argoDesiredStateAPI) { *a = nil }},
		{name: "Argo store", mutate: func(_ **gitProjectionAPI, a **argoDesiredStateAPI) { (*a).store = nil }},
		{name: "Argo readiness", mutate: func(_ **gitProjectionAPI, a **argoDesiredStateAPI) { (*a).readiness = nil }},
		{name: "Argo readiness store", mutate: func(_ **gitProjectionAPI, a **argoDesiredStateAPI) { (*a).readiness.Store = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projectionCopy, argoCopy := *projection, *argoRuntime
			readinessCopy := *projection.readiness
			projectionCopy.readiness = &readinessCopy
			argoReadinessCopy := *argoRuntime.readiness
			argoCopy.readiness = &argoReadinessCopy
			candidateProjection, candidateArgo := &projectionCopy, &argoCopy
			test.mutate(&candidateProjection, &candidateArgo)
			if got, authorityErr := validateHelmAPIAuthorities(config, candidateProjection, candidateArgo); !errors.Is(authorityErr, helmapps.ErrInvalid) || got != (argo.DesiredStateRuntimeIdentity{}) {
				t.Fatalf("authority=%#v err=%v", got, authorityErr)
			}
		})
	}
}
