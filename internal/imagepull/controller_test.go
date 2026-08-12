package imagepull

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

type controllerMaterialReader struct {
	value  []byte
	err    error
	called int
}

func (r *controllerMaterialReader) ReadDockerConfig(context.Context, Profile) ([]byte, error) {
	r.called++
	return append([]byte(nil), r.value...), r.err
}

type controllerSecretAPI struct {
	observation SecretObservation
	err         error
	called      int
	captured    []byte
}

type controllerProjectionInvalidator struct {
	called        int
	environmentID string
	targetID      string
	profileName   string
	revision      int64
	invalidated   bool
	err           error
}

func (i *controllerProjectionInvalidator) InvalidateMatchingProfileMismatch(_ context.Context, environmentID, targetID, profileName string, revision int64, _ time.Time) (bool, error) {
	i.called++
	i.environmentID = environmentID
	i.targetID = targetID
	i.profileName = profileName
	i.revision = revision
	return i.invalidated, i.err
}

func (a *controllerSecretAPI) EnsureImagePullSecret(_ context.Context, request SecretRequest) (SecretObservation, error) {
	a.called++
	a.captured = request.DockerConfig
	return a.observation, a.err
}

func newControllerFixture(t *testing.T) (*RuntimeController, *MemoryStore, *controllerMaterialReader, *controllerSecretAPI, time.Time, string) {
	t.Helper()
	config := testRuntimeConfig()
	store := NewMemoryStore()
	now := time.Date(2026, 8, 9, 7, 0, 0, 0, time.UTC)
	desired, err := Desired(config, testEnvironmentID, "tenant-a-dev", testTargetID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.EnsureArtifact(t.Context(), desired, now); err != nil {
		t.Fatal(err)
	}
	reader := &controllerMaterialReader{value: []byte(`{"auths":{"registry.example.test:5000":{"auth":"dXNlcjpwYXNz"}}}`)}
	api := &controllerSecretAPI{observation: SecretObservation{Namespace: desired.Namespace, Name: desired.SecretName,
		UID: "44444444-4444-4444-8444-444444444444", ResourceVersion: "123"}}
	controller := &RuntimeController{Store: store, Reader: reader, Secrets: api, Projections: &controllerProjectionInvalidator{}, Config: config,
		WorkerID: "registry-pull-worker:one", WorkerEpoch: 1, Now: func() time.Time { return now }}
	digest, _ := config.Digest()
	return controller, store, reader, api, now, digest
}

func TestControllerReconcilesExactSecretAndClearsMaterial(t *testing.T) {
	controller, store, reader, api, now, digest := newControllerFixture(t)
	didWork, err := controller.Reconcile(t.Context(), digest)
	if err != nil || !didWork || reader.called != 1 || api.called != 1 {
		t.Fatalf("work=%t err=%v reads=%d calls=%d", didWork, err, reader.called, api.called)
	}
	if len(api.captured) == 0 || !bytes.Equal(api.captured, make([]byte, len(api.captured))) {
		t.Fatalf("credential buffer retained after provider return: %q", api.captured)
	}
	desired, _ := Desired(controller.Config, testEnvironmentID, "tenant-a-dev", testTargetID)
	artifact, err := store.Artifact(t.Context(), desired.ArtifactKey)
	if err != nil || artifact.State != StateReady || artifact.LastObservedAt == nil || !artifact.LastObservedAt.Equal(now) ||
		artifact.ObservedUID != api.observation.UID || artifact.LeaseOwner != "" {
		t.Fatalf("artifact=%#v err=%v", artifact, err)
	}
	invalidator := controller.Projections.(*controllerProjectionInvalidator)
	if invalidator.called != 0 {
		t.Fatalf("initial awaiting artifact invalidated projection: %#v", invalidator)
	}
	controller.Now = func() time.Time { return artifact.NextObservationAt }
	if didWork, err = controller.Reconcile(t.Context(), digest); err != nil || !didWork {
		t.Fatalf("ready observation work=%t err=%v", didWork, err)
	}
	if invalidator.called != 1 || invalidator.environmentID != testEnvironmentID || invalidator.targetID != testTargetID {
		t.Fatalf("ready artifact did not inspect stale projection: %#v", invalidator)
	}
}

func TestControllerFailsClosedForInvalidCredentialAndObservation(t *testing.T) {
	controller, store, _, api, _, digest := newControllerFixture(t)
	controller.Reader.(*controllerMaterialReader).value = []byte(`{"auths":{"evil.example.test":{"auth":"private"}}}`)
	didWork, err := controller.Reconcile(t.Context(), digest)
	if err != nil || !didWork || api.called != 0 {
		t.Fatalf("invalid credential work=%t err=%v apiCalls=%d", didWork, err, api.called)
	}
	desired, _ := Desired(controller.Config, testEnvironmentID, "tenant-a-dev", testTargetID)
	artifact, _ := store.Artifact(t.Context(), desired.ArtifactKey)
	if artifact.State != StateFailed || artifact.LastFailureCode != "credential-source-invalid" {
		t.Fatalf("invalid credential artifact=%#v", artifact)
	}

	controller, store, _, api, _, digest = newControllerFixture(t)
	api.observation.Name = "attacker-selected"
	didWork, err = controller.Reconcile(t.Context(), digest)
	if err != nil || !didWork {
		t.Fatalf("mismatch work=%t err=%v", didWork, err)
	}
	desired, _ = Desired(controller.Config, testEnvironmentID, "tenant-a-dev", testTargetID)
	artifact, _ = store.Artifact(t.Context(), desired.ArtifactKey)
	if artifact.State != StateFailed || artifact.LastFailureCode != "secret-observation-mismatch" {
		t.Fatalf("mismatched observation artifact=%#v", artifact)
	}
}

func TestControllerInfrastructureFailurePersistsBackoffAndStalesRuntime(t *testing.T) {
	controller, store, _, api, now, digest := newControllerFixture(t)
	api.err = errors.New("Kubernetes API unavailable with private body")
	didWork, err := controller.Reconcile(t.Context(), digest)
	if !didWork || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("work=%t err=%v", didWork, err)
	}
	desired, _ := Desired(controller.Config, testEnvironmentID, "tenant-a-dev", testTargetID)
	artifact, loadErr := store.Artifact(t.Context(), desired.ArtifactKey)
	if loadErr != nil || artifact.State != StateAwaiting || artifact.LastFailureCode != "kubernetes-unavailable" ||
		artifact.ConsecutiveFailures != 1 || !artifact.NextObservationAt.Equal(now.Add(controller.Config.MinimumBackoff)) || artifact.LeaseOwner != "" {
		t.Fatalf("artifact=%#v err=%v", artifact, loadErr)
	}
}

func TestControllerTreatsLiveSecretMutationAsPermanent(t *testing.T) {
	controller, store, _, api, _, digest := newControllerFixture(t)
	api.err = ErrConflict
	didWork, err := controller.Reconcile(t.Context(), digest)
	if err != nil || !didWork {
		t.Fatalf("work=%t err=%v", didWork, err)
	}
	desired, _ := Desired(controller.Config, testEnvironmentID, "tenant-a-dev", testTargetID)
	artifact, loadErr := store.Artifact(t.Context(), desired.ArtifactKey)
	if loadErr != nil || artifact.State != StateFailed || artifact.LastFailureCode != "secret-mutation" || artifact.LeaseOwner != "" {
		t.Fatalf("artifact=%#v err=%v", artifact, loadErr)
	}
}

func TestControllerRecoversExactProfileMismatchAfterOperatorCorrection(t *testing.T) {
	controller, store, reader, api, now, _ := newControllerFixture(t)
	original := controller.Config
	original.Profiles = append([]Profile(nil), controller.Config.Profiles...)
	controller.Config.Profiles[0].Name = "rotated-profile"
	mismatchedDigest, err := controller.Config.Digest()
	if err != nil {
		t.Fatal(err)
	}
	didWork, err := controller.Reconcile(t.Context(), mismatchedDigest)
	if err != nil || !didWork || reader.called != 0 || api.called != 0 {
		t.Fatalf("mismatch work=%t err=%v reads=%d calls=%d", didWork, err, reader.called, api.called)
	}
	desired, _ := Desired(original, testEnvironmentID, "tenant-a-dev", testTargetID)
	failed, loadErr := store.Artifact(t.Context(), desired.ArtifactKey)
	if loadErr != nil || failed.State != StateFailed || failed.LastFailureCode != profileMismatchFailureCode {
		t.Fatalf("failed artifact=%#v err=%v", failed, loadErr)
	}

	controller.Config = original
	recoveryAt := failed.NextObservationAt
	controller.Now = func() time.Time { return recoveryAt }
	recoveredDigest, err := controller.Config.Digest()
	if err != nil {
		t.Fatal(err)
	}
	didWork, err = controller.Reconcile(t.Context(), recoveredDigest)
	if err != nil || !didWork || reader.called != 1 || api.called != 1 {
		t.Fatalf("recovery work=%t err=%v reads=%d calls=%d", didWork, err, reader.called, api.called)
	}
	recovered, loadErr := store.Artifact(t.Context(), desired.ArtifactKey)
	if loadErr != nil || recovered.State != StateReady || recovered.LastFailureCode != "" ||
		recovered.ConsecutiveFailures != 0 || recovered.LastObservedAt == nil || !recovered.LastObservedAt.Equal(recoveryAt) {
		t.Fatalf("recovered artifact=%#v err=%v now=%s", recovered, loadErr, now)
	}
	invalidator := controller.Projections.(*controllerProjectionInvalidator)
	if invalidator.called != 1 || invalidator.environmentID != testEnvironmentID || invalidator.targetID != testTargetID ||
		invalidator.profileName != original.Profiles[0].Name || invalidator.revision != original.Profiles[0].Revision {
		t.Fatalf("projection invalidation=%#v", invalidator)
	}
}

func TestControllerKeepsRecoveredArtifactFailedWhenProjectionInvalidationFails(t *testing.T) {
	controller, store, _, _, _, _ := newControllerFixture(t)
	original := controller.Config
	original.Profiles = append([]Profile(nil), controller.Config.Profiles...)
	controller.Config.Profiles[0].Name = "rotated-profile"
	mismatchedDigest, _ := controller.Config.Digest()
	if worked, err := controller.Reconcile(t.Context(), mismatchedDigest); err != nil || !worked {
		t.Fatalf("profile mismatch work=%t err=%v", worked, err)
	}
	desired, _ := Desired(original, testEnvironmentID, "tenant-a-dev", testTargetID)
	failed, _ := store.Artifact(t.Context(), desired.ArtifactKey)
	controller.Config = original
	controller.Now = func() time.Time { return failed.NextObservationAt }
	controller.Projections.(*controllerProjectionInvalidator).err = errors.New("projection unavailable")
	recoveredDigest, _ := controller.Config.Digest()
	worked, err := controller.Reconcile(t.Context(), recoveredDigest)
	if !worked || err == nil {
		t.Fatalf("work=%t err=%v", worked, err)
	}
	artifact, loadErr := store.Artifact(t.Context(), desired.ArtifactKey)
	if loadErr != nil || artifact.State != StateFailed || artifact.LastFailureCode != profileMismatchFailureCode || artifact.LastObservedAt != nil {
		t.Fatalf("artifact=%#v err=%v", artifact, loadErr)
	}
}

func TestControllerDoesNotReclaimOtherPermanentFailures(t *testing.T) {
	controller, store, _, api, _, digest := newControllerFixture(t)
	api.err = ErrConflict
	didWork, err := controller.Reconcile(t.Context(), digest)
	if err != nil || !didWork {
		t.Fatalf("initial work=%t err=%v", didWork, err)
	}
	desired, _ := Desired(controller.Config, testEnvironmentID, "tenant-a-dev", testTargetID)
	failed, loadErr := store.Artifact(t.Context(), desired.ArtifactKey)
	if loadErr != nil || failed.State != StateFailed || failed.LastFailureCode != "secret-mutation" {
		t.Fatalf("failed artifact=%#v err=%v", failed, loadErr)
	}
	controller.Now = func() time.Time { return failed.NextObservationAt }
	didWork, err = controller.Reconcile(t.Context(), digest)
	if err != nil || didWork || api.called != 1 {
		t.Fatalf("terminal replay work=%t err=%v calls=%d", didWork, err, api.called)
	}
}

func TestControllerRejectsWrongRuntimeDigestBeforeClaim(t *testing.T) {
	controller, store, _, api, now, _ := newControllerFixture(t)
	didWork, err := controller.Reconcile(t.Context(), "sha256:"+stringsOf('a', 64))
	if didWork || !errors.Is(err, ErrUnavailable) || api.called != 0 {
		t.Fatalf("work=%t err=%v calls=%d", didWork, err, api.called)
	}
	desired, _ := Desired(controller.Config, testEnvironmentID, "tenant-a-dev", testTargetID)
	artifact, _ := store.Artifact(t.Context(), desired.ArtifactKey)
	if artifact.LeaseOwner != "" || !artifact.NextObservationAt.Equal(now) {
		t.Fatalf("wrong digest mutated work: %#v", artifact)
	}
}

func TestControllerDoesNotAdvertiseReadinessBeforeCredentialPreflight(t *testing.T) {
	controller, store, reader, _, now, digest := newControllerFixture(t)
	reader.value = []byte(`{"auths":{"evil.example.test":{"auth":"dXNlcjpwYXNz"}}}`)
	if err := controller.Run(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("invalid projected credential startup err=%v", err)
	}
	if reader.called != 1 {
		t.Fatalf("credential preflight reads=%d", reader.called)
	}
	if err := store.RuntimeReady(t.Context(), RuntimeContract, digest, len(controller.Config.Profiles), now); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("runtime advertised readiness before credential preflight: %v", err)
	}
}
