package deploymentrollback_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/deploymentrollback"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
	"github.com/kuberploy/kuberploy/internal/store"
)

const (
	actorID       = "11111111-1111-4111-8111-111111111111"
	deploymentID  = "22222222-2222-4222-8222-222222222222"
	environmentID = "33333333-3333-4333-8333-333333333333"
	applicationID = "44444444-4444-4444-8444-444444444444"
	sourceID      = "55555555-5555-4555-8555-555555555555"
)

type history struct {
	current  domain.Deployment
	snapshot domain.Deployment
	op       domain.Operation
	list     []domain.Operation
	getErr   error
	authErr  error
}

func (h history) GetDeploymentForActor(context.Context, string, string) (domain.Deployment, error) {
	return h.current, h.getErr
}
func (h history) GetDeploymentForOperation(context.Context, string) (domain.Deployment, error) {
	return h.snapshot, h.getErr
}
func (h history) GetOperationForActor(context.Context, string, string) (domain.Operation, error) {
	return h.op, h.getErr
}
func (h history) ListOperationsForActor(context.Context, string) ([]domain.Operation, error) {
	if h.getErr != nil {
		return nil, h.getErr
	}
	if h.list != nil {
		return h.list, nil
	}
	return []domain.Operation{h.op}, nil
}
func (h history) Authorize(context.Context, string, domain.Permission, domain.AccessTarget) error {
	return h.authErr
}

type artifacts struct {
	applicationID string
	image         string
	managed       bool
	err           error
}

func (a *artifacts) VerifyRetainedDeploymentImage(_ context.Context, applicationID, image string) (bool, error) {
	a.applicationID, a.image = applicationID, image
	return a.managed, a.err
}

func fixture() (history, *artifacts, deploymentrollback.Request) {
	now := time.Now().UTC()
	runtime := domain.DefaultWorkloadRuntime(8080, map[string]string{"MODE": "prior"})
	snapshot := domain.Deployment{ID: deploymentID, EnvironmentID: environmentID, ApplicationID: applicationID,
		Image: "registry.test/team/api@sha256:" + strings.Repeat("a", 64), Route: &domain.Route{Hostname: "api.example.test", PathPrefix: "/", TLSMode: "httpOnly", DNSMode: "manual"},
		Runtime: runtime, OperationID: sourceID, Generation: 2, ConfigRaw: []byte("immutable-server-appconfig"), CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute)}
	current := snapshot
	current.OperationID = "66666666-6666-4666-8666-666666666666"
	current.Generation = 3
	current.Image = "registry.test/team/api@sha256:" + strings.Repeat("b", 64)
	op := domain.Operation{ID: sourceID, Kind: "deployment.git-write", Status: "succeeded", TargetType: "deployment", TargetID: deploymentID, Generation: 2, GitRevision: strings.Repeat("c", 40), CreatedAt: now.Add(-time.Minute)}
	return history{current: current, snapshot: snapshot, op: op}, &artifacts{managed: true}, deploymentrollback.Request{ActorID: actorID, DeploymentID: deploymentID, SourceOperationID: sourceID}
}

func TestCatalogProjectsOnlySafeEligibleMetadataAndAssurance(t *testing.T) {
	h, registry, _ := fixture()
	registry.managed = false
	items, err := (&deploymentrollback.Resolver{History: h, Artifacts: registry}).Catalog(t.Context(), actorID, deploymentID, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	item := items[0]
	if item.SourceOperationID != sourceID || item.Generation != 2 || item.Image != h.snapshot.Image ||
		item.ArtifactAssurance != deploymentrollback.ArtifactExternalDigestUnverified || item.CreatedAt.IsZero() {
		t.Fatalf("unsafe or incomplete catalog item=%#v", item)
	}
	registry.err = deploymentrollback.ErrArtifactUnavailable
	items, err = (&deploymentrollback.Resolver{History: h, Artifacts: registry}).Catalog(t.Context(), actorID, deploymentID, 10)
	if err != nil || len(items) != 0 {
		t.Fatalf("unavailable managed source leaked into catalog: %#v err=%v", items, err)
	}
}

func TestResolverReconstructsOnlyExactPriorServerSnapshot(t *testing.T) {
	h, registry, request := fixture()
	source, err := (&deploymentrollback.Resolver{History: h, Artifacts: registry}).Resolve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if source.Create.EnvironmentID != environmentID || source.Create.ApplicationID != applicationID ||
		source.Create.Image != h.snapshot.Image || source.Create.Route == h.current.Route ||
		registry.applicationID != applicationID || registry.image != h.snapshot.Image || !source.ManagedReleaseVerified {
		t.Fatalf("source=%#v registry=%#v", source, registry)
	}
	// Returned history must not share caller-mutable nested state.
	changed := "changed"
	source.Create.Runtime.Env[0].Value = &changed
	if h.snapshot.Runtime.Env[0].Value == nil || *h.snapshot.Runtime.Env[0].Value != "prior" {
		t.Fatal("resolver shared mutable runtime history")
	}
}

type publications struct{ value gitpublication.Publication }

func (p publications) Publication(context.Context, string) (gitpublication.Publication, error) {
	return p.value, nil
}

func verifiedPublication(t *testing.T) gitpublication.Publication {
	t.Helper()
	now := time.Now().UTC().Add(-time.Minute)
	p, err := gitpublication.NewPublication(sourceID, "88888888-8888-4888-8888-888888888888",
		gitpublication.Repository{InstallationID: 7, ID: 9, Owner: "kuberploy", Name: "desired"}, "refs/heads/main", strings.Repeat("a", 40), now)
	if err != nil {
		t.Fatal(err)
	}
	p, err = p.WithWriteBase(strings.Repeat("a", 40), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	p, err = p.WithCandidate(strings.Repeat("b", 40), now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	observation := gitpublication.PullRequestObservation{Repository: p.Repository, TargetRef: p.TargetRef, HeadRef: p.CandidateRef,
		HeadRevision: p.CandidateRevision, Number: 17, URL: "https://github.com/kuberploy/desired/pull/17",
		State: gitpublication.PullRequestClosed, Merged: true, MergeRevision: strings.Repeat("c", 40), ObservedAt: now.Add(3 * time.Second)}
	p, err = p.WithPullRequest(observation, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	p, err = p.WithVerifiedMerge(strings.Repeat("d", 40), now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolverRequiresVerifiedMergeForProtectedSource(t *testing.T) {
	h, registry, request := fixture()
	p := verifiedPublication(t)
	h.op.GitRevision = ""
	h.op.PullRequest = &domain.PullRequestPublication{Number: p.PullRequestNumber, URL: p.PullRequestURL,
		State: string(p.PullRequestState), CandidateRevision: p.CandidateRevision}
	if _, err := (&deploymentrollback.Resolver{History: h, Artifacts: registry, Publications: publications{p}}).Resolve(t.Context(), request); err != nil {
		t.Fatalf("verified protected source rejected: %v", err)
	}
	h.op.PullRequest = nil // memory history may not decorate operation reads.
	if _, err := (&deploymentrollback.Resolver{History: h, Artifacts: registry, Publications: publications{p}}).Resolve(t.Context(), request); err != nil {
		t.Fatalf("verified durable publication rejected without optional view decoration: %v", err)
	}
	p.State = gitpublication.StatePullRequestClosed
	p.MergeRevision, p.TargetRevision = "", ""
	if _, err := (&deploymentrollback.Resolver{History: h, Artifacts: registry, Publications: publications{p}}).Resolve(t.Context(), request); !errors.Is(err, deploymentrollback.ErrSourceNotEligible) {
		t.Fatalf("unmerged protected source classification=%v", err)
	}
}

func TestResolverRejectsCrossScopeCurrentFailedAndUnavailableSources(t *testing.T) {
	tests := map[string]func(*history, *artifacts, *deploymentrollback.Request){
		"cross deployment": func(h *history, _ *artifacts, _ *deploymentrollback.Request) {
			h.snapshot.ID = "77777777-7777-4777-8777-777777777777"
		},
		"cross environment": func(h *history, _ *artifacts, _ *deploymentrollback.Request) {
			h.snapshot.EnvironmentID = "77777777-7777-4777-8777-777777777777"
		},
		"current generation": func(h *history, _ *artifacts, _ *deploymentrollback.Request) {
			h.snapshot.Generation = h.current.Generation
			h.op.Generation = h.current.Generation
		},
		"failed source": func(h *history, _ *artifacts, _ *deploymentrollback.Request) { h.op.Status = "failed" },
		"wrong kind":    func(h *history, _ *artifacts, _ *deploymentrollback.Request) { h.op.Kind = "platform-upgrade" },
		"artifact unavailable": func(_ *history, a *artifacts, _ *deploymentrollback.Request) {
			a.err = deploymentrollback.ErrArtifactUnavailable
		},
		"forbidden":               func(h *history, _ *artifacts, _ *deploymentrollback.Request) { h.authErr = store.ErrForbidden },
		"caller image impossible": func(_ *history, _ *artifacts, r *deploymentrollback.Request) { r.SourceOperationID += "-forged" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			h, registry, request := fixture()
			mutate(&h, registry, &request)
			_, err := (&deploymentrollback.Resolver{History: h, Artifacts: registry}).Resolve(t.Context(), request)
			if err == nil {
				t.Fatal("unsafe rollback source accepted")
			}
		})
	}
}

func TestResolverHidesMissingAndRejectsCorruptHistory(t *testing.T) {
	h, registry, request := fixture()
	h.getErr = store.ErrNotFound
	if _, err := (&deploymentrollback.Resolver{History: h, Artifacts: registry}).Resolve(t.Context(), request); !errors.Is(err, deploymentrollback.ErrNotFound) {
		t.Fatalf("missing classification=%v", err)
	}
	h, registry, request = fixture()
	h.snapshot.ConfigRaw = nil
	if _, err := (&deploymentrollback.Resolver{History: h, Artifacts: registry}).Resolve(t.Context(), request); !errors.Is(err, deploymentrollback.ErrConflict) {
		t.Fatalf("corrupt classification=%v", err)
	}
}

func TestResolveAuthorizedAllowsReplayRecoveryBeforeMutableArtifactCheck(t *testing.T) {
	h, registry, request := fixture()
	registry.err = deploymentrollback.ErrArtifactUnavailable
	resolver := &deploymentrollback.Resolver{History: h, Artifacts: registry}
	source, err := resolver.ResolveAuthorized(t.Context(), request)
	if err != nil || source.Create.Image != h.snapshot.Image {
		t.Fatalf("authorized source=%#v err=%v", source, err)
	}
	if _, err = resolver.VerifyArtifact(t.Context(), source); !errors.Is(err, deploymentrollback.ErrArtifactUnavailable) {
		t.Fatalf("artifact classification=%v", err)
	}
}
