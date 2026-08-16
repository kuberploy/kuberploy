package certificates

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

const (
	CertificateObservationEnabledEnv               = "KUBERPLOY_CERTIFICATE_OBSERVATION_ENABLED"
	CertificateObservationPollSecondsEnv           = "KUBERPLOY_CERTIFICATE_OBSERVATION_POLL_SECONDS"
	CertificateObservationWorkLeaseSecondsEnv      = "KUBERPLOY_CERTIFICATE_OBSERVATION_WORK_LEASE_SECONDS"
	CertificateObservationHeartbeatSecondsEnv      = "KUBERPLOY_CERTIFICATE_OBSERVATION_HEARTBEAT_SECONDS"
	CertificateObservationIdleSecondsEnv           = "KUBERPLOY_CERTIFICATE_OBSERVATION_IDLE_SECONDS"
	CertificateObservationMinimumBackoffSecondsEnv = "KUBERPLOY_CERTIFICATE_OBSERVATION_MINIMUM_BACKOFF_SECONDS"
	CertificateObservationMaximumBackoffSecondsEnv = "KUBERPLOY_CERTIFICATE_OBSERVATION_MAXIMUM_BACKOFF_SECONDS"
	CertificateObservationMaximumAgeSecondsEnv     = "KUBERPLOY_CERTIFICATE_OBSERVATION_MAXIMUM_AGE_SECONDS"
)

// ObservationConfigFromEnvironment is default-off and derives its exact
// namespace allowlist from the strict runtime-secret configuration. There is
// deliberately no certificate-observer namespace environment variable that
// could drift from the runtime which creates the immutable SealedSecrets.
func ObservationConfigFromEnvironment(runtimeSecrets secrets.RuntimeConfig) (ObservationConfig, error) {
	return ObservationConfigFromLookup(os.LookupEnv, runtimeSecrets)
}

// ObservationConfigFromLookup accepts only canonical operator-owned timing
// metadata. The observer never receives fingerprint/HMAC material; the strict
// SealedSecret adapter is read-only and its public sealing-certificate source
// remains the fixed runtime-secret projection contract.
func ObservationConfigFromLookup(lookup func(string) (string, bool), runtimeSecrets secrets.RuntimeConfig) (ObservationConfig, error) {
	if lookup == nil {
		return ObservationConfig{}, ErrObservationUnavailable
	}
	enabled, present := lookup(CertificateObservationEnabledEnv)
	// Disabled integrations do not consume their dormant timing settings.
	// This mirrors the other optional runtimes and keeps a chart/config upgrade
	// from failing merely because an unused feature left old values behind.
	if !present || enabled == "" || enabled == "false" {
		return ObservationConfig{}, nil
	}
	if enabled != "true" || runtimeSecrets.Validate() != nil {
		return ObservationConfig{}, ErrObservationUnavailable
	}

	namespaces, err := NormalizeObservationNamespaces(runtimeSecrets.Namespaces)
	if err != nil || !slices.Equal(namespaces, runtimeSecrets.Namespaces) {
		return ObservationConfig{}, ErrObservationUnavailable
	}
	config := DefaultObservationConfig()
	config.Enabled = true
	config.Namespaces = append([]string(nil), namespaces...)
	for name, destination := range map[string]*time.Duration{
		CertificateObservationPollSecondsEnv:           &config.PollInterval,
		CertificateObservationWorkLeaseSecondsEnv:      &config.WorkLease,
		CertificateObservationHeartbeatSecondsEnv:      &config.HeartbeatInterval,
		CertificateObservationIdleSecondsEnv:           &config.IdleDelay,
		CertificateObservationMinimumBackoffSecondsEnv: &config.MinimumBackoff,
		CertificateObservationMaximumBackoffSecondsEnv: &config.MaximumBackoff,
		CertificateObservationMaximumAgeSecondsEnv:     &config.MaximumObservationAge,
	} {
		value, configured := lookup(name)
		if !configured {
			continue
		}
		if value == "" || strings.TrimSpace(value) != value {
			return ObservationConfig{}, ErrObservationUnavailable
		}
		seconds, parseErr := strconv.ParseInt(value, 10, 32)
		if parseErr != nil || seconds < 1 || strconv.FormatInt(seconds, 10) != value {
			return ObservationConfig{}, ErrObservationUnavailable
		}
		*destination = time.Duration(seconds) * time.Second
	}
	if config.Validate() != nil {
		return ObservationConfig{}, ErrObservationUnavailable
	}
	return config, nil
}

// ObservationPolicyDigest gives Git projection a stable default-off identity
// as well as the exact enabled observer identity. The enclosing projection
// policy also digests runtimeSecrets, which binds the fixed public sealing-
// certificate projection metadata without exposing any key material.
func ObservationPolicyDigest(config ObservationConfig) (string, error) {
	if !config.Enabled {
		if len(config.Namespaces) != 0 || config.PollInterval != 0 || config.WorkLease != 0 ||
			config.HeartbeatInterval != 0 || config.IdleDelay != 0 || config.MinimumBackoff != 0 ||
			config.MaximumBackoff != 0 || config.MaximumObservationAge != 0 {
			return "", ErrObservationUnavailable
		}
		encoded, _ := json.Marshal(struct {
			Contract string `json:"contract"`
			Enabled  bool   `json:"enabled"`
		}{CertificateObservationContract, false})
		digest := sha256.Sum256(encoded)
		return "sha256:" + hex.EncodeToString(digest[:]), nil
	}
	identity, err := ObservationIdentityForConfig(config)
	if err != nil {
		return "", ErrObservationUnavailable
	}
	return identity.ConfigDigest, nil
}
