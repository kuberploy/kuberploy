package argo

import (
	"errors"
	"testing"
)

func TestMaterializeFirstEligibleCandidateSkipsTenantPolicyBlock(t *testing.T) {
	candidates := []desiredStateMaterializationCandidate{
		{environmentID: "blocked"},
		{environmentID: "ready"},
		{environmentID: "not-reached"},
	}
	visited := make([]string, 0, len(candidates))
	created, err := materializeFirstEligibleCandidate(candidates, func(candidate desiredStateMaterializationCandidate) (bool, error) {
		visited = append(visited, candidate.environmentID)
		if candidate.environmentID == "blocked" {
			return false, errDesiredStateCandidateBlocked
		}
		return true, nil
	})
	if err != nil || !created {
		t.Fatalf("eligible candidate was not materialized: created=%v err=%v", created, err)
	}
	if len(visited) != 2 || visited[0] != "blocked" || visited[1] != "ready" {
		t.Fatalf("unexpected candidate order: %#v", visited)
	}
}

func TestMaterializeFirstEligibleCandidatePreservesInfrastructureFailure(t *testing.T) {
	want := errors.New("database unavailable")
	created, err := materializeFirstEligibleCandidate(
		[]desiredStateMaterializationCandidate{{environmentID: "broken"}},
		func(desiredStateMaterializationCandidate) (bool, error) { return false, want },
	)
	if created || !errors.Is(err, want) {
		t.Fatalf("infrastructure failure was hidden: created=%v err=%v", created, err)
	}
}

func TestMaterializeFirstEligibleCandidateRequiresMaterializer(t *testing.T) {
	if created, err := materializeFirstEligibleCandidate(nil, nil); created || !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil materializer accepted: created=%v err=%v", created, err)
	}
}
