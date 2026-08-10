package certificates

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

func observationLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func observationRuntimeSecrets() secrets.RuntimeConfig {
	config := secrets.DefaultRuntimeConfig()
	config.Enabled = true
	config.Namespaces = []string{"payments-production", "search-production"}
	config.FingerprintSecretRef = "runtime-secret-fingerprint"
	config.SealingCertificateSecretRef = "sealed-secrets-key"
	return config
}

func TestObservationConfigIsDefaultOffAndRejectsDormantSettings(t *testing.T) {
	for _, values := range []map[string]string{
		nil,
		{CertificateObservationEnabledEnv: "false"},
	} {
		config, err := ObservationConfigFromLookup(observationLookup(values), secrets.RuntimeConfig{})
		if err != nil || !reflect.DeepEqual(config, ObservationConfig{}) {
			t.Fatalf("config=%#v err=%v", config, err)
		}
	}
	for _, values := range []map[string]string{
		{CertificateObservationPollSecondsEnv: "30"},
		{CertificateObservationEnabledEnv: "false", CertificateObservationMaximumAgeSecondsEnv: "90"},
		{CertificateObservationEnabledEnv: "TRUE"},
		{CertificateObservationEnabledEnv: " true"},
		{CertificateObservationEnabledEnv: ""},
	} {
		if _, err := ObservationConfigFromLookup(observationLookup(values), observationRuntimeSecrets()); !errors.Is(err, ErrObservationUnavailable) {
			t.Fatalf("unsafe disabled config accepted: %#v err=%v", values, err)
		}
	}
	if _, err := ObservationConfigFromLookup(nil, observationRuntimeSecrets()); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("nil lookup error=%v", err)
	}
}

func TestObservationConfigDerivesExactRuntimeSecretNamespaces(t *testing.T) {
	config, err := ObservationConfigFromLookup(observationLookup(map[string]string{
		CertificateObservationEnabledEnv: "true",
	}), observationRuntimeSecrets())
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || !reflect.DeepEqual(config.Namespaces, []string{"payments-production", "search-production"}) ||
		config.PollInterval != CertificateObservationPollInterval || config.WorkLease != CertificateObservationWorkLease ||
		config.HeartbeatInterval != CertificateObservationHeartbeat || config.IdleDelay != CertificateObservationIdleDelay ||
		config.MinimumBackoff != CertificateObservationMinimumBackoff || config.MaximumBackoff != CertificateObservationMaximumBackoff ||
		config.MaximumObservationAge != CertificateObservationMaximumAge {
		t.Fatalf("config=%#v", config)
	}
	runtimeSecrets := observationRuntimeSecrets()
	runtimeSecrets.Namespaces[0], runtimeSecrets.Namespaces[1] = runtimeSecrets.Namespaces[1], runtimeSecrets.Namespaces[0]
	if _, err = ObservationConfigFromLookup(observationLookup(map[string]string{CertificateObservationEnabledEnv: "true"}), runtimeSecrets); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("unsorted runtime-secret allowlist accepted: %v", err)
	}
	runtimeSecrets = observationRuntimeSecrets()
	runtimeSecrets.SealingCertificateSecretRef = ""
	if _, err = ObservationConfigFromLookup(observationLookup(map[string]string{CertificateObservationEnabledEnv: "true"}), runtimeSecrets); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("missing fixed sealing-certificate projection accepted: %v", err)
	}
}

func TestObservationConfigParsesCanonicalTimingOverrides(t *testing.T) {
	values := map[string]string{
		CertificateObservationEnabledEnv:               "true",
		CertificateObservationPollSecondsEnv:           "40",
		CertificateObservationWorkLeaseSecondsEnv:      "60",
		CertificateObservationHeartbeatSecondsEnv:      "12",
		CertificateObservationIdleSecondsEnv:           "2",
		CertificateObservationMinimumBackoffSecondsEnv: "3",
		CertificateObservationMaximumBackoffSecondsEnv: "600",
		CertificateObservationMaximumAgeSecondsEnv:     "120",
	}
	config, err := ObservationConfigFromLookup(observationLookup(values), observationRuntimeSecrets())
	if err != nil || config.PollInterval != 40*time.Second || config.WorkLease != time.Minute ||
		config.HeartbeatInterval != 12*time.Second || config.IdleDelay != 2*time.Second ||
		config.MinimumBackoff != 3*time.Second || config.MaximumBackoff != 10*time.Minute ||
		config.MaximumObservationAge != 2*time.Minute {
		t.Fatalf("config=%#v err=%v", config, err)
	}
	for name, mutation := range map[string]string{
		CertificateObservationPollSecondsEnv:           "04",
		CertificateObservationWorkLeaseSecondsEnv:      "19",
		CertificateObservationHeartbeatSecondsEnv:      "30",
		CertificateObservationIdleSecondsEnv:           "0",
		CertificateObservationMinimumBackoffSecondsEnv: "-1",
		CertificateObservationMaximumBackoffSecondsEnv: "3601",
		CertificateObservationMaximumAgeSecondsEnv:     "59",
	} {
		invalid := map[string]string{CertificateObservationEnabledEnv: "true", name: mutation}
		if _, err = ObservationConfigFromLookup(observationLookup(invalid), observationRuntimeSecrets()); !errors.Is(err, ErrObservationUnavailable) {
			t.Fatalf("%s=%q accepted: %v", name, mutation, err)
		}
	}
}

func TestObservationPolicyDigestIsCanonicalAndDefaultOff(t *testing.T) {
	disabled, err := ObservationPolicyDigest(ObservationConfig{})
	if err != nil || disabled == "" {
		t.Fatalf("disabled digest=%q err=%v", disabled, err)
	}
	dormant := DefaultObservationConfig()
	if _, err = ObservationPolicyDigest(dormant); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("dormant config accepted: %v", err)
	}
	config, err := ObservationConfigFromLookup(observationLookup(map[string]string{CertificateObservationEnabledEnv: "true"}), observationRuntimeSecrets())
	if err != nil {
		t.Fatal(err)
	}
	digest, err := ObservationPolicyDigest(config)
	identity, identityErr := ObservationIdentityForConfig(config)
	if err != nil || identityErr != nil || digest != identity.ConfigDigest || digest == disabled {
		t.Fatalf("digest=%q identity=%#v err=%v identityErr=%v", digest, identity, err, identityErr)
	}
}
