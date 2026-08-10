// Package automation defines the coarse service-account token policy. It does
// not replace object-level access grants; both boundaries must allow a request.
package automation

import (
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/domain"
)

const MaxTokenTTL = 90 * 24 * time.Hour

var tokenPrefixPattern = regexp.MustCompile(`^kp_sa_[A-Za-z0-9_-]{8}$`)

func ValidRole(role domain.AccessRole) bool {
	switch role {
	case domain.RoleViewer, domain.RoleDeveloper, domain.RoleProjectAdmin:
		return true
	default:
		return false
	}
}

func ValidName(name string) bool {
	if len(name) < 1 || len(name) > 100 || !utf8.ValidString(name) || strings.TrimSpace(name) != name {
		return false
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func ValidTokenPrefix(prefix string) bool { return tokenPrefixPattern.MatchString(prefix) }

// NormalizeScopes validates a closed set and rejects duplicates before
// returning a stable order used by persistence and request fingerprints.
func NormalizeScopes(scopes []domain.AutomationScope) ([]domain.AutomationScope, bool) {
	if len(scopes) == 0 || len(scopes) > 4 {
		return nil, false
	}
	seen := make(map[domain.AutomationScope]struct{}, len(scopes))
	result := make([]domain.AutomationScope, 0, len(scopes))
	for _, scope := range scopes {
		switch scope {
		case domain.AutomationScopeAppRead, domain.AutomationScopeAppEdit, domain.AutomationScopeBuildCreate, domain.AutomationScopeLogsRead:
		default:
			return nil, false
		}
		if _, duplicate := seen[scope]; duplicate {
			return nil, false
		}
		seen[scope] = struct{}{}
		result = append(result, scope)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, true
}

func Allows(scopes []domain.AutomationScope, required domain.AutomationScope) bool {
	for _, scope := range scopes {
		if scope == required {
			return true
		}
	}
	return false
}

func ValidExpiry(now, expires time.Time) bool {
	return expires.After(now.Add(5*time.Minute)) && !expires.After(now.Add(MaxTokenTTL))
}
