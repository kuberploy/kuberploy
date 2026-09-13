package main

import (
	"context"
	"errors"
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
	rootErr         error
	setErr          error
}

func (t *verifiedMergeRefreshTarget) RefreshPlatformRootApplication(_ context.Context, expectation argo.PlatformRootApplicationExpectation, refreshedAt time.Time) error {
	t.root, t.rootRefreshedAt = expectation, refreshedAt
	return t.rootErr
}

func (t *verifiedMergeRefreshTarget) RefreshEnvironmentApplicationSet(_ context.Context, expectation argo.EnvironmentApplicationSetExpectation, refreshedAt time.Time) error {
	t.applicationSet, t.setRefreshedAt = expectation, refreshedAt
	return t.setErr
}

type verifiedMergeObservationWaker struct {
	namespace string
	wakeAt    time.Time
	err       error
}

func (w *verifiedMergeObservationWaker) WakeObservation(_ context.Context, namespace string, wakeAt time.Time) error {
	w.namespace, w.wakeAt = namespace, wakeAt
	return w.err
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
	observation := gitpublication.TargetHeadObservation{
		Repository: publication.Repository, TargetRef: targetRef, Revision: targetRevision, ObservedAt: now,
	}
	refreshErr := errors.New("fixture refresh failed")
	for _, test := range []struct {
		name                        string
		change                      func(*gitprojection.Binding, *gitprojection.Binding, *verifiedMergeRefreshTarget, *verifiedMergeObservationWaker)
		wantRoot, wantSet, wantWake bool
		wantErr                     error
	}{
		{name: "shared repository and ref", wantRoot: true, wantSet: true, wantWake: true},
		{name: "separate branch", wantSet: true, wantWake: true, change: func(platform, _ *gitprojection.Binding, _ *verifiedMergeRefreshTarget, _ *verifiedMergeObservationWaker) {
			platform.TargetRef = "refs/heads/platform"
		}},
		{name: "separate repository", wantSet: true, wantWake: true, change: func(platform, _ *gitprojection.Binding, _ *verifiedMergeRefreshTarget, _ *verifiedMergeObservationWaker) {
			platform.Repository.RepositoryID++
			platform.Repository.Name = "platform-gitops"
		}},
		{name: "mismatched environment ref", wantErr: argo.ErrInvalid, change: func(_, environment *gitprojection.Binding, _ *verifiedMergeRefreshTarget, _ *verifiedMergeObservationWaker) {
			environment.TargetRef = "refs/heads/unrelated"
		}},
		{name: "mismatched environment repository", wantErr: argo.ErrInvalid, change: func(_, environment *gitprojection.Binding, _ *verifiedMergeRefreshTarget, _ *verifiedMergeObservationWaker) {
			environment.Repository.RepositoryID++
		}},
		{name: "mismatched environment identity", wantErr: argo.ErrInvalid, change: func(_, environment *gitprojection.Binding, _ *verifiedMergeRefreshTarget, _ *verifiedMergeObservationWaker) {
			environment.ID = "66666666-6666-4666-8666-666666666666"
		}},
		{name: "invalid platform identity", wantErr: argo.ErrInvalid, change: func(platform, _ *gitprojection.Binding, _ *verifiedMergeRefreshTarget, _ *verifiedMergeObservationWaker) {
			platform.ID = "66666666-6666-4666-8666-666666666666"
			platform.ScopeID = platform.ID
			platform.TargetRef = "refs/heads/platform"
		}},
		{name: "root refresh failure", wantRoot: true, wantErr: refreshErr, change: func(_, _ *gitprojection.Binding, target *verifiedMergeRefreshTarget, _ *verifiedMergeObservationWaker) {
			target.rootErr = refreshErr
		}},
		{name: "separate branch ApplicationSet failure", wantSet: true, wantErr: refreshErr, change: func(platform, _ *gitprojection.Binding, target *verifiedMergeRefreshTarget, _ *verifiedMergeObservationWaker) {
			platform.TargetRef = "refs/heads/platform"
			target.setErr = refreshErr
		}},
		{name: "separate branch observation wake failure", wantSet: true, wantWake: true, wantErr: refreshErr, change: func(platform, _ *gitprojection.Binding, _ *verifiedMergeRefreshTarget, waker *verifiedMergeObservationWaker) {
			platform.TargetRef = "refs/heads/platform"
			waker.err = refreshErr
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			currentPlatform, currentEnvironment := platform, environment
			target := &verifiedMergeRefreshTarget{}
			waker := &verifiedMergeObservationWaker{}
			if test.change != nil {
				test.change(&currentPlatform, &currentEnvironment, target, waker)
			}
			refresher := verifiedPublicationArgoRefresher{
				bindings: verifiedMergeBindingStore{platform.ID: currentPlatform, environment.ID: currentEnvironment},
				target:   target, waker: waker, identity: identity,
			}
			if err := refresher.RefreshVerifiedMerge(t.Context(), publication, observation); !errors.Is(err, test.wantErr) {
				t.Fatalf("refresh error=%v want=%v", err, test.wantErr)
			}
			if got := !target.rootRefreshedAt.IsZero(); got != test.wantRoot {
				t.Fatalf("platform root refreshed=%v want=%v", got, test.wantRoot)
			}
			if test.wantRoot && (target.root.ExpectedGitRevision != targetRevision || target.root.Name != identity.RootApplicationName || target.rootRefreshedAt != now) {
				t.Fatalf("root refresh=%#v at=%v", target.root, target.rootRefreshedAt)
			}
			if got := !target.setRefreshedAt.IsZero(); got != test.wantSet {
				t.Fatalf("Environment ApplicationSet refreshed=%v want=%v", got, test.wantSet)
			}
			if test.wantSet && (target.applicationSet.Name != argo.ApplicationSetName(environmentID) || target.applicationSet.Namespace != identity.ArgoNamespace ||
				target.applicationSet.ProjectID != projectID || target.applicationSet.EnvironmentID != environmentID || target.setRefreshedAt != now) {
				t.Fatalf("ApplicationSet refresh=%#v at=%v", target.applicationSet, target.setRefreshedAt)
			}
			if got := !waker.wakeAt.IsZero(); got != test.wantWake {
				t.Fatalf("observation woken=%v want=%v", got, test.wantWake)
			}
			if test.wantWake && (waker.namespace != identity.ArgoNamespace || waker.wakeAt != now) {
				t.Fatalf("observation wake namespace=%q at=%v", waker.namespace, waker.wakeAt)
			}
		})
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
