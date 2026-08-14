package helmapps

import (
	"testing"
)

func runtimeEnvironmentFixture(t *testing.T) (map[string]string, ProtectedPublisherIdentity) {
	t.Helper()
	config := validRuntimeConfig(t)
	values := map[string]string{
		RuntimeEnabledEnv: "true", RuntimeRendererNamespaceEnv: config.Renderer.Namespace,
		RuntimeRendererServiceAccountEnv: config.Renderer.ServiceAccount,
		RuntimeRendererPollMillisEnv:     "100", RuntimeWorkPollMillisEnv: "1000",
		RuntimeRenderLeaseSecondsEnv: "60", RuntimePublishLeaseSecondsEnv: "60", RuntimeReadinessSecondsEnv: "30",
		RuntimeOCIRequestSecondsEnv: "15", RuntimeOCIRegistryHostsEnv: "ghcr.io,registry.example.com",
		RuntimeOCIAuthHostsEnv: "auth.example.com,ghcr.io", RuntimePackageCacheBytesEnv: "67108864",
		RuntimeOCIRedirectHostsEnv:      "pkg-containers.githubusercontent.com",
		RuntimeOCICredentialProfilesEnv: `[{"registryHost":"registry.example.com","authHost":"auth.example.com","name":"private-main","mode":"basic","projectionDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`,
		RuntimeArgoNamespaceEnv:         config.Application.ArgoNamespace,
	}
	return values, config.Publisher
}

func runtimeLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { value, ok := values[name]; return value, ok }
}

func TestRuntimeConfigFromLookupIsStrictlyDefaultOff(t *testing.T) {
	config, err := RuntimeConfigFromLookup(runtimeLookup(nil), ProtectedPublisherIdentity{})
	if err != nil || config.Enabled {
		t.Fatalf("config=%+v err=%v", config, err)
	}
	for _, enabled := range []string{"false", "False", "1", " true"} {
		values := map[string]string{RuntimeEnabledEnv: enabled}
		if enabled == "false" {
			values[RuntimeOCIRegistryHostsEnv] = "ghcr.io"
		}
		if _, err = RuntimeConfigFromLookup(runtimeLookup(values), ProtectedPublisherIdentity{}); err == nil {
			t.Fatalf("enabled value %q or dormant field was accepted", enabled)
		}
	}
}

func TestRuntimeConfigFromLookupBuildsExactEnabledPolicy(t *testing.T) {
	values, publisher := runtimeEnvironmentFixture(t)
	config, err := RuntimeConfigFromLookup(runtimeLookup(values), publisher)
	if err != nil || config.Validate() != nil || config.Publisher != publisher ||
		len(config.OCIRegistryHosts) != 2 || len(config.OCIAuthHosts) != 2 || len(config.OCIRedirectHosts) != 1 ||
		len(config.OCICredentialProfiles) != 1 || config.OCICredentialProfiles[0].Name != "private-main" {
		t.Fatalf("config=%+v err=%v", config, err)
	}
	for name := range values {
		if name == RuntimeEnabledEnv {
			continue
		}
		mutation := make(map[string]string, len(values)-1)
		for key, value := range values {
			if key != name {
				mutation[key] = value
			}
		}
		if name == RuntimeOCIAuthHostsEnv || name == RuntimeOCIRedirectHostsEnv || name == RuntimeOCICredentialProfilesEnv {
			continue
		}
		if _, err = RuntimeConfigFromLookup(runtimeLookup(mutation), publisher); err == nil {
			t.Fatalf("missing required environment %s was accepted", name)
		}
	}
}

func TestRuntimeConfigFromLookupRejectsNonCanonicalOrUntrustedInput(t *testing.T) {
	values, publisher := runtimeEnvironmentFixture(t)
	mutations := []func(map[string]string){
		func(v map[string]string) { v[RuntimeOCIRegistryHostsEnv] = "registry.example.com,ghcr.io" },
		func(v map[string]string) { v[RuntimeOCIRegistryHostsEnv] = "ghcr.io,ghcr.io" },
		func(v map[string]string) { v[RuntimeOCIAuthHostsEnv] = "" },
		func(v map[string]string) {
			v[RuntimeOCIRedirectHostsEnv] = "https://pkg-containers.githubusercontent.com/path?query=1"
		},
		func(v map[string]string) { v[RuntimeOCIRedirectHostsEnv] = "z.example.com,a.example.com" },
		func(v map[string]string) {
			v[RuntimeOCICredentialProfilesEnv] = `[{"registryHost":"registry.example.com","authHost":"auth.example.com","name":"private-main","mode":"basic","projectionDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","secret":"caller"}]`
		},
		func(v map[string]string) {
			v[RuntimeOCICredentialProfilesEnv] = `[{"registryHost":"other.example.com","authHost":"auth.example.com","name":"private-main","mode":"basic","projectionDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`
		},
		func(v map[string]string) {
			v[RuntimeOCICredentialProfilesEnv] = `[{"registryHost":"registry.example.com","authHost":"other.example.com","name":"private-main","mode":"basic","projectionDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`
		},
		func(v map[string]string) {
			v[RuntimeOCICredentialProfilesEnv] = `[{"registryHost":"registry.example.com","registryHost":"ghcr.io","authHost":"ghcr.io","name":"private-main","mode":"basic","projectionDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`
		},
		func(v map[string]string) { v[RuntimePackageCacheBytesEnv] = "+67108864" },
		func(v map[string]string) { v[RuntimeRendererPollMillisEnv] = "0100" },
		func(v map[string]string) { v[RuntimePublishLeaseSecondsEnv] = "5" },
		func(v map[string]string) { v[RuntimeArgoNamespaceEnv] = "ARGO" },
	}
	for index, mutate := range mutations {
		copyValues := make(map[string]string, len(values))
		for key, value := range values {
			copyValues[key] = value
		}
		mutate(copyValues)
		if _, err := RuntimeConfigFromLookup(runtimeLookup(copyValues), publisher); err == nil {
			t.Fatalf("mutation %d was accepted", index)
		}
	}
	if _, err := RuntimeConfigFromLookup(runtimeLookup(values), ProtectedPublisherIdentity{}); err == nil {
		t.Fatal("untrusted publisher identity was accepted")
	}
}
