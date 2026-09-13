package externaldns

import (
	"encoding/json"
	"path"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

func runtimeTemplate() ManagedRuntimeTemplate {
	return ManagedRuntimeTemplate{Namespace: "kuberploy-dns", Version: "v0.18.0", Image: "registry.k8s.io/external-dns/external-dns@sha256:" + strings.Repeat("a", 64), ServiceAccount: "external-dns-managed"}
}
func runtimeIntegration() domain.ExternalDNSIntegration {
	return domain.ExternalDNSIntegration{ID: "11111111-1111-4111-8111-111111111111", Slug: "primary", Name: "Primary", Mode: ModeManaged, ProviderKind: "cloudflare", TXTOwnerID: "kuberploy.primary", AllowedDomainSuffixes: []string{"example.com", "prod.example.com"}, SyncPolicy: SyncPolicyUpsert, CredentialSecretRef: "cloudflare-credentials", ProviderConfigRef: "cloudflare-provider", EgressConfigRef: "cloudflare-egress", EnvironmentIDs: []string{"22222222-2222-4222-8222-222222222222"}, RuntimeRevision: 3, Lifecycle: "active"}
}

func TestManagedRuntimeIntegrationsOwnDisjointResources(t *testing.T) {
	first, second := runtimeIntegration(), runtimeIntegration()
	second.ID, second.Slug = "33333333-3333-4333-8333-333333333333", "secondary"
	template := runtimeTemplate()
	template.ServiceAccount = strings.Repeat("a", 29) + "-" + strings.Repeat("b", 18)
	identities := map[string]bool{}
	for _, item := range []domain.ExternalDNSIntegration{first, second} {
		content, _, err := RenderManagedBundle(item, template)
		if err != nil {
			t.Fatal(err)
		}
		account := managedServiceAccount(item, template)
		if len(account) > 63 || !slugRE.MatchString(account) || account == template.ServiceAccount {
			t.Fatalf("invalid integration account %q", account)
		}
		kinds := map[string]bool{}
		for _, raw := range strings.Split(string(content), "\n---\n") {
			var object struct {
				Kind     string `json:"kind"`
				Metadata struct {
					Name, Namespace string
					Labels          map[string]string
				}
				Spec struct {
					Template struct {
						Spec struct{ ServiceAccountName string }
					}
				}
				Subjects []struct{ Kind, Name, Namespace string }
				Rules    []struct{ APIGroups, Resources, Verbs []string }
			}
			if err := json.Unmarshal([]byte(raw), &object); err != nil {
				t.Fatal(err)
			}
			identity := object.Kind + "/" + object.Metadata.Namespace + "/" + object.Metadata.Name
			if identities[identity] || object.Metadata.Labels["kuberploy.io/dns-integration"] != item.ID {
				t.Fatalf("shared or incorrect object ownership: %s", identity)
			}
			identities[identity], kinds[object.Kind] = true, true
			switch object.Kind {
			case "ServiceAccount":
				if object.Metadata.Name != account {
					t.Fatal("account name differs from integration identity")
				}
			case "Deployment":
				if object.Spec.Template.Spec.ServiceAccountName != account {
					t.Fatal("controller references another account")
				}
			case "ClusterRoleBinding":
				if len(object.Subjects) != 1 || object.Subjects[0].Kind != "ServiceAccount" || object.Subjects[0].Name != account || object.Subjects[0].Namespace != template.Namespace {
					t.Fatal("RBAC subject differs from controller account")
				}
			case "ClusterRole":
				if len(object.Rules) != 1 || !reflect.DeepEqual(object.Rules[0].APIGroups, []string{"networking.k8s.io"}) || !reflect.DeepEqual(object.Rules[0].Resources, []string{"ingresses"}) || !reflect.DeepEqual(object.Rules[0].Verbs, []string{"get", "list", "watch"}) {
					t.Fatal("integration permissions changed")
				}
			}
		}
		if len(kinds) != 5 {
			t.Fatal("missing managed resource")
		}
	}
}

func TestManagedRuntimeUpgradeReplacesSharedAccountThroughProtectedGit(t *testing.T) {
	f := newPublicationFixture(t)
	item := runtimeIntegration()
	content, profile, err := RenderManagedBundle(item, f.config.Template)
	if err != nil {
		t.Fatal(err)
	}
	// Seed the previously deployed shared-account shape at the exact protected
	// path; replacement must use normal publication CAS, without deleting it.
	old, _, err := renderManagedBundle(item, f.config.Template, true)
	if err != nil {
		t.Fatal(err)
	}
	documentPath := path.Join(gitprojection.PlatformPrefix(), "argocd", "platform", "external-dns", item.ID+".yaml")
	f.advance(t, documentPath, old, time.Second)
	base := f.binding.IndexedRevision
	receipt, err := f.publisher(t, f.store).Reconcile(t.Context(), item)
	if err != nil || !receipt.Changed || receipt.Deleted || receipt.CommittedRevision == base {
		t.Fatalf("shared-account upgrade failed: %#v %v", receipt, err)
	}
	actual := publicationGit(t, "", "--git-dir", f.remote, "show", receipt.CommittedRevision+":"+documentPath)
	if actual != strings.TrimSpace(string(content)) || publicationGit(t, "", "--git-dir", f.remote, "rev-parse", receipt.CommittedRevision+"^") != base {
		t.Fatal("upgrade did not preserve the exact protected path and Git parent")
	}
	changedTemplate := f.config.Template
	changedTemplate.ServiceAccount = "another-prefix"
	changed, err := ManagedProfile(item, changedTemplate)
	if err != nil || changed.Deployment.SpecDigest == profile.Deployment.SpecDigest {
		t.Fatal("readiness identity failed to bind the account change")
	}
}

func TestManagedRuntimeBundleIsClosedAndExact(t *testing.T) {
	content, profile, err := RenderManagedBundle(runtimeIntegration(), runtimeTemplate())
	if err != nil {
		t.Fatal(err)
	}
	if profile.Validate() != nil || profile.Revision != 3 {
		t.Fatalf("invalid profile %#v", profile)
	}
	if profile.LabelFilter != "kuberploy.io/dns-integration=primary" {
		t.Fatalf("runtime must select the immutable integration slug rendered by the workload chart, got %q", profile.LabelFilter)
	}
	text := string(content)
	for _, required := range []string{`"kind": "Deployment"`, `"kind": "ClusterRole"`, `"name": "cloudflare-credentials"`, `"--domain-filter=prod.example.com"`, `"--label-filter=kuberploy.io/dns-integration=primary"`, `"--txt-prefix=%{record_type}-"`, `"--managed-record-types=TXT"`, `"kuberploy.io/edge-spec-digest"`} {
		if !strings.Contains(text, required) {
			t.Fatalf("missing %q", required)
		}
	}
	for _, required := range []string{`"fsGroup": 65534`, `"runAsGroup": 65532`, `"runAsNonRoot": true`, `"runAsUser": 65532`} {
		if !strings.Contains(text, required) {
			t.Fatalf("missing non-root runtime identity %q", required)
		}
	}
	for _, required := range []string{`"name": "CF_API_TOKEN"`, `"key": "apiToken"`, `"name": "cloudflare-provider"`} {
		if !strings.Contains(text, required) {
			t.Fatalf("missing Cloudflare credential mapping %q", required)
		}
	}
	if strings.Contains(text, `"secretRef": {`) {
		t.Fatal("Cloudflare runtime must map the provider token to CF_API_TOKEN")
	}
	for _, forbidden := range []string{"secretValue", "token-value", "latest", "--domain-filter=evil.example"} {
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
	values := map[string]string{OperationalEnabledEnv: "true", OperationalBindingIDEnv: "11111111-1111-4111-8111-111111111111", OperationalNamespaceEnv: "kuberploy-dns", OperationalVersionEnv: "v0.18.0", OperationalImageEnv: runtimeTemplate().Image, OperationalServiceAccountEnv: "external-dns-managed", OperationalPollSecondsEnv: "5"}
	config, err = OperationalConfigFromLookup(func(name string) (string, bool) { v, ok := values[name]; return v, ok })
	if err != nil || config.Validate() != nil {
		t.Fatalf("enabled config %#v %v", config, err)
	}
}
