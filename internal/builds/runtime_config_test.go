package builds

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/githubapp"
)

func TestWorkerRuntimeConfigDefaultsDisabled(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"unset": {},
		"false": {GitHubBuildsEnabledEnv: "false"},
	} {
		t.Run(name, func(t *testing.T) {
			config, err := WorkerRuntimeConfigFromLookup(mapLookup(values))
			if err != nil || config.Enabled || config.BuilderNamespace != "" {
				t.Fatalf("config=%#v err=%v", config, err)
			}
		})
	}
}

func TestWorkerRuntimeConfigEnabledUsesFixedProjectedRefsAndNarrowScope(t *testing.T) {
	config, err := WorkerRuntimeConfigFromLookup(mapLookup(map[string]string{
		GitHubBuildsEnabledEnv:        "true",
		GitHubAppIDEnv:                "12345",
		GitHubAppClientIDEnv:          "Iv1_KuberployClient",
		BuilderNamespaceEnv:           "kuberploy-build-dind",
		BuilderPodServiceAccountEnv:   "kuberploy-build-pod",
		BuilderAgentImageEnv:          "ghcr.io/kuberploy/builder@sha256:" + strings.Repeat("a", 64),
		BuilderBuildKitImageEnv:       builder.DefaultBuildKitImage,
		BuilderSourceEgressCIDRsEnv:   "192.0.2.10/32,2001:db8::10/128",
		BuilderRegistryEgressCIDRsEnv: "192.0.2.20/32",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.BuilderNamespace != "kuberploy-build-dind" || config.GitHub.AppID != 12345 || config.GitHub.ClientID != "Iv1_KuberployClient" {
		t.Fatalf("config=%#v", config)
	}
	if config.GitHub.PrivateKeySecret != (githubapp.SecretRef{Name: "runtime", Key: "private-key.pem"}) ||
		config.GitHub.WebhookSecret != (githubapp.SecretRef{Name: "runtime", Key: "webhook-secret"}) ||
		config.GitHub.StateSigningSecret != (githubapp.SecretRef{Name: "runtime", Key: "state-signing-secret"}) {
		t.Fatalf("projected refs are not fixed: %#v", config.GitHub)
	}
	wantPermissions := githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead}
	if !reflect.DeepEqual(config.GitHub.MaximumTokenPermissions, wantPermissions) {
		t.Fatalf("permissions=%v", config.GitHub.MaximumTokenPermissions)
	}
	digest, err := config.RuntimeDigest()
	if err != nil || !digestRE.MatchString(digest) {
		t.Fatalf("digest=%q err=%v", digest, err)
	}
	execution, err := config.ExecutionSettings(5000)
	if err != nil || execution.BuilderAgentImage != config.BuilderAgentImage || len(execution.Egress) != 3 || execution.Egress[1].Ports[0] != 5000 {
		t.Fatalf("execution=%#v err=%v", execution, err)
	}
}

func TestWorkerRuntimeConfigFailsClosedOnPartialOrAmbiguousEnablement(t *testing.T) {
	valid := map[string]string{
		GitHubBuildsEnabledEnv: "true", GitHubAppIDEnv: "12345", GitHubAppClientIDEnv: "Iv1_KuberployClient", BuilderNamespaceEnv: "kuberploy-build-dind",
		BuilderPodServiceAccountEnv: "kuberploy-build-pod", BuilderAgentImageEnv: "ghcr.io/kuberploy/builder@sha256:" + strings.Repeat("a", 64), BuilderBuildKitImageEnv: builder.DefaultBuildKitImage,
		BuilderSourceEgressCIDRsEnv: "192.0.2.10/32", BuilderRegistryEgressCIDRsEnv: "192.0.2.20/32",
	}
	cases := map[string]map[string]string{
		"ambiguous flag":          cloneStringValues(valid),
		"whitespace flag":         cloneStringValues(valid),
		"missing app id":          cloneStringValues(valid),
		"noncanonical app id":     cloneStringValues(valid),
		"whitespace app id":       cloneStringValues(valid),
		"missing client id":       cloneStringValues(valid),
		"whitespace client id":    cloneStringValues(valid),
		"invalid namespace":       cloneStringValues(valid),
		"whitespace namespace":    cloneStringValues(valid),
		"mutable agent image":     cloneStringValues(valid),
		"undersized agent image":  cloneStringValues(valid),
		"missing BuildKit image":  cloneStringValues(valid),
		"mutable BuildKit image":  cloneStringValues(valid),
		"wrong BuildKit version":  cloneStringValues(valid),
		"broad source egress":     cloneStringValues(valid),
		"missing registry egress": cloneStringValues(valid),
	}
	cases["ambiguous flag"][GitHubBuildsEnabledEnv] = "1"
	cases["whitespace flag"][GitHubBuildsEnabledEnv] = " true "
	delete(cases["missing app id"], GitHubAppIDEnv)
	cases["noncanonical app id"][GitHubAppIDEnv] = "012345"
	cases["whitespace app id"][GitHubAppIDEnv] = " 12345"
	delete(cases["missing client id"], GitHubAppClientIDEnv)
	cases["whitespace client id"][GitHubAppClientIDEnv] = "Iv1_KuberployClient "
	cases["invalid namespace"][BuilderNamespaceEnv] = "other/namespace"
	cases["whitespace namespace"][BuilderNamespaceEnv] = " kuberploy-build-dind"
	cases["mutable agent image"][BuilderAgentImageEnv] = "ghcr.io/kuberploy/builder:latest"
	cases["undersized agent image"][BuilderAgentImageEnv] = "a@sha256:" + strings.Repeat("a", 64)
	delete(cases["missing BuildKit image"], BuilderBuildKitImageEnv)
	cases["mutable BuildKit image"][BuilderBuildKitImageEnv] = "docker.io/moby/buildkit:latest"
	cases["wrong BuildKit version"][BuilderBuildKitImageEnv] = "docker.io/moby/buildkit:v0.32.1"
	cases["broad source egress"][BuilderSourceEgressCIDRsEnv] = "0.0.0.0/0"
	delete(cases["missing registry egress"], BuilderRegistryEgressCIDRsEnv)
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			if config, err := WorkerRuntimeConfigFromLookup(mapLookup(values)); err == nil || config.Enabled {
				t.Fatalf("partial config accepted: %#v err=%v", config, err)
			}
		})
	}
}

func TestRuntimeDigestBindsEveryOperatorOwnedWorkerIdentity(t *testing.T) {
	base := runtimeConfigFixture(t)
	want, err := base.RuntimeDigest()
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*WorkerRuntimeConfig){
		"app id":              func(c *WorkerRuntimeConfig) { c.GitHub.AppID++ },
		"client id":           func(c *WorkerRuntimeConfig) { c.GitHub.ClientID = "Iv1_AnotherClient" },
		"namespace":           func(c *WorkerRuntimeConfig) { c.BuilderNamespace = "other-builder" },
		"pod service account": func(c *WorkerRuntimeConfig) { c.BuilderPodServiceAccount = "other-builder-pod" },
		"agent image": func(c *WorkerRuntimeConfig) {
			c.BuilderAgentImage = "ghcr.io/kuberploy/builder@sha256:" + strings.Repeat("b", 64)
		},
		"BuildKit image":  func(c *WorkerRuntimeConfig) { c.BuildKitImage = "registry.example.test/platform/buildkit:v0.32.2" },
		"source egress":   func(c *WorkerRuntimeConfig) { c.SourceEgressCIDRs = []string{"192.0.2.11/32"} },
		"registry egress": func(c *WorkerRuntimeConfig) { c.RegistryEgressCIDRs = []string{"192.0.2.21/32"} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := base
			candidate.SourceEgressCIDRs = append([]string(nil), base.SourceEgressCIDRs...)
			candidate.RegistryEgressCIDRs = append([]string(nil), base.RegistryEgressCIDRs...)
			candidate.GitHub.MaximumTokenPermissions = githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead}
			mutate(&candidate)
			got, digestErr := candidate.RuntimeDigest()
			if digestErr != nil || got == want {
				t.Fatalf("mutated runtime digest=%q base=%q err=%v", got, want, digestErr)
			}
		})
	}
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, found := values[name]
		return value, found
	}
}

func cloneStringValues(values map[string]string) map[string]string {
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}
