package gitprojection

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type appConfigPolicyFunc func(context.Context, AppConfigPolicyInput) (AppConfigPolicyValidation, error)

func (fn appConfigPolicyFunc) ValidateAppConfigs(ctx context.Context, input AppConfigPolicyInput) (AppConfigPolicyValidation, error) {
	return fn(ctx, input)
}

func TestMemoryActivationPersistsPolicyDiagnosticsAndRollsBackPolicyFailure(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	store := NewMemoryStore()
	binding := coordinatorBinding(t, base)
	if err := store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	work, err := store.ClaimReconciliation(t.Context(), "projection-policy-owner", base.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	binding, _, err = store.RecordVerifiedHead(t.Context(), VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef,
		Commit: head, Source: ObservationPoll, ProviderRequest: "projection-policy-1", ObservedAt: base.Add(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := store.BeginGeneration(t.Context(), work.Lease, head, binding.ParserVersion, base.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	applicationID := "44444444-4444-4444-8444-444444444444"
	document, err := NewDocument(binding, generation.Number, applicationID, head, head, strings.Repeat("b", 40), []byte("kind: AppConfig\n"), map[string]any{"kind": "AppConfig"}, nil, base.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutDocuments(t.Context(), generation, []Document{document}); err != nil {
		t.Fatal(err)
	}
	policy := appConfigPolicyFunc(func(_ context.Context, input AppConfigPolicyInput) (AppConfigPolicyValidation, error) {
		if input.Binding.ID != binding.ID || input.Generation != generation || len(input.Current) != 1 || len(input.Previous) != 0 {
			t.Fatalf("unexpected policy input: %#v", input)
		}
		return AppConfigPolicyValidation{Diagnostics: map[string][]Diagnostic{document.Path: {{
			Code: "DynamicPolicyDenied", Detail: "The exact dynamic policy denied this AppConfig.", Pointer: "/spec/routes/0",
		}}}}, nil
	})
	binding, err = store.ActivateGeneration(t.Context(), work.Lease, generation, policy, base.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	projected, err := store.Document(t.Context(), binding.ID, document.Path)
	if err != nil {
		t.Fatal(err)
	}
	if projected.Valid || len(projected.Diagnostics) != 1 || projected.Diagnostics[0].Code != "DynamicPolicyDenied" {
		t.Fatalf("policy diagnostics were not persisted: %#v", projected)
	}

	secondHead := strings.Repeat("c", 40)
	binding, _, err = store.RecordVerifiedHead(t.Context(), VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef,
		Commit: secondHead, Source: ObservationPoll, ProviderRequest: "projection-policy-2", ObservedAt: base.Add(6 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.BeginGeneration(t.Context(), work.Lease, secondHead, binding.ParserVersion, base.Add(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	secondDocument, err := NewDocument(binding, second.Number, applicationID, secondHead, secondHead, strings.Repeat("d", 40), []byte("kind: AppConfig\n"), map[string]any{"kind": "AppConfig"}, nil, base.Add(8*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutDocuments(t.Context(), second, []Document{secondDocument}); err != nil {
		t.Fatal(err)
	}
	policyFailure := errors.New("metadata store unavailable")
	if _, err = store.ActivateGeneration(t.Context(), work.Lease, second, appConfigPolicyFunc(func(context.Context, AppConfigPolicyInput) (AppConfigPolicyValidation, error) {
		return AppConfigPolicyValidation{}, policyFailure
	}), base.Add(9*time.Second)); !errors.Is(err, policyFailure) {
		t.Fatalf("policy failure was not propagated: %v", err)
	}
	current, err := store.Binding(t.Context(), binding.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.IndexedRevision != head || current.ProjectionGeneration != generation.Number || current.State != BindingIndexing {
		t.Fatalf("failed policy activation replaced the prior indexed generation: %#v", current)
	}
	stillActive, err := store.Document(t.Context(), binding.ID, document.Path)
	if err != nil || stillActive.SourceRevision != head || stillActive.Valid {
		t.Fatalf("prior diagnostic generation was not preserved: %#v err=%v", stillActive, err)
	}
}

func TestVerifiedHeadPreservesMetadataRevalidationRequestAndMonotonicUpdate(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	store := NewMemoryStore()
	binding := coordinatorBinding(t, base)
	head := strings.Repeat("e", 40)
	indexedAt := base.Add(time.Second)
	binding.TargetHeadRevision, binding.IndexedRevision = head, head
	binding.TargetHeadObservedAt, binding.IndexedAt, binding.UpdatedAt = indexedAt, indexedAt, indexedAt
	binding.ProjectionGeneration, binding.State = 1, BindingReady
	if err := store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	metadataChangedAt := base.Add(20 * time.Second)
	if err := store.SetBindingState(t.Context(), binding.ID, head, BindingIndexing, metadataChangedAt); err != nil {
		t.Fatal(err)
	}
	// The provider response may have been timestamped just before the database
	// metadata transaction committed. It must not regress the durable wakeup.
	observedAt := base.Add(19 * time.Second)
	observed, _, err := store.RecordVerifiedHead(t.Context(), VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef,
		Commit: head, Source: ObservationPoll, ProviderRequest: "metadata-revalidation", ObservedAt: observedAt})
	if err != nil {
		t.Fatal(err)
	}
	if observed.State != BindingIndexing || !observed.UpdatedAt.After(metadataChangedAt) || !observed.TargetHeadObservedAt.Equal(observedAt) {
		t.Fatalf("provider observation erased policy revalidation request: %#v", observed)
	}
}
