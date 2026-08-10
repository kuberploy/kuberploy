package gitprojection

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

type controlPlaneCatalog struct {
	environment domain.Environment
	application domain.Application
	binding     Binding
	err         error
}

func (c controlPlaneCatalog) GetEnvironmentForActor(context.Context, string, string) (domain.Environment, error) {
	return c.environment, c.err
}
func (c controlPlaneCatalog) GetApplicationForActor(context.Context, string, string) (domain.Application, error) {
	return c.application, c.err
}
func (c controlPlaneCatalog) GetEnvironmentGitBindingForActor(context.Context, string, string) (Binding, error) {
	return c.binding, c.err
}

func controlPlaneBinding(t *testing.T, now time.Time) Binding {
	t.Helper()
	binding, err := NewGitHubEnvironmentBinding("11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333",
		RepositoryIdentity{Provider: "github", InstallationID: 7, RepositoryID: 8, Owner: "kuberploy", Name: "desired-state"},
		"refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	binding.State = BindingReady
	binding.TargetHeadRevision = strings.Repeat("a", 40)
	binding.IndexedRevision = binding.TargetHeadRevision
	binding.ProjectionGeneration = 1
	binding.TargetHeadObservedAt = now.Add(time.Second)
	binding.IndexedAt = now.Add(time.Second)
	binding.UpdatedAt = now.Add(time.Second)
	if err = binding.Validate(); err != nil {
		t.Fatal(err)
	}
	return binding
}

func TestControlPlanePlansExplicitAbsentOrExactETagMutation(t *testing.T) {
	now := time.Now().UTC().Add(-time.Minute)
	binding := controlPlaneBinding(t, now)
	store := NewMemoryStore()
	if err := store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	catalog := controlPlaneCatalog{
		environment: domain.Environment{ID: binding.EnvironmentID, ProjectID: binding.ProjectID},
		application: domain.Application{ID: "44444444-4444-4444-8444-444444444444", ProjectID: binding.ProjectID},
		binding:     binding,
	}
	control := &ControlPlane{Catalog: catalog, Store: store, ChartDigest: "sha256:" + strings.Repeat("c", 64), PolicyVersion: "policy-v1"}
	plan, err := control.PlanMutation(t.Context(), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", binding.EnvironmentID, catalog.application.ID, "")
	if err != nil || plan.Precondition != MutationCreateIfAbsent || plan.ExpectedETag != "" || plan.BaseRevision != binding.IndexedRevision {
		t.Fatalf("create plan=%#v err=%v", plan, err)
	}

	document, err := NewDocument(binding, 1, catalog.application.ID, binding.IndexedRevision, binding.IndexedRevision,
		strings.Repeat("b", 40), []byte("apiVersion: config.kuberploy.io/v1alpha1\nkind: AppConfig\n"), nil, nil, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.documents[binding.ID] = map[int64]map[string]Document{1: {document.Path: document}}
	store.mu.Unlock()
	etag, err := StrongETag(binding, []Document{document}, nil, control.ChartDigest, control.PolicyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = control.PlanMutation(t.Context(), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", binding.EnvironmentID, catalog.application.ID, ""); !errors.Is(err, ErrPreconditionRequired) {
		t.Fatalf("update without ETag error=%v", err)
	}
	if _, err = control.PlanMutation(t.Context(), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", binding.EnvironmentID, catalog.application.ID, `"sha256:`+strings.Repeat("0", 64)+`"`); !errors.Is(err, ErrConflict) {
		t.Fatalf("forged ETag error=%v", err)
	}
	plan, err = control.PlanMutation(t.Context(), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", binding.EnvironmentID, catalog.application.ID, etag)
	if err != nil || plan.Precondition != MutationMatchETag || plan.ExpectedETag != etag || plan.Validate(binding) != nil {
		t.Fatalf("update plan=%#v err=%v", plan, err)
	}
}

func TestControlPlaneBundleUsesBoundedExactRevisionFence(t *testing.T) {
	now := time.Now().UTC().Add(-time.Minute)
	binding := controlPlaneBinding(t, now)
	applicationID := "44444444-4444-4444-8444-444444444444"
	document, err := NewDocument(binding, 1, applicationID, binding.IndexedRevision, binding.IndexedRevision,
		strings.Repeat("b", 40), []byte("apiVersion: config.kuberploy.io/v1alpha1\nkind: AppConfig\n"), nil, nil, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	if err = store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.documents[binding.ID] = map[int64]map[string]Document{1: {document.Path: document}}
	store.mu.Unlock()
	control := &ControlPlane{Catalog: controlPlaneCatalog{
		environment: domain.Environment{ID: binding.EnvironmentID, ProjectID: binding.ProjectID},
		application: domain.Application{ID: applicationID, ProjectID: binding.ProjectID}, binding: binding,
	}, Store: store, ChartDigest: "sha256:" + strings.Repeat("c", 64), PolicyVersion: "policy-v1"}
	deployment := domain.Deployment{EnvironmentID: binding.EnvironmentID, ApplicationID: applicationID}
	bundle, err := control.Bundle(t.Context(), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", deployment, binding.IndexedRevision, 0)
	if err != nil || bundle.ETag == "" || bundle.IndexedRevision != binding.IndexedRevision {
		t.Fatalf("exact bundle=%#v err=%v", bundle, err)
	}
	if _, err = control.Bundle(t.Context(), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", deployment, strings.Repeat("d", 40), 0); !errors.Is(err, ErrStale) {
		t.Fatalf("unsatisfied zero-wait fence error=%v", err)
	}
	started := time.Now()
	if _, err = control.Bundle(t.Context(), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", deployment, strings.Repeat("d", 40), 150*time.Millisecond); !errors.Is(err, ErrStale) {
		t.Fatalf("unsatisfied bounded fence error=%v", err)
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Fatalf("revision fence wait was not bounded: %s", elapsed)
	}
	if _, err = control.Bundle(t.Context(), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", deployment, strings.Repeat("d", 40), MaximumBundleWait+time.Second); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized wait error=%v", err)
	}
}
