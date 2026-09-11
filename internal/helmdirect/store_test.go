package helmdirect

import (
	"context"
	"testing"
	"time"
)

type serviceStoreStub struct {
	Store
	applied int
	failed  string
}

func (s *serviceStoreStub) MarkApplied(context.Context, string, time.Time) error {
	s.applied++
	return nil
}

func (s *serviceStoreStub) MarkFailed(_ context.Context, _ string, code string, _ time.Time) error {
	s.failed = code
	return nil
}

type reconcilerFunc func(context.Context, Revision) error

func (f reconcilerFunc) Reconcile(ctx context.Context, revision Revision) error {
	return f(ctx, revision)
}

func TestServiceKeepsProgressingArgoRevisionPending(t *testing.T) {
	now := time.Date(2026, 8, 9, 4, 0, 0, 0, time.UTC)
	revision := renderFixture(SourceOCI)
	repository := &serviceStoreStub{}
	service := Service{Store: repository, Reconciler: reconcilerFunc(func(context.Context, Revision) error { return ErrPending })}
	result, replay, err := service.finish(t.Context(), revision, now)
	if err != nil || replay || result.State != StatePending || repository.applied != 0 || repository.failed != "" {
		t.Fatalf("result=%#v replay=%v applied=%d failed=%q err=%v", result, replay, repository.applied, repository.failed, err)
	}
}

func TestServicePersistsArgoFailureCode(t *testing.T) {
	now := time.Date(2026, 8, 9, 4, 0, 0, 0, time.UTC)
	revision := renderFixture(SourceGit)
	repository := &serviceStoreStub{}
	service := Service{Store: repository, Reconciler: reconcilerFunc(func(context.Context, Revision) error {
		return ReconcileFailure{Code: "argo-invalid-spec"}
	})}
	result, replay, err := service.finish(t.Context(), revision, now)
	if err != nil || replay || result.State != StateFailed || result.FailureCode != "argo-invalid-spec" || repository.failed != "argo-invalid-spec" {
		t.Fatalf("result=%#v replay=%v failed=%q err=%v", result, replay, repository.failed, err)
	}
}
