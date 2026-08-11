package builds

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
)

func TestValidateStoredAttemptHistoryAcceptsLegacyProtocolSummary(t *testing.T) {
	now := time.Now().UTC()
	attempt := BuildAttempt{
		ID:                "11111111-1111-4111-8111-111111111111",
		DefinitionID:      "22222222-2222-4222-8222-222222222222",
		ProjectID:         "33333333-3333-4333-8333-333333333333",
		ServiceID:         "44444444-4444-4444-8444-444444444444",
		CommitSHA:         strings.Repeat("a", 40),
		GitRef:            "refs/heads/main",
		Generation:        7,
		State:             AttemptFailed,
		ExecutionAttempts: 1,
		MaxAttempts:       3,
		FailureCode:       "build-failed",
		CompletedAt:       &now,
		CreatedAt:         now.Add(-time.Minute),
		UpdatedAt:         now,
		// PlanRequest intentionally has no BuildKitImage. History never uses
		// the private execution request, while strict command reads still do.
		PlanRequest: builder.JobPlanRequest{},
	}
	if err := validateStoredAttemptHistory(attempt); err != nil {
		t.Fatalf("legacy history summary rejected: %v", err)
	}
	if err := validateStoredAttempt(attempt); !errors.Is(err, ErrInvalid) {
		t.Fatalf("legacy execution protocol unexpectedly accepted: %v", err)
	}
}

func TestValidateStoredAttemptHistoryRejectsUnsafeSummary(t *testing.T) {
	now := time.Now().UTC()
	attempt := BuildAttempt{
		ID:                "11111111-1111-4111-8111-111111111111",
		DefinitionID:      "22222222-2222-4222-8222-222222222222",
		ProjectID:         "33333333-3333-4333-8333-333333333333",
		ServiceID:         "44444444-4444-4444-8444-444444444444",
		CommitSHA:         strings.Repeat("a", 40),
		GitRef:            "refs/heads/main",
		Generation:        7,
		State:             AttemptState("forged"),
		ExecutionAttempts: 1,
		MaxAttempts:       3,
		CreatedAt:         now.Add(-time.Minute),
		UpdatedAt:         now,
	}
	if err := validateStoredAttemptHistory(attempt); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown history state accepted: %v", err)
	}
}
