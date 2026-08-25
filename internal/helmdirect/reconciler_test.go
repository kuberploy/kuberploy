package helmdirect

import (
	"context"
	"testing"
)

type recordingApplicationAPI struct {
	applied []byte
	deleted string
}

func (f *recordingApplicationAPI) Apply(_ context.Context, _, _ string, manifest []byte) error {
	f.applied = append([]byte(nil), manifest...)
	return nil
}
func (f *recordingApplicationAPI) Delete(_ context.Context, _, name string) error {
	f.deleted = name
	return nil
}

func TestArgoReconcilerAppliesAndDeletesOnlyOwnedApplication(t *testing.T) {
	api := &recordingApplicationAPI{}
	reconciler := ArgoReconciler{API: api, Namespace: ArgoNamespace}
	revision := renderFixture(SourceGit)
	if err := reconciler.Reconcile(t.Context(), revision); err != nil || len(api.applied) == 0 {
		t.Fatalf("apply failed: bytes=%d err=%v", len(api.applied), err)
	}
	revision.Action, revision.DesiredEnabled = ActionDisable, false
	revision.Generation = 2
	revision.ParentRevisionID = revision.ID
	revision.ID = "66666666-6666-4666-8666-666666666666"
	revision.UpdatedAt = revision.UpdatedAt.Add(1)
	if err := reconciler.Reconcile(t.Context(), revision); err != nil || api.deleted != ApplicationName(revision.Target.ApplicationID) {
		t.Fatalf("delete failed: name=%q err=%v", api.deleted, err)
	}
}
