package httpapi

import (
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func TestAggregateRollbackCapabilityCoversBothIndependentImplementations(t *testing.T) {
	for name, input := range map[string]struct {
		deployment bool
		helm       bool
		want       bool
	}{
		"neither":         {},
		"deployment only": {deployment: true, want: true},
		"helm only":       {helm: true, want: true},
		"both":            {deployment: true, helm: true, want: true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := aggregateRollbacksConfigured(input.deployment, input.helm); got != input.want {
				t.Fatalf("got=%v want=%v", got, input.want)
			}
		})
	}
}

func TestFilterAutomationActionsDoesNotAdvertiseHumanOnlySecretBindingMutation(t *testing.T) {
	actions := filterAutomationActions([]string{
		"applications:create",
		"secret-bindings:bind",
		"secret-bindings:read",
	}, []domain.AutomationScope{domain.AutomationScopeAppEdit})

	if !hasCapabilityAction(actions, "applications:create") {
		t.Fatalf("app.edit lost supported action: %#v", actions)
	}
	if hasCapabilityAction(actions, "secret-bindings:bind") {
		t.Fatalf("bearer capabilities advertised human-only secret binding mutation: %#v", actions)
	}
	if hasCapabilityAction(actions, "secret-bindings:read") {
		t.Fatalf("app.edit unexpectedly gained app.read action: %#v", actions)
	}
}

func hasCapabilityAction(actions []string, want string) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}
