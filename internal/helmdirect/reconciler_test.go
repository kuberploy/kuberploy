package helmdirect

import (
	"context"
	"errors"
	"testing"
	"time"
)

type recordingApplicationAPI struct {
	applied []byte
	names   []string
	deleted []string
	exists  map[string]bool
	state   ApplicationState
	states  map[string]ApplicationState
}

func (f *recordingApplicationAPI) Apply(_ context.Context, _, name string, manifest []byte) error {
	f.applied = append([]byte(nil), manifest...)
	f.names = append(f.names, name)
	if f.exists == nil {
		f.exists = make(map[string]bool)
	}
	f.exists[name] = true
	return nil
}
func (f *recordingApplicationAPI) Delete(_ context.Context, _, name string) error {
	f.deleted = append(f.deleted, name)
	if f.exists == nil {
		f.exists = make(map[string]bool)
	}
	f.exists[name] = false
	return nil
}
func (f *recordingApplicationAPI) Observe(_ context.Context, _, name string) (ApplicationState, error) {
	if !f.exists[name] {
		return ApplicationState{}, nil
	}
	state := f.state
	if named, ok := f.states[name]; ok {
		state = named
	}
	state.Exists = true
	return state, nil
}

func TestArgoReconcilerAppliesAndDeletesOnlyOwnedApplication(t *testing.T) {
	api := &recordingApplicationAPI{}
	reconciler := ArgoReconciler{API: api, Namespace: ArgoNamespace}
	revision := renderFixture(SourceGit)
	legacyName := legacyApplicationName(revision.Target.ApplicationID)
	settledAt := revision.UpdatedAt
	api.state = ApplicationState{Sync: "Synced", Health: "Healthy", ReconciledAt: &settledAt}
	api.exists = map[string]bool{legacyName: true}
	api.states = map[string]ApplicationState{legacyName: {EnvironmentID: revision.Target.EnvironmentID}}
	if err := reconciler.Reconcile(t.Context(), revision); err != nil || len(api.applied) == 0 || len(api.names) != 1 {
		t.Fatalf("apply failed: bytes=%d err=%v", len(api.applied), err)
	}
	if api.names[0] != ApplicationName(revision.Target.EnvironmentID, revision.Target.ApplicationID) || len(api.deleted) != 1 || api.deleted[0] != legacyName {
		t.Fatalf("apply names=%v legacy deletes=%v", api.names, api.deleted)
	}
	revision.Action, revision.DesiredEnabled = ActionDisable, false
	revision.Generation = 2
	revision.ParentRevisionID = revision.ID
	revision.ID = "66666666-6666-4666-8666-666666666666"
	revision.UpdatedAt = revision.UpdatedAt.Add(1)
	if err := reconciler.Reconcile(t.Context(), revision); err != nil || len(api.deleted) != 2 || api.deleted[1] != ApplicationName(revision.Target.EnvironmentID, revision.Target.ApplicationID) {
		t.Fatalf("delete failed: names=%v err=%v", api.deleted, err)
	}
}

func TestArgoReconcilerWaitsForHealthAndReportsArgoFailure(t *testing.T) {
	revision := renderFixture(SourceHelmRepository)
	settledAt := revision.UpdatedAt.Add(time.Second)
	api := &recordingApplicationAPI{state: ApplicationState{Sync: "OutOfSync", Health: "Progressing", ReconciledAt: &settledAt}}
	reconciler := ArgoReconciler{API: api, Namespace: ArgoNamespace}
	if err := reconciler.Reconcile(t.Context(), revision); !errors.Is(err, ErrPending) {
		t.Fatalf("progressing reconcile err=%v", err)
	}
	api.state = ApplicationState{ConditionType: "ComparisonError", ReconciledAt: &settledAt}
	var failure ReconcileFailure
	if err := reconciler.Reconcile(t.Context(), revision); !errors.As(err, &failure) || failure.Code != "argo-comparison-failed" {
		t.Fatalf("failed reconcile code=%q err=%v", failure.Code, err)
	}
}

func TestArgoReconcilerIgnoresStaleSuccessAndFailure(t *testing.T) {
	revision := renderFixture(SourceOCI)
	staleAt := revision.UpdatedAt.Add(-time.Nanosecond)
	api := &recordingApplicationAPI{state: ApplicationState{Sync: "Synced", Health: "Healthy", ReconciledAt: &staleAt}}
	reconciler := ArgoReconciler{API: api, Namespace: ArgoNamespace}
	if err := reconciler.Reconcile(t.Context(), revision); !errors.Is(err, ErrPending) {
		t.Fatalf("stale success reconcile err=%v", err)
	}
	api.state = ApplicationState{ConditionType: "InvalidSpecError", ReconciledAt: &staleAt}
	if err := reconciler.Reconcile(t.Context(), revision); !errors.Is(err, ErrPending) {
		t.Fatalf("stale failure reconcile err=%v", err)
	}
	api.state = ApplicationState{Operation: "Failed", ReconciledAt: &staleAt}
	if err := reconciler.Reconcile(t.Context(), revision); !errors.Is(err, ErrPending) {
		t.Fatalf("stale operation failure reconcile err=%v", err)
	}
	api.state = ApplicationState{Sync: "Synced", Health: "Healthy"}
	if err := reconciler.Reconcile(t.Context(), revision); !errors.Is(err, ErrPending) {
		t.Fatalf("missing reconciledAt reconcile err=%v", err)
	}
}

func TestArgoReconcilerPreservesLegacyApplicationOwnedByAnotherEnvironment(t *testing.T) {
	revision := renderFixture(SourceGit)
	legacyName := legacyApplicationName(revision.Target.ApplicationID)
	settledAt := revision.UpdatedAt.Add(time.Second)
	api := &recordingApplicationAPI{
		state:  ApplicationState{Sync: "Synced", Health: "Healthy", ReconciledAt: &settledAt},
		exists: map[string]bool{legacyName: true},
		states: map[string]ApplicationState{legacyName: {EnvironmentID: "77777777-7777-4777-8777-777777777777"}},
	}
	reconciler := ArgoReconciler{API: api, Namespace: ArgoNamespace}
	if err := reconciler.Reconcile(t.Context(), revision); err != nil {
		t.Fatalf("deploy reconcile err=%v", err)
	}
	if !api.exists[legacyName] || len(api.deleted) != 0 {
		t.Fatalf("foreign legacy application changed: exists=%t deletes=%v", api.exists[legacyName], api.deleted)
	}

	revision.Action, revision.DesiredEnabled = ActionDisable, false
	revision.Generation = 2
	revision.ParentRevisionID = revision.ID
	revision.ID = "66666666-6666-4666-8666-666666666666"
	revision.UpdatedAt = revision.UpdatedAt.Add(time.Second)
	if err := reconciler.Reconcile(t.Context(), revision); err != nil {
		t.Fatalf("disable reconcile err=%v", err)
	}
	if !api.exists[legacyName] || len(api.deleted) != 1 || api.deleted[0] != ApplicationName(revision.Target.EnvironmentID, revision.Target.ApplicationID) {
		t.Fatalf("foreign legacy application changed during disable: exists=%t deletes=%v", api.exists[legacyName], api.deleted)
	}
}

func TestArgoApplicationNameIncludesEnvironmentIdentity(t *testing.T) {
	first := renderFixture(SourceGit)
	second := first
	second.Target.EnvironmentID = "77777777-7777-4777-8777-777777777777"
	firstName := ApplicationName(first.Target.EnvironmentID, first.Target.ApplicationID)
	secondName := ApplicationName(second.Target.EnvironmentID, second.Target.ApplicationID)
	if firstName == secondName || len(firstName) > 63 || len(secondName) > 63 {
		t.Fatalf("environment-scoped names collide or exceed DNS limit: %q %q", firstName, secondName)
	}
	firstManifest, err := RenderApplication(first, ArgoNamespace)
	if err != nil {
		t.Fatal(err)
	}
	secondManifest, err := RenderApplication(second, ArgoNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstManifest) == string(secondManifest) {
		t.Fatal("distinct Environment Helm Apps rendered identical manifests")
	}
}
