package imageresolution

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/imagepull"
)

const (
	resolutionTargetID      = "11111111-1111-4111-8111-111111111111"
	resolutionApplicationID = "22222222-2222-4222-8222-222222222222"
	resolutionEnvironmentID = "33333333-3333-4333-8333-333333333333"
)

func resolutionProfile() imagepull.Profile {
	return imagepull.Profile{Name: "registry-main", TargetID: resolutionTargetID, RegistryServer: "registry.example.test:5000",
		CredentialRef: "runtime-pull/main", Revision: 3, SourceSecretRef: "registry-pull-main", SourceSecretKey: ".dockerconfigjson"}
}

func resolutionSource() AuthorizedSource {
	return AuthorizedSource{Target: domain.RegistryTarget{ID: resolutionTargetID, Mode: domain.RegistryTargetExternal,
		Endpoint: "https://registry.example.test:5000", RepositoryPrefix: "tenant", PullCredentialRef: "runtime-pull/main"},
		Policy: domain.ServiceRegistryPolicy{RegistryTargetID: resolutionTargetID, ServiceID: resolutionApplicationID, Repository: "tenant/service"}}
}

func anonymousResolutionSource() AuthorizedSource {
	source := resolutionSource()
	source.Target.Endpoint = "https://registry.example.test"
	source.Target.PullCredentialRef = ""
	return source
}

type resolverCatalog struct {
	sources []AuthorizedSource
	err     error
	calls   int
}

func (c *resolverCatalog) AuthorizedImageSourcesForActor(_ context.Context, actor, applicationID, environmentID string) ([]AuthorizedSource, error) {
	c.calls++
	if actor != "deployer" || applicationID != resolutionApplicationID || environmentID != resolutionEnvironmentID {
		return nil, ErrForbidden
	}
	return append([]AuthorizedSource(nil), c.sources...), c.err
}

type resolverProvider struct {
	digest    string
	err       error
	calls     int
	authority *ProviderAuthority
}

func (p *resolverProvider) ResolveTag(_ context.Context, _ AuthorizedSource, _ TagReference, authority *ProviderAuthority, _ Platform) (string, error) {
	p.calls++
	p.authority = authority
	return p.digest, p.err
}

func TestResolverAuthorizesDigestWithoutProvider(t *testing.T) {
	catalog := &resolverCatalog{sources: []AuthorizedSource{resolutionSource()}}
	provider := &resolverProvider{}
	resolver := &Resolver{Catalog: catalog, Provider: provider, Config: RuntimeConfig{Platform: DefaultPlatform()}}
	image := "registry.example.test:5000/tenant/service@sha256:" + strings.Repeat("a", 64)
	result, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, image)
	if err != nil || result.ImmutableImage != image || result.Resolved || catalog.calls != 1 || provider.calls != 0 {
		t.Fatalf("result=%+v catalog=%d provider=%d err=%v", result, catalog.calls, provider.calls, err)
	}
}

func TestResolverDigestRejectsSiblingRepositoryOnAuthorizedPrivateHost(t *testing.T) {
	catalog := &resolverCatalog{sources: []AuthorizedSource{resolutionSource()}}
	resolver := &Resolver{Catalog: catalog}
	image := "registry.example.test:5000/tenant/sibling@sha256:" + strings.Repeat("a", 64)
	if _, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, image); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
}

func TestResolverDigestAllowsExplicitPublicHostAfterScopeAuthorization(t *testing.T) {
	catalog := &resolverCatalog{sources: []AuthorizedSource{resolutionSource()}}
	resolver := &Resolver{Catalog: catalog}
	image := "public.example.test/library/service@sha256:" + strings.Repeat("a", 64)
	result, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, image)
	if err != nil || result.ImmutableImage != image || result.Resolved || catalog.calls != 1 {
		t.Fatalf("result=%+v catalog=%d err=%v", result, catalog.calls, err)
	}
}

func TestResolverValidatesScopeIdentityBeforeDigestFastPath(t *testing.T) {
	resolver := &Resolver{}
	image := "registry.example.test/tenant/service@sha256:" + strings.Repeat("a", 64)
	for name, identity := range map[string][3]string{
		"actor":       {" actor", resolutionApplicationID, resolutionEnvironmentID},
		"application": {"deployer", "not-an-id", resolutionEnvironmentID},
		"environment": {"deployer", resolutionApplicationID, "not-an-id"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := resolver.Resolve(t.Context(), identity[0], identity[1], identity[2], image); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestResolverUsesOnlyExactAuthorizedSourceAndOperatorProfile(t *testing.T) {
	catalog := &resolverCatalog{sources: []AuthorizedSource{resolutionSource()}}
	provider := &resolverProvider{digest: "sha256:" + strings.Repeat("b", 64)}
	resolver := &Resolver{Catalog: catalog, Provider: provider, Config: RuntimeConfig{Profiles: []imagepull.Profile{resolutionProfile()}, Platform: DefaultPlatform()}}
	result, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, "registry.example.test:5000/tenant/service:stable")
	if err != nil || !result.Resolved || result.ImmutableImage != "registry.example.test:5000/tenant/service@"+provider.digest || provider.calls != 1 ||
		provider.authority == nil || provider.authority.Profile == nil || provider.authority.Profile.Name != "registry-main" || provider.authority.Anonymous {
		t.Fatalf("result=%+v authority=%+v err=%v", result, provider.authority, err)
	}
}

func TestResolverAllowsAnonymousOnlyForExplicitOperatorTarget(t *testing.T) {
	source := anonymousResolutionSource()
	provider := &resolverProvider{digest: "sha256:" + strings.Repeat("c", 64)}
	for name, anonymousIDs := range map[string][]string{"not allowed": nil, "explicit": {resolutionTargetID}} {
		t.Run(name, func(t *testing.T) {
			provider.calls = 0
			resolver := &Resolver{Catalog: &resolverCatalog{sources: []AuthorizedSource{source}}, Provider: provider,
				Config: RuntimeConfig{AnonymousTargetIDs: anonymousIDs, Platform: DefaultPlatform()}}
			_, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, "registry.example.test/tenant/service:latest")
			if name == "not allowed" {
				if !errors.Is(err, ErrUnavailable) || provider.calls != 0 {
					t.Fatalf("calls=%d err=%v", provider.calls, err)
				}
				return
			}
			if err != nil || provider.calls != 1 || provider.authority == nil || !provider.authority.Anonymous || provider.authority.Profile != nil {
				t.Fatalf("calls=%d authority=%+v err=%v", provider.calls, provider.authority, err)
			}
		})
	}
}

func TestResolverAnonymousAuthorityIsRestrictedToCanonicalHTTPS443(t *testing.T) {
	provider := &resolverProvider{digest: "sha256:" + strings.Repeat("c", 64)}
	resolver := &Resolver{Catalog: &resolverCatalog{sources: []AuthorizedSource{resolutionSource()}}, Provider: provider,
		Config: RuntimeConfig{AnonymousTargetIDs: []string{resolutionTargetID}, Platform: DefaultPlatform()}}
	if _, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, "registry.example.test:5000/tenant/service:latest"); !errors.Is(err, ErrConflict) || provider.calls != 0 {
		t.Fatalf("calls=%d err=%v", provider.calls, err)
	}
}

func TestResolverRejectsCallerSelectedOrAmbiguousRegistryCoordinates(t *testing.T) {
	valid := resolutionSource()
	provider := &resolverProvider{digest: "sha256:" + strings.Repeat("d", 64)}
	config := RuntimeConfig{Profiles: []imagepull.Profile{resolutionProfile()}, Platform: DefaultPlatform()}
	for name, image := range map[string]string{
		"different host":       "evil.example.test/tenant/service:latest",
		"different repository": "registry.example.test:5000/other/service:latest",
		"implicit registry":    "tenant/service:latest",
		"traversal":            "registry.example.test:5000/tenant/../service:latest",
	} {
		t.Run(name, func(t *testing.T) {
			provider.calls = 0
			resolver := &Resolver{Catalog: &resolverCatalog{sources: []AuthorizedSource{valid}}, Provider: provider, Config: config}
			if _, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, image); err == nil || provider.calls != 0 {
				t.Fatalf("calls=%d err=%v", provider.calls, err)
			}
		})
	}
	resolver := &Resolver{Catalog: &resolverCatalog{sources: []AuthorizedSource{valid, valid}}, Provider: provider, Config: config}
	if _, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, "registry.example.test:5000/tenant/service:latest"); !errors.Is(err, ErrConflict) {
		t.Fatalf("ambiguous err=%v", err)
	}
}
