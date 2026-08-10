package gitpublication_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/gitpublication"
)

const (
	operationID  = "11111111-1111-4111-8111-111111111111"
	bindingID    = "22222222-2222-4222-8222-222222222222"
	baseSHA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	candidateSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	mergeSHA     = "cccccccccccccccccccccccccccccccccccccccc"
	targetSHA    = "dddddddddddddddddddddddddddddddddddddddd"
)

var repository = gitpublication.Repository{InstallationID: 51, ID: 41, Owner: "kuberploy", Name: "platform"}

func publicationFixture(t *testing.T) (gitpublication.Publication, time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	publication, err := gitpublication.NewPublication(operationID, bindingID, repository, "refs/heads/main", baseSHA, now)
	if err != nil {
		t.Fatal(err)
	}
	return publication, now
}

func TestCandidateRefIsDeterministicAndClosed(t *testing.T) {
	want := "refs/heads/kuberploy/operations/" + operationID
	first, err := gitpublication.CandidateRef(operationID)
	if err != nil || first != want {
		t.Fatalf("candidate ref=%q err=%v", first, err)
	}
	second, err := gitpublication.CandidateRef(operationID)
	if err != nil || second != first {
		t.Fatalf("candidate ref replay=%q err=%v", second, err)
	}
	for _, invalid := range []string{"", "11111111-1111-4111-8111-11111111111A", "refs/heads/tenant", "11111111-1111-1111-1111-111111111111"} {
		if _, err = gitpublication.CandidateRef(invalid); !errors.Is(err, gitpublication.ErrInvalid) {
			t.Fatalf("invalid operation %q accepted: %v", invalid, err)
		}
	}
}

func TestOpenAndClosedPullRequestsNeverExposeDesiredRevision(t *testing.T) {
	publication, now := publicationFixture(t)
	publication, err := publication.WithWriteBase(baseSHA, now.Add(time.Minute))
	if err == nil {
		publication, err = publication.WithCandidate(candidateSHA, now.Add(2*time.Minute))
	}
	if err != nil {
		t.Fatal(err)
	}
	open := gitpublication.PullRequestObservation{
		Repository: repository, Number: 7, URL: "https://github.com/kuberploy/platform/pull/7",
		TargetRef: publication.TargetRef, HeadRef: publication.CandidateRef, HeadRevision: candidateSHA,
		State: gitpublication.PullRequestOpen, ObservedAt: now.Add(3 * time.Minute),
	}
	publication, err = publication.WithPullRequest(open, now.Add(3*time.Minute))
	if err != nil || publication.State != gitpublication.StatePullRequestOpen {
		t.Fatalf("open publication=%#v err=%v", publication, err)
	}
	if revision, desired := publication.DesiredRevision(); desired || revision != "" {
		t.Fatalf("open PR became desired: %q %v", revision, desired)
	}
	closed := open
	closed.State, closed.ObservedAt = gitpublication.PullRequestClosed, now.Add(4*time.Minute)
	publication, err = publication.WithPullRequest(closed, now.Add(5*time.Minute))
	if err != nil || publication.State != gitpublication.StatePullRequestClosed {
		t.Fatalf("closed publication=%#v err=%v", publication, err)
	}
	if revision, desired := publication.DesiredRevision(); desired || revision != "" {
		t.Fatalf("closed unmerged PR became desired: %q %v", revision, desired)
	}
}

func TestPullRequestObservationRejectsSubstitutedIdentity(t *testing.T) {
	publication, now := publicationFixture(t)
	publication, _ = publication.WithWriteBase(baseSHA, now.Add(time.Minute))
	publication, _ = publication.WithCandidate(candidateSHA, now.Add(2*time.Minute))
	valid := gitpublication.PullRequestObservation{
		Repository: repository, Number: 7, URL: "https://github.com/kuberploy/platform/pull/7",
		TargetRef: publication.TargetRef, HeadRef: publication.CandidateRef, HeadRevision: candidateSHA,
		State: gitpublication.PullRequestOpen, ObservedAt: now.Add(3 * time.Minute),
	}
	tests := map[string]func(*gitpublication.PullRequestObservation){
		"repository": func(value *gitpublication.PullRequestObservation) { value.Repository.ID++ },
		"base":       func(value *gitpublication.PullRequestObservation) { value.TargetRef = "refs/heads/release" },
		"head":       func(value *gitpublication.PullRequestObservation) { value.HeadRef += "-other" },
		"sha":        func(value *gitpublication.PullRequestObservation) { value.HeadRevision = strings.Repeat("e", 40) },
		"url":        func(value *gitpublication.PullRequestObservation) { value.URL = "https://evil.example/pull/7" },
		"merged-open": func(value *gitpublication.PullRequestObservation) {
			value.Merged, value.MergeRevision = true, mergeSHA
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := candidate.ValidateFor(publication); !errors.Is(err, gitpublication.ErrProviderMismatch) {
				t.Fatalf("substitution accepted: %v", err)
			}
		})
	}
}

func TestMemoryStoreFencesImmutableProviderIdentity(t *testing.T) {
	publication, now := publicationFixture(t)
	store := gitpublication.NewMemoryStore()
	if err := store.CreatePublication(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	writeBase, _ := publication.WithWriteBase(baseSHA, now.Add(time.Minute))
	if err := store.CompareAndSwapPublication(t.Context(), publication, writeBase); err != nil {
		t.Fatal(err)
	}
	next, _ := writeBase.WithCandidate(candidateSHA, now.Add(2*time.Minute))
	if err := store.CompareAndSwapPublication(t.Context(), writeBase, next); err != nil {
		t.Fatal(err)
	}
	substituted := next
	substituted.CandidateRevision = strings.Repeat("e", 40)
	substituted.Version++
	substituted.UpdatedAt = now.Add(3 * time.Minute)
	if err := store.CompareAndSwapPublication(t.Context(), next, substituted); !errors.Is(err, gitpublication.ErrInvalid) {
		t.Fatalf("candidate substitution accepted: %v", err)
	}
}
