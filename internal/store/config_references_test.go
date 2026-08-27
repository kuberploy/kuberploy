package store

import (
	"errors"
	"strings"
	"testing"
)

func TestLocalDraftReferencePlanRequiresExactValidAttestation(t *testing.T) {
	plan := &AppConfigReferencePlan{RuntimeSecretDigest: "sha256:" + strings.Repeat("a", 64)}
	if _, err := NormalizeAppConfigReferencePlan(nil, []*AppConfigReferencePlan{plan}); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("ordinary non-Git normalization accepted references: %v", err)
	}
	accepted, err := NormalizeLocalDraftAppConfigReferencePlan([]*AppConfigReferencePlan{plan})
	if err != nil || accepted != plan {
		t.Fatalf("local draft reference plan=%#v err=%v", accepted, err)
	}
	if _, err = NormalizeLocalDraftAppConfigReferencePlan([]*AppConfigReferencePlan{{RuntimeSecretDigest: "not-a-digest"}}); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("local draft accepted invalid reference plan: %v", err)
	}
}
