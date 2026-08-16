package edge

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestRuntimeConfigDefaultOffAndStrictEnvironment(t *testing.T) {
	if config, err := RuntimeConfigFromLookup(func(string) (string, bool) { return "", false }); err != nil || !reflect.DeepEqual(config, RuntimeConfig{}) {
		t.Fatalf("default-off drifted: %#v %v", config, err)
	}
	config := testRuntimeConfig()
	raw, err := json.Marshal(config.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		RuntimeEnabledEnv: "true", RuntimeProfilesJSONEnv: string(raw), RuntimePollSecondsEnv: "30",
		RuntimeWorkLeaseSecondsEnv: "120", RuntimeHeartbeatSecondsEnv: "20", RuntimeReadinessSecondsEnv: "90",
		RuntimeMinimumBackoffEnv: "5", RuntimeMaximumBackoffEnv: "300",
	}
	parsed, err := RuntimeConfigFromLookup(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil || parsed.Validate() != nil || parsed.TargetCount() != 3 {
		t.Fatalf("valid config rejected: %#v %v", parsed, err)
	}
	if digest, digestErr := parsed.Digest(); digestErr != nil || !validDigest(digest) {
		t.Fatalf("config digest invalid: %q %v", digest, digestErr)
	}
	values[RuntimeProfilesJSONEnv] = string(raw) + "{}"
	if _, err = RuntimeConfigFromLookup(func(key string) (string, bool) { value, ok := values[key]; return value, ok }); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	values[RuntimeProfilesJSONEnv] = `{"unknown":true}`
	if _, err = RuntimeConfigFromLookup(func(key string) (string, bool) { value, ok := values[key]; return value, ok }); err == nil {
		t.Fatal("unknown profile field accepted")
	}
	values[RuntimeEnabledEnv] = "TRUE"
	if _, err = RuntimeConfigFromLookup(func(key string) (string, bool) { value, ok := values[key]; return value, ok }); err == nil {
		t.Fatal("non-canonical enable flag accepted")
	}
}

func TestProfilesAreExactAndSorted(t *testing.T) {
	config := testRuntimeConfig()
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	targets, err := config.DesiredTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 || targets[0].Key != "cert-manager" || targets[1].Key != "external-dns/"+testExternalDNSIntegrationID || targets[2].Key != "traefik" {
		t.Fatalf("targets are not canonical: %#v", targets)
	}
	external := targets[1]
	if external.Mode != ModeAdopted || external.ExternalTXTOwnerID != "kuberploy.primary" || external.ExternalPolicy != "upsert-only" || external.ExternalDomains != "example.com,prod.example.com" {
		t.Fatalf("external safe identity missing: %#v", external)
	}
	mutated := config
	mutated.Profiles = cloneRuntimeProfiles(config.Profiles)
	mutated.Profiles.ExternalDNS[0].DomainFilters = []string{"prod.example.com", "example.com"}
	if !errors.Is(mutated.Validate(), ErrInvalid) {
		t.Fatal("unsorted domains accepted")
	}
	mutated = config
	mutated.Profiles = cloneRuntimeProfiles(config.Profiles)
	mutated.Profiles.Traefik.CRDs = append(mutated.Profiles.Traefik.CRDs, ObjectExpectation{Name: "unused.traefik.io", SpecDigest: testDigest("unused")})
	if !errors.Is(mutated.Validate(), ErrInvalid) {
		t.Fatal("non-exact CRD set accepted")
	}
}

func TestRuntimeConfigFromLookupNormalizesUserLists(t *testing.T) {
	config := testRuntimeConfig()
	config.Profiles.ExternalDNS[0].DomainFilters = []string{"prod.example.com", "example.com", "prod.example.com"}
	config.Profiles.CertManager.ProductionSolverTypes = []string{"http01", "dns01", "http01"}
	config.Profiles.CertManager.ProductionDNS01Profiles = []string{"cloudflare-secondary", "cloudflare-primary", "cloudflare-primary"}
	raw, err := json.Marshal(config.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		RuntimeEnabledEnv: "true", RuntimeProfilesJSONEnv: string(raw), RuntimePollSecondsEnv: "30",
		RuntimeWorkLeaseSecondsEnv: "120", RuntimeHeartbeatSecondsEnv: "20", RuntimeReadinessSecondsEnv: "90",
		RuntimeMinimumBackoffEnv: "5", RuntimeMaximumBackoffEnv: "300",
	}
	parsed, err := RuntimeConfigFromLookup(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Profiles.ExternalDNS[0].DomainFilters, []string{"example.com", "prod.example.com"}) ||
		!reflect.DeepEqual(parsed.Profiles.CertManager.ProductionSolverTypes, []string{"dns01", "http01"}) ||
		!reflect.DeepEqual(parsed.Profiles.CertManager.ProductionDNS01Profiles, []string{"cloudflare-primary", "cloudflare-secondary"}) {
		t.Fatalf("lists were not normalized: %#v", parsed.Profiles)
	}
}

func TestCertManagerProfileFencesServerAndSolverIdentity(t *testing.T) {
	base := *testRuntimeConfig().Profiles.CertManager
	base.ProductionSolverTypes = []string{"dns01", "http01"}
	base.ProductionDNS01Profiles = []string{"cloudflare-primary", "cloudflare-secondary"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid mixed solver profile rejected: %v", err)
	}
	catalog := base.ApprovedIssuerCatalog()
	if len(catalog) != 2 || catalog[0].Name != "letsencrypt-production" || catalog[0].Environment != "production" ||
		!reflect.DeepEqual(catalog[0].SolverTypes, []string{"dns01", "http01"}) ||
		!reflect.DeepEqual(catalog[0].DNS01Profiles, []string{"cloudflare-primary", "cloudflare-secondary"}) {
		t.Fatalf("safe issuer catalog drifted: %#v", catalog)
	}
	catalog[0].SolverTypes[0] = "substituted"
	if base.ProductionSolverTypes[0] != "dns01" {
		t.Fatal("safe issuer catalog leaked mutable profile storage")
	}
	tests := []struct {
		name   string
		mutate func(*CertManagerProfile)
	}{
		{"server substitution", func(profile *CertManagerProfile) { profile.ProductionServerClass = "letsencrypt-staging" }},
		{"HTTP fallback missing", func(profile *CertManagerProfile) { profile.ProductionSolverTypes = []string{"dns01"} }},
		{"DNS metadata on HTTP only", func(profile *CertManagerProfile) { profile.ProductionSolverTypes = []string{"http01"} }},
		{"unsorted DNS profiles", func(profile *CertManagerProfile) {
			profile.ProductionDNS01Profiles = []string{"cloudflare-secondary", "cloudflare-primary"}
		}},
		{"duplicate DNS profiles", func(profile *CertManagerProfile) {
			profile.ProductionDNS01Profiles = []string{"cloudflare-primary", "cloudflare-primary"}
		}},
		{"disabled issuer retains authority", func(profile *CertManagerProfile) { profile.ProductionIssuer = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			candidate.ProductionSolverTypes = append([]string(nil), base.ProductionSolverTypes...)
			candidate.ProductionDNS01Profiles = append([]string(nil), base.ProductionDNS01Profiles...)
			test.mutate(&candidate)
			if !errors.Is(candidate.Validate(), ErrInvalid) {
				t.Fatal("invalid certificate runtime authority accepted")
			}
		})
	}
}
