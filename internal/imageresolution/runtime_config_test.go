package imageresolution

import (
	"reflect"
	"testing"

	"github.com/kuberploy/kuberploy/internal/imagepull"
)

func TestRuntimeConfigBindsAnonymousChallengeAndPlatformToOperatorInput(t *testing.T) {
	profile := resolutionProfile()
	pull := imagepull.DefaultRuntimeConfig()
	pull.Enabled = true
	pull.Namespaces = []string{"tenant-dev"}
	pull.Profiles = []imagepull.Profile{profile}
	values := map[string]string{
		AnonymousTargetsEnv: "44444444-4444-4444-8444-444444444444",
		TokenAuthoritiesEnv: `[{"targetId":"11111111-1111-4111-8111-111111111111","realmUrl":"https://auth.example.test/token","service":"registry.example.test"}]`,
		PlatformEnv:         "linux/arm64/v8",
	}
	config, err := RuntimeConfigFromLookup(pull, func(name string) (string, bool) { value, ok := values[name]; return value, ok })
	if err != nil || len(config.Profiles) != 1 || len(config.AnonymousTargetIDs) != 1 || len(config.TokenAuthorities) != 1 ||
		config.Platform != (Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}) {
		t.Fatalf("config=%+v err=%v", config, err)
	}
}

func TestRuntimeConfigRejectsUnboundOrAmbiguousOperatorAuthority(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"duplicate anonymous":  {AnonymousTargetsEnv: "44444444-4444-4444-8444-444444444444,44444444-4444-4444-8444-444444444444"},
		"unknown token target": {TokenAuthoritiesEnv: `[{"targetId":"44444444-4444-4444-8444-444444444444","realmUrl":"https://auth.example.test/token","service":"registry.example.test"}]`},
		"http token realm":     {AnonymousTargetsEnv: resolutionTargetID, TokenAuthoritiesEnv: `[{"targetId":"11111111-1111-4111-8111-111111111111","realmUrl":"http://auth.example.test/token","service":"registry.example.test"}]`},
		"duplicate json":       {AnonymousTargetsEnv: resolutionTargetID, TokenAuthoritiesEnv: `[{"targetId":"11111111-1111-4111-8111-111111111111","targetId":"11111111-1111-4111-8111-111111111111","realmUrl":"https://auth.example.test/token","service":"registry.example.test"}]`},
		"unsupported platform": {PlatformEnv: "windows/amd64"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := RuntimeConfigFromLookup(imagepull.RuntimeConfig{}, func(key string) (string, bool) { value, ok := values[key]; return value, ok }); err == nil {
				t.Fatal("unsafe runtime config accepted")
			}
		})
	}
}

func TestRuntimeConfigNormalizesAnonymousTargetOrder(t *testing.T) {
	pull := imagepull.DefaultRuntimeConfig()
	pull.Enabled = true
	pull.Namespaces = []string{"tenant-dev"}
	pull.Profiles = []imagepull.Profile{resolutionProfile()}
	values := map[string]string{
		AnonymousTargetsEnv: "55555555-5555-4555-8555-555555555555,44444444-4444-4444-8444-444444444444",
	}
	config, err := RuntimeConfigFromLookup(pull, func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555"}
	if !reflect.DeepEqual(config.AnonymousTargetIDs, want) {
		t.Fatalf("anonymous targets=%v, want %v", config.AnonymousTargetIDs, want)
	}
}

func TestRuntimeConfigNormalizesTokenAuthorityOrder(t *testing.T) {
	pull := imagepull.DefaultRuntimeConfig()
	pull.Enabled = true
	pull.Namespaces = []string{"tenant-dev"}
	pull.Profiles = []imagepull.Profile{resolutionProfile()}
	values := map[string]string{
		AnonymousTargetsEnv: "22222222-2222-4222-8222-222222222222",
		TokenAuthoritiesEnv: `[{"targetId":"22222222-2222-4222-8222-222222222222","realmUrl":"https://auth.example.test/token","service":"registry.example.test"},{"targetId":"11111111-1111-4111-8111-111111111111","realmUrl":"https://auth.example.test/token","service":"registry.example.test"}]`,
	}
	config, err := RuntimeConfigFromLookup(pull, func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(config.TokenAuthorities) != 2 || config.TokenAuthorities[0].TargetID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("token authorities were not canonicalized: %#v", config.TokenAuthorities)
	}
}
