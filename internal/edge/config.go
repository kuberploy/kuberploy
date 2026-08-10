package edge

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	RuntimeEnabledEnv          = "KUBERPLOY_EDGE_RUNTIME_ENABLED"
	RuntimeProfilesJSONEnv     = "KUBERPLOY_EDGE_RUNTIME_PROFILES_JSON"
	RuntimePollSecondsEnv      = "KUBERPLOY_EDGE_RUNTIME_POLL_SECONDS"
	RuntimeWorkLeaseSecondsEnv = "KUBERPLOY_EDGE_RUNTIME_WORK_LEASE_SECONDS"
	RuntimeHeartbeatSecondsEnv = "KUBERPLOY_EDGE_RUNTIME_HEARTBEAT_SECONDS"
	RuntimeReadinessSecondsEnv = "KUBERPLOY_EDGE_RUNTIME_READINESS_SECONDS"
	RuntimeMinimumBackoffEnv   = "KUBERPLOY_EDGE_RUNTIME_MINIMUM_BACKOFF_SECONDS"
	RuntimeMaximumBackoffEnv   = "KUBERPLOY_EDGE_RUNTIME_MAXIMUM_BACKOFF_SECONDS"

	maximumProfilesJSONBytes = 128 << 10
)

type RuntimeProfiles struct {
	Traefik     *TraefikProfile      `json:"traefik,omitempty"`
	CertManager *CertManagerProfile  `json:"certManager,omitempty"`
	ExternalDNS []ExternalDNSProfile `json:"externalDNS,omitempty"`
}

type RuntimeConfig struct {
	Enabled           bool
	Profiles          RuntimeProfiles
	PollInterval      time.Duration
	WorkLease         time.Duration
	HeartbeatInterval time.Duration
	ReadinessMaxAge   time.Duration
	MinimumBackoff    time.Duration
	MaximumBackoff    time.Duration
}

func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		PollInterval: 30 * time.Second, WorkLease: 2 * time.Minute,
		HeartbeatInterval: 20 * time.Second, ReadinessMaxAge: 90 * time.Second,
		MinimumBackoff: 5 * time.Second, MaximumBackoff: 5 * time.Minute,
	}
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
	raw, found := lookup(RuntimeProfilesJSONEnv)
	if !found || len(raw) == 0 || len(raw) > maximumProfilesJSONBytes || strings.TrimSpace(raw) != raw {
		return RuntimeConfig{}, errors.New(RuntimeProfilesJSONEnv + " is required and must be bounded canonical JSON")
	}
	var profiles RuntimeProfiles
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profiles); err != nil {
		return RuntimeConfig{}, errors.New(RuntimeProfilesJSONEnv + " is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return RuntimeConfig{}, errors.New(RuntimeProfilesJSONEnv + " contains trailing JSON")
	}
	config := RuntimeConfig{Enabled: true, Profiles: cloneRuntimeProfiles(profiles)}
	values := []struct {
		name        string
		destination *time.Duration
		minimum     int64
		maximum     int64
	}{
		{RuntimePollSecondsEnv, &config.PollInterval, 5, 3600},
		{RuntimeWorkLeaseSecondsEnv, &config.WorkLease, 30, 900},
		{RuntimeHeartbeatSecondsEnv, &config.HeartbeatInterval, 5, 300},
		{RuntimeReadinessSecondsEnv, &config.ReadinessMaxAge, 30, 900},
		{RuntimeMinimumBackoffEnv, &config.MinimumBackoff, 1, 300},
		{RuntimeMaximumBackoffEnv, &config.MaximumBackoff, 1, 3600},
	}
	for _, value := range values {
		seconds, err := canonicalRuntimeInteger(lookup, value.name, value.minimum, value.maximum)
		if err != nil {
			return RuntimeConfig{}, err
		}
		*value.destination = time.Duration(seconds) * time.Second
	}
	if config.Validate() != nil {
		return RuntimeConfig{}, ErrInvalid
	}
	return config, nil
}

func canonicalRuntimeInteger(lookup func(string) (string, bool), name string, minimum, maximum int64) (int64, error) {
	value, found := lookup(name)
	if !found || value == "" || strings.TrimSpace(value) != value {
		return 0, errors.New(name + " is required")
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < minimum || parsed > maximum || strconv.FormatInt(parsed, 10) != value {
		return 0, errors.New(name + " must be a canonical bounded integer")
	}
	return parsed, nil
}

func (c RuntimeConfig) Validate() error {
	if !c.Enabled {
		if c.Profiles.Traefik != nil || c.Profiles.CertManager != nil || len(c.Profiles.ExternalDNS) != 0 || c.PollInterval != 0 ||
			c.WorkLease != 0 || c.HeartbeatInterval != 0 || c.ReadinessMaxAge != 0 || c.MinimumBackoff != 0 || c.MaximumBackoff != 0 {
			return ErrInvalid
		}
		return nil
	}
	targets := 0
	if c.Profiles.Traefik != nil {
		if c.Profiles.Traefik.Validate() != nil {
			return ErrInvalid
		}
		targets++
	}
	if c.Profiles.CertManager != nil {
		if c.Profiles.CertManager.Validate() != nil {
			return ErrInvalid
		}
		targets++
	}
	if len(c.Profiles.ExternalDNS) > MaximumExternalDNSProfiles {
		return ErrInvalid
	}
	for index, profile := range c.Profiles.ExternalDNS {
		if profile.Validate() != nil || index > 0 && c.Profiles.ExternalDNS[index-1].IntegrationID >= profile.IntegrationID {
			return ErrInvalid
		}
		targets++
	}
	if targets < 1 || targets > MaximumTargets || c.PollInterval < 5*time.Second || c.PollInterval > time.Hour ||
		c.WorkLease < 30*time.Second || c.WorkLease > 15*time.Minute || c.HeartbeatInterval < 5*time.Second ||
		c.HeartbeatInterval*2 >= c.WorkLease || c.ReadinessMaxAge < c.PollInterval*2 || c.ReadinessMaxAge < c.HeartbeatInterval*2 ||
		c.ReadinessMaxAge > 15*time.Minute || c.MinimumBackoff < time.Second || c.MaximumBackoff < c.MinimumBackoff || c.MaximumBackoff > time.Hour {
		return ErrInvalid
	}
	return nil
}

func (c RuntimeConfig) TargetCount() int {
	if c.Validate() != nil || !c.Enabled {
		return 0
	}
	count := len(c.Profiles.ExternalDNS)
	if c.Profiles.Traefik != nil {
		count++
	}
	if c.Profiles.CertManager != nil {
		count++
	}
	return count
}

func (c RuntimeConfig) Digest() (string, error) {
	if c.Validate() != nil || !c.Enabled {
		return "", ErrInvalid
	}
	encoded, err := json.Marshal(struct {
		Contract          string          `json:"contract"`
		Profiles          RuntimeProfiles `json:"profiles"`
		PollSeconds       int64           `json:"pollSeconds"`
		WorkLeaseSeconds  int64           `json:"workLeaseSeconds"`
		HeartbeatSeconds  int64           `json:"heartbeatSeconds"`
		ReadinessSeconds  int64           `json:"readinessSeconds"`
		MinimumBackoffSec int64           `json:"minimumBackoffSeconds"`
		MaximumBackoffSec int64           `json:"maximumBackoffSeconds"`
	}{RuntimeContract, cloneRuntimeProfiles(c.Profiles), int64(c.PollInterval.Seconds()), int64(c.WorkLease.Seconds()),
		int64(c.HeartbeatInterval.Seconds()), int64(c.ReadinessMaxAge.Seconds()), int64(c.MinimumBackoff.Seconds()), int64(c.MaximumBackoff.Seconds())})
	if err != nil {
		return "", ErrInvalid
	}
	return digestBytes(encoded), nil
}

type TargetProfile struct {
	Kind        Kind
	Traefik     *TraefikProfile
	CertManager *CertManagerProfile
	ExternalDNS *ExternalDNSProfile
}

func (p TargetProfile) Desired(configDigest string) (DesiredTarget, error) {
	if !validDigest(configDigest) {
		return DesiredTarget{}, ErrInvalid
	}
	var target DesiredTarget
	switch p.Kind {
	case KindTraefik:
		if p.Traefik == nil || p.CertManager != nil || p.ExternalDNS != nil || p.Traefik.Validate() != nil {
			return DesiredTarget{}, ErrInvalid
		}
		digest, err := p.Traefik.Digest()
		if err != nil {
			return DesiredTarget{}, err
		}
		target = DesiredTarget{Key: "traefik", Kind: KindTraefik, Mode: p.Traefik.Mode, Namespace: p.Traefik.Namespace,
			ProfileConfigMap: p.Traefik.ProfileConfigMap, Revision: p.Traefik.Revision, DesiredDigest: digest, RuntimeConfigDigest: configDigest}
	case KindCertManager:
		if p.CertManager == nil || p.Traefik != nil || p.ExternalDNS != nil || p.CertManager.Validate() != nil {
			return DesiredTarget{}, ErrInvalid
		}
		digest, err := p.CertManager.Digest()
		if err != nil {
			return DesiredTarget{}, err
		}
		target = DesiredTarget{Key: "cert-manager", Kind: KindCertManager, Mode: p.CertManager.Mode, Namespace: p.CertManager.Namespace,
			ProfileConfigMap: p.CertManager.ProfileConfigMap, Revision: p.CertManager.Revision, DesiredDigest: digest, RuntimeConfigDigest: configDigest}
	case KindExternalDNS:
		if p.ExternalDNS == nil || p.Traefik != nil || p.CertManager != nil || p.ExternalDNS.Validate() != nil {
			return DesiredTarget{}, ErrInvalid
		}
		digest, err := p.ExternalDNS.Digest()
		if err != nil {
			return DesiredTarget{}, err
		}
		target = DesiredTarget{Key: "external-dns/" + p.ExternalDNS.IntegrationID, Kind: KindExternalDNS, Mode: p.ExternalDNS.Mode,
			IntegrationID: p.ExternalDNS.IntegrationID, Namespace: p.ExternalDNS.Namespace, ProfileConfigMap: p.ExternalDNS.ProfileConfigMap,
			Revision: p.ExternalDNS.Revision, ExternalTXTOwnerID: p.ExternalDNS.TXTOwnerID, ExternalPolicy: p.ExternalDNS.Policy,
			ExternalDomains: strings.Join(p.ExternalDNS.DomainFilters, ","), ExternalProviderKind: p.ExternalDNS.ProviderKind,
			ExternalCredentialSecretRef: p.ExternalDNS.CredentialSecretRef, ExternalProviderConfigRef: p.ExternalDNS.ProviderConfigRef,
			ExternalEgressConfigRef: p.ExternalDNS.EgressConfigRef, DesiredDigest: digest, RuntimeConfigDigest: configDigest}
	default:
		return DesiredTarget{}, ErrInvalid
	}
	return target, target.Validate()
}

func (c RuntimeConfig) TargetProfiles() ([]TargetProfile, error) {
	digest, err := c.Digest()
	if err != nil {
		return nil, err
	}
	profiles := make([]TargetProfile, 0, c.TargetCount())
	if c.Profiles.Traefik != nil {
		value := *c.Profiles.Traefik
		value.CRDs = slices.Clone(value.CRDs)
		profiles = append(profiles, TargetProfile{Kind: KindTraefik, Traefik: &value})
	}
	if c.Profiles.CertManager != nil {
		value := *c.Profiles.CertManager
		value.Deployments, value.CRDs = slices.Clone(value.Deployments), slices.Clone(value.CRDs)
		profiles = append(profiles, TargetProfile{Kind: KindCertManager, CertManager: &value})
	}
	for _, original := range c.Profiles.ExternalDNS {
		value := original
		value.DomainFilters = slices.Clone(original.DomainFilters)
		profiles = append(profiles, TargetProfile{Kind: KindExternalDNS, ExternalDNS: &value})
	}
	slices.SortFunc(profiles, func(left, right TargetProfile) int {
		leftTarget, _ := left.Desired(digest)
		rightTarget, _ := right.Desired(digest)
		return strings.Compare(leftTarget.Key, rightTarget.Key)
	})
	return profiles, nil
}

func (c RuntimeConfig) DesiredTargets() ([]DesiredTarget, error) {
	digest, err := c.Digest()
	if err != nil {
		return nil, err
	}
	profiles, err := c.TargetProfiles()
	if err != nil {
		return nil, err
	}
	targets := make([]DesiredTarget, len(profiles))
	for index, profile := range profiles {
		if targets[index], err = profile.Desired(digest); err != nil {
			return nil, err
		}
	}
	return targets, nil
}

func (c RuntimeConfig) ProfileForTarget(target DesiredTarget) (TargetProfile, bool) {
	profiles, err := c.TargetProfiles()
	if err != nil || target.Validate() != nil {
		return TargetProfile{}, false
	}
	digest, _ := c.Digest()
	index, found := slices.BinarySearchFunc(profiles, target.Key, func(profile TargetProfile, key string) int {
		desired, _ := profile.Desired(digest)
		return strings.Compare(desired.Key, key)
	})
	if !found {
		return TargetProfile{}, false
	}
	desired, err := profiles[index].Desired(digest)
	return profiles[index], err == nil && desired == target
}

func cloneRuntimeProfiles(value RuntimeProfiles) RuntimeProfiles {
	result := RuntimeProfiles{ExternalDNS: make([]ExternalDNSProfile, len(value.ExternalDNS))}
	if value.Traefik != nil {
		copy := *value.Traefik
		copy.CRDs = slices.Clone(value.Traefik.CRDs)
		if value.Traefik.SSLIP != nil {
			sslip := *value.Traefik.SSLIP
			copy.SSLIP = &sslip
		}
		result.Traefik = &copy
	}
	if value.CertManager != nil {
		copy := *value.CertManager
		copy.Deployments, copy.CRDs = slices.Clone(value.CertManager.Deployments), slices.Clone(value.CertManager.CRDs)
		result.CertManager = &copy
	}
	for index, profile := range value.ExternalDNS {
		result.ExternalDNS[index] = profile
		result.ExternalDNS[index].DomainFilters = slices.Clone(profile.DomainFilters)
	}
	return result
}
