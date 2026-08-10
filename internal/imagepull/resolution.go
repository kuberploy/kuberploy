package imagepull

import (
	"context"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/registry"
)

// Reference is the complete safe AppConfig identity. It deliberately omits
// credential references, source Secret coordinates, and destination names.
type Reference struct {
	TargetID        string
	ProfileName     string
	ProfileRevision int64
}

// ExactRegistryPolicyTx loads and validates one durable target/application
// policy under a row lock, returning its canonical registry server.
func ExactRegistryPolicyTx(ctx context.Context, tx pgx.Tx, targetID, applicationID string) (domain.RegistryTarget, domain.ServiceRegistryPolicy, string, error) {
	if tx == nil || !uuidPattern.MatchString(targetID) || !uuidPattern.MatchString(applicationID) {
		return domain.RegistryTarget{}, domain.ServiceRegistryPolicy{}, "", ErrInvalid
	}
	target, policy, err := scanRegistryPullPolicy(tx.QueryRow(ctx, `SELECT
		t.id::text,t.name,t.mode,t.endpoint,t.repository_prefix,t.pull_credential_ref,
		t.push_credential_ref,t.cache_credential_ref,t.created_at,t.updated_at,
		p.registry_target_id::text,p.service_id::text,p.repository,p.keep_last_successful,
		p.minimum_safety_age_seconds,p.cache_keep_generations,p.cache_unused_expiry_seconds,
		p.cache_byte_quota,p.created_at,p.updated_at
		FROM registry_targets t
		JOIN service_registry_policies p ON p.registry_target_id=t.id
		WHERE t.id=$1 AND p.service_id=$2
		FOR SHARE OF t,p`, targetID, applicationID))
	if err != nil {
		return domain.RegistryTarget{}, domain.ServiceRegistryPolicy{}, "", err
	}
	server, serverOK := registryServerForPull(target.Endpoint)
	if registry.ValidateTarget(target) != nil || registry.ValidatePolicy(policy) != nil || !serverOK ||
		target.ID != targetID || policy.RegistryTargetID != targetID || policy.ServiceID != applicationID ||
		!strings.HasPrefix(policy.Repository, target.RepositoryPrefix+"/") || !validRegistryRepository(policy.Repository) {
		return domain.RegistryTarget{}, domain.ServiceRegistryPolicy{}, "", ErrConflict
	}
	return target, policy, server, nil
}

// ResolveReferenceTx derives a locked pull profile from one exact durable
// application/environment/repository snapshot and the immutable operator
// runtime configuration. An unmatched repository is the public-image form.
func ResolveReferenceTx(
	ctx context.Context,
	tx pgx.Tx,
	config RuntimeConfig,
	applicationID, environmentID, releaseRepository string,
) (reference Reference, present bool, err error) {
	if tx == nil || config.Validate() != nil || !uuidPattern.MatchString(applicationID) ||
		!uuidPattern.MatchString(environmentID) || !validReleaseRepository(releaseRepository) {
		return Reference{}, false, ErrInvalid
	}
	var namespace string
	if err = tx.QueryRow(ctx, `SELECT e.namespace
		FROM environments e
		JOIN applications a ON a.project_id=e.project_id
		WHERE e.id=$1 AND a.id=$2
		FOR SHARE OF e,a`, environmentID, applicationID).Scan(&namespace); err != nil {
		return Reference{}, false, err
	}
	if !dnsLabelPattern.MatchString(namespace) {
		return Reference{}, false, ErrConflict
	}
	rows, err := tx.Query(ctx, `SELECT
		t.id::text,t.name,t.mode,t.endpoint,t.repository_prefix,t.pull_credential_ref,
		t.push_credential_ref,t.cache_credential_ref,t.created_at,t.updated_at,
		p.registry_target_id::text,p.service_id::text,p.repository,p.keep_last_successful,
		p.minimum_safety_age_seconds,p.cache_keep_generations,p.cache_unused_expiry_seconds,
		p.cache_byte_quota,p.created_at,p.updated_at
		FROM service_registry_policies p
		JOIN registry_targets t ON t.id=p.registry_target_id
		WHERE p.service_id=$1
		ORDER BY t.id
		LIMIT 65
		FOR SHARE OF t,p`, applicationID)
	if err != nil {
		return Reference{}, false, err
	}
	defer rows.Close()
	type matchedPolicy struct {
		target domain.RegistryTarget
		policy domain.ServiceRegistryPolicy
		server string
	}
	matches := make([]matchedPolicy, 0, 1)
	rowCount := 0
	for rows.Next() {
		rowCount++
		target, policy, scanErr := scanRegistryPullPolicy(rows)
		if scanErr != nil {
			return Reference{}, false, scanErr
		}
		server, serverOK := registryServerForPull(target.Endpoint)
		if registry.ValidateTarget(target) != nil || registry.ValidatePolicy(policy) != nil || !serverOK ||
			policy.RegistryTargetID != target.ID || policy.ServiceID != applicationID ||
			!strings.HasPrefix(policy.Repository, target.RepositoryPrefix+"/") || !validRegistryRepository(policy.Repository) {
			return Reference{}, false, ErrConflict
		}
		if server+"/"+policy.Repository == releaseRepository {
			matches = append(matches, matchedPolicy{target: target, policy: policy, server: server})
		}
	}
	if err = rows.Err(); err != nil {
		return Reference{}, false, err
	}
	if rowCount > 64 || len(matches) > 1 {
		return Reference{}, false, ErrConflict
	}
	if len(matches) == 0 || matches[0].target.PullCredentialRef == "" {
		return Reference{}, false, nil
	}
	match := matches[0]
	if !config.Enabled || !config.AllowsNamespace(namespace) {
		return Reference{}, false, ErrUnavailable
	}
	profile, found := config.ProfileForTarget(match.target.ID)
	if !found {
		return Reference{}, false, ErrUnavailable
	}
	if profile.RegistryServer != match.server || profile.CredentialRef != match.target.PullCredentialRef {
		return Reference{}, false, ErrConflict
	}
	return Reference{TargetID: match.target.ID, ProfileName: profile.Name, ProfileRevision: profile.Revision}, true, nil
}

type registryPullRowScanner interface{ Scan(...any) error }

func scanRegistryPullPolicy(row registryPullRowScanner) (domain.RegistryTarget, domain.ServiceRegistryPolicy, error) {
	var target domain.RegistryTarget
	var policy domain.ServiceRegistryPolicy
	var minimumSafetyAgeSeconds, cacheUnusedExpirySeconds int64
	err := row.Scan(
		&target.ID, &target.Name, &target.Mode, &target.Endpoint, &target.RepositoryPrefix, &target.PullCredentialRef,
		&target.PushCredentialRef, &target.CacheCredentialRef, &target.CreatedAt, &target.UpdatedAt,
		&policy.RegistryTargetID, &policy.ServiceID, &policy.Repository, &policy.KeepLastSuccessful,
		&minimumSafetyAgeSeconds, &policy.CacheKeepGenerations, &cacheUnusedExpirySeconds,
		&policy.CacheByteQuota, &policy.CreatedAt, &policy.UpdatedAt,
	)
	policy.MinimumSafetyAge = time.Duration(minimumSafetyAgeSeconds) * time.Second
	policy.CacheUnusedExpiry = time.Duration(cacheUnusedExpirySeconds) * time.Second
	return target, policy, err
}

func registryServerForPull(raw string) (string, bool) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\x00\r\n") {
		return "", false
	}
	candidate := raw
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Host != strings.ToLower(parsed.Host) {
		return "", false
	}
	return parsed.Host, true
}

func validReleaseRepository(value string) bool {
	return value != "" && len(value) <= 255 && utf8.ValidString(value) && !strings.Contains(value, "@") &&
		strings.IndexFunc(value, unicode.IsSpace) == -1
}

func validRegistryRepository(value string) bool {
	return validReleaseRepository(value) && !strings.HasPrefix(value, "/") && !strings.HasSuffix(value, "/") &&
		!strings.Contains(value, "//") && !strings.Contains(value, "://")
}
