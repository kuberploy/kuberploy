package externaldns

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	OperationalEnabledEnv        = "KUBERPLOY_EXTERNAL_DNS_OPERATIONAL_ENABLED"
	OperationalBindingIDEnv      = "KUBERPLOY_EXTERNAL_DNS_PLATFORM_BINDING_ID"
	OperationalNamespaceEnv      = "KUBERPLOY_EXTERNAL_DNS_NAMESPACE"
	OperationalVersionEnv        = "KUBERPLOY_EXTERNAL_DNS_VERSION"
	OperationalImageEnv          = "KUBERPLOY_EXTERNAL_DNS_IMAGE"
	OperationalServiceAccountEnv = "KUBERPLOY_EXTERNAL_DNS_SERVICE_ACCOUNT"
	OperationalPollSecondsEnv    = "KUBERPLOY_EXTERNAL_DNS_POLL_SECONDS"
)

type OperationalConfig struct {
	Enabled      bool
	BindingID    string
	Template     ManagedRuntimeTemplate
	PollInterval time.Duration
}

func (c OperationalConfig) Validate() error {
	if !c.Enabled {
		if c.BindingID != "" || c.Template != (ManagedRuntimeTemplate{}) || c.PollInterval != 0 {
			return ErrRuntimeUnavailable
		}
		return nil
	}
	if !uuidRE.MatchString(c.BindingID) || c.Template.Validate() != nil || c.PollInterval < time.Second || c.PollInterval > time.Minute {
		return ErrRuntimeUnavailable
	}
	return nil
}
func OperationalConfigFromEnvironment() (OperationalConfig, error) {
	return OperationalConfigFromLookup(os.LookupEnv)
}
func OperationalConfigFromLookup(lookup func(string) (string, bool)) (OperationalConfig, error) {
	if lookup == nil {
		return OperationalConfig{}, ErrRuntimeUnavailable
	}
	enabled, _ := lookup(OperationalEnabledEnv)
	if enabled == "" || enabled == "false" {
		return OperationalConfig{}, nil
	}
	if enabled != "true" {
		return OperationalConfig{}, errors.New(OperationalEnabledEnv + " must be exactly true or false")
	}
	required := func(name string) (string, error) {
		v, ok := lookup(name)
		if !ok || v == "" || strings.TrimSpace(v) != v {
			return "", errors.New(name + " is required")
		}
		return v, nil
	}
	values := make([]string, 5)
	names := []string{OperationalBindingIDEnv, OperationalNamespaceEnv, OperationalVersionEnv, OperationalImageEnv, OperationalServiceAccountEnv}
	for i, name := range names {
		v, err := required(name)
		if err != nil {
			return OperationalConfig{}, err
		}
		values[i] = v
	}
	pollRaw, err := required(OperationalPollSecondsEnv)
	if err != nil {
		return OperationalConfig{}, err
	}
	poll, err := strconv.ParseInt(pollRaw, 10, 64)
	if err != nil || poll < 1 || poll > 60 || strconv.FormatInt(poll, 10) != pollRaw {
		return OperationalConfig{}, errors.New(OperationalPollSecondsEnv + " must be a canonical integer from 1 to 60")
	}
	c := OperationalConfig{Enabled: true, BindingID: values[0], Template: ManagedRuntimeTemplate{Namespace: values[1], Version: values[2], Image: values[3], ServiceAccount: values[4]}, PollInterval: time.Duration(poll) * time.Second}
	return c, c.Validate()
}
