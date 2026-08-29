package gitpublication_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/gitpublication"
)

type providerStub struct {
	createObservation gitpublication.PullRequestObservation
	createErr         error
	findResults       []findResult
	getObservation    gitpublication.PullRequestObservation
	getErr            error
	target            gitpublication.TargetHeadObservation
	targetErr         error
	ancestor          bool
	ancestorErr       error
	createRequests    []gitpublication.CreatePullRequestRequest
	findRequests      []gitpublication.FindPullRequestRequest
	getRequests       []gitpublication.GetPullRequestRequest
	ancestorCalls     [][2]string
}

type findResult struct {
	observation gitpublication.PullRequestObservation
	found       bool
	err         error
}

type verifiedMergeRefresherStub struct {
	err   error
	calls []gitpublication.Publication
}

func (r *verifiedMergeRefresherStub) RefreshVerifiedMerge(_ context.Context, publication gitpublication.Publication) error {
	r.calls = append(r.calls, publication)
	return r.err
}

func (p *providerStub) CreatePullRequest(_ context.Context, request gitpublication.CreatePullRequestRequest) (gitpublication.PullRequestObservation, error) {
	p.createRequests = append(p.createRequests, request)
	return p.createObservation, p.createErr
}

func (p *providerStub) FindPullRequest(_ context.Context, request gitpublication.FindPullRequestRequest) (gitpublication.PullRequestObservation, bool, error) {
	p.findRequests = append(p.findRequests, request)
	if len(p.findResults) == 0 {
		return gitpublication.PullRequestObservation{}, false, errors.New("unexpected find")
	}
	result := p.findResults[0]
	p.findResults = p.findResults[1:]
	return result.observation, result.found, result.err
}

func (p *providerStub) GetPullRequest(_ context.Context, request gitpublication.GetPullRequestRequest) (gitpublication.PullRequestObservation, error) {
	p.getRequests = append(p.getRequests, request)
	return p.getObservation, p.getErr
}

func (p *providerStub) ResolveTargetHead(_ context.Context, _ gitpublication.Repository, _ string) (gitpublication.TargetHeadObservation, error) {
	return p.target, p.targetErr
}

func (p *providerStub) IsAncestor(_ context.Context, _ gitpublication.Repository, ancestor, descendant string) (bool, error) {
	p.ancestorCalls = append(p.ancestorCalls, [2]string{ancestor, descendant})
	return p.ancestor, p.ancestorErr
}

func readyService(t *testing.T, provider *providerStub) (gitpublication.Service, *gitpublication.MemoryStore, gitpublication.Publication, time.Time) {
	t.Helper()
	publication, started := publicationFixture(t)
	store := gitpublication.NewMemoryStore()
	if err := store.CreatePublication(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	now := started.Add(time.Minute)
	service := gitpublication.Service{Store: store, Provider: provider, Now: func() time.Time { return now }}
	publication, err := service.RecordWriteBase(t.Context(), operationID, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	now = started.Add(2 * time.Minute)
	publication, err = service.RecordCandidate(t.Context(), operationID, candidateSHA)
	if err != nil {
		t.Fatal(err)
	}
	return service, store, publication, started
}

func observationFor(publication gitpublication.Publication, state gitpublication.PullRequestState, merged bool, observedAt time.Time) gitpublication.PullRequestObservation {
	observation := gitpublication.PullRequestObservation{
		Repository: repository, Number: 7, URL: "https://github.com/kuberploy/platform/pull/7",
		TargetRef: publication.TargetRef, HeadRef: publication.CandidateRef, HeadRevision: publication.CandidateRevision,
		State: state, Merged: merged, ObservedAt: observedAt,
	}
	if merged {
		observation.MergeRevision = mergeSHA
	}
	return observation
}

func TestEnsurePullRequestUsesOnlyExactServerDerivedIdentity(t *testing.T) {
	provider := &providerStub{}
	service, _, publication, started := readyService(t, provider)
	observation := observationFor(publication, gitpublication.PullRequestOpen, false, started.Add(90*time.Second))
	provider.findResults = []findResult{{found: false}}
	provider.createObservation = observation
	service.Now = func() time.Time { return started.Add(2 * time.Minute) }

	result, err := service.EnsurePullRequest(t.Context(), operationID)
	if err != nil || result.State != gitpublication.StatePullRequestOpen {
		t.Fatalf("publication=%#v err=%v", result, err)
	}
	if _, desired := result.DesiredRevision(); desired {
		t.Fatal("open pull request advanced desired state")
	}
	if len(provider.createRequests) != 1 {
		t.Fatalf("create calls=%d", len(provider.createRequests))
	}
	wantTitle, _ := gitpublication.PullRequestTitle(operationID)
	wantBody, _ := gitpublication.PullRequestBody(publication)
	want := gitpublication.CreatePullRequestRequest{
		Repository: repository, TargetRef: "refs/heads/main", HeadRef: publication.CandidateRef,
		HeadSHA: candidateSHA, Title: wantTitle, Body: wantBody,
	}
	if !reflect.DeepEqual(provider.createRequests[0], want) {
		t.Fatalf("create request=%#v want %#v", provider.createRequests[0], want)
	}
}

func TestLostCreateResponseRecoversSamePullRequest(t *testing.T) {
	provider := &providerStub{createErr: errors.New("connection reset after provider accepted request")}
	service, _, publication, started := readyService(t, provider)
	observation := observationFor(publication, gitpublication.PullRequestOpen, false, started.Add(90*time.Second))
	provider.findResults = []findResult{{found: false}, {observation: observation, found: true}}
	service.Now = func() time.Time { return started.Add(2 * time.Minute) }

	result, err := service.EnsurePullRequest(t.Context(), operationID)
	if err != nil || result.PullRequestNumber != 7 || len(provider.createRequests) != 1 || len(provider.findRequests) != 2 {
		t.Fatalf("publication=%#v create=%d find=%d err=%v", result, len(provider.createRequests), len(provider.findRequests), err)
	}
	if provider.findRequests[0] != provider.findRequests[1] {
		t.Fatalf("recovery lookup changed identity: %#v %#v", provider.findRequests[0], provider.findRequests[1])
	}
}

func TestClosedUnmergedObservationDoesNotAdvanceDesired(t *testing.T) {
	provider := &providerStub{}
	service, _, publication, started := readyService(t, provider)
	provider.findResults = []findResult{{observation: observationFor(publication, gitpublication.PullRequestClosed, false, started.Add(90*time.Second)), found: true}}
	service.Now = func() time.Time { return started.Add(2 * time.Minute) }

	result, err := service.EnsurePullRequest(t.Context(), operationID)
	if err != nil || result.State != gitpublication.StatePullRequestClosed {
		t.Fatalf("publication=%#v err=%v", result, err)
	}
	if revision, desired := result.DesiredRevision(); desired || revision != "" {
		t.Fatalf("closed unmerged PR became desired: %q %v", revision, desired)
	}
	if len(provider.ancestorCalls) != 0 {
		t.Fatal("closed unmerged PR reached merge ancestry verification")
	}
}

func TestMergedPullRequestRequiresExactVisibleMerge(t *testing.T) {
	provider := &providerStub{}
	service, store, publication, started := readyService(t, provider)
	merged := observationFor(publication, gitpublication.PullRequestClosed, true, started.Add(90*time.Second))
	provider.findResults = []findResult{{observation: merged, found: true}}
	provider.target = gitpublication.TargetHeadObservation{Repository: repository, TargetRef: publication.TargetRef, Revision: targetSHA, ObservedAt: started.Add(100 * time.Second)}
	service.Now = func() time.Time { return started.Add(2 * time.Minute) }

	result, err := service.EnsurePullRequest(t.Context(), operationID)
	if !errors.Is(err, gitpublication.ErrMergeNotVisible) || result.State != gitpublication.StateMergePending {
		t.Fatalf("publication=%#v err=%v", result, err)
	}
	stored, getErr := store.Publication(t.Context(), operationID)
	if getErr != nil || stored.State != gitpublication.StateMergePending {
		t.Fatalf("stored=%#v err=%v", stored, getErr)
	}
	if revision, desired := stored.DesiredRevision(); desired || revision != "" {
		t.Fatalf("unverified merge became desired: %q %v", revision, desired)
	}

	provider.getObservation = merged
	provider.ancestor = true
	service.Now = func() time.Time { return started.Add(3 * time.Minute) }
	result, err = service.Observe(t.Context(), operationID)
	if err != nil || result.State != gitpublication.StateMergeVerified {
		t.Fatalf("verified publication=%#v err=%v", result, err)
	}
	if revision, desired := result.DesiredRevision(); !desired || revision != targetSHA {
		t.Fatalf("desired revision=%q desired=%v", revision, desired)
	}
	if !reflect.DeepEqual(provider.ancestorCalls, [][2]string{{mergeSHA, targetSHA}, {mergeSHA, targetSHA}}) {
		t.Fatalf("ancestry calls=%#v", provider.ancestorCalls)
	}
}

func TestVerifiedMergeRefreshRetriesBeforePublicationBecomesTerminal(t *testing.T) {
	provider := &providerStub{}
	service, store, publication, started := readyService(t, provider)
	merged := observationFor(publication, gitpublication.PullRequestClosed, true, started.Add(90*time.Second))
	provider.findResults = []findResult{{observation: merged, found: true}}
	provider.target = gitpublication.TargetHeadObservation{Repository: repository, TargetRef: publication.TargetRef, Revision: targetSHA, ObservedAt: started.Add(100 * time.Second)}
	service.Now = func() time.Time { return started.Add(2 * time.Minute) }

	if result, err := service.EnsurePullRequest(t.Context(), operationID); !errors.Is(err, gitpublication.ErrMergeNotVisible) || result.State != gitpublication.StateMergePending {
		t.Fatalf("merge pending publication=%#v err=%v", result, err)
	}
	provider.getObservation = merged
	provider.ancestor = true
	refreshFailure := errors.New("Argo refresh unavailable")
	refresher := &verifiedMergeRefresherStub{err: refreshFailure}
	service.VerifiedMerge = refresher
	service.Now = func() time.Time { return started.Add(3 * time.Minute) }

	if result, err := service.Observe(t.Context(), operationID); !errors.Is(err, refreshFailure) || result.State != gitpublication.StateMergePending {
		t.Fatalf("failed refresh publication=%#v err=%v", result, err)
	}
	stored, err := store.Publication(t.Context(), operationID)
	if err != nil || stored.State != gitpublication.StateMergePending || len(refresher.calls) != 1 || refresher.calls[0].State != gitpublication.StateMergeVerified {
		t.Fatalf("stored=%#v refreshes=%#v err=%v", stored, refresher.calls, err)
	}

	refresher.err = nil
	service.Now = func() time.Time { return started.Add(4 * time.Minute) }
	verified, err := service.Observe(t.Context(), operationID)
	if err != nil || verified.State != gitpublication.StateMergeVerified || len(refresher.calls) != 2 {
		t.Fatalf("verified=%#v refreshes=%d err=%v", verified, len(refresher.calls), err)
	}
}

func TestSubstitutedTargetHeadCannotVerifyMerge(t *testing.T) {
	provider := &providerStub{ancestor: true}
	service, store, publication, started := readyService(t, provider)
	provider.findResults = []findResult{{observation: observationFor(publication, gitpublication.PullRequestClosed, true, started.Add(90*time.Second)), found: true}}
	provider.target = gitpublication.TargetHeadObservation{
		Repository: gitpublication.Repository{ID: repository.ID + 1, Owner: repository.Owner, Name: repository.Name},
		TargetRef:  publication.TargetRef, Revision: targetSHA, ObservedAt: started.Add(100 * time.Second),
	}
	service.Now = func() time.Time { return started.Add(2 * time.Minute) }

	_, err := service.EnsurePullRequest(t.Context(), operationID)
	if !errors.Is(err, gitpublication.ErrProviderMismatch) {
		t.Fatalf("substituted target accepted: %v", err)
	}
	stored, _ := store.Publication(t.Context(), operationID)
	if stored.State != gitpublication.StateMergePending {
		t.Fatalf("stored state=%s", stored.State)
	}
	if _, desired := stored.DesiredRevision(); desired {
		t.Fatal("substituted target advanced desired state")
	}
}

func TestReconcilerObservesExactMergeAndLeavesClosedUnmergedPending(t *testing.T) {
	provider := &providerStub{ancestor: true}
	service, store, publication, started := readyService(t, provider)
	clock := started.Add(2 * time.Minute)
	service.Now = func() time.Time { return clock }
	provider.findResults = []findResult{{observation: observationFor(publication, gitpublication.PullRequestOpen, false, started.Add(90*time.Second)), found: true}}
	opened, err := service.EnsurePullRequest(t.Context(), operationID)
	if err != nil || opened.State != gitpublication.StatePullRequestOpen {
		t.Fatalf("open=%#v err=%v", opened, err)
	}
	clock = started.Add(3 * time.Minute)
	provider.getObservation = observationFor(opened, gitpublication.PullRequestClosed, true, started.Add(150*time.Second))
	provider.target = gitpublication.TargetHeadObservation{Repository: repository, TargetRef: opened.TargetRef, Revision: targetSHA, ObservedAt: started.Add(160 * time.Second)}
	reconciler := &gitpublication.Reconciler{Store: store, Service: service, Batch: 10, PollInterval: time.Second}
	if observed, err := reconciler.RunOnce(t.Context()); err != nil || observed != 1 {
		t.Fatalf("observed=%d err=%v", observed, err)
	}
	verified, err := store.Publication(t.Context(), operationID)
	if err != nil || verified.State != gitpublication.StateMergeVerified || verified.TargetRevision != targetSHA {
		t.Fatalf("verified=%#v err=%v", verified, err)
	}
	if pending, err := store.PendingPublications(t.Context(), 10); err != nil || len(pending) != 0 {
		t.Fatalf("verified publication remained pending: %#v err=%v", pending, err)
	}
}
