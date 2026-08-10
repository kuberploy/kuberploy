package projectionpolicy

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

func privatePolicyParsed(targetID, profileName string, revision int64) map[string]any {
	return map[string]any{"spec": map[string]any{"delivery": map[string]any{
		"mode": "image",
		"release": map[string]any{
			"repository":     "registry.example.test/tenant/api",
			"digest":         "sha256:" + strings.Repeat("a", 64),
			"sourceRevision": strings.Repeat("b", 40),
		},
		"registryPull": map[string]any{
			"targetId":        targetID,
			"profileName":     profileName,
			"profileRevision": json.Number(strconv.FormatInt(revision, 10)),
		},
	}}}
}

func TestAppConfigPolicyDocumentIsTypedAndDetachedInMemory(t *testing.T) {
	scope := runtimeSecretDocumentScope(t, "apps-production")
	runtime := domain.NormalizeWorkloadRuntime(domain.DefaultWorkloadRuntime(8080, map[string]string{"MODE": "production"}))
	runtime.NodeSelector = map[string]string{"pool": "applications"}
	parsed := privatePolicyParsed("66666666-6666-4666-8666-666666666666", "managed-main", 7)
	document, err := newAppConfigPolicyDocument(scope, parsed, runtime)
	if err != nil {
		t.Fatal(err)
	}

	// Mutating every caller-owned input and every returned composite value must
	// not alter what a later child policy observes.
	runtime.NodeSelector["pool"] = "attacker"
	parsed["spec"].(map[string]any)["delivery"].(map[string]any)["mode"] = "attacker"
	firstRuntime := document.Runtime()
	firstRuntime.NodeSelector["pool"] = "mutated"
	firstRuntime.Ports[0].ContainerPort = 1
	firstScope := document.Scope()
	firstScope.Binding.ID = "attacker"
	firstDelivery := document.Delivery()
	firstDelivery.Repository = "attacker.invalid/repository"

	delivery := document.Delivery()
	detached := document.Runtime()
	if delivery.Mode != "image" || delivery.Repository != "registry.example.test/tenant/api" ||
		delivery.Digest != "sha256:"+strings.Repeat("a", 64) || !delivery.HasSourceRevision ||
		delivery.SourceRevision != strings.Repeat("b", 40) || !delivery.HasRegistryPull ||
		delivery.RegistryPull != (RegistryPullReference{TargetID: "66666666-6666-4666-8666-666666666666", ProfileName: "managed-main", ProfileRevision: 7}) {
		t.Fatalf("delivery was not preserved exactly: %#v", delivery)
	}
	if detached.NodeSelector["pool"] != "applications" || detached.Ports[0].ContainerPort != 8080 || document.Scope().Binding.ID != scope.Binding.ID {
		t.Fatalf("document aliases mutable input/output: scope=%#v runtime=%#v", document.Scope(), detached)
	}
	encodedPull, err := json.Marshal(delivery.RegistryPull)
	if err != nil || string(encodedPull) != `{"targetId":"66666666-6666-4666-8666-666666666666","profileName":"managed-main","profileRevision":7}` ||
		strings.Contains(string(encodedPull), "secret") || strings.Contains(string(encodedPull), "credential") {
		t.Fatalf("unsafe locked pull serialization=%s err=%v", encodedPull, err)
	}
}

func TestAppConfigPolicyDocumentCarriesDetachedClosedRouteView(t *testing.T) {
	scope := runtimeSecretDocumentScope(t, "apps-production")
	runtime := domain.NormalizeWorkloadRuntime(domain.DefaultWorkloadRuntime(8080, nil))
	parsed := privatePolicyParsed("66666666-6666-4666-8666-666666666666", "managed-main", 7)
	spec := parsed["spec"].(map[string]any)
	spec["middlewares"] = []any{map[string]any{
		"id": "77777777-7777-4777-8777-777777777777", "name": "secure", "spec": map[string]any{"compress": map[string]any{}},
	}}
	spec["routes"] = []any{map[string]any{
		"id": "public", "host": "api.example.test", "path": "/", "port": "http", "ingressClassName": "traefik",
		"middlewareRefs": []any{"secure"},
		"dns":            map[string]any{"mode": "externalDns", "integrationRef": "public-dns", "ttl": json.Number("300")},
		"tls":            map[string]any{"mode": "letsencrypt", "issuerRef": "letsencrypt-production", "redirectHttp": true},
	}}
	document, err := newAppConfigPolicyDocument(scope, parsed, runtime)
	if err != nil {
		t.Fatal(err)
	}

	// Caller and accessor mutations cannot alter the policy seen later.
	spec["routes"].([]any)[0].(map[string]any)["host"] = "attacker.invalid"
	first := document.Routes()
	first[0].Host = "mutated.invalid"
	first[0].MiddlewareRefs[0] = "mutated"
	names := document.MiddlewareNames()
	names[0] = "mutated"
	routes := document.Routes()
	if len(routes) != 1 || routes[0].Host != "api.example.test" || routes[0].MiddlewareRefs[0] != "secure" ||
		routes[0].DNS != (AppConfigDNSDocument{Mode: "externalDns", IntegrationRef: "public-dns", TTL: 300, HasTTL: true}) ||
		routes[0].TLS != (AppConfigTLSDocument{Mode: "letsencrypt", IssuerRef: "letsencrypt-production", RedirectHTTP: true, HasRedirectHTTP: true}) ||
		document.MiddlewareNames()[0] != "secure" {
		t.Fatalf("route view aliased or changed: routes=%#v names=%#v", routes, document.MiddlewareNames())
	}
}

func TestAppConfigPolicyDocumentCarriesOnlyTypedCertificateBindingIdentity(t *testing.T) {
	scope := runtimeSecretDocumentScope(t, "apps-production")
	runtime := domain.NormalizeWorkloadRuntime(domain.DefaultWorkloadRuntime(8080, nil))
	parsed := privatePolicyParsed("66666666-6666-4666-8666-666666666666", "managed-main", 7)
	spec := parsed["spec"].(map[string]any)
	spec["routes"] = []any{map[string]any{
		"host": "api.example.test", "path": "/", "port": "http",
		"tls": map[string]any{"mode": "customCertificate", "secretRef": map[string]any{
			"bindingId": "77777777-7777-4777-8777-777777777777", "name": "route-certificate", "version": json.Number("7"),
		}},
	}}
	document, err := newAppConfigPolicyDocument(scope, parsed, runtime)
	if err != nil {
		t.Fatal(err)
	}
	routes := document.Routes()
	want := certificates.Reference{BindingID: "77777777-7777-4777-8777-777777777777", Name: "route-certificate", Version: 7}
	if len(routes) != 1 || routes[0].TLS.SecretRef == nil || *routes[0].TLS.SecretRef != want {
		t.Fatalf("typed reference not preserved: %#v", routes)
	}
	// Accessors return detached pointer values.
	routes[0].TLS.SecretRef.Name = "mutated"
	if got := document.Routes()[0].TLS.SecretRef; got == nil || *got != want {
		t.Fatalf("certificate identity aliases caller output: %#v", got)
	}
	encoded, err := json.Marshal(document.Routes()[0].TLS.SecretRef)
	if err != nil || string(encoded) != `{"bindingId":"77777777-7777-4777-8777-777777777777","name":"route-certificate","version":7}` ||
		strings.Contains(string(encoded), "secretName") || strings.Contains(string(encoded), "private") {
		t.Fatalf("unsafe certificate identity serialization=%s err=%v", encoded, err)
	}
}

func TestAppConfigPolicyDocumentCarriesOnlyClosedSSLIPIntent(t *testing.T) {
	scope := runtimeSecretDocumentScope(t, "apps-production")
	runtime := domain.NormalizeWorkloadRuntime(domain.DefaultWorkloadRuntime(8080, nil))
	parsed := privatePolicyParsed("66666666-6666-4666-8666-666666666666", "managed-main", 7)
	parsed["spec"].(map[string]any)["routes"] = []any{map[string]any{
		"host": "kp-32f2a03ad59c10f96a1a.8-8-8-8.sslip.io", "path": "/", "port": "http",
		"dns": map[string]any{"mode": "sslip"}, "tls": map[string]any{"mode": "httpOnly"},
	}}
	document, err := newAppConfigPolicyDocument(scope, parsed, runtime)
	if err != nil || len(document.Routes()) != 1 || document.Routes()[0].DNS != (AppConfigDNSDocument{Mode: "sslip"}) {
		t.Fatalf("document=%#v err=%v", document.Routes(), err)
	}
	for name, value := range map[string]any{"ip": "8.8.8.8", "hostname": "caller.sslip.io", "integrationRef": "caller", "ttl": json.Number("300")} {
		parsed["spec"].(map[string]any)["routes"].([]any)[0].(map[string]any)["dns"] = map[string]any{"mode": "sslip", name: value}
		if _, err = newAppConfigPolicyDocument(scope, parsed, runtime); !errors.Is(err, gitprojection.ErrInvalid) {
			t.Fatalf("sslip accepted caller field %s: %v", name, err)
		}
	}
}

func TestAppConfigPolicyDocumentRejectsUntypedRouteFields(t *testing.T) {
	scope := runtimeSecretDocumentScope(t, "apps-production")
	runtime := domain.NormalizeWorkloadRuntime(domain.DefaultWorkloadRuntime(8080, nil))
	base := func() map[string]any {
		parsed := privatePolicyParsed("66666666-6666-4666-8666-666666666666", "managed-main", 7)
		parsed["spec"].(map[string]any)["routes"] = []any{map[string]any{
			"host": "api.example.test", "path": "/", "port": "http", "tls": map[string]any{"mode": "httpOnly"},
		}}
		return parsed
	}
	tests := map[string]func(map[string]any){
		"unknown route credential": func(parsed map[string]any) {
			parsed["spec"].(map[string]any)["routes"].([]any)[0].(map[string]any)["credentialRef"] = "secret"
		},
		"floating ttl": func(parsed map[string]any) {
			route := parsed["spec"].(map[string]any)["routes"].([]any)[0].(map[string]any)
			route["dns"] = map[string]any{"mode": "externalDns", "integrationRef": "public-dns", "ttl": float64(300)}
		},
		"custom tls credential": func(parsed map[string]any) {
			route := parsed["spec"].(map[string]any)["routes"].([]any)[0].(map[string]any)
			route["tls"] = map[string]any{"mode": "customCertificate", "secretRef": "tls", "privateKey": "forbidden"}
		},
		"legacy custom Secret name": func(parsed map[string]any) {
			route := parsed["spec"].(map[string]any)["routes"].([]any)[0].(map[string]any)
			route["tls"] = map[string]any{"mode": "customCertificate", "secretRef": "caller-secret"}
		},
		"floating certificate version": func(parsed map[string]any) {
			route := parsed["spec"].(map[string]any)["routes"].([]any)[0].(map[string]any)
			route["tls"] = map[string]any{"mode": "customCertificate", "secretRef": map[string]any{
				"bindingId": "77777777-7777-4777-8777-777777777777", "name": "route-certificate", "version": float64(7),
			}}
		},
		"caller target Secret name": func(parsed map[string]any) {
			route := parsed["spec"].(map[string]any)["routes"].([]any)[0].(map[string]any)
			route["tls"] = map[string]any{"mode": "customCertificate", "secretRef": map[string]any{
				"bindingId": "77777777-7777-4777-8777-777777777777", "name": "route-certificate", "version": json.Number("7"),
				"secretName": "caller-secret",
			}}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			parsed := base()
			mutate(parsed)
			if _, err := newAppConfigPolicyDocument(scope, parsed, runtime); !errors.Is(err, gitprojection.ErrInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestAppConfigPolicyDocumentRejectsUntypedOrCredentialBearingDelivery(t *testing.T) {
	scope := runtimeSecretDocumentScope(t, "apps-production")
	runtime := domain.NormalizeWorkloadRuntime(domain.DefaultWorkloadRuntime(8080, nil))
	targetID := "66666666-6666-4666-8666-666666666666"

	tests := map[string]func(map[string]any){
		"secret name": func(parsed map[string]any) {
			parsed["spec"].(map[string]any)["delivery"].(map[string]any)["registryPull"].(map[string]any)["secretName"] = "caller-secret"
		},
		"credential reference": func(parsed map[string]any) {
			parsed["spec"].(map[string]any)["delivery"].(map[string]any)["registryPull"].(map[string]any)["credentialRef"] = "caller-credential"
		},
		"floating revision": func(parsed map[string]any) {
			parsed["spec"].(map[string]any)["delivery"].(map[string]any)["registryPull"].(map[string]any)["profileRevision"] = float64(7)
		},
		"fractional revision": func(parsed map[string]any) {
			parsed["spec"].(map[string]any)["delivery"].(map[string]any)["registryPull"].(map[string]any)["profileRevision"] = json.Number("7.5")
		},
		"unknown delivery": func(parsed map[string]any) {
			parsed["spec"].(map[string]any)["delivery"].(map[string]any)["credential"] = "caller"
		},
		"mutable repository": func(parsed map[string]any) {
			parsed["spec"].(map[string]any)["delivery"].(map[string]any)["release"].(map[string]any)["repository"] = "registry.example.test/api:latest@sha256:bad"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			parsed := privatePolicyParsed(targetID, "managed-main", 7)
			mutate(parsed)
			if _, err := newAppConfigPolicyDocument(scope, parsed, runtime); !errors.Is(err, gitprojection.ErrInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
