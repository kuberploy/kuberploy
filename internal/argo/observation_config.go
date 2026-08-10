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
	ObservationNamespaceEnv   = "KUBERPLOY_ARGO_NAMESPACE"
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
	if !present || enabled == "" || enabled == "false" {
		return ObservationRuntimeConfig{}, nil
	}
	if enabled != "true" {
		return ObservationRuntimeConfig{}, errors.New(ObservationEnabledEnv + " must be exactly true or false")
	}
	namespace := exactObservationConfig(lookup, ObservationNamespaceEnv)
	if !kubeRE.MatchString(namespace) {
		return ObservationRuntimeConfig{}, errors.New(ObservationNamespaceEnv + " must be an exact Kubernetes namespace")
	}
	pollValue := exactObservationConfig(lookup, ObservationPollSecondsEnv)
	pollSeconds, err := strconv.ParseInt(pollValue, 10, 64)
	if err != nil || pollSeconds < 15 || pollSeconds > int64(maximumObservationPoll/time.Second) || strconv.FormatInt(pollSeconds, 10) != pollValue {
		return ObservationRuntimeConfig{}, errors.New(ObservationPollSecondsEnv + " must be a canonical integer from 15 to 900")
	}
	return ObservationRuntimeConfig{Enabled: true, Namespace: namespace, PollInterval: time.Duration(pollSeconds) * time.Second}, nil
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
