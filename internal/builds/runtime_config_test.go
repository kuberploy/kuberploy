package builds

import (
	"reflect"
	"strconv"
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
		GitHubBuildsEnabledEnv:            "true",
		GitHubAppIDEnv:                    "12345",
		GitHubAppClientIDEnv:              "Iv1_KuberployClient",
		BuilderNamespaceEnv:               "kuberploy-build-dind",
		BuilderPodServiceAccountEnv:       "kuberploy-build-pod",
		BuilderAgentImageEnv:              "ghcr.io/kuberploy/builder@sha256:" + strings.Repeat("a", 64),
		BuilderBuildKitImageEnv:           builder.DefaultBuildKitImage,
		BuilderSourceEgressCIDRsEnv:       "2001:db8::/29,192.0.0.0/20",
		BuilderRegistryEgressCIDRsEnv:     "192.0.2.20/32",
		"KUBERPLOY_KUBE_API_SERVER_CIDRS": "10.43.0.1/32",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.BuilderNamespace != "kuberploy-build-dind" || config.GitHub.AppID != 12345 || config.GitHub.ClientID != "Iv1_KuberployClient" {
		t.Fatalf("config=%#v", config)
	}
	if !reflect.DeepEqual(config.SourceEgressCIDRs, []string{"192.0.0.0/20", "2001:db8::/29"}) {
		t.Fatalf("source CIDRs were not normalized: %v", config.SourceEgressCIDRs)
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
	if config.NodeIsolation || len(execution.NodeSelector) != 0 || execution.Toleration != (builder.TaintToleration{}) {
		t.Fatalf("single-node builder was not the default: config=%#v execution=%#v", config, execution)
	}

	isolatedValues := map[string]string{
		GitHubBuildsEnabledEnv: "true", GitHubAppIDEnv: "12345", GitHubAppClientIDEnv: "Iv1_KuberployClient",
		BuilderNamespaceEnv: "kuberploy-build-dind", BuilderPodServiceAccountEnv: "kuberploy-build-pod",
		BuilderAgentImageEnv: "ghcr.io/kuberploy/builder@sha256:" + strings.Repeat("a", 64), BuilderBuildKitImageEnv: builder.DefaultBuildKitImage,
		BuilderNodeIsolationEnv: "true",
	}
	isolated, err := WorkerRuntimeConfigFromLookup(mapLookup(isolatedValues))
	if err != nil {
		t.Fatal(err)
	}
	isolatedExecution, err := isolated.ExecutionSettings(5000)
	if err != nil || isolatedExecution.NodeSelector["kuberploy.io/node-class"] != "dind-builder" || isolatedExecution.Toleration.Key != "kuberploy.io/dind-builder" {
		t.Fatalf("isolated execution=%#v err=%v", isolatedExecution, err)
	}
}

func TestWorkerRuntimeConfigFailsClosedOnPartialOrAmbiguousEnablement(t *testing.T) {
	valid := map[string]string{
		GitHubBuildsEnabledEnv: "true", GitHubAppIDEnv: "12345", GitHubAppClientIDEnv: "Iv1_KuberployClient", BuilderNamespaceEnv: "kuberploy-build-dind",
		BuilderPodServiceAccountEnv: "kuberploy-build-pod", BuilderAgentImageEnv: "ghcr.io/kuberploy/builder@sha256:" + strings.Repeat("a", 64), BuilderBuildKitImageEnv: builder.DefaultBuildKitImage,
		BuilderSourceEgressCIDRsEnv: "192.0.2.10/32", BuilderRegistryEgressCIDRsEnv: "192.0.2.20/32",
		"KUBERPLOY_KUBE_API_SERVER_CIDRS": "10.43.0.1/32",
	}
	cases := map[string]map[string]string{
		"ambiguous flag":         cloneStringValues(valid),
		"whitespace flag":        cloneStringValues(valid),
		"missing app id":         cloneStringValues(valid),
		"noncanonical app id":    cloneStringValues(valid),
		"whitespace app id":      cloneStringValues(valid),
		"missing client id":      cloneStringValues(valid),
		"whitespace client id":   cloneStringValues(valid),
		"invalid namespace":      cloneStringValues(valid),
		"whitespace namespace":   cloneStringValues(valid),
		"missing BuildKit image": cloneStringValues(valid),
		"broad source egress":    cloneStringValues(valid),
	}
	cases["ambiguous flag"][GitHubBuildsEnabledEnv] = "1"
	cases["invalid node isolation"] = cloneStringValues(valid)
	cases["invalid node isolation"][BuilderNodeIsolationEnv] = "yes"
	cases["whitespace flag"][GitHubBuildsEnabledEnv] = " true "
	delete(cases["missing app id"], GitHubAppIDEnv)
	cases["noncanonical app id"][GitHubAppIDEnv] = "012345"
	cases["whitespace app id"][GitHubAppIDEnv] = " 12345"
	delete(cases["missing client id"], GitHubAppClientIDEnv)
	cases["whitespace client id"][GitHubAppClientIDEnv] = "Iv1_KuberployClient "
	cases["invalid namespace"][BuilderNamespaceEnv] = "other/namespace"
	cases["whitespace namespace"][BuilderNamespaceEnv] = " kuberploy-build-dind"
	delete(cases["missing BuildKit image"], BuilderBuildKitImageEnv)
	cases["broad source egress"][BuilderSourceEgressCIDRsEnv] = "0.0.0.0/0"
	cases["too broad source egress"] = cloneStringValues(valid)
	cases["too broad source egress"][BuilderSourceEgressCIDRsEnv] = "10.0.0.0/7"
	cases["noncanonical source egress"] = cloneStringValues(valid)
	cases["noncanonical source egress"][BuilderSourceEgressCIDRsEnv] = "192.0.2.1/24"
	cases["broad registry egress"] = cloneStringValues(valid)
	cases["broad registry egress"][BuilderRegistryEgressCIDRsEnv] = "192.0.2.0/24"
	tooMany := make([]string, 129)
	for index := range tooMany {
		tooMany[index] = "10." + strconv.Itoa(index/256) + "." + strconv.Itoa(index%256) + ".0/24"
	}
	cases["too many source egress"] = cloneStringValues(valid)
	cases["too many source egress"][BuilderSourceEgressCIDRsEnv] = strings.Join(tooMany, ",")
	crossListDuplicate := append([]string{"192.0.2.20/32"}, tooMany[:127]...)
	cases["too many raw entries across source and registry"] = cloneStringValues(valid)
	cases["too many raw entries across source and registry"][BuilderSourceEgressCIDRsEnv] = strings.Join(crossListDuplicate, ",")
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			if config, err := WorkerRuntimeConfigFromLookup(mapLookup(values)); err == nil || config.Enabled {
				t.Fatalf("partial config accepted: %#v err=%v", config, err)
			}
		})
	}
}

func TestWorkerRuntimeConfigDefaultsPublicBuildEgressWithAPIExclusions(t *testing.T) {
	values := map[string]string{
		GitHubBuildsEnabledEnv: "true", GitHubAppIDEnv: "12345", GitHubAppClientIDEnv: "Iv1_KuberployClient",
		BuilderNamespaceEnv: "kuberploy-build-dind", BuilderPodServiceAccountEnv: "kuberploy-build-pod",
		BuilderAgentImageEnv: "ghcr.io/kuberploy/builder@sha256:" + strings.Repeat("a", 64), BuilderBuildKitImageEnv: builder.DefaultBuildKitImage,
		"KUBERPLOY_KUBE_API_SERVER_CIDRS": "fd00::1/128,10.43.0.1/32",
	}
	config, err := WorkerRuntimeConfigFromLookup(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	settings, err := config.ExecutionSettings(5000)
	if err != nil || len(settings.Egress) != 2 {
		t.Fatalf("settings=%#v err=%v", settings, err)
	}
	if !reflect.DeepEqual(settings.Egress[0], builder.EgressEndpoint{CIDR: "0.0.0.0/0", Ports: []int{443, 5000}, Except: []string{"10.43.0.1/32"}}) ||
		!reflect.DeepEqual(settings.Egress[1], builder.EgressEndpoint{CIDR: "::/0", Ports: []int{443, 5000}, Except: []string{"fd00::1/128"}}) {
		t.Fatalf("default egress=%#v", settings.Egress)
	}
	delete(values, "KUBERPLOY_KUBE_API_SERVER_CIDRS")
	values[BuilderAgentImageEnv] = "registry.example.test/kuberploy/builder:v1.2.3"
	values[BuilderBuildKitImageEnv] = "registry.example.test/moby/buildkit:v0.32.2"
	values[BuilderDinDImageEnv] = "registry.example.test/docker@sha256:" + strings.Repeat("b", 64)
	values[BuilderSourceEgressCIDRsEnv] = "192.0.2.10/32,192.0.2.10/32"
	config, err = WorkerRuntimeConfigFromLookup(mapLookup(values))
	if err != nil || len(config.KubeAPIServerCIDRs) != 0 || len(config.SourceEgressCIDRs) != 1 {
		t.Fatalf("functional defaults or list normalization failed: %#v err=%v", config, err)
	}
}

func TestWorkerRuntimeConfigResolvesOnlyOperatorApprovedBuildProfiles(t *testing.T) {
	applicationID := "33333333-3333-4333-8333-333333333333"
	otherApplicationID := "55555555-5555-4555-8555-555555555555"
	values := map[string]string{
		GitHubBuildsEnabledEnv: "true", GitHubAppIDEnv: "12345", GitHubAppClientIDEnv: "Iv1_KuberployClient",
		BuilderNamespaceEnv: "kuberploy-build-dind", BuilderPodServiceAccountEnv: "kuberploy-build-pod",
		BuilderAgentImageEnv: "ghcr.io/kuberploy/builder@sha256:" + strings.Repeat("a", 64), BuilderBuildKitImageEnv: builder.DefaultBuildKitImage,
		BuilderBuildSecretEnv: "kuberploy-build-secrets", BuilderSSHSecretEnv: "kuberploy-ssh-secrets",
		BuilderBuildSecretProfilesEnv: `[{"id":"npmrc","label":"Private npm registry","key":"npmrc","applicationIds":["33333333-3333-4333-8333-333333333333"]}]`,
		BuilderSSHSecretProfilesEnv:   `[{"id":"github","label":"GitHub deploy key","key":"id_ed25519","applicationIds":["33333333-3333-4333-8333-333333333333"]}]`,
	}
	config, err := WorkerRuntimeConfigFromLookup(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	selection, err := config.ResolveSecretProfiles(applicationID, []string{"npmrc"}, []string{"github"})
	if err != nil {
		t.Fatal(err)
	}
	if selection.BuildSecret != "kuberploy-build-secrets" || selection.SSHSecret != "kuberploy-ssh-secrets" ||
		!reflect.DeepEqual(selection.SecretFiles, []builder.FileReference{{ID: "npmrc", Path: builder.BuildSecretRoot + "/npmrc"}}) ||
		!reflect.DeepEqual(selection.SSHFiles, []builder.FileReference{{ID: "github", Path: builder.SSHSecretRoot + "/id_ed25519"}}) {
		t.Fatalf("selection=%#v", selection)
	}
	if _, err = config.ResolveSecretProfiles(applicationID, []string{"unknown"}, nil); err == nil {
		t.Fatal("unknown build profile accepted")
	}
	if _, err = config.ResolveSecretProfiles(otherApplicationID, []string{"npmrc"}, nil); err == nil {
		t.Fatal("profile from another application accepted")
	}
	if _, err = config.ResolveSecretFiles(applicationID, selection.SecretFiles, []builder.FileReference{{ID: "unknown", Path: builder.SSHSecretRoot + "/id_ed25519"}}); err == nil {
		t.Fatal("unapproved file reference accepted")
	}
	if catalog, catalogErr := config.SecretProfileCatalog(applicationID); catalogErr != nil || len(catalog.Build) != 1 || catalog.Build[0].ID != "npmrc" || catalog.Build[0].Label != "Private npm registry" {
		t.Fatalf("catalog=%#v", catalog)
	}
	if catalog, catalogErr := config.SecretProfileCatalog(otherApplicationID); catalogErr != nil || len(catalog.Build) != 0 || len(catalog.SSH) != 0 {
		t.Fatalf("cross-application catalog=%#v err=%v", catalog, catalogErr)
	}
}

func TestWorkerRuntimeConfigRejectsUnscopedSecretProfiles(t *testing.T) {
	values := map[string]string{
		GitHubBuildsEnabledEnv: "true", GitHubAppIDEnv: "12345", GitHubAppClientIDEnv: "Iv1_KuberployClient",
		BuilderNamespaceEnv: "kuberploy-build-dind", BuilderPodServiceAccountEnv: "kuberploy-build-pod",
		BuilderAgentImageEnv: "ghcr.io/kuberploy/builder@sha256:" + strings.Repeat("a", 64), BuilderBuildKitImageEnv: builder.DefaultBuildKitImage,
		BuilderBuildSecretProfilesEnv: `[{"id":"npmrc","label":"Private npm registry","key":"npmrc"}]`,
	}
	if _, err := WorkerRuntimeConfigFromLookup(mapLookup(values)); err == nil {
		t.Fatal("unscoped profile accepted")
	}
}

func TestWorkerRuntimeConfigRejectsNoncanonicalKubeAPIServerCIDRs(t *testing.T) {
	base := runtimeConfigFixture(t)
	for name, values := range map[string][]string{
		"noncanonical network": {"10.43.0.1/24"},
		"zero prefix":          {"10.43.0.0/0"},
		"duplicate":            {"10.43.0.1/32", "10.43.0.1/32"},
		"unsorted":             {"fd00::1/128", "10.43.0.1/32"},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			candidate.KubeAPIServerCIDRs = values
			if _, err := candidate.RuntimeDigest(); err == nil {
				t.Fatalf("accepted noncanonical API exclusions: %v", values)
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
		"node isolation":  func(c *WorkerRuntimeConfig) { c.NodeIsolation = !c.NodeIsolation },
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
