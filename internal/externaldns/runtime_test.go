package externaldns

import (
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func runtimeTemplate() ManagedRuntimeTemplate {
	return ManagedRuntimeTemplate{Namespace: "kuberploy-dns", Version: "v0.18.0", Image: "registry.k8s.io/external-dns/external-dns@sha256:" + strings.Repeat("a", 64), ServiceAccount: "external-dns-managed"}
}
func runtimeIntegration() domain.ExternalDNSIntegration {
	return domain.ExternalDNSIntegration{ID: "11111111-1111-4111-8111-111111111111", Slug: "primary", Name: "Primary", Mode: ModeManaged, ProviderKind: "cloudflare", TXTOwnerID: "kuberploy.primary", AllowedDomainSuffixes: []string{"example.com", "prod.example.com"}, SyncPolicy: SyncPolicyUpsert, CredentialSecretRef: "cloudflare-credentials", ProviderConfigRef: "cloudflare-provider", EgressConfigRef: "cloudflare-egress", EnvironmentIDs: []string{"22222222-2222-4222-8222-222222222222"}, RuntimeRevision: 3, Lifecycle: "active"}
}

func TestManagedRuntimeBundleIsClosedAndExact(t *testing.T) {
	content, profile, err := RenderManagedBundle(runtimeIntegration(), runtimeTemplate())
	if err != nil {
		t.Fatal(err)
	}
	if profile.Validate() != nil || profile.Revision != 3 {
		t.Fatalf("invalid profile %#v", profile)
	}
	text := string(content)
	for _, required := range []string{`"kind": "Deployment"`, `"kind": "ClusterRole"`, `"name": "cloudflare-credentials"`, `"--domain-filter=prod.example.com"`, `"kuberploy.io/edge-spec-digest"`} {
		if !strings.Contains(text, required) {
			t.Fatalf("missing %q", required)
		}
	}
	for _, forbidden := range []string{"apiToken", "secretValue", "latest", "--domain-filter=evil.example"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("unexpected %q", forbidden)
		}
	}
}

func TestManagedRuntimeRejectsAdoptedAndMutableImage(t *testing.T) {
	item := runtimeIntegration()
	item.Mode = ModeAdopted
	item.OperatorProfileRef = "adopted"
	item.CredentialSecretRef, item.ProviderConfigRef, item.EgressConfigRef = "", "", ""
	if _, err := ManagedProfile(item, runtimeTemplate()); err == nil {
		t.Fatal("adopted integration entered managed materializer")
	}
	template := runtimeTemplate()
	template.Image = "registry.k8s.io/external-dns/external-dns:latest"
	if template.Validate() == nil {
		t.Fatal("mutable image accepted")
	}
}

func TestOperationalConfigDefaultOffAndExact(t *testing.T) {
	config, err := OperationalConfigFromLookup(func(string) (string, bool) { return "", false })
	if err != nil || config.Enabled {
		t.Fatalf("default off %#v %v", config, err)
	}
	values := map[string]string{OperationalEnabledEnv: "true", OperationalBindingIDEnv: "11111111-1111-4111-8111-111111111111", OperationalClusterIDEnv: "22222222-2222-4222-8222-222222222222", OperationalNamespaceEnv: "kuberploy-dns", OperationalVersionEnv: "v0.18.0", OperationalImageEnv: runtimeTemplate().Image, OperationalServiceAccountEnv: "external-dns-managed", OperationalPollSecondsEnv: "5"}
	config, err = OperationalConfigFromLookup(func(name string) (string, bool) { v, ok := values[name]; return v, ok })
	if err != nil || config.Validate() != nil {
		t.Fatalf("enabled config %#v %v", config, err)
	}
}
