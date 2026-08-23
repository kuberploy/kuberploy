package autodeploy

import (
	"strings"
	"testing"
)

func TestRuntimeConfigDerivesClosedExactIdentity(t *testing.T) {
	lookup := func(values map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	}
	config, err := runtimeConfigFromLookup(lookup(map[string]string{RuntimeEnabledEnv: "true"}))
	if err != nil || !config.Enabled || config.Identity != (RuntimeIdentity{}) {
		t.Fatalf("enabled config=%#v err=%v", config, err)
	}
	for _, value := range []string{"1", "TRUE", " true "} {
		if _, err = runtimeConfigFromLookup(lookup(map[string]string{RuntimeEnabledEnv: value})); err == nil {
			t.Fatalf("ambiguous flag %q accepted", value)
		}
	}
	authorities := RuntimeAuthorities{
		SourceBuildConfigDigest:     "sha256:" + strings.Repeat("a", 64),
		SourceBuildGitHubAppID:      1001,
		GitProjectionConfigDigest:   "sha256:" + strings.Repeat("b", 64),
		GitProjectionGitHubAppID:    1001,
		FoundationConfigDigest:      "sha256:" + strings.Repeat("9", 64),
		FoundationPollNanos:         1_000_000_000,
		FoundationPlatformBindingID: "11111111-1111-4111-8111-111111111111",
		ArgoConfigDigest:            "sha256:" + strings.Repeat("c", 64),
		ArgoGitHubAppID:             1001,
		ArgoPlatformBindingID:       "11111111-1111-4111-8111-111111111111",
	}
	identity, err := RuntimeIdentityForAuthorities(authorities)
	if err != nil || identity.Validate() != nil {
		t.Fatalf("derived identity=%#v err=%v", identity, err)
	}
	mutations := map[string]func(*RuntimeAuthorities){
		"source digest":     func(a *RuntimeAuthorities) { a.SourceBuildConfigDigest = "sha256:" + strings.Repeat("d", 64) },
		"git digest":        func(a *RuntimeAuthorities) { a.GitProjectionConfigDigest = "sha256:" + strings.Repeat("e", 64) },
		"foundation digest": func(a *RuntimeAuthorities) { a.FoundationConfigDigest = "sha256:" + strings.Repeat("8", 64) },
		"foundation poll":   func(a *RuntimeAuthorities) { a.FoundationPollNanos = 2_000_000_000 },
		"argo digest":       func(a *RuntimeAuthorities) { a.ArgoConfigDigest = "sha256:" + strings.Repeat("f", 64) },
		"binding": func(a *RuntimeAuthorities) {
			a.ArgoPlatformBindingID, a.FoundationPlatformBindingID = "33333333-3333-4333-8333-333333333333", "33333333-3333-4333-8333-333333333333"
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := authorities
			mutate(&changed)
			other, otherErr := RuntimeIdentityForAuthorities(changed)
			if otherErr != nil || other == identity {
				t.Fatalf("authority mutation not reflected: identity=%#v err=%v", other, otherErr)
			}
		})
	}
	mismatched := authorities
	mismatched.ArgoGitHubAppID++
	if _, err = RuntimeIdentityForAuthorities(mismatched); err == nil {
		t.Fatal("cross-authority GitHub App mismatch accepted")
	}
}
