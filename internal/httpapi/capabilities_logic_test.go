package httpapi

import "testing"

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
