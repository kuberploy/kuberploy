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
		{ObservationEnabledEnv: "true", ObservationNamespaceEnv: "", ProductionNamespaceEnv: "legacy-argocd", ObservationPollSecondsEnv: "30"},
		{ObservationPollSecondsEnv: "30"},
		{ObservationEnabledEnv: "", ObservationPollSecondsEnv: "30"},
		{ObservationEnabledEnv: "", ObservationNamespaceEnv: "argocd"},
		{ObservationEnabledEnv: "false", ObservationNamespaceEnv: "argocd"},
		{ObservationEnabledEnv: "false", ObservationPollSecondsEnv: ""},
		{ObservationEnabledEnv: "false", ObservationPollSecondsEnv: "014"},
		{ObservationEnabledEnv: "false", ObservationPollSecondsEnv: "901"},
	}
	for index, values := range invalid {
		if _, err = ObservationRuntimeConfigFromLookup(lookup(values)); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("invalid config %d accepted: %#v err=%v", index, values, err)
		}
	}
	for _, poll := range []string{"30", "45", "900"} {
		legacyDisabled, legacyErr := ObservationRuntimeConfigFromLookup(lookup(map[string]string{
			ObservationEnabledEnv: "false", ObservationPollSecondsEnv: poll,
		}))
		if legacyErr != nil || legacyDisabled.Enabled || legacyDisabled.Validate() != nil {
			t.Fatalf("legacy disabled observation poll=%s config=%#v err=%v", poll, legacyDisabled, legacyErr)
		}
	}
	legacy, err := ObservationRuntimeConfigFromLookup(lookup(map[string]string{
		ObservationEnabledEnv: "true", ProductionNamespaceEnv: "legacy-argocd", ObservationPollSecondsEnv: "30",
	}))
	if err != nil || !legacy.Enabled || legacy.Namespace != "legacy-argocd" {
		t.Fatalf("legacy observation config=%#v err=%v", legacy, err)
	}
}
