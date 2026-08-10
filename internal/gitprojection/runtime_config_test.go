package gitprojection

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
)

func TestRuntimeConfigDefaultsDisabled(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"unset": {},
		"false": {ProjectionEnabledEnv: "false"},
		"false with chart defaults": {
			ProjectionEnabledEnv: "false", ProjectionCacheMaxBytesEnv: "536870912", ProjectionPollSecondsEnv: "300",
			ProjectionGitHubAppIDEnv: "0", ProjectionGitHubClientEnv: "", ProjectionGitAuthModeEnv: "github-app",
		},
	} {
		t.Run(name, func(t *testing.T) {
			config, err := RuntimeConfigFromLookup(projectionMapLookup(values))
			if err != nil || config.Enabled || config.Validate() != nil {
				t.Fatalf("config=%#v err=%v", config, err)
			}
		})
	}
}

func TestRuntimeConfigEnabledIsBoundedAndUsesFixedProjectedKey(t *testing.T) {
	config, err := RuntimeConfigFromLookup(projectionMapLookup(validProjectionEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.CacheMaxBytes != 512<<20 || config.PollInterval != 5*time.Minute || !config.WebhookWake || config.Validate() != nil {
		t.Fatalf("config=%#v", config)
	}
	firstDigest, err := config.RuntimeDigest()
	if err != nil || !digestRE.MatchString(firstDigest) {
		t.Fatalf("runtime digest=%q err=%v", firstDigest, err)
	}
	changed := config
	changed.PolicyVersion = "runtime-policy-v2"
	secondDigest, err := changed.RuntimeDigest()
	if err != nil || secondDigest == firstDigest {
		t.Fatalf("operator setting was omitted from runtime identity: %q %v", secondDigest, err)
	}
	withoutWake := config
	withoutWake.WebhookWake = false
	withoutWakeDigest, err := withoutWake.RuntimeDigest()
	if err != nil || withoutWakeDigest == firstDigest {
		t.Fatalf("webhook-wake setting was omitted from runtime identity: %q %v", withoutWakeDigest, err)
	}
	if config.GitHub.PrivateKeySecret != (githubapp.SecretRef{Name: "runtime", Key: "private-key.pem"}) ||
		config.GitHub.WebhookSecret != (githubapp.SecretRef{Name: "runtime", Key: "webhook-secret"}) ||
		config.GitHub.StateSigningSecret != (githubapp.SecretRef{Name: "runtime", Key: "state-signing-secret"}) {
		t.Fatalf("projected refs=%#v", config.GitHub)
	}
	if !reflect.DeepEqual(config.GitHub.MaximumTokenPermissions, githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionWrite, "pull_requests": githubapp.PermissionWrite, "administration": githubapp.PermissionRead}) {
		t.Fatalf("permissions=%v", config.GitHub.MaximumTokenPermissions)
	}
	manager := config.MirrorManager()
	if manager.Root != ProjectionCacheRoot || manager.MaxBytes != config.CacheMaxBytes || manager.MaxFiles != 100_000 || manager.UseCredentials || manager.CredentialProvider != nil || manager.AllowLocalTests || manager.LocalRemote != "" {
		t.Fatalf("manager=%#v", manager)
	}
	indexer := config.Indexer(NewMemoryStore())
	if indexer.MaxDocuments != 1_000 || indexer.MaxBytes != 32<<20 {
		t.Fatalf("indexer=%#v", indexer)
	}
}

func TestRuntimeIdentityIncludesExactAppConfigPolicyDigest(t *testing.T) {
	config, err := RuntimeConfigFromLookup(projectionMapLookup(validProjectionEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	first, err := RuntimeIdentityForConfig(config, "sha256:"+strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	second, err := RuntimeIdentityForConfig(config, "sha256:"+strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first.ContractVersion != RuntimeContract || first.Validate() != nil || second.Validate() != nil {
		t.Fatalf("policy digest was omitted from exact runtime identity: first=%#v second=%#v", first, second)
	}
	if _, err = RuntimeIdentityForConfig(config, ""); err == nil {
		t.Fatal("missing AppConfig policy digest was accepted")
	}
}

func TestRuntimeConfigFailsClosedOnPartialAmbiguousOrUnboundedSettings(t *testing.T) {
	valid := validProjectionEnvironment()
	cases := map[string]map[string]string{
		"ambiguous enabled":      cloneProjectionEnvironment(valid),
		"whitespace enabled":     cloneProjectionEnvironment(valid),
		"missing cache":          cloneProjectionEnvironment(valid),
		"small cache":            cloneProjectionEnvironment(valid),
		"large cache":            cloneProjectionEnvironment(valid),
		"noncanonical cache":     cloneProjectionEnvironment(valid),
		"missing poll":           cloneProjectionEnvironment(valid),
		"short poll":             cloneProjectionEnvironment(valid),
		"noncanonical poll":      cloneProjectionEnvironment(valid),
		"missing webhook wake":   cloneProjectionEnvironment(valid),
		"ambiguous webhook wake": cloneProjectionEnvironment(valid),
		"missing app":            cloneProjectionEnvironment(valid),
		"noncanonical app":       cloneProjectionEnvironment(valid),
		"missing client":         cloneProjectionEnvironment(valid),
		"whitespace client":      cloneProjectionEnvironment(valid),
		"missing auth":           cloneProjectionEnvironment(valid),
		"ambiguous auth":         cloneProjectionEnvironment(valid),
		"missing chart":          cloneProjectionEnvironment(valid),
		"tagged chart":           cloneProjectionEnvironment(valid),
		"missing policy":         cloneProjectionEnvironment(valid),
		"whitespace policy":      cloneProjectionEnvironment(valid),
	}
	cases["ambiguous enabled"][ProjectionEnabledEnv] = "1"
	cases["whitespace enabled"][ProjectionEnabledEnv] = " true "
	delete(cases["missing cache"], ProjectionCacheMaxBytesEnv)
	cases["small cache"][ProjectionCacheMaxBytesEnv] = "67108863"
	cases["large cache"][ProjectionCacheMaxBytesEnv] = "2147483649"
	cases["noncanonical cache"][ProjectionCacheMaxBytesEnv] = "0536870912"
	delete(cases["missing poll"], ProjectionPollSecondsEnv)
	cases["short poll"][ProjectionPollSecondsEnv] = "14"
	cases["noncanonical poll"][ProjectionPollSecondsEnv] = "0300"
	delete(cases["missing webhook wake"], ProjectionWebhookWakeEnv)
	cases["ambiguous webhook wake"][ProjectionWebhookWakeEnv] = "1"
	delete(cases["missing app"], ProjectionGitHubAppIDEnv)
	cases["noncanonical app"][ProjectionGitHubAppIDEnv] = "012345"
	delete(cases["missing client"], ProjectionGitHubClientEnv)
	cases["whitespace client"][ProjectionGitHubClientEnv] = " Iv1_KuberployClient"
	delete(cases["missing auth"], ProjectionGitAuthModeEnv)
	cases["ambiguous auth"][ProjectionGitAuthModeEnv] = "secret"
	delete(cases["missing chart"], ProjectionChartVersionEnv)
	cases["tagged chart"][ProjectionChartVersionEnv] = "kuberploy-runtime:latest"
	delete(cases["missing policy"], ProjectionPolicyVersionEnv)
	cases["whitespace policy"][ProjectionPolicyVersionEnv] = " runtime-policy-v1 "
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			if config, err := RuntimeConfigFromLookup(projectionMapLookup(values)); err == nil || config.Enabled {
				t.Fatalf("config=%#v err=%v", config, err)
			}
		})
	}
}

func validProjectionEnvironment() map[string]string {
	return map[string]string{
		ProjectionEnabledEnv: "true", ProjectionCacheMaxBytesEnv: "536870912", ProjectionPollSecondsEnv: "300",
		ProjectionWebhookWakeEnv: "true",
		ProjectionGitHubAppIDEnv: "12345", ProjectionGitHubClientEnv: "Iv1_KuberployClient", ProjectionGitAuthModeEnv: "github-app",
		ProjectionChartVersionEnv: "0.1.0-rc.8", ProjectionPolicyVersionEnv: "runtime-policy-v1",
	}
}

func projectionMapLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, found := values[name]
		return value, found
	}
}

func cloneProjectionEnvironment(values map[string]string) map[string]string {
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}
