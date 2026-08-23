package certissuers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	ObserverContract                 = "cert-manager-cluster-issuer-observer.v1"
	ObserverEnabledEnv               = "KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_ENABLED"
	ObserverBindingIDEnv             = "KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_BINDING_ID"
	ObserverNamespaceEnv             = "KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_NAMESPACE"
	ObserverServiceAccountEnv        = "KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_SERVICE_ACCOUNT"
	ObserverPollSecondsEnv           = "KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_POLL_SECONDS"
	ObserverRequestTimeoutSecondsEnv = "KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_REQUEST_TIMEOUT_SECONDS"
	ObserverMaximumAgeSecondsEnv     = "KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_MAXIMUM_AGE_SECONDS"
	ObserverReadinessLeaseSecondsEnv = "KUBERPLOY_CERTIFICATE_ISSUER_OBSERVER_READINESS_LEASE_SECONDS"
	MaximumObservedIssuers           = 128
	ObserverAPIGroup                 = "cert-manager.io"
	ObserverAPIVersion               = "v1"
	ObserverKind                     = "ClusterIssuer"
	ObserverNamespace                = ""
)

var ErrObservationUnavailable = errors.New("cert-manager issuer observation is unavailable")

type ObserverConfig struct {
	Enabled        bool
	BindingID      string
	Namespace      string
	ServiceAccount string
	PollInterval   time.Duration
	RequestTimeout time.Duration
	MaximumAge     time.Duration
	ReadinessLease time.Duration
}

type ObserverRuntimeIdentity struct {
	ContractVersion string
	ConfigDigest    string
}

func ObserverConfigFromEnvironment() (ObserverConfig, error) {
	return ObserverConfigFromLookup(os.LookupEnv)
}

// ObserverConfigFromLookup is default-off and ignores dormant companion
// values. It accepts no URL, Kubernetes path, selector, arbitrary GVK, or
// credential setting. Active issuer names come only from the bounded database
// catalog; protected Git emits one exact resourceNames/get RBAC rule per name.
func ObserverConfigFromLookup(lookup func(string) (string, bool)) (ObserverConfig, error) {
	if lookup == nil {
		return ObserverConfig{}, ErrObservationUnavailable
	}
	enabled, present := lookup(ObserverEnabledEnv)
	if !present || enabled == "" || enabled == "false" {
		return ObserverConfig{}, nil
	}
	if enabled != "true" {
		return ObserverConfig{}, ErrObservationUnavailable
	}
	bindingID, bindingConfigured := lookup(ObserverBindingIDEnv)
	namespace, namespaceConfigured := lookup(ObserverNamespaceEnv)
	serviceAccount, serviceAccountConfigured := lookup(ObserverServiceAccountEnv)
	if !bindingConfigured || !namespaceConfigured || !serviceAccountConfigured {
		return ObserverConfig{}, ErrObservationUnavailable
	}
	config := ObserverConfig{Enabled: true, BindingID: bindingID, Namespace: namespace, ServiceAccount: serviceAccount, PollInterval: 30 * time.Second,
		RequestTimeout: 10 * time.Second, MaximumAge: 2 * time.Minute, ReadinessLease: 3 * time.Minute}
	for name, destination := range map[string]*time.Duration{
		ObserverPollSecondsEnv:           &config.PollInterval,
		ObserverRequestTimeoutSecondsEnv: &config.RequestTimeout,
		ObserverMaximumAgeSecondsEnv:     &config.MaximumAge,
		ObserverReadinessLeaseSecondsEnv: &config.ReadinessLease,
	} {
		value, exists := lookup(name)
		if !exists {
			continue
		}
		if value == "" || strings.TrimSpace(value) != value {
			return ObserverConfig{}, ErrObservationUnavailable
		}
		seconds, parseErr := strconv.ParseInt(value, 10, 32)
		if parseErr != nil || seconds < 1 || strconv.FormatInt(seconds, 10) != value {
			return ObserverConfig{}, ErrObservationUnavailable
		}
		*destination = time.Duration(seconds) * time.Second
	}
	if config.Validate() != nil {
		return ObserverConfig{}, ErrObservationUnavailable
	}
	return config, nil
}

func observerCompanionEnvs() []string {
	return []string{ObserverBindingIDEnv, ObserverNamespaceEnv, ObserverServiceAccountEnv, ObserverPollSecondsEnv,
		ObserverRequestTimeoutSecondsEnv, ObserverMaximumAgeSecondsEnv, ObserverReadinessLeaseSecondsEnv}
}

func (c ObserverConfig) Validate() error {
	if !c.Enabled {
		if c.BindingID != "" || c.Namespace != "" || c.ServiceAccount != "" || c.PollInterval != 0 || c.RequestTimeout != 0 || c.MaximumAge != 0 || c.ReadinessLease != 0 {
			return ErrObservationUnavailable
		}
		return nil
	}
	if !uuidRE.MatchString(c.BindingID) || !dnsLabelRE.MatchString(c.Namespace) ||
		!dnsLabelRE.MatchString(c.ServiceAccount) || c.PollInterval < 5*time.Second ||
		c.PollInterval > time.Hour || c.RequestTimeout < time.Second || c.RequestTimeout > 30*time.Second ||
		c.RequestTimeout >= c.PollInterval || c.MaximumAge < 2*c.PollInterval || c.MaximumAge > 15*time.Minute ||
		c.ReadinessLease < c.MaximumAge || c.ReadinessLease < 30*time.Second || c.ReadinessLease > 15*time.Minute || c.PollInterval%time.Second != 0 ||
		c.RequestTimeout%time.Second != 0 || c.MaximumAge%time.Second != 0 || c.ReadinessLease%time.Second != 0 {
		return ErrObservationUnavailable
	}
	return nil
}

func ObserverIdentityForConfig(config ObserverConfig) (ObserverRuntimeIdentity, error) {
	if config.Validate() != nil || !config.Enabled {
		return ObserverRuntimeIdentity{}, ErrObservationUnavailable
	}
	encoded, err := json.Marshal(struct {
		Contract               string `json:"contract"`
		Enabled                bool   `json:"enabled"`
		BindingID              string `json:"bindingId"`
		APIGroup               string `json:"apiGroup"`
		APIVersion             string `json:"apiVersion"`
		Kind                   string `json:"kind"`
		IssuerNamespace        string `json:"issuerNamespace"`
		ObserverNamespace      string `json:"observerNamespace"`
		ObserverServiceAccount string `json:"observerServiceAccount"`
		PollSeconds            int64  `json:"pollSeconds"`
		RequestTimeoutSeconds  int64  `json:"requestTimeoutSeconds"`
		MaximumAgeSeconds      int64  `json:"maximumAgeSeconds"`
		ReadinessLeaseSeconds  int64  `json:"readinessLeaseSeconds"`
	}{ObserverContract, true, config.BindingID, ObserverAPIGroup, ObserverAPIVersion, ObserverKind, ObserverNamespace,
		config.Namespace, config.ServiceAccount, int64(config.PollInterval.Seconds()), int64(config.RequestTimeout.Seconds()),
		int64(config.MaximumAge.Seconds()), int64(config.ReadinessLease.Seconds())})
	if err != nil {
		return ObserverRuntimeIdentity{}, ErrObservationUnavailable
	}
	digest := sha256.Sum256(encoded)
	return ObserverRuntimeIdentity{ContractVersion: ObserverContract, ConfigDigest: "sha256:" + hex.EncodeToString(digest[:])}, nil
}

// ObserverPolicyDigest gives projection policy an exact identity even when
// observation is disabled. Disabled configuration must be entirely empty.
func ObserverPolicyDigest(config ObserverConfig) (string, error) {
	if config.Validate() != nil {
		return "", ErrObservationUnavailable
	}
	if config.Enabled {
		identity, err := ObserverIdentityForConfig(config)
		if err != nil {
			return "", ErrObservationUnavailable
		}
		return identity.ConfigDigest, nil
	}
	encoded, _ := json.Marshal(struct {
		Contract   string `json:"contract"`
		Enabled    bool   `json:"enabled"`
		APIGroup   string `json:"apiGroup"`
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Namespace  string `json:"namespace"`
	}{ObserverContract, false, ObserverAPIGroup, ObserverAPIVersion, ObserverKind, ObserverNamespace})
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (i ObserverRuntimeIdentity) Validate() error {
	if i.ContractVersion != ObserverContract || !digestRE.MatchString(i.ConfigDigest) {
		return ErrObservationUnavailable
	}
	return nil
}

func observerIdentityEqual(left, right ObserverRuntimeIdentity) bool {
	return left == right
}
