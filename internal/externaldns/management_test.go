package externaldns

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

type managementStoreStub struct {
	created      domain.ExternalDNSIntegration
	integrations []domain.ExternalDNSIntegration
	application  []domain.ExternalDNSIntegration
}

func (stub *managementStoreStub) ListExternalDNSIntegrationsForActor(context.Context, string) ([]domain.ExternalDNSIntegration, error) {
	return stub.integrations, nil
}
func (stub *managementStoreStub) CreateExternalDNSIntegrationForActor(_ context.Context, _ string, _, _, _ string, item domain.ExternalDNSIntegration) (store.Result[domain.ExternalDNSIntegration], error) {
	stub.created = item
	return store.Result[domain.ExternalDNSIntegration]{Value: item}, nil
}
func (stub *managementStoreStub) UpdateExternalDNSIntegrationForActor(_ context.Context, _ string, _, _, _ string, item domain.ExternalDNSIntegration) (store.Result[domain.ExternalDNSIntegration], error) {
	return store.Result[domain.ExternalDNSIntegration]{Value: item}, nil
}
func (stub *managementStoreStub) ExternalDNSIntegrationsForEnvironmentActor(context.Context, string, string) ([]domain.ExternalDNSIntegration, error) {
	return stub.application, nil
}
func (stub *managementStoreStub) ExternalDNSIntegrationsForApplicationActor(context.Context, string, string, string) ([]domain.ExternalDNSIntegration, error) {
	return stub.application, nil
}

func validManagedInput() IntegrationInput {
	return IntegrationInput{
		Slug: "public-dns", Name: "Public DNS", Mode: ModeManaged, ProviderKind: "cloudflare",
		TXTOwnerID: "kuberploy.prod", AllowedDomainSuffixes: []string{" Apps.Example.COM. ", "api.example.com"},
		CredentialSecretRef: "external-dns-credentials", ProviderConfigRef: "cloudflare-provider",
		EgressConfigRef: "internet-egress", EnvironmentIDs: []string{"22222222-2222-2222-2222-222222222222", "11111111-1111-1111-1111-111111111111"},
	}
}

func TestCreateNormalizesSafeMetadataAndDefaultsToUpsertOnly(t *testing.T) {
	stub := &managementStoreStub{}
	management := NewManagement(stub, WithIDGenerator(func() string { return "33333333-3333-3333-3333-333333333333" }))
	result, err := management.Create(context.Background(), "actor", "0123456789abcdef", "fingerprint", "request", validManagedInput())
	if err != nil {
		t.Fatal(err)
	}
	if result.Value.SyncPolicy != SyncPolicyUpsert || result.Value.DestructiveSyncConfirmed {
		t.Fatalf("unsafe default: %#v", result.Value)
	}
	if !reflect.DeepEqual(result.Value.AllowedDomainSuffixes, []string{"api.example.com", "apps.example.com"}) {
		t.Fatalf("suffix normalization drifted: %#v", result.Value.AllowedDomainSuffixes)
	}
	if !reflect.DeepEqual(result.Value.EnvironmentIDs, []string{"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"}) {
		t.Fatalf("environment assignments are not deterministic: %#v", result.Value.EnvironmentIDs)
	}
}

func TestValidationRejectsMixedModesUnsafeSyncAndEndpointLikeRefs(t *testing.T) {
	base := integrationFromInput("33333333-3333-3333-3333-333333333333", "actor", validManagedInput())
	tests := []domain.ExternalDNSIntegration{
		func() domain.ExternalDNSIntegration { item := base; item.OperatorProfileRef = "operator"; return item }(),
		func() domain.ExternalDNSIntegration { item := base; item.SyncPolicy = SyncPolicySync; return item }(),
		func() domain.ExternalDNSIntegration { item := base; item.DestructiveSyncConfirmed = true; return item }(),
		func() domain.ExternalDNSIntegration {
			item := base
			item.CredentialSecretRef = "https://credentials.example"
			return item
		}(),
		func() domain.ExternalDNSIntegration {
			item := base
			item.AllowedDomainSuffixes = []string{"example.com", "example.com"}
			return item
		}(),
		func() domain.ExternalDNSIntegration {
			item := base
			item.EnvironmentIDs = []string{"not-a-uuid"}
			return item
		}(),
	}
	for index, item := range tests {
		if err := Validate(item); !errors.Is(err, ErrInvalid) {
			t.Fatalf("case %d accepted unsafe profile: %#v err=%v", index, item, err)
		}
	}
	validSync := base
	validSync.SyncPolicy, validSync.DestructiveSyncConfirmed = SyncPolicySync, true
	if err := Validate(validSync); err != nil {
		t.Fatalf("explicit destructive sync rejected: %v", err)
	}
}

func TestCatalogCannotExposeCredentialOrOperatorReferences(t *testing.T) {
	stub := &managementStoreStub{application: []domain.ExternalDNSIntegration{{
		ID: "id", Slug: "public-dns", Name: "Public DNS", Mode: ModeManaged, ProviderKind: "aws",
		TXTOwnerID: "sensitive-owner", AllowedDomainSuffixes: []string{"example.com"},
		CredentialSecretRef: "credentials", ProviderConfigRef: "provider", EgressConfigRef: "egress",
	}}}
	items, err := NewManagement(stub).ApplicationCatalog(context.Background(), "actor", "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	if err != nil || len(items) != 1 {
		t.Fatalf("catalog failed: %#v %v", items, err)
	}
	encoded := reflect.ValueOf(items[0])
	for _, forbidden := range []string{"CredentialSecretRef", "ProviderConfigRef", "EgressConfigRef", "OperatorProfileRef", "TXTOwnerID"} {
		if encoded.FieldByName(forbidden).IsValid() {
			t.Fatalf("catalog item exposes platform-only field %q", forbidden)
		}
	}
}

func TestHostnameMatchingRequiresAnExactLabelBoundary(t *testing.T) {
	allowed := []string{"example.com"}
	for _, host := range []string{"example.com", "api.example.com", "API.EXAMPLE.COM."} {
		if !HostnameAllowed(host, allowed) {
			t.Fatalf("expected hostname %q to be allowed", host)
		}
	}
	for _, host := range []string{"evil-example.com", "example.com.evil.test", "*.example.com", "example.com/path", ""} {
		if HostnameAllowed(host, allowed) {
			t.Fatalf("adversarial hostname %q crossed suffix boundary", host)
		}
	}
}

func TestValidateApplicationRouteIsScopedToCatalogSlugAndSuffix(t *testing.T) {
	stub := &managementStoreStub{application: []domain.ExternalDNSIntegration{{
		ID: "id", Slug: "public-dns", Name: "Public", Mode: ModeAdopted,
		ProviderKind: "google", AllowedDomainSuffixes: []string{"example.com"}, OperatorProfileRef: "operator-profile",
	}}}
	management := NewManagement(stub)
	applicationID, environmentID := "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	if err := management.ValidateApplicationRoute(context.Background(), "actor", applicationID, environmentID, "public-dns", "www.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := management.ValidateApplicationRoute(context.Background(), "actor", applicationID, environmentID, "other", "www.example.com"); !errors.Is(err, ErrIntegrationReference) {
		t.Fatalf("unknown integration error=%v", err)
	}
	if err := management.ValidateApplicationRoute(context.Background(), "actor", applicationID, environmentID, "public-dns", "www.example.net"); !errors.Is(err, ErrHostnameNotAllowed) {
		t.Fatalf("suffix mismatch error=%v", err)
	}
}
