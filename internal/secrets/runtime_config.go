package secrets

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	RuntimeSecretsEnabledEnv                    = "KUBERPLOY_RUNTIME_SECRETS_ENABLED"
	RuntimeSecretNamespacesEnv                  = "KUBERPLOY_RUNTIME_SECRET_NAMESPACES"
	RuntimeSecretFingerprintSecretRefEnv        = "KUBERPLOY_RUNTIME_SECRET_FINGERPRINT_SECRET_REF"
	RuntimeSecretFingerprintSecretKeyEnv        = "KUBERPLOY_RUNTIME_SECRET_FINGERPRINT_SECRET_KEY"
	RuntimeSecretFingerprintKeyIDEnv            = "KUBERPLOY_RUNTIME_SECRET_FINGERPRINT_KEY_ID"
	RuntimeSecretSealingCertificateSecretRefEnv = "KUBERPLOY_RUNTIME_SECRET_SEALING_CERTIFICATE_SECRET_REF"
	RuntimeSecretSealingCertificateSecretKeyEnv = "KUBERPLOY_RUNTIME_SECRET_SEALING_CERTIFICATE_SECRET_KEY"
	RuntimeSecretPollSecondsEnv                 = "KUBERPLOY_RUNTIME_SECRET_POLL_SECONDS"
	RuntimeSecretWorkLeaseSecondsEnv            = "KUBERPLOY_RUNTIME_SECRET_WORK_LEASE_SECONDS"
	RuntimeSecretHeartbeatSecondsEnv            = "KUBERPLOY_RUNTIME_SECRET_HEARTBEAT_SECONDS"
	RuntimeSecretIdleSecondsEnv                 = "KUBERPLOY_RUNTIME_SECRET_IDLE_SECONDS"
	RuntimeSecretMinimumBackoffSecondsEnv       = "KUBERPLOY_RUNTIME_SECRET_MINIMUM_BACKOFF_SECONDS"
	RuntimeSecretMaximumBackoffSecondsEnv       = "KUBERPLOY_RUNTIME_SECRET_MAXIMUM_BACKOFF_SECONDS"
)

// RuntimeConfigFromEnvironment reads only operator-owned metadata. Projected
// key and certificate bytes stay on their fixed filesystem paths and never
// enter the process environment.
func RuntimeConfigFromEnvironment() (RuntimeConfig, error) {
	return RuntimeConfigFromLookup(os.LookupEnv)
}

// RuntimeConfigFromLookup is default-off. Dormant values are ignored, while an
// enabled runtime normalizes user lists into its canonical durable identity.
func RuntimeConfigFromLookup(lookup func(string) (string, bool)) (RuntimeConfig, error) {
	if lookup == nil {
		return RuntimeConfig{}, ErrRuntimeUnavailable
	}
	enabled, present := lookup(RuntimeSecretsEnabledEnv)
	if !present || enabled == "" || enabled == "false" {
		return RuntimeConfig{}, nil
	}
	if enabled != "true" {
		return RuntimeConfig{}, ErrRuntimeUnavailable
	}

	namespaceValue, ok := exactRuntimeSecretEnv(lookup, RuntimeSecretNamespacesEnv, true)
	if !ok {
		return RuntimeConfig{}, ErrRuntimeUnavailable
	}
	namespaces := strings.Split(namespaceValue, ",")
	normalized, err := NormalizeRuntimeNamespaces(namespaces)
	if err != nil {
		return RuntimeConfig{}, ErrRuntimeUnavailable
	}
	fingerprintRef, ok := exactRuntimeSecretEnv(lookup, RuntimeSecretFingerprintSecretRefEnv, true)
	if !ok {
		return RuntimeConfig{}, ErrRuntimeUnavailable
	}
	fingerprintSecretKey, ok := exactRuntimeSecretEnv(lookup, RuntimeSecretFingerprintSecretKeyEnv, true)
	if !ok {
		return RuntimeConfig{}, ErrRuntimeUnavailable
	}
	fingerprintKeyID, ok := exactRuntimeSecretEnv(lookup, RuntimeSecretFingerprintKeyIDEnv, true)
	if !ok {
		return RuntimeConfig{}, ErrRuntimeUnavailable
	}
	certificateRef, ok := exactRuntimeSecretEnv(lookup, RuntimeSecretSealingCertificateSecretRefEnv, true)
	if !ok {
		return RuntimeConfig{}, ErrRuntimeUnavailable
	}
	certificateSecretKey, ok := exactRuntimeSecretEnv(lookup, RuntimeSecretSealingCertificateSecretKeyEnv, true)
	if !ok {
		return RuntimeConfig{}, ErrRuntimeUnavailable
	}

	config := DefaultRuntimeConfig()
	config.Enabled = true
	config.Namespaces = normalized
	config.FingerprintSecretRef = fingerprintRef
	config.FingerprintSecretKey = fingerprintSecretKey
	config.FingerprintKeyID = fingerprintKeyID
	config.SealingCertificateSecretRef = certificateRef
	config.SealingCertificateSecretKey = certificateSecretKey
	for name, destination := range map[string]*time.Duration{
		RuntimeSecretPollSecondsEnv:           &config.PollInterval,
		RuntimeSecretWorkLeaseSecondsEnv:      &config.WorkLease,
		RuntimeSecretHeartbeatSecondsEnv:      &config.HeartbeatInterval,
		RuntimeSecretIdleSecondsEnv:           &config.IdleDelay,
		RuntimeSecretMinimumBackoffSecondsEnv: &config.MinimumBackoff,
		RuntimeSecretMaximumBackoffSecondsEnv: &config.MaximumBackoff,
	} {
		value, configured := lookup(name)
		if !configured {
			continue
		}
		if value == "" || strings.TrimSpace(value) != value {
			return RuntimeConfig{}, ErrRuntimeUnavailable
		}
		seconds, parseErr := strconv.ParseInt(value, 10, 32)
		if parseErr != nil || seconds < 1 || strconv.FormatInt(seconds, 10) != value {
			return RuntimeConfig{}, ErrRuntimeUnavailable
		}
		*destination = time.Duration(seconds) * time.Second
	}
	if config.Validate() != nil {
		return RuntimeConfig{}, ErrRuntimeUnavailable
	}
	return config, nil
}

func exactRuntimeSecretEnv(lookup func(string) (string, bool), name string, required bool) (string, bool) {
	value, present := lookup(name)
	if !present {
		return "", !required
	}
	if value == "" || strings.TrimSpace(value) != value {
		return "", false
	}
	return value, true
}
