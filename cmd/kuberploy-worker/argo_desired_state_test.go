package main

import (
	"context"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
	"github.com/kuberploy/kuberploy/internal/imagepull"
)

type verifiedMergeBindingStore map[string]gitprojection.Binding

func (s verifiedMergeBindingStore) Binding(_ context.Context, bindingID string) (gitprojection.Binding, error) {
	binding, found := s[bindingID]
	if !found {
		return gitprojection.Binding{}, argo.ErrNotFound
	}
	return binding, nil
}

type verifiedMergeRefreshTarget struct {
	root            argo.PlatformRootApplicationExpectation
	applicationSet  argo.EnvironmentApplicationSetExpectation
	rootRefreshedAt time.Time
	setRefreshedAt  time.Time
}

func (t *verifiedMergeRefreshTarget) RefreshPlatformRootApplication(_ context.Context, expectation argo.PlatformRootApplicationExpectation, refreshedAt time.Time) error {
	t.root, t.rootRefreshedAt = expectation, refreshedAt
	return nil
}

func (t *verifiedMergeRefreshTarget) RefreshEnvironmentApplicationSet(_ context.Context, expectation argo.EnvironmentApplicationSetExpectation, refreshedAt time.Time) error {
	t.applicationSet, t.setRefreshedAt = expectation, refreshedAt
	return nil
}

type verifiedMergeObservationWaker struct {
	namespace string
	wakeAt    time.Time
}

func (w *verifiedMergeObservationWaker) WakeObservation(_ context.Context, namespace string, wakeAt time.Time) error {
	w.namespace, w.wakeAt = namespace, wakeAt
	return nil
}

func TestArgoDesiredStateWorkerIDChangesAcrossSamePodRestart(t *testing.T) {
	firstStartedAt := time.Date(2026, time.August, 25, 1, 2, 3, 4, time.UTC)
	secondStartedAt := firstStartedAt.Add(time.Nanosecond)
	first := argoDesiredStateWorkerID("worker-pod", 1, firstStartedAt)
	second := argoDesiredStateWorkerID("worker-pod", 1, secondStartedAt)
	if first == second {
		t.Fatalf("same-pod restarts must have distinct Argo desired-state worker IDs: %q", first)
	}
}

func TestNewArgoDesiredStateRuntimeIsStrictlyDefaultOff(t *testing.T) {
	runtime, err := newArgoDesiredStateRuntime(t.Context(), "not-a-database-url", "worker", argo.ProductionRuntimeConfig{}, imagepull.RuntimeConfig{}, nil, nil, nil)
	if err != nil || runtime != nil {
		t.Fatalf("runtime=%v err=%v", runtime, err)
	}
}

func TestArgoDesiredStateRuntimeRejectsMissingProjectionBeforeExternalIO(t *testing.T) {
	values := productionConfigEnvironmentForWorker()
	config, err := argo.ProductionRuntimeConfigFromLookup(func(name string) (string, bool) { value, found := values[name]; return value, found })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = newArgoDesiredStateRuntime(context.Background(), "not-a-database-url", "worker", config, imagepull.RuntimeConfig{}, nil, nil, nil); err == nil {
		t.Fatal("enabled runtime without projection was accepted")
	}
}

func TestVerifiedPublicationRefreshesExactArgoResourcesAndWakesObservation(t *testing.T) {
	now := time.Date(2026, time.August, 30, 1, 2, 3, 0, time.UTC)
	config, err := argo.ProductionRuntimeConfigFromLookup(func(name string) (string, bool) {
		value, found := productionConfigEnvironmentForWorker()[name]
		return value, found
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := argo.DesiredStateRuntimeIdentityForConfig(config.DesiredState)
	if err != nil {
		t.Fatal(err)
	}
	repository := gitprojection.RepositoryIdentity{
		Provider: "github", InstallationID: 123, RepositoryID: 456, Owner: "kuberploy", Name: "kuberploy-gitops-test",
	}
	projectID := "22222222-2222-4222-8222-222222222222"
	environmentID := "33333333-3333-4333-8333-333333333333"
	environmentBindingID := "44444444-4444-4444-8444-444444444444"
	targetRef := "refs/heads/rc413"
	targetRevision := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	platform := gitprojection.Binding{
		ID: identity.PlatformBindingID, Kind: gitprojection.BindingPlatform, ScopeID: identity.PlatformBindingID,
		Repository: repository, TargetRef: targetRef, Prefix: gitprojection.PlatformPrefix(), CredentialMode: gitprojection.CredentialGitHubApp,
		State: gitprojection.BindingWaiting, ParserVersion: "appconfig-v1alpha1", CreatedAt: now.Add(-time.Minute), UpdatedAt: now,
	}
	environment := gitprojection.Binding{
		ID: environmentBindingID, Kind: gitprojection.BindingEnvironment, ScopeID: environmentID, ProjectID: projectID, EnvironmentID: environmentID,
		Repository: repository, TargetRef: targetRef, Prefix: gitprojection.EnvironmentPrefix(projectID, environmentID), CredentialMode: gitprojection.CredentialGitHubApp,
		State: gitprojection.BindingWaiting, ParserVersion: "appconfig-v1alpha1", CreatedAt: now.Add(-time.Minute), UpdatedAt: now,
	}
	providerObservedAt := now
	publication := gitpublication.Publication{
		OperationID: "55555555-5555-4555-8555-555555555555", BindingID: environmentBindingID,
		Repository: gitpublication.Repository{InstallationID: repository.InstallationID, ID: repository.RepositoryID, Owner: repository.Owner, Name: repository.Name},
		TargetRef:  targetRef, BaseRevision: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", WriteBaseRevision: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		CandidateRef: "refs/heads/kuberploy/operations/55555555-5555-4555-8555-555555555555", CandidateRevision: "cccccccccccccccccccccccccccccccccccccccc",
		PullRequestNumber: 42, PullRequestURL: "https://github.com/kuberploy/kuberploy-gitops-test/pull/42", PullRequestState: gitpublication.PullRequestClosed,
		MergeRevision: targetRevision, TargetRevision: targetRevision, State: gitpublication.StateMergeVerified, ProviderObservedAt: &providerObservedAt,
		CreatedAt: now.Add(-time.Minute), UpdatedAt: now, Version: 7,
	}
	target := &verifiedMergeRefreshTarget{}
	waker := &verifiedMergeObservationWaker{}
	refresher := verifiedPublicationArgoRefresher{
		bindings: verifiedMergeBindingStore{platform.ID: platform, environment.ID: environment},
		target:   target, waker: waker, identity: identity,
	}

	observation := gitpublication.TargetHeadObservation{
		Repository: publication.Repository, TargetRef: targetRef, Revision: targetRevision, ObservedAt: now,
	}
	if err = refresher.RefreshVerifiedMerge(t.Context(), publication, observation); err != nil {
		t.Fatal(err)
	}
	if target.root.ExpectedGitRevision != targetRevision || target.root.Name != identity.RootApplicationName || target.rootRefreshedAt != now {
		t.Fatalf("root refresh=%#v at=%v", target.root, target.rootRefreshedAt)
	}
	if target.applicationSet.Name != argo.ApplicationSetName(environmentID) || target.applicationSet.ProjectID != projectID ||
		target.applicationSet.EnvironmentID != environmentID || target.setRefreshedAt != now {
		t.Fatalf("ApplicationSet refresh=%#v at=%v", target.applicationSet, target.setRefreshedAt)
	}
	if waker.namespace != identity.ArgoNamespace || waker.wakeAt != now {
		t.Fatalf("observation wake namespace=%q at=%v", waker.namespace, waker.wakeAt)
	}
}

func productionConfigEnvironmentForWorker() map[string]string {
	return map[string]string{
		argo.ProductionEnabledEnv:              "true",
		"KUBERPLOY_GITHUB_APP_ID":              "12345",
		"KUBERPLOY_GITHUB_APP_CLIENT_ID":       "Iv1_client",
		argo.ProductionPlatformBindingIDEnv:    "11111111-1111-4111-8111-111111111111",
		argo.ProductionNamespaceEnv:            "argocd",
		argo.ProductionChartRepositoryEnv:      "oci://ghcr.io/kuberploy/charts",
		argo.ProductionChartVersionEnv:         "1.2.3",
		argo.ProductionChartDigestEnv:          "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		argo.ProductionRendererImageEnv:        "ghcr.io/kuberploy/kuberploy-worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		argo.ProductionPollIntervalSecondsEnv:  "2",
		argo.ProductionCatalogMaxAgeSecondsEnv: "300",
	}
}
