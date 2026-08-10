package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func referenceRuntime(ref domain.SecretBindingRef, environmentName string) domain.WorkloadRuntime {
	return domain.WorkloadRuntime{
		Replicas: 1,
		Ports:    []domain.WorkloadPort{{Name: "http", ContainerPort: 3000, Protocol: "TCP"}},
		Env: []domain.WorkloadEnv{{
			Name: environmentName, ValueFrom: &domain.WorkloadEnvValueFrom{SecretBindingRef: ref},
		}},
		Resources: domain.WorkloadResources{Requests: domain.ResourceList{CPU: "50m", Memory: "100Mi"}},
	}
}

func TestResolveWorkloadBindingReferencesProducesCanonicalSafePlan(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	provider := &exactReferenceProvider{}
	service := referenceService(store, provider)
	created, err := service.Create(ctx, createRequest(t, ProviderSealedSecrets, "reference-plan-value", "reference-plan-001"))
	if err != nil {
		t.Fatal(err)
	}
	active, err := service.ReconcileVersion(ctx, created.Version.ID, "reference-plan-ready")
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.SecretBindingRef{BindingID: active.Binding.ID, Name: active.Binding.Name, Key: "password", Version: 1}
	plan, err := ResolveWorkloadBindingReferences(ctx, store, active.Binding.Scope, referenceRuntime(ref, "DATABASE_PASSWORD"))
	if err != nil || plan.Validate() != nil || len(plan.Uses) != 1 {
		t.Fatalf("plan=%#v err=%v", plan, err)
	}
	use := plan.Uses[0]
	if use.BindingID != active.Binding.ID || use.VersionID != active.Version.ID || use.Delivery.EnvironmentName != "DATABASE_PASSWORD" ||
		use.TargetSecretName != TargetSecretName(active.Binding, 1) {
		t.Fatalf("use=%#v", use)
	}
	identities := plan.BindingVersions()
	if len(identities) != 1 || identities[0].BindingID != active.Binding.ID || identities[0].VersionID != active.Version.ID {
		t.Fatalf("identities=%#v", identities)
	}
	digest, err := plan.Digest()
	if err != nil || !digestRE.MatchString(digest) {
		t.Fatalf("digest=%q err=%v", digest, err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"reference-plan-value", "artifact", "ciphertext", "fingerprint", "providerRevision", "manifestDigest"} {
		if strings.Contains(strings.ToLower(string(encoded)), strings.ToLower(forbidden)) {
			t.Fatalf("unsafe plan: %s", encoded)
		}
	}
}

func TestResolveWorkloadBindingReferencesRejectsDeliveryAndScopeDrift(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	provider := &exactReferenceProvider{}
	service := referenceService(store, provider)
	created, err := service.Create(ctx, createRequest(t, ProviderSealedSecrets, "reference-plan-value", "reference-plan-002"))
	if err != nil {
		t.Fatal(err)
	}
	active, err := service.ReconcileVersion(ctx, created.Version.ID, "reference-plan-ready-2")
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.SecretBindingRef{BindingID: active.Binding.ID, Name: active.Binding.Name, Key: "password", Version: 1}
	if _, err = ResolveWorkloadBindingReferences(ctx, store, active.Binding.Scope, referenceRuntime(ref, "OTHER_VARIABLE")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("delivery drift err=%v", err)
	}
	wrongScope := active.Binding.Scope
	wrongScope.EnvironmentID = "10000000-0000-4000-8000-000000000099"
	if _, err = ResolveWorkloadBindingReferences(ctx, store, wrongScope, referenceRuntime(ref, "DATABASE_PASSWORD")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("scope drift err=%v", err)
	}

	empty := referenceRuntime(ref, "DATABASE_PASSWORD")
	empty.Env = nil
	plan, err := ResolveWorkloadBindingReferences(ctx, store, active.Binding.Scope, empty)
	if err != nil || len(plan.Uses) != 0 || plan.BindingVersions() == nil {
		t.Fatalf("empty plan=%#v err=%v", plan, err)
	}
}
