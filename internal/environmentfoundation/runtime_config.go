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
	RuntimeEnabledEnv           = "KUBERPLOY_ENVIRONMENT_FOUNDATION_ENABLED"
	RuntimePlatformBindingIDEnv = "KUBERPLOY_ENVIRONMENT_FOUNDATION_PLATFORM_BINDING_ID"
	RuntimeClusterIDEnv         = "KUBERPLOY_CLUSTER_ID"
	RuntimePSAVersionEnv        = "KUBERPLOY_ENVIRONMENT_FOUNDATION_PSA_VERSION"
	RuntimePollSecondsEnv       = "KUBERPLOY_ENVIRONMENT_FOUNDATION_POLL_INTERVAL_SECONDS"
)

var runtimeExclusiveEnvironment = []string{RuntimePlatformBindingIDEnv, RuntimePSAVersionEnv, RuntimePollSecondsEnv}

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
		for _, name := range runtimeExclusiveEnvironment {
			if value, found := lookup(name); found && value != "" {
				return RuntimeConfig{}, errors.New(name + " cannot be configured while environment foundation is disabled")
			}
		}
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
	platformBindingID, clusterID := value(RuntimePlatformBindingIDEnv), value(RuntimeClusterIDEnv)
	profile := DefaultProfile(clusterID, platformBindingID, "sha256:"+strings.Repeat("0", 64), value(RuntimePSAVersionEnv))
	canonical, err := json.Marshal(struct {
		Contract, PlatformBindingID, ClusterID, PSAVersion string
		Quota                                              Quota
		Limits                                             Limits
		ControlPlaneNamespace, ObserverServiceAccount      string
	}{Contract, platformBindingID, clusterID, profile.PSAVersion, profile.Quota, profile.Limits, profile.ControlPlaneNamespace, profile.ObserverServiceAccount})
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
