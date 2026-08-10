package githubapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	RepositoryProtectionContract = "github-platform-repository-protection-v1"
	maximumProtectionRulePages   = 10
)

// RepositoryProtectionObservation is a credential-free, exact-repository
// provider observation. The digest covers the closed branch-protection profile
// and every active ruleset rule returned for the exact branch.
type RepositoryProtectionObservation struct {
	InstallationID int64
	RepositoryID   int64
	Ref            string
	Head           string
	WriterAppID    int64
	PolicyDigest   string
	ObservedAt     time.Time
}

type apiEnabledSetting struct {
	Enabled bool `json:"enabled"`
}

func (s *apiEnabledSetting) UnmarshalJSON(data []byte) error {
	if err := rejectUnknownObjectFields(data, map[string]struct{}{"url": {}, "enabled": {}}); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields["enabled"] == nil || string(fields["enabled"]) == "null" {
		return ErrProviderResponse
	}
	type alias apiEnabledSetting
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*s = apiEnabledSetting(decoded)
	return nil
}

type apiProtectionApp struct {
	ID int64 `json:"id"`
}

type apiPushRestrictions struct {
	Users []json.RawMessage  `json:"users"`
	Teams []json.RawMessage  `json:"teams"`
	Apps  []apiProtectionApp `json:"apps"`
}

func (r *apiPushRestrictions) UnmarshalJSON(data []byte) error {
	type alias apiPushRestrictions
	if err := rejectUnknownObjectFields(data, map[string]struct{}{
		"url": {}, "users_url": {}, "teams_url": {}, "apps_url": {}, "users": {}, "teams": {}, "apps": {},
	}); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, required := range []string{"users", "teams", "apps"} {
		if fields[required] == nil || string(fields[required]) == "null" {
			return ErrProviderResponse
		}
	}
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = apiPushRestrictions(decoded)
	return nil
}

type apiBranchProtection struct {
	RequiredStatusChecks           json.RawMessage      `json:"required_status_checks"`
	RequiredPullRequestReviews     json.RawMessage      `json:"required_pull_request_reviews"`
	RequiredDeployments            json.RawMessage      `json:"required_deployments"`
	RequiredSignatures             *apiEnabledSetting   `json:"required_signatures"`
	EnforceAdmins                  *apiEnabledSetting   `json:"enforce_admins"`
	Restrictions                   *apiPushRestrictions `json:"restrictions"`
	RequiredLinearHistory          *apiEnabledSetting   `json:"required_linear_history"`
	AllowForcePushes               *apiEnabledSetting   `json:"allow_force_pushes"`
	AllowDeletions                 *apiEnabledSetting   `json:"allow_deletions"`
	BlockCreations                 *apiEnabledSetting   `json:"block_creations"`
	RequiredConversationResolution *apiEnabledSetting   `json:"required_conversation_resolution"`
	LockBranch                     *apiEnabledSetting   `json:"lock_branch"`
	AllowForkSyncing               *apiEnabledSetting   `json:"allow_fork_syncing"`
}

func (p *apiBranchProtection) UnmarshalJSON(data []byte) error {
	type alias apiBranchProtection
	if err := rejectUnknownObjectFields(data, map[string]struct{}{
		"url": {}, "required_status_checks": {}, "required_pull_request_reviews": {}, "required_deployments": {},
		"required_signatures": {}, "enforce_admins": {}, "restrictions": {}, "required_linear_history": {},
		"allow_force_pushes": {}, "allow_deletions": {}, "block_creations": {}, "required_conversation_resolution": {},
		"lock_branch": {}, "allow_fork_syncing": {},
	}); err != nil {
		return err
	}
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = apiBranchProtection(decoded)
	return nil
}

func rejectUnknownObjectFields(data []byte, allowed map[string]struct{}) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for field := range fields {
		if _, ok := allowed[field]; !ok {
			return ErrProviderResponse
		}
	}
	return nil
}

type apiActiveBranchRule struct {
	Type              string `json:"type"`
	RulesetSourceType string `json:"ruleset_source_type"`
	RulesetSource     string `json:"ruleset_source"`
	RulesetID         int64  `json:"ruleset_id"`
}

type canonicalProtectionRule struct {
	RulesetID  int64  `json:"rulesetId"`
	Source     string `json:"source"`
	SourceType string `json:"sourceType"`
	Type       string `json:"type"`
}

// ObserveRepositoryProtection re-resolves the exact provider ref, then reads
// only the exact branch protection and active-branch-rules endpoints. It does
// not accept URLs, selectors, redirects, or a broad repository token.
func (c *Client) ObserveRepositoryProtection(ctx context.Context, token InstallationToken, repository RepositoryIdentity, fullRef, expectedHead string, writerAppID int64) (RepositoryProtectionObservation, error) {
	now := c.clock.Now().UTC()
	if token.credential.empty() || !now.Before(token.ExpiresAt) || token.installationID <= 0 || !token.authorizes(repository.ID) ||
		repository.validate() != nil || writerAppID <= 0 || !builderObjectIDPattern.MatchString(expectedHead) ||
		token.permissions["metadata"] != PermissionRead || !permissionAllows(token.permissions["contents"], PermissionRead) ||
		token.permissions["administration"] != PermissionRead {
		return RepositoryProtectionObservation{}, ErrScopeMismatch
	}
	kind, branch, ok := splitFullRef(fullRef)
	if !ok || kind != "heads" {
		return RepositoryProtectionObservation{}, ErrInvalidTokenRequest
	}
	resolved, err := c.ResolveRemoteRef(ctx, token, repository, fullRef)
	if err != nil {
		return RepositoryProtectionObservation{}, err
	}
	if resolved.Ref != fullRef || resolved.CommitSHA != expectedHead {
		return RepositoryProtectionObservation{}, ErrRepositoryProtection
	}

	var protection apiBranchProtection
	if err = c.doJSON(ctx, http.MethodGet, token.credential,
		[]string{"repos", repository.OwnerLogin, repository.Name, "branches", branch, "protection"}, nil, nil, http.StatusOK, &protection); err != nil {
		return RepositoryProtectionObservation{}, err
	}
	if err = validateBranchProtection(protection, writerAppID); err != nil {
		return RepositoryProtectionObservation{}, err
	}
	rules, err := c.activeBranchRules(ctx, token.credential, repository, branch)
	if err != nil {
		return RepositoryProtectionObservation{}, err
	}

	canonical := struct {
		Contract              string                    `json:"contract"`
		InstallationID        int64                     `json:"installationId"`
		RepositoryID          int64                     `json:"repositoryId"`
		OwnerID               int64                     `json:"ownerId"`
		OwnerLogin            string                    `json:"ownerLogin"`
		RepositoryName        string                    `json:"repositoryName"`
		Ref                   string                    `json:"ref"`
		WriterAppID           int64                     `json:"writerAppId"`
		EnforceAdmins         bool                      `json:"enforceAdmins"`
		AllowForcePushes      bool                      `json:"allowForcePushes"`
		AllowDeletions        bool                      `json:"allowDeletions"`
		RequiredLinearHistory bool                      `json:"requiredLinearHistory"`
		BlockCreations        bool                      `json:"blockCreations"`
		Rules                 []canonicalProtectionRule `json:"rules"`
	}{
		Contract: RepositoryProtectionContract, InstallationID: token.installationID, RepositoryID: repository.ID,
		OwnerID: repository.OwnerID, OwnerLogin: strings.ToLower(repository.OwnerLogin), RepositoryName: repository.Name,
		Ref: fullRef, WriterAppID: writerAppID, EnforceAdmins: protection.EnforceAdmins.Enabled,
		AllowForcePushes: protection.AllowForcePushes.Enabled, AllowDeletions: protection.AllowDeletions.Enabled,
		RequiredLinearHistory: enabled(protection.RequiredLinearHistory), BlockCreations: enabled(protection.BlockCreations), Rules: rules,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return RepositoryProtectionObservation{}, ErrProviderResponse
	}
	digest := sha256.Sum256(encoded)
	observedAt := c.clock.Now().UTC()
	if observedAt.Before(now) {
		return RepositoryProtectionObservation{}, ErrProviderResponse
	}
	return RepositoryProtectionObservation{
		InstallationID: token.installationID, RepositoryID: repository.ID, Ref: fullRef, Head: expectedHead,
		WriterAppID: writerAppID, PolicyDigest: "sha256:" + hex.EncodeToString(digest[:]), ObservedAt: observedAt,
	}, nil
}

func validateBranchProtection(protection apiBranchProtection, writerAppID int64) error {
	if writerAppID <= 0 || protection.EnforceAdmins == nil || !protection.EnforceAdmins.Enabled || protection.Restrictions == nil ||
		len(protection.Restrictions.Users) != 0 || len(protection.Restrictions.Teams) != 0 || len(protection.Restrictions.Apps) != 1 ||
		protection.Restrictions.Apps[0].ID != writerAppID || protection.AllowForcePushes == nil || protection.AllowForcePushes.Enabled ||
		protection.AllowDeletions == nil || protection.AllowDeletions.Enabled || protection.LockBranch == nil || protection.LockBranch.Enabled ||
		enabled(protection.AllowForkSyncing) || !nullJSON(protection.RequiredStatusChecks) || !nullJSON(protection.RequiredPullRequestReviews) ||
		!nullJSON(protection.RequiredDeployments) || enabled(protection.RequiredSignatures) {
		return ErrRepositoryProtection
	}
	return nil
}

func enabled(setting *apiEnabledSetting) bool { return setting != nil && setting.Enabled }

func nullJSON(value json.RawMessage) bool {
	return len(value) == 0 || string(value) == "null"
}

func (c *Client) activeBranchRules(ctx context.Context, token Credential, repository RepositoryIdentity, branch string) ([]canonicalProtectionRule, error) {
	rules := make([]canonicalProtectionRule, 0)
	seen := make(map[string]struct{})
	for page := 1; page <= maximumProtectionRulePages; page++ {
		query := url.Values{"page": {strconv.Itoa(page)}, "per_page": {strconv.Itoa(providerPageSize)}}
		var response []apiActiveBranchRule
		if err := c.doJSON(ctx, http.MethodGet, token,
			[]string{"repos", repository.OwnerLogin, repository.Name, "rules", "branches", branch}, query, nil, http.StatusOK, &response); err != nil {
			return nil, err
		}
		if len(response) > providerPageSize {
			return nil, ErrProviderResponse
		}
		for _, rule := range response {
			if !writerCompatibleRule(rule.Type) || rule.RulesetID <= 0 || !validProtectionSource(rule.RulesetSource) ||
				(rule.RulesetSourceType != "Repository" && rule.RulesetSourceType != "Organization" && rule.RulesetSourceType != "Enterprise") {
				return nil, ErrRepositoryProtection
			}
			key := strconv.FormatInt(rule.RulesetID, 10) + "\x00" + rule.Type + "\x00" + rule.RulesetSourceType + "\x00" + rule.RulesetSource
			if _, duplicate := seen[key]; duplicate {
				return nil, ErrProviderResponse
			}
			seen[key] = struct{}{}
			rules = append(rules, canonicalProtectionRule{RulesetID: rule.RulesetID, Source: rule.RulesetSource, SourceType: rule.RulesetSourceType, Type: rule.Type})
		}
		if len(response) < providerPageSize {
			break
		}
		if page == maximumProtectionRulePages {
			return nil, ErrProviderResponse
		}
	}
	slices.SortFunc(rules, func(left, right canonicalProtectionRule) int {
		if left.RulesetID != right.RulesetID {
			if left.RulesetID < right.RulesetID {
				return -1
			}
			return 1
		}
		if value := strings.Compare(left.Type, right.Type); value != 0 {
			return value
		}
		if value := strings.Compare(left.SourceType, right.SourceType); value != 0 {
			return value
		}
		return strings.Compare(left.Source, right.Source)
	})
	return rules, nil
}

func writerCompatibleRule(ruleType string) bool {
	switch ruleType {
	case "deletion", "non_fast_forward", "required_linear_history":
		return true
	default:
		return false
	}
}

func validProtectionSource(value string) bool {
	if len(value) < 1 || len(value) > 256 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}
