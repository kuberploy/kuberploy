package domain

import (
	"strings"
	"testing"
)

func TestEnvironmentDestinationsAreReadableBoundedAndProjectIsolated(t *testing.T) {
	first := Project{ID: "11111111-1111-4111-8111-111111111111", Slug: "payments"}
	second := Project{ID: "22222222-2222-4222-8222-222222222222", Slug: "payments-copy"}
	namespace, argoProject := DeriveEnvironmentDestination(first, "dev")
	if namespace != "kp-payments-dev" || argoProject != "kp-p-11111111111141118111111111111111" {
		t.Fatalf("unexpected short destinations: %q %q", namespace, argoProject)
	}
	_, otherArgoProject := DeriveEnvironmentDestination(second, "dev")
	if argoProject == otherArgoProject {
		t.Fatal("different projects shared an Argo CD boundary")
	}

	longProject := first
	longProject.Slug = strings.Repeat("p", 63)
	longNamespace, _ := DeriveEnvironmentDestination(longProject, strings.Repeat("e", 63))
	if len(longNamespace) != 63 || strings.HasSuffix(longNamespace, "-") {
		t.Fatalf("long namespace is not a DNS label: %q", longNamespace)
	}
	if again, _ := DeriveEnvironmentDestination(longProject, strings.Repeat("e", 63)); again != longNamespace {
		t.Fatal("long namespace derivation is not deterministic")
	}
}
