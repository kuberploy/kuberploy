package imagepull

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	RuntimeEnabledEnv           = "KUBERPLOY_RUNTIME_REGISTRY_PULLS_ENABLED"
	RuntimeNamespacesEnv        = "KUBERPLOY_RUNTIME_REGISTRY_PULL_NAMESPACES"
	RuntimeProfilesEnv          = "KUBERPLOY_RUNTIME_REGISTRY_PULL_PROFILES"
	RuntimePollSecondsEnv       = "KUBERPLOY_RUNTIME_REGISTRY_PULL_POLL_SECONDS"
	RuntimeWorkLeaseSecondsEnv  = "KUBERPLOY_RUNTIME_REGISTRY_PULL_WORK_LEASE_SECONDS"
	RuntimeHeartbeatSecondsEnv  = "KUBERPLOY_RUNTIME_REGISTRY_PULL_HEARTBEAT_SECONDS"
	RuntimeReadinessSecondsEnv  = "KUBERPLOY_RUNTIME_REGISTRY_PULL_READINESS_SECONDS"
	RuntimeMinBackoffSecondsEnv = "KUBERPLOY_RUNTIME_REGISTRY_PULL_MINIMUM_BACKOFF_SECONDS"
	RuntimeMaxBackoffSecondsEnv = "KUBERPLOY_RUNTIME_REGISTRY_PULL_MAXIMUM_BACKOFF_SECONDS"
)

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
		return RuntimeConfig{}, ErrInvalid
	}
	namespaceValue, ok := exactEnvironment(lookup, RuntimeNamespacesEnv)
	if !ok {
		return RuntimeConfig{}, ErrInvalid
	}
	namespaces := strings.Split(namespaceValue, ",")
	slices.Sort(namespaces)
	namespaces = slices.Compact(namespaces)
	if len(namespaces) == 0 || slices.Contains(namespaces, "") {
		return RuntimeConfig{}, ErrInvalid
	}
	profileValue, ok := exactEnvironment(lookup, RuntimeProfilesEnv)
	if !ok || len(profileValue) > 64<<10 {
		return RuntimeConfig{}, ErrInvalid
	}
	profiles, err := decodeProfiles([]byte(profileValue))
	if err != nil {
		return RuntimeConfig{}, ErrInvalid
	}
	slices.SortFunc(profiles, compareProfiles)
	profiles = slices.Compact(profiles)
	config := DefaultRuntimeConfig()
	config.Enabled = true
	config.Namespaces = namespaces
	config.Profiles = profiles
	for name, target := range map[string]*time.Duration{
		RuntimePollSecondsEnv:       &config.PollInterval,
		RuntimeWorkLeaseSecondsEnv:  &config.WorkLease,
		RuntimeHeartbeatSecondsEnv:  &config.HeartbeatInterval,
		RuntimeReadinessSecondsEnv:  &config.ReadinessMaxAge,
		RuntimeMinBackoffSecondsEnv: &config.MinimumBackoff,
		RuntimeMaxBackoffSecondsEnv: &config.MaximumBackoff,
	} {
		value, configured := lookup(name)
		if !configured {
			continue
		}
		if value == "" || strings.TrimSpace(value) != value {
			return RuntimeConfig{}, ErrInvalid
		}
		seconds, parseErr := strconv.ParseInt(value, 10, 32)
		if parseErr != nil || seconds < 1 || strconv.FormatInt(seconds, 10) != value {
			return RuntimeConfig{}, ErrInvalid
		}
		*target = time.Duration(seconds) * time.Second
	}
	if config.Validate() != nil {
		return RuntimeConfig{}, ErrInvalid
	}
	return config, nil
}

func exactEnvironment(lookup func(string) (string, bool), name string) (string, bool) {
	value, present := lookup(name)
	return value, present && value != "" && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func decodeProfiles(raw []byte) ([]Profile, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, ErrInvalid
	}
	profiles := make([]Profile, 0)
	for decoder.More() {
		if len(profiles) == MaximumProfiles {
			return nil, ErrInvalid
		}
		var encoded json.RawMessage
		if err = decoder.Decode(&encoded); err != nil {
			return nil, ErrInvalid
		}
		profile, parseErr := decodeProfile(encoded)
		if parseErr != nil {
			return nil, ErrInvalid
		}
		profiles = append(profiles, profile)
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim(']') {
		return nil, ErrInvalid
	}
	if token, err = decoder.Token(); err != io.EOF || token != nil {
		return nil, ErrInvalid
	}
	if len(profiles) == 0 {
		return nil, ErrInvalid
	}
	return profiles, nil
}

func decodeProfile(raw []byte) (Profile, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return Profile{}, ErrInvalid
	}
	var profile Profile
	seen := make(map[string]struct{}, 7)
	for decoder.More() {
		keyToken, keyErr := decoder.Token()
		key, ok := keyToken.(string)
		if keyErr != nil || !ok {
			return Profile{}, ErrInvalid
		}
		if _, duplicate := seen[key]; duplicate {
			return Profile{}, ErrInvalid
		}
		seen[key] = struct{}{}
		switch key {
		case "name":
			err = decoder.Decode(&profile.Name)
		case "targetId":
			err = decoder.Decode(&profile.TargetID)
		case "registryServer":
			err = decoder.Decode(&profile.RegistryServer)
		case "credentialRef":
			err = decoder.Decode(&profile.CredentialRef)
		case "revision":
			err = decoder.Decode(&profile.Revision)
		case "sourceSecretRef":
			err = decoder.Decode(&profile.SourceSecretRef)
		case "sourceSecretKey":
			err = decoder.Decode(&profile.SourceSecretKey)
		default:
			return Profile{}, ErrInvalid
		}
		if err != nil {
			return Profile{}, ErrInvalid
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return Profile{}, ErrInvalid
	}
	if token, err = decoder.Token(); err != io.EOF || token != nil || len(seen) != 7 || profile.Validate() != nil {
		return Profile{}, ErrInvalid
	}
	return profile, nil
}
