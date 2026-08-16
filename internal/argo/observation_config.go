package argo

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	ObservationEnabledEnv     = "KUBERPLOY_ARGO_OBSERVATION_ENABLED"
	ObservationNamespaceEnv   = "KUBERPLOY_ARGO_OBSERVATION_NAMESPACE"
	ObservationPollSecondsEnv = "KUBERPLOY_ARGO_OBSERVATION_POLL_INTERVAL_SECONDS"
)

type ObservationRuntimeConfig struct {
	Enabled      bool
	Namespace    string
	PollInterval time.Duration
}

func ObservationRuntimeConfigFromEnvironment() (ObservationRuntimeConfig, error) {
	return ObservationRuntimeConfigFromLookup(os.LookupEnv)
}

func ObservationRuntimeConfigFromLookup(lookup func(string) (string, bool)) (ObservationRuntimeConfig, error) {
	if lookup == nil {
		return ObservationRuntimeConfig{}, ErrInvalid
	}
	enabled, present := lookup(ObservationEnabledEnv)
	_, namespacePresent := lookup(ObservationNamespaceEnv)
	_, pollPresent := lookup(ObservationPollSecondsEnv)
	if !present || enabled == "" {
		if namespacePresent || pollPresent {
			return ObservationRuntimeConfig{}, errors.New(ObservationEnabledEnv + " must be explicit when observation settings are present")
		}
		return ObservationRuntimeConfig{}, nil
	}
	if enabled == "false" {
		if namespacePresent {
			return ObservationRuntimeConfig{}, errors.New(ObservationNamespaceEnv + " must be omitted when observation is disabled")
		}
		if !pollPresent {
			return ObservationRuntimeConfig{}, nil
		}
		if _, err := parseObservationPoll(exactObservationConfig(lookup, ObservationPollSecondsEnv)); err != nil {
			return ObservationRuntimeConfig{}, err
		}
		return ObservationRuntimeConfig{}, nil
	}
	if enabled != "true" {
		return ObservationRuntimeConfig{}, errors.New(ObservationEnabledEnv + " must be exactly true or false")
	}
	namespaceEnv := ObservationNamespaceEnv
	if _, found := lookup(namespaceEnv); !found {
		// Earlier charts used the protected desired-state namespace variable
		// for observation too. Accept that exact legacy shape only when the new
		// dedicated variable is absent so a rolling chart/binary upgrade remains
		// live without allowing an empty or malformed new value to fall back.
		namespaceEnv = ProductionNamespaceEnv
	}
	namespace := exactObservationConfig(lookup, namespaceEnv)
	if !kubeRE.MatchString(namespace) {
		return ObservationRuntimeConfig{}, errors.New(namespaceEnv + " must be an exact Kubernetes namespace")
	}
	pollInterval, err := parseObservationPoll(exactObservationConfig(lookup, ObservationPollSecondsEnv))
	if err != nil {
		return ObservationRuntimeConfig{}, err
	}
	return ObservationRuntimeConfig{Enabled: true, Namespace: namespace, PollInterval: pollInterval}, nil
}

func parseObservationPoll(raw string) (time.Duration, error) {
	pollSeconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || pollSeconds < 15 || pollSeconds > int64(maximumObservationPoll/time.Second) || strconv.FormatInt(pollSeconds, 10) != raw {
		return 0, errors.New(ObservationPollSecondsEnv + " must be a canonical integer from 15 to 900")
	}
	return time.Duration(pollSeconds) * time.Second, nil
}

func (c ObservationRuntimeConfig) Validate() error {
	if !c.Enabled {
		if c.Namespace != "" || c.PollInterval != 0 {
			return ErrInvalid
		}
		return nil
	}
	if !kubeRE.MatchString(c.Namespace) || c.PollInterval < 15*time.Second || c.PollInterval > maximumObservationPoll {
		return ErrInvalid
	}
	return nil
}

func exactObservationConfig(lookup func(string) (string, bool), name string) string {
	value, ok := lookup(name)
	if !ok || value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
		return ""
	}
	return value
}
