package helmapps

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/id"
	"go.yaml.in/yaml/v3"
)

func TestRenderProtectedArgoApplicationPinsOnlyTheVerifiedPayloadCommit(t *testing.T) {
	release, payload, runtime := protectedApplicationFixture(t)
	intentID := id.New()
	first, err := renderProtectedArgoApplication(intentID, release, payload, runtime,
		"kuberploy", "platform", "helm-release", "helm-release")
	if err != nil {
		t.Fatal(err)
	}
	second, err := renderProtectedArgoApplication(intentID, release, payload, runtime,
		"kuberploy", "platform", "helm-release", "helm-release")
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("Application rendering was not deterministic: %v", err)
	}
	var document map[string]any
	if err = yaml.Unmarshal(first, &document); err != nil {
		t.Fatal(err)
	}
	spec := requireProtectedMap(t, document, "spec")
	source := requireProtectedMap(t, spec, "source")
	if source["repoURL"] != "https://github.com/kuberploy/platform.git" ||
		source["targetRevision"] != payload.CommittedRevision ||
		source["path"] != protectedSourceDirectory(payload.Binding.ClusterID,
			release.Target.EnvironmentID, release.Target.ApplicationID, release.ID) {
		t.Fatalf("unsafe or mutable source: %#v", source)
	}
	directory := requireProtectedMap(t, source, "directory")
	if len(directory) != 2 || directory["recurse"] != false || directory["include"] != "release.yaml" {
		t.Fatalf("directory source escaped the one protected file: %#v", directory)
	}
	for _, forbidden := range []string{"helm", "plugin", "kustomize", "exclude", "chart", "ref"} {
		if _, present := source[forbidden]; present {
			t.Fatalf("forbidden Argo source field %q was rendered", forbidden)
		}
	}
	destination := requireProtectedMap(t, spec, "destination")
	if destination["server"] != ArgoInClusterServer || destination["namespace"] != "helm-release" {
		t.Fatalf("destination was not server-derived: %#v", destination)
	}
	if spec["project"] != "helm-release" {
		t.Fatalf("Argo project=%#v", spec["project"])
	}
	metadata := requireProtectedMap(t, document, "metadata")
	if metadata["namespace"] != runtime.ArgoNamespace || !strings.HasPrefix(metadata["name"].(string), "kp-h-") {
		t.Fatalf("Application metadata was not platform-derived: %#v", metadata)
	}
	requireProtectedForegroundResourcesFinalizer(t, first)
}

func TestRenderProtectedArgoApplicationRollbackCarriesForegroundResourcesFinalizer(t *testing.T) {
	release, payload, runtime := protectedApplicationFixture(t)
	release.ID, release.Generation, release.Action = id.New(), 2, ReleaseRollback
	release.ParentRevisionID, release.RollbackSourceRevisionID = id.New(), id.New()
	release.RenderCommandID = id.New()
	release.IntentDigest = digestBytes([]byte("rollback-release-intent"))
	release.IdempotencyKey, release.RequestID = "helm-release-rollback-0001", "rollback-test"
	release.CreatedAt = release.CreatedAt.Add(time.Second)
	if err := release.Validate(); err != nil {
		t.Fatalf("invalid rollback release fixture: %v", err)
	}
	payload.ID, payload.ReleaseRevisionID, payload.ReleaseGeneration = id.New(), release.ID, release.Generation
	payload.Path = protectedPayloadPath(payload.Binding.ClusterID, release.Target.EnvironmentID,
		release.Target.ApplicationID, release.ID, false)
	payload.IntentDigest = digestBytes([]byte("rollback-payload-intent"))
	payload.CommitTrailer = "Kuberploy-Helm-Payload-Intent: " + payload.ID
	if err := payload.Validate(); err != nil {
		t.Fatalf("invalid rollback payload fixture: %v", err)
	}

	content, err := renderProtectedArgoApplication(id.New(), release, payload, runtime,
		"kuberploy", "platform", "helm-release", "helm-release")
	if err != nil {
		t.Fatal(err)
	}
	requireProtectedForegroundResourcesFinalizer(t, content)
}

func TestProtectedMutationRejectsEveryUnownedPathAndMutableTarget(t *testing.T) {
	release, payload, runtime := protectedApplicationFixture(t)
	intentID := id.New()
	content, err := renderProtectedArgoApplication(intentID, release, payload, runtime,
		"kuberploy", "platform", "helm-release", "helm-release")
	if err != nil {
		t.Fatal(err)
	}
	application := ProtectedApplicationIntent{
		ID: intentID, ReleaseRevisionID: release.ID, PayloadIntentID: payload.ID,
		ReleaseGeneration: release.Generation, Target: release.Target,
		Action: ProtectedApplicationPublish, Binding: payload.Binding,
		PayloadRevision: payload.CommittedRevision, PayloadPath: payload.Path,
		SourceDirectory: protectedSourceDirectory(payload.Binding.ClusterID,
			release.Target.EnvironmentID, release.Target.ApplicationID, release.ID),
		ApplicationPath: protectedApplicationPath(payload.Binding.ClusterID,
			release.Target.EnvironmentID, release.Target.ApplicationID),
		Operation: "create", Precondition: "create-if-absent", Content: content,
		ContentDigest: digestBytes(content), IntentDigest: digestBytes([]byte("application-intent")),
		CommitTrailer: "Kuberploy-Helm-Application-Intent: " + intentID,
		Publisher:     payload.Publisher, Message: "publish", State: ProtectedPending,
		NextAttemptAt: payload.CreatedAt, CreatedAt: payload.CreatedAt, UpdatedAt: payload.CreatedAt,
	}
	if application.Validate() != nil {
		t.Fatal("valid protected Application fixture was rejected")
	}
	mutation, err := application.Mutation()
	if err != nil {
		t.Fatal(err)
	}
	mutations := []ProtectedMutation{
		func() ProtectedMutation { value := mutation; value.Path = "clusters/outside.yaml"; return value }(),
		func() ProtectedMutation { value := mutation; value.TargetRef = "refs/heads/main//evil"; return value }(),
		func() ProtectedMutation { value := mutation; value.RequiredAncestor = "refs/heads/main"; return value }(),
		func() ProtectedMutation {
			value := mutation
			value.CommitTrailer = "Kuberploy-Operation: caller"
			return value
		}(),
		func() ProtectedMutation { value := mutation; value.Action = ProtectedMutationDelete; return value }(),
	}
	for index, unsafe := range mutations {
		if unsafe.Validate() == nil {
			t.Fatalf("unsafe protected mutation %d was accepted: %#v", index, unsafe)
		}
	}
	payloadMutation, err := payload.Mutation()
	if err != nil {
		t.Fatal(err)
	}
	payloadMutation.RequiredAncestor = payload.CommittedRevision
	if payloadMutation.Validate() == nil {
		t.Fatal("phase-one payload accepted a caller-selected ancestor")
	}
}

func TestProtectedRuntimeShapeRejectsPartialOrUnfencedReceipts(t *testing.T) {
	_, payload, _ := protectedApplicationFixture(t)
	invalid := payload
	invalid.State, invalid.LeaseOwner, invalid.LeaseUntil = ProtectedClaimed, "", nil
	invalid.VerifiedAt, invalid.VerifiedPathDigest, invalid.ProviderRequest, invalid.CompletedAt = nil, "", "", nil
	invalid.CommittedRevision, invalid.CommittedParentRevision, invalid.CommittedAt = "", "", nil
	if !errors.Is(invalid.Validate(), ErrInvalid) {
		t.Fatal("claimed intent without a fenced lease was accepted")
	}
	invalid = payload
	invalid.VerifiedPathDigest = digestBytes([]byte("different"))
	if !errors.Is(invalid.Validate(), ErrInvalid) {
		t.Fatal("provider verification of different path bytes was accepted")
	}
	invalid = payload
	invalid.Binding.PlatformTargetRef = "refs/heads/main..attacker"
	if !errors.Is(invalid.Validate(), ErrInvalid) {
		t.Fatal("mutable/ambiguous Git ref was accepted")
	}
}

func protectedApplicationFixture(t *testing.T) (ReleaseRevision, ProtectedPayloadIntent, ProtectedApplicationRuntime) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	target := ReleaseTarget{ProjectID: id.New(), EnvironmentID: id.New(), ApplicationID: id.New()}
	release := ReleaseRevision{
		ID: id.New(), Generation: 1, Target: target, ReleaseName: "sample",
		Action: ReleaseInitial, DesiredEnabled: true,
		Approval: ApprovalKey{ID: id.New(), Revision: 1}, RenderCommandID: id.New(),
		ValuesYAML: []byte("{}\n"), ActorID: id.New(),
		IdempotencyKey: "helm-release-test-0001", RequestID: "test", CreatedAt: now,
	}
	release.ValuesDigest = digestBytes(release.ValuesYAML)
	release.IntentDigest = digestBytes([]byte("release-intent"))
	if release.Validate() != nil {
		t.Fatal("invalid release fixture")
	}
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: sample\n")
	writeBaseAt, committedAt := now.Add(time.Second), now.Add(2*time.Second)
	verifiedAt := now.Add(3 * time.Second)
	payload := ProtectedPayloadIntent{
		ID: id.New(), ReleaseRevisionID: release.ID, ReleaseGeneration: 1, Target: target,
		Action: ProtectedPayloadPublish,
		Binding: ProtectedBindingSnapshot{
			PlatformBindingID: id.New(), EnvironmentBindingID: id.New(), ClusterID: id.New(),
			PlatformTargetRef: "refs/heads/main", EnvironmentTargetRef: "refs/heads/main",
			EnvironmentRevision: strings.Repeat("a", 40), EnvironmentGeneration: 1,
			CatalogDigest: digestBytes([]byte("catalog")), PlannedBaseRevision: strings.Repeat("b", 40),
		},
		Content: content, ContentDigest: digestBytes(content),
		InventoryDigest: digestBytes([]byte("inventory")), ResourceCount: 1,
		IntentDigest: digestBytes([]byte("payload-intent")), Publisher: ProtectedPublisherIdentity{
			Contract: ProtectedPublisherContract, PolicyVersion: ProtectedGitPolicy,
			ConfigDigest: digestBytes([]byte("publisher")),
		},
		Message: "publish", State: ProtectedVerified, Attempts: 1, LeaseEpoch: 1,
		WriteBaseRevision: strings.Repeat("c", 40), WriteBaseObservedAt: &writeBaseAt,
		CommittedRevision: strings.Repeat("d", 40), CommittedParentRevision: strings.Repeat("c", 40),
		CommittedAt: &committedAt, VerifiedAt: &verifiedAt,
		VerifiedPathDigest: digestBytes(content), ProviderRequest: "provider-request",
		NextAttemptAt: now, CreatedAt: now, UpdatedAt: verifiedAt, CompletedAt: &verifiedAt,
	}
	payload.Path = protectedPayloadPath(payload.Binding.ClusterID, target.EnvironmentID,
		target.ApplicationID, release.ID, false)
	payload.CommitTrailer = "Kuberploy-Helm-Payload-Intent: " + payload.ID
	if payload.Validate() != nil {
		t.Fatal("invalid payload fixture")
	}
	return release, payload, ProtectedApplicationRuntime{ArgoNamespace: "argocd"}
}

func requireProtectedMap(t *testing.T, value map[string]any, key string) map[string]any {
	t.Helper()
	nested, ok := value[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not a mapping: %#v", key, value[key])
	}
	return nested
}

func requireProtectedForegroundResourcesFinalizer(t *testing.T, content []byte) {
	t.Helper()
	var document struct {
		Metadata struct {
			Finalizers []string `yaml:"finalizers"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	want := []string{"resources-finalizer.argocd.argoproj.io"}
	if len(document.Metadata.Finalizers) != len(want) || document.Metadata.Finalizers[0] != want[0] {
		t.Fatalf("Application must use the exact foreground cascading-delete finalizer: %#v",
			document.Metadata.Finalizers)
	}
}

func TestCascadePreflightAllowsOnlyExactLegacyFinalizerAdoption(t *testing.T) {
	release, payload, runtime := protectedApplicationFixture(t)
	applicationID := id.New()
	adopted, err := renderProtectedArgoApplication(applicationID, release, payload, runtime,
		"kuberploy", "platform", "application-ns", "project-runtime")
	if err != nil {
		t.Fatal(err)
	}
	var legacy protectedArgoApplication
	if err = yaml.Unmarshal(adopted, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.Metadata.Finalizers = nil
	source, err := yaml.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	now := release.CreatedAt.Add(5 * time.Second)
	preflight := ProtectedApplicationCascadePreflight{
		ID: id.New(), DeleteIntentID: id.New(), ReleaseRevisionID: id.New(),
		PayloadIntentID: id.New(), BaseApplicationIntentID: id.New(),
		PayloadRevision: payload.CommittedRevision, ArgoNamespace: runtime.ArgoNamespace,
		ReleaseGeneration: 2, Target: release.Target, Binding: payload.Binding,
		ApplicationPath: protectedApplicationPath(payload.Binding.ClusterID,
			release.Target.EnvironmentID, release.Target.ApplicationID),
		SourceContent: source, SourceContentDigest: digestBytes(source),
		AdoptedContent: adopted, AdoptedContentDigest: digestBytes(adopted),
		Operation: "update", Precondition: "match-etag",
		Contract: protectedCascadeContract, Publisher: payload.Publisher,
		OriginalPublisherConfigDigest: payload.Publisher.ConfigDigest,
		State:                         ProtectedPending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
	preflight.ExpectedETag = `"` + preflight.SourceContentDigest + `"`
	preflight.CommitTrailer = "Kuberploy-Helm-Cascade-Preflight: " + preflight.ID
	preflight.IntentDigest, err = cascadePreflightIntentDigest(preflight)
	if err != nil || preflight.Validate() != nil {
		t.Fatalf("valid exact adoption rejected: %+v err=%v", preflight, err)
	}
	expectation, err := preflight.ApplicationExpectation()
	if err != nil || expectation.ReleaseRevisionID != release.ID ||
		expectation.TargetRevision != payload.CommittedRevision || expectation.PayloadDigest != payload.ContentDigest {
		t.Fatalf("child identity drifted: %+v err=%v", expectation, err)
	}
	mutated := preflight
	mutated.AdoptedContent = append([]byte(nil), adopted...)
	mutated.AdoptedContent = append(mutated.AdoptedContent, []byte("# extra\n")...)
	mutated.AdoptedContentDigest = digestBytes(mutated.AdoptedContent)
	mutated.IntentDigest, _ = cascadePreflightIntentDigest(mutated)
	if !errors.Is(mutated.Validate(), ErrInvalid) {
		t.Fatal("cascade preflight accepted a postimage that changed more than the finalizer")
	}
	superseded := preflight
	completed := now.Add(time.Second)
	superseded.State, superseded.ConsecutiveFailures = ProtectedSuperseded, 1
	superseded.LastFailureCode, superseded.CompletedAt = "cascade-projection-superseded", &completed
	superseded.UpdatedAt = completed
	if superseded.Validate() != nil || superseded.Attempts != 0 || superseded.LeaseEpoch != 0 {
		t.Fatalf("pristine superseded replacement predecessor rejected: %+v", superseded)
	}
	legacy.Metadata.Finalizers = []string{"resources-finalizer.argocd.argoproj.io/background"}
	alternate, _ := yaml.Marshal(legacy)
	if _, _, err = adoptProtectedArgoResourcesFinalizer(alternate); !errors.Is(err, ErrInvalid) {
		t.Fatalf("alternate finalizer was accepted: %v", err)
	}
}
