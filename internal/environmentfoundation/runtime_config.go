package environmentfoundation

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
	RuntimeEnabledEnv                = "KUBERPLOY_ENVIRONMENT_FOUNDATION_ENABLED"
	RuntimePlatformBindingIDEnv      = "KUBERPLOY_ENVIRONMENT_FOUNDATION_PLATFORM_BINDING_ID"
	RuntimePSAVersionEnv             = "KUBERPLOY_ENVIRONMENT_FOUNDATION_PSA_VERSION"
	RuntimePollSecondsEnv            = "KUBERPLOY_ENVIRONMENT_FOUNDATION_POLL_INTERVAL_SECONDS"
	RuntimeControlPlaneNamespaceEnv  = "KUBERPLOY_ENVIRONMENT_FOUNDATION_CONTROL_PLANE_NAMESPACE"
	RuntimeObserverServiceAccountEnv = "KUBERPLOY_ENVIRONMENT_FOUNDATION_OBSERVER_SERVICE_ACCOUNT"
)

var runtimeExclusiveEnvironment = []string{
	RuntimePlatformBindingIDEnv,
	RuntimePSAVersionEnv,
	RuntimePollSecondsEnv,
	RuntimeControlPlaneNamespaceEnv,
	RuntimeObserverServiceAccountEnv,
}

type RuntimeConfig struct {
	Enabled           bool
	PlatformBindingID string
	Profile           Profile
	PollInterval      time.Duration
	Publisher         PublisherIdentity
}

func RuntimeConfigFromEnvironment() (RuntimeConfig, error) {
	return RuntimeConfigFromLookup(os.LookupEnv)
}

func RuntimeConfigFromLookup(lookup func(string) (string, bool)) (RuntimeConfig, error) {
	if lookup == nil {
		return RuntimeConfig{}, ErrInvalid
	}
	enabled, present := lookup(RuntimeEnabledEnv)
	if !present || enabled == "" || enabled == "false" {
		return RuntimeConfig{}, nil
	}
	if enabled != "true" {
		return RuntimeConfig{}, errors.New(RuntimeEnabledEnv + " must be exactly true or false")
	}
	value := func(name string) string {
		raw, found := lookup(name)
		if !found || raw == "" || strings.TrimSpace(raw) != raw {
			return ""
		}
		return raw
	}
	pollRaw := value(RuntimePollSecondsEnv)
	pollSeconds, err := strconv.ParseInt(pollRaw, 10, 64)
	if err != nil || pollSeconds < 1 || pollSeconds > 60 || strconv.FormatInt(pollSeconds, 10) != pollRaw {
		return RuntimeConfig{}, errors.New(RuntimePollSecondsEnv + " must be a canonical integer from 1 through 60")
	}
	platformBindingID := value(RuntimePlatformBindingIDEnv)
	profile := DefaultProfile(platformBindingID, "sha256:"+strings.Repeat("0", 64), value(RuntimePSAVersionEnv))
	profile.ControlPlaneNamespace = value(RuntimeControlPlaneNamespaceEnv)
	profile.ObserverServiceAccount = value(RuntimeObserverServiceAccountEnv)
	canonical, err := json.Marshal(struct {
		Contract, PlatformBindingID, PSAVersion       string
		Quota                                         Quota
		Limits                                        Limits
		ControlPlaneNamespace, ObserverServiceAccount string
	}{Contract, platformBindingID, profile.PSAVersion, profile.Quota, profile.Limits, profile.ControlPlaneNamespace, profile.ObserverServiceAccount})
	if err != nil {
		return RuntimeConfig{}, ErrInvalid
	}
	sum := sha256.Sum256(canonical)
	profile.PublisherConfigDigest = "sha256:" + hex.EncodeToString(sum[:])
	config := RuntimeConfig{Enabled: true, PlatformBindingID: platformBindingID, Profile: profile,
		PollInterval: time.Duration(pollSeconds) * time.Second,
		Publisher:    PublisherIdentity{Contract: PublisherContract, Policy: ProtectedGitPolicy, ConfigDigest: profile.PublisherConfigDigest}}
	if config.Validate() != nil {
		return RuntimeConfig{}, ErrInvalid
	}
	return config, nil
}

func (c RuntimeConfig) Validate() error {
	if !c.Enabled {
		if c.PlatformBindingID != "" || c.Profile != (Profile{}) || c.PollInterval != 0 || c.Publisher != (PublisherIdentity{}) {
			return ErrInvalid
		}
		return nil
	}
	if !uuidRE.MatchString(c.PlatformBindingID) || c.Profile.Validate() != nil || c.Publisher.Validate() != nil ||
		c.Publisher.ConfigDigest != c.Profile.PublisherConfigDigest || c.PollInterval < time.Second || c.PollInterval > time.Minute {
		return ErrInvalid
	}
	return nil
}
