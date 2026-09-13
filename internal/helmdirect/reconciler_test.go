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

func observedRevisionState(revision Revision) ApplicationState {
	source, _ := revision.Source.Normalize()
	observed := map[string]any{"repoURL": source.RepositoryURL, "targetRevision": source.TargetRevision,
		"helm": map[string]any{"releaseName": revision.ReleaseName, "values": string(revision.ValuesYAML)}}
	if source.Chart != "" {
		observed["chart"] = source.Chart
	}
	if source.Path != "" {
		observed["path"] = source.Path
	}
	return ApplicationState{EnvironmentID: revision.Target.EnvironmentID, ApplicationID: revision.Target.ApplicationID,
		ProjectID: revision.Target.ProjectID, ArgoProject: HelmAppProject, ObservedSource: observed,
		ObservedDestination: map[string]any{"server": InClusterURL, "namespace": revision.DestinationNamespace},
		Sync:                "Synced", Health: "Healthy"}
}

func TestArgoReconcilerAppliesAndDeletesOnlyOwnedApplication(t *testing.T) {
	api := &recordingApplicationAPI{}
	reconciler := ArgoReconciler{API: api, Namespace: ArgoNamespace}
	revision := renderFixture(SourceGit)
	legacyName := legacyApplicationName(revision.Target.ApplicationID)
	settledAt := revision.UpdatedAt
	api.state = observedRevisionState(revision)
	api.state.ReconciledAt = &settledAt
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
	api := &recordingApplicationAPI{state: observedRevisionState(revision)}
	api.state.Sync, api.state.Health, api.state.ReconciledAt = "OutOfSync", "Progressing", &settledAt
	reconciler := ArgoReconciler{API: api, Namespace: ArgoNamespace}
	if err := reconciler.Reconcile(t.Context(), revision); !errors.Is(err, ErrPending) {
		t.Fatalf("progressing reconcile err=%v", err)
	}
	api.state = observedRevisionState(revision)
	api.state.ConditionType, api.state.ReconciledAt = "ComparisonError", &settledAt
	var failure ReconcileFailure
	if err := reconciler.Reconcile(t.Context(), revision); !errors.As(err, &failure) || failure.Code != "argo-comparison-failed" {
		t.Fatalf("failed reconcile code=%q err=%v", failure.Code, err)
	}
}

func TestArgoReconcilerRejectsPreviousSourceSuccessAndFailure(t *testing.T) {
	revision := renderFixture(SourceOCI)
	for _, status := range []string{"success", "condition", "operation"} {
		t.Run(status, func(t *testing.T) {
			state := observedRevisionState(revision)
			state.ObservedSource["helm"].(map[string]any)["values"] = "replicas: 99\n"
			switch status {
			case "condition":
				state.ConditionType = "InvalidSpecError"
			case "operation":
				state.Operation = "Failed"
			}
			api := &recordingApplicationAPI{state: state}
			if err := (ArgoReconciler{API: api, Namespace: ArgoNamespace}).Reconcile(t.Context(), revision); !errors.Is(err, ErrPending) {
				t.Fatalf("previous source %s err=%v", status, err)
			}
		})
	}
}

func TestArgoReconcilerAcceptsExactObservedIntentWithOlderTimestamp(t *testing.T) {
	revision := renderFixture(SourceOCI)
	revision.UpdatedAt = revision.UpdatedAt.Add(76 * time.Millisecond)
	for _, age := range []time.Duration{0, 3 * time.Second, 3 * time.Minute} {
		state := observedRevisionState(revision)
		at := revision.UpdatedAt.Truncate(time.Second).Add(-age)
		state.ReconciledAt = &at
		api := &recordingApplicationAPI{state: state}
		if err := (ArgoReconciler{API: api, Namespace: ArgoNamespace}).Reconcile(t.Context(), revision); err != nil {
			t.Fatalf("exact observed intent with timestamp age %s: %v", age, err)
		}
	}
}

func TestArgoReconcilerFencesFullSourceDestinationAndOwnership(t *testing.T) {
	revision := renderFixture(SourceGit)
	cases := map[string]func(*ApplicationState){
		"missing source": func(s *ApplicationState) { s.ObservedSource = nil },
		"repository":     func(s *ApplicationState) { s.ObservedSource["repoURL"] = "https://example.com/another.git" },
		"revision":       func(s *ApplicationState) { s.ObservedSource["targetRevision"] = "other" },
		"path":           func(s *ApplicationState) { s.ObservedSource["path"] = "other" },
		"release name":   func(s *ApplicationState) { s.ObservedSource["helm"].(map[string]any)["releaseName"] = "other" },
		"extra Helm setting": func(s *ApplicationState) {
			s.ObservedSource["helm"].(map[string]any)["valueFiles"] = []any{"other.yaml"}
		},
		"namespace":    func(s *ApplicationState) { s.ObservedDestination["namespace"] = "other" },
		"server":       func(s *ApplicationState) { s.ObservedDestination["server"] = "https://other.example.com" },
		"environment":  func(s *ApplicationState) { s.EnvironmentID = "other" },
		"application":  func(s *ApplicationState) { s.ApplicationID = "other" },
		"project":      func(s *ApplicationState) { s.ProjectID = "other" },
		"Argo project": func(s *ApplicationState) { s.ArgoProject = "other" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			state := observedRevisionState(revision)
			change(&state)
			api := &recordingApplicationAPI{state: state}
			if err := (ArgoReconciler{API: api, Namespace: ArgoNamespace}).Reconcile(t.Context(), revision); !errors.Is(err, ErrPending) {
				t.Fatalf("mismatched %s err=%v", name, err)
			}
		})
	}
}

func TestArgoReconcilerLaterValuesAndRollbackRequireTheirObservedValues(t *testing.T) {
	first := renderFixture(SourceHelmRepository)
	api := &recordingApplicationAPI{state: observedRevisionState(first)}
	r := ArgoReconciler{API: api, Namespace: ArgoNamespace}
	second := first
	second.ValuesYAML = []byte("replicas: 2\n")
	second.ValuesDigest = Digest(second.ValuesYAML)
	second.UpdatedAt = first.UpdatedAt.Add(time.Second)
	if err := r.Reconcile(t.Context(), second); !errors.Is(err, ErrPending) {
		t.Fatalf("new values accepted old observation: %v", err)
	}
	api.state = observedRevisionState(second)
	if err := r.Reconcile(t.Context(), second); err != nil {
		t.Fatalf("new values: %v", err)
	}
	repeated := second
	repeated.UpdatedAt = second.UpdatedAt.Add(time.Minute)
	if err := r.Reconcile(t.Context(), repeated); err != nil {
		t.Fatalf("identical intent should already be satisfied: %v", err)
	}
	rollback := first
	rollback.Action = ActionRollback
	rollback.Generation = 2
	rollback.ParentRevisionID = first.ID
	rollback.RollbackSourceRevisionID = first.ID
	rollback.ID = "66666666-6666-4666-8666-666666666666"
	rollback.UpdatedAt = second.UpdatedAt.Add(time.Minute)
	if err := r.Reconcile(t.Context(), rollback); !errors.Is(err, ErrPending) {
		t.Fatalf("rollback accepted newer values: %v", err)
	}
	api.state = observedRevisionState(rollback)
	if err := r.Reconcile(t.Context(), rollback); err != nil {
		t.Fatalf("observed rollback: %v", err)
	}
}

func TestArgoReconcilerPreservesLegacyApplicationOwnedByAnotherEnvironment(t *testing.T) {
	revision := renderFixture(SourceGit)
	legacyName := legacyApplicationName(revision.Target.ApplicationID)
	api := &recordingApplicationAPI{
		state:  observedRevisionState(revision),
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

func TestArgoReconcilerMatchesRenderedBlankValuesForEverySource(t *testing.T) {
	for _, kind := range []SourceKind{SourceHelmRepository, SourceOCI, SourceGit} {
		t.Run(string(kind), func(t *testing.T) {
			revision := renderFixture(kind)
			values, err := NormalizeValues(nil)
			if err != nil {
				t.Fatal(err)
			}
			revision.ValuesYAML, revision.ValuesDigest = values, Digest(values)
			if kind == SourceGit {
				revision.Source.Path = "."
			}
			state := observedRevisionState(revision)
			if state.ObservedSource["helm"].(map[string]any)["values"] != "{}\n" {
				t.Fatal("blank values were not normalized")
			}
			api := &recordingApplicationAPI{state: state}
			if err := (ArgoReconciler{API: api, Namespace: ArgoNamespace}).Reconcile(t.Context(), revision); err != nil {
				t.Fatal(err)
			}
			for _, missing := range []any{nil, map[string]any{}, map[string]any{"values": nil}} {
				state.ObservedSource["helm"] = missing
				api.state = state
				if err := (ArgoReconciler{API: api, Namespace: ArgoNamespace}).Reconcile(t.Context(), revision); !errors.Is(err, ErrPending) {
					t.Fatalf("incomplete Helm observation accepted: %v", err)
				}
			}
		})
	}
}
