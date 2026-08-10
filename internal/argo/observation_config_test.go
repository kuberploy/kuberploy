package argo

import (
	"errors"
	"testing"
	"time"
)

func TestObservationRuntimeConfigIsStrictAndDefaultOff(t *testing.T) {
	lookup := func(values map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) {
			value, ok := values[name]
			return value, ok
		}
	}
	disabled, err := ObservationRuntimeConfigFromLookup(lookup(nil))
	if err != nil || disabled.Enabled || disabled.Validate() != nil {
		t.Fatalf("disabled=%#v err=%v", disabled, err)
	}
	configured, err := ObservationRuntimeConfigFromLookup(lookup(map[string]string{
		ObservationEnabledEnv: "true", ObservationNamespaceEnv: "argocd", ObservationPollSecondsEnv: "30",
	}))
	if err != nil || !configured.Enabled || configured.Namespace != "argocd" || configured.PollInterval != 30*time.Second || configured.Validate() != nil {
		t.Fatalf("configured=%#v err=%v", configured, err)
	}
	invalid := []map[string]string{
		{ObservationEnabledEnv: "TRUE", ObservationNamespaceEnv: "argocd", ObservationPollSecondsEnv: "30"},
		{ObservationEnabledEnv: "true", ObservationNamespaceEnv: " argocd", ObservationPollSecondsEnv: "30"},
		{ObservationEnabledEnv: "true", ObservationNamespaceEnv: "argocd", ObservationPollSecondsEnv: "030"},
		{ObservationEnabledEnv: "true", ObservationNamespaceEnv: "argocd", ObservationPollSecondsEnv: "14"},
		{ObservationEnabledEnv: "true", ObservationNamespaceEnv: "argocd", ObservationPollSecondsEnv: "901"},
	}
	for index, values := range invalid {
		if _, err = ObservationRuntimeConfigFromLookup(lookup(values)); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("invalid config %d accepted: %#v err=%v", index, values, err)
		}
	}
}
