package automation

import (
	"slices"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func TestNormalizeScopesIsClosedUniqueAndStable(t *testing.T) {
	got, ok := NormalizeScopes([]domain.AutomationScope{domain.AutomationScopeLogsRead, domain.AutomationScopeAppRead})
	if !ok || !slices.Equal(got, []domain.AutomationScope{domain.AutomationScopeAppRead, domain.AutomationScopeLogsRead}) {
		t.Fatalf("normalized=%v ok=%t", got, ok)
	}
	for _, invalid := range [][]domain.AutomationScope{
		nil,
		{domain.AutomationScopeAppRead, domain.AutomationScopeAppRead},
		{"platform.admin"},
		{domain.AutomationScopeAppRead, domain.AutomationScopeAppEdit, domain.AutomationScopeBuildCreate, domain.AutomationScopeLogsRead, "extra"},
	} {
		if _, ok = NormalizeScopes(invalid); ok {
			t.Fatalf("accepted invalid scopes %v", invalid)
		}
	}
}

func TestTokenPolicyRejectsLongLivedAndPrivilegedCredentials(t *testing.T) {
	now := time.Now().UTC()
	if !ValidRole(domain.RoleDeveloper) || ValidRole(domain.RolePlatformAdmin) || ValidRole(domain.RoleOrganizationAdmin) {
		t.Fatal("service-account role boundary is wrong")
	}
	if !ValidExpiry(now, now.Add(30*24*time.Hour)) || ValidExpiry(now, now.Add(91*24*time.Hour)) || ValidExpiry(now, now.Add(time.Minute)) {
		t.Fatal("token expiry boundary is wrong")
	}
	if !ValidTokenPrefix("kp_sa_AbCd012_") || ValidTokenPrefix("kp_sa_too-long-secret") {
		t.Fatal("token prefix validation is wrong")
	}
}

func TestAutomationNameIsBoundedTrimmedAndDisplaySafe(t *testing.T) {
	if !ValidName("Release bot") {
		t.Fatal("valid automation name was rejected")
	}
	for _, invalid := range []string{"", " leading", "trailing ", "line\nbreak", string([]byte{0xff}), string(make([]byte, 101))} {
		if ValidName(invalid) {
			t.Fatalf("invalid automation name %q was accepted", invalid)
		}
	}
}
