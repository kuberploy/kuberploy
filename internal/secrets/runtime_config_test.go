package secrets

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func validRuntimeSecretEnvironment() map[string]string {
	return map[string]string{
		RuntimeSecretsEnabledEnv:                    "true",
		RuntimeSecretNamespacesEnv:                  "payments-production,search-production",
		RuntimeSecretFingerprintSecretRefEnv:        "runtime-secret-fingerprint",
		RuntimeSecretFingerprintSecretKeyEnv:        DefaultFingerprintSecretKey,
		RuntimeSecretFingerprintKeyIDEnv:            DefaultFingerprintKeyID,
		RuntimeSecretSealingCertificateSecretRefEnv: "sealed-secrets-key",
		RuntimeSecretSealingCertificateSecretKeyEnv: DefaultSealedSecretsCertificateKey,
	}
}

func runtimeSecretLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func TestRuntimeSecretConfigIsDefaultOffAndRejectsDormantSettings(t *testing.T) {
	config, err := RuntimeConfigFromLookup(runtimeSecretLookup(nil))
	if err != nil || !reflect.DeepEqual(config, RuntimeConfig{}) {
		t.Fatalf("default config=%#v err=%v", config, err)
	}
	config, err = RuntimeConfigFromLookup(runtimeSecretLookup(map[string]string{RuntimeSecretsEnabledEnv: "false"}))
	if err != nil || !reflect.DeepEqual(config, RuntimeConfig{}) {
		t.Fatalf("disabled config=%#v err=%v", config, err)
	}
	for _, values := range []map[string]string{
		{RuntimeSecretNamespacesEnv: "payments-production"},
		{RuntimeSecretsEnabledEnv: "false", RuntimeSecretFingerprintSecretRefEnv: ""},
		{RuntimeSecretsEnabledEnv: " false"},
		{RuntimeSecretsEnabledEnv: "TRUE"},
		{RuntimeSecretsEnabledEnv: ""},
	} {
		if _, err = RuntimeConfigFromLookup(runtimeSecretLookup(values)); !errors.Is(err, ErrRuntimeUnavailable) {
			t.Fatalf("partial disabled settings accepted: %#v err=%v", values, err)
		}
	}
	if _, err = RuntimeConfigFromLookup(nil); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("nil lookup error=%v", err)
	}
}

func TestRuntimeSecretConfigParsesCanonicalAllowlistAndDefaults(t *testing.T) {
	config, err := RuntimeConfigFromLookup(runtimeSecretLookup(validRuntimeSecretEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || len(config.Namespaces) != 2 || config.Namespaces[0] != "payments-production" ||
		config.Namespaces[1] != "search-production" || !config.AllowsNamespace("payments-production") ||
		config.AllowsNamespace("other") || config.FingerprintSecretRef != "runtime-secret-fingerprint" ||
		config.FingerprintSecretKey != DefaultFingerprintSecretKey || config.FingerprintKeyID != DefaultFingerprintKeyID ||
		config.SealingCertificateSecretRef != "sealed-secrets-key" || config.SealingCertificateSecretKey != DefaultSealedSecretsCertificateKey ||
		config.PollInterval != RuntimeSecretPollInterval || config.WorkLease != RuntimeSecretWorkLease ||
		config.HeartbeatInterval != RuntimeSecretHeartbeatInterval || config.IdleDelay != RuntimeSecretIdleDelay ||
		config.MinimumBackoff != RuntimeSecretMinimumBackoff || config.MaximumBackoff != RuntimeSecretMaximumBackoff {
		t.Fatalf("config=%#v", config)
	}

	values := validRuntimeSecretEnvironment()
	values[RuntimeSecretPollSecondsEnv] = "7"
	values[RuntimeSecretWorkLeaseSecondsEnv] = "60"
	values[RuntimeSecretHeartbeatSecondsEnv] = "12"
	values[RuntimeSecretIdleSecondsEnv] = "2"
	values[RuntimeSecretMinimumBackoffSecondsEnv] = "3"
	values[RuntimeSecretMaximumBackoffSecondsEnv] = "600"
	config, err = RuntimeConfigFromLookup(runtimeSecretLookup(values))
	if err != nil || config.PollInterval != 7*time.Second || config.WorkLease != time.Minute ||
		config.HeartbeatInterval != 12*time.Second || config.IdleDelay != 2*time.Second ||
		config.MinimumBackoff != 3*time.Second || config.MaximumBackoff != 10*time.Minute {
		t.Fatalf("overridden config=%#v err=%v", config, err)
	}
}

func TestRuntimeSecretConfigRejectsAmbiguousOrUnsafeValues(t *testing.T) {
	mutations := map[string]string{
		RuntimeSecretNamespacesEnv:                  "search-production,payments-production",
		RuntimeSecretFingerprintSecretRefEnv:        "other/secret",
		RuntimeSecretFingerprintSecretKeyEnv:        "../key",
		RuntimeSecretFingerprintKeyIDEnv:            " key-id ",
		RuntimeSecretSealingCertificateSecretRefEnv: "../../secret",
		RuntimeSecretSealingCertificateSecretKeyEnv: "../tls.crt",
		RuntimeSecretPollSecondsEnv:                 "05",
		RuntimeSecretWorkLeaseSecondsEnv:            "19",
		RuntimeSecretHeartbeatSecondsEnv:            "23",
		RuntimeSecretIdleSecondsEnv:                 "0",
		RuntimeSecretMinimumBackoffSecondsEnv:       "-1",
		RuntimeSecretMaximumBackoffSecondsEnv:       "3601",
	}
	for name, mutation := range mutations {
		values := validRuntimeSecretEnvironment()
		values[name] = mutation
		if _, err := RuntimeConfigFromLookup(runtimeSecretLookup(values)); !errors.Is(err, ErrRuntimeUnavailable) {
			t.Fatalf("%s=%q accepted: %v", name, mutation, err)
		}
	}
	for _, namespaces := range []string{
		"payments-production,payments-production",
		"payments-production,",
		"payments-production, search-production",
		"Payments-production",
	} {
		values := validRuntimeSecretEnvironment()
		values[RuntimeSecretNamespacesEnv] = namespaces
		if _, err := RuntimeConfigFromLookup(runtimeSecretLookup(values)); !errors.Is(err, ErrRuntimeUnavailable) {
			t.Fatalf("namespaces=%q accepted: %v", namespaces, err)
		}
	}
	for _, missing := range []string{
		RuntimeSecretNamespacesEnv,
		RuntimeSecretFingerprintSecretRefEnv,
		RuntimeSecretFingerprintSecretKeyEnv,
		RuntimeSecretFingerprintKeyIDEnv,
		RuntimeSecretSealingCertificateSecretRefEnv,
		RuntimeSecretSealingCertificateSecretKeyEnv,
	} {
		values := validRuntimeSecretEnvironment()
		delete(values, missing)
		if _, err := RuntimeConfigFromLookup(runtimeSecretLookup(values)); !errors.Is(err, ErrRuntimeUnavailable) {
			t.Fatalf("missing %s accepted: %v", missing, err)
		}
	}
}
