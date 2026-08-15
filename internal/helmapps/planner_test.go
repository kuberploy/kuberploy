package helmapps

import (
	"context"
	"errors"
	"testing"
	"time"
)

type planningStoreStub struct {
	ProtectedPublicationStore
	ProtectedCascadeStore
	nextPayload, nextCascade, nextApplication PublicationCandidate
	payloadErr, cascadeErr, applicationErr    error
	createdPayload                            ProtectedPayloadIntent
	createdCascade                            ProtectedApplicationCascadePreflight
	createdApplication                        ProtectedApplicationIntent
	payloadCreates, appCreates                int
	publisherReadiness                        []ProtectedPublisherReadiness
}

func (s *planningStoreStub) PutPublisherReadiness(_ context.Context, value ProtectedPublisherReadiness) error {
	if value.Validate() != nil {
		return ErrInvalid
	}
	s.publisherReadiness = append(s.publisherReadiness, value)
	return nil
}

func (s *planningStoreStub) NextPayloadCandidate(context.Context) (PublicationCandidate, error) {
	return s.nextPayload, s.payloadErr
}

func (s *planningStoreStub) NextApplicationCandidate(context.Context, ProtectedPublisherIdentity) (PublicationCandidate, error) {
	return s.nextApplication, s.applicationErr
}

func (s *planningStoreStub) NextCascadeCandidate(context.Context) (PublicationCandidate, error) {
	if s.cascadeErr == nil && s.nextCascade.Kind == "" {
		return PublicationCandidate{}, ErrNotFound
	}
	return s.nextCascade, s.cascadeErr
}

func (s *planningStoreStub) CreateCascadePreflightForPayload(_ context.Context,
	preflightID, deleteIntentID, _ string, _ ProtectedApplicationRuntime,
	_ ProtectedPublisherIdentity, _ time.Time) (ProtectedApplicationCascadePreflight, bool, error) {
	value := s.createdCascade
	value.ID, value.DeleteIntentID = preflightID, deleteIntentID
	return value, false, nil
}

func (s *planningStoreStub) CreatePayloadForHead(_ context.Context, intentID string, target ReleaseTarget,
	binding ProtectedBindingSnapshot, _ ProtectedPublisherIdentity, _ time.Time) (ProtectedPayloadIntent, bool, error) {
	s.payloadCreates++
	value := s.createdPayload
	value.ID, value.Target, value.Binding = intentID, target, binding
	return value, false, nil
}

func (s *planningStoreStub) CreateApplicationForPayload(_ context.Context, intentID, _ string,
	_ ProtectedApplicationRuntime, _ ProtectedPublisherIdentity, _ time.Time) (ProtectedApplicationIntent, bool, error) {
	s.appCreates++
	value := s.createdApplication
	value.ID = intentID
	return value, false, nil
}

type bindingResolverStub struct {
	binding ProtectedBindingSnapshot
	err     error
	target  ReleaseTarget
	calls   int
}

func (r *bindingResolverStub) ResolveProtectedBinding(_ context.Context, target ReleaseTarget) (ProtectedBindingSnapshot, error) {
	r.calls++
	r.target = target
	return r.binding, r.err
}

func testPublicationPlanner(t *testing.T, store *planningStoreStub, resolver *bindingResolverStub,
	payload ProtectedPayloadIntent, runtime ProtectedApplicationRuntime) PublicationPlanner {
	t.Helper()
	return PublicationPlanner{Store: store, Bindings: resolver, Publisher: payload.Publisher,
		Application: runtime, NewID: func() string { return testCommandID }, Now: func() time.Time { return payload.UpdatedAt.Add(time.Second) }}
}

func TestPublicationPlannerPromotesVerifiedPayloadBeforeNewPayload(t *testing.T) {
	release, payload, runtime := protectedApplicationFixture(t)
	store := &planningStoreStub{nextApplication: PublicationCandidate{Kind: PublicationApplication,
		ReleaseRevisionID: release.ID, PayloadIntentID: payload.ID, Target: release.Target},
		createdApplication: ProtectedApplicationIntent{ReleaseRevisionID: release.ID, Target: release.Target}}
	resolver := &bindingResolverStub{binding: payload.Binding}
	planner := testPublicationPlanner(t, store, resolver, payload, runtime)

	result, err := planner.ProcessOne(context.Background())
	if err != nil || result.Kind != PublicationApplication || result.IntentID != testCommandID {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if store.appCreates != 1 || store.payloadCreates != 0 || resolver.calls != 0 {
		t.Fatalf("unexpected calls: application=%d payload=%d resolver=%d", store.appCreates, store.payloadCreates, resolver.calls)
	}
}

func TestPublicationPlannerDerivesPayloadBindingOnlyFromTrustedResolver(t *testing.T) {
	release, payload, runtime := protectedApplicationFixture(t)
	store := &planningStoreStub{applicationErr: ErrNotFound,
		nextPayload:    PublicationCandidate{Kind: PublicationPayload, ReleaseRevisionID: release.ID, Target: release.Target},
		createdPayload: ProtectedPayloadIntent{ReleaseRevisionID: release.ID}}
	resolver := &bindingResolverStub{binding: payload.Binding}
	planner := testPublicationPlanner(t, store, resolver, payload, runtime)

	result, err := planner.ProcessOne(context.Background())
	if err != nil || result.Kind != PublicationPayload || result.IntentID != testCommandID {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if resolver.calls != 1 || resolver.target != release.Target || store.payloadCreates != 1 || store.appCreates != 0 {
		t.Fatalf("unexpected calls: resolver=%d payload=%d application=%d", resolver.calls, store.payloadCreates, store.appCreates)
	}
}

func TestPublicationPlannerFailsClosedOnInvalidResolvedBinding(t *testing.T) {
	release, payload, runtime := protectedApplicationFixture(t)
	store := &planningStoreStub{applicationErr: ErrNotFound,
		nextPayload: PublicationCandidate{Kind: PublicationPayload, ReleaseRevisionID: release.ID, Target: release.Target}}
	resolver := &bindingResolverStub{binding: ProtectedBindingSnapshot{}}
	planner := testPublicationPlanner(t, store, resolver, payload, runtime)

	if _, err := planner.ProcessOne(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("ProcessOne() error = %v, want %v", err, ErrConflict)
	}
	if store.payloadCreates != 0 {
		t.Fatal("invalid resolver output reached the durable protected store")
	}
}

func TestPublicationCandidateRejectsMixedOrCallerShapedIdentity(t *testing.T) {
	release, payload, _ := protectedApplicationFixture(t)
	valid := PublicationCandidate{Kind: PublicationPayload, ReleaseRevisionID: release.ID, Target: release.Target}
	mutations := []PublicationCandidate{
		{},
		func() PublicationCandidate { value := valid; value.Kind = "caller"; return value }(),
		func() PublicationCandidate { value := valid; value.PayloadIntentID = payload.ID; return value }(),
		func() PublicationCandidate { value := valid; value.ReleaseRevisionID = "not-a-uuid"; return value }(),
		func() PublicationCandidate { value := valid; value.Target = ReleaseTarget{}; return value }(),
		{Kind: PublicationApplication, ReleaseRevisionID: release.ID, Target: release.Target},
	}
	for index, mutation := range mutations {
		if mutation.Validate() == nil {
			t.Fatalf("mutation %d unexpectedly validated: %+v", index, mutation)
		}
	}
}
