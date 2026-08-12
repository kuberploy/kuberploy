package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/builds"
)

type retryExecutionStore struct {
	builds.APIStore
	source            builds.BuildAttempt
	definition        builds.BuildDefinition
	existing          *builds.BuildAttempt
	commandReplay     bool
	capturedExecution builds.ExecutionSettings
}

func (s *retryExecutionStore) Attempt(_ context.Context, attemptID string) (builds.BuildAttempt, error) {
	if attemptID == s.source.ID {
		return s.source, nil
	}
	if s.existing != nil && attemptID == s.existing.ID {
		return *s.existing, nil
	}
	return builds.BuildAttempt{}, builds.ErrNotFound
}

func (s *retryExecutionStore) Definition(context.Context, string) (builds.BuildDefinition, error) {
	return s.definition, nil
}

func (s *retryExecutionStore) ClaimAPICommand(_ context.Context, _, _, _, _, _, resourceID string, _ time.Time) (string, bool, error) {
	return resourceID, s.commandReplay, nil
}

func (s *retryExecutionStore) RetryAttempt(_ context.Context, _, retryID, claimKey string, execution builds.ExecutionSettings, now time.Time) (builds.BuildAttempt, bool, error) {
	s.capturedExecution = execution
	return builds.BuildAttempt{ID: retryID, DefinitionID: s.definition.ID, DeliveryClaimKey: claimKey, State: builds.AttemptQueued, CreatedAt: now}, false, nil
}

type retryExecutionResolver struct {
	resolution BuildDefinitionResolution
	err        error
	calls      int
}

func (r *retryExecutionResolver) ResolveBuildDefinition(context.Context, string, string, string, string) (BuildDefinitionResolution, error) {
	r.calls++
	return r.resolution, r.err
}

func TestBuildRetryRefreshesTrustedExecutionAndReplayKeepsAcceptedAttempt(t *testing.T) {
	definitionID := "11111111-1111-4111-8111-111111111111"
	sourceID := "22222222-2222-4222-8222-222222222222"
	targetID := "33333333-3333-4333-8333-333333333333"
	definition := builds.BuildDefinition{ID: definitionID, ProjectID: "44444444-4444-4444-8444-444444444444", ServiceID: "55555555-5555-4555-8555-555555555555",
		Spec: builds.DefinitionSpec{Registry: builds.RegistryBinding{TargetID: targetID}}}
	source := builds.BuildAttempt{ID: sourceID, DefinitionID: definitionID, State: builds.AttemptFailed}
	current := builds.ExecutionSettings{BuilderAgentImage: "registry.test/builder@sha256:" + strings.Repeat("9", 64)}
	store := &retryExecutionStore{source: source, definition: definition}
	resolver := &retryExecutionResolver{resolution: BuildDefinitionResolution{Registry: builds.RegistryBinding{TargetID: targetID}, Execution: current}}
	backend, err := NewBuildBackendWithClock(store, resolver, func() time.Time { return time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}

	attempt, replay, err := backend.Retry(t.Context(), "66666666-6666-4666-8666-666666666666", sourceID, "retry-runtime-0001", "sha256:"+strings.Repeat("a", 64))
	if err != nil || replay || attempt.State != builds.AttemptQueued || store.capturedExecution.BuilderAgentImage != current.BuilderAgentImage || resolver.calls != 1 {
		t.Fatalf("attempt=%#v replay=%v captured=%#v resolverCalls=%d err=%v", attempt, replay, store.capturedExecution, resolver.calls, err)
	}

	store.commandReplay = true
	store.existing = &attempt
	resolver.err = errors.New("mutable resolver must not run for accepted replay")
	replayed, replay, err := backend.Retry(t.Context(), "66666666-6666-4666-8666-666666666666", sourceID, "retry-runtime-0001", "sha256:"+strings.Repeat("a", 64))
	if err != nil || !replay || replayed.ID != attempt.ID || resolver.calls != 1 {
		t.Fatalf("replayed=%#v replay=%v resolverCalls=%d err=%v", replayed, replay, resolver.calls, err)
	}
}
