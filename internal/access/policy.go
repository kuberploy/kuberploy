// Package access contains Kuberploy's shared, data-plane-independent
// authorization policy. Stores resolve durable bindings and resource ancestry;
// this package is the only place that maps roles/scopes to permissions.
package access

import (
	"sort"

	"github.com/kuberploy/kuberploy/internal/domain"
)

var roleRank = map[domain.AccessRole]int{
	domain.RoleViewer:            10,
	domain.RoleDeveloper:         20,
	domain.RoleProjectAdmin:      30,
	domain.RoleOrganizationAdmin: 40,
	domain.RolePlatformAdmin:     50,
}

var rolePermissions = map[domain.AccessRole][]domain.Permission{
	domain.RoleViewer: {
		domain.PermissionResourcesRead,
		domain.PermissionConfigRead,
		domain.PermissionOperationsRead,
		domain.PermissionMetricsRead,
		domain.PermissionSecretsRead,
		domain.PermissionCertificatesRead,
		domain.PermissionBuildsRead,
		domain.PermissionHelmRead,
		domain.PermissionRegistryRead,
		domain.PermissionExternalDNSRead,
	},
	domain.RoleDeveloper: {
		domain.PermissionResourcesRead,
		domain.PermissionResourcesWrite,
		domain.PermissionConfigRead,
		domain.PermissionConfigWrite,
		domain.PermissionOperationsRead,
		domain.PermissionMetricsRead,
		domain.PermissionLogsRead,
		domain.PermissionSecretsRead,
		domain.PermissionSecretsBind,
		domain.PermissionCertificatesRead,
		domain.PermissionBuildsRead,
		domain.PermissionBuildsCancel,
		domain.PermissionBuildsRetry,
		domain.PermissionHelmRead,
		domain.PermissionHelmDeploy,
		domain.PermissionHelmRetry,
		domain.PermissionRegistryRead,
		domain.PermissionExternalDNSRead,
	},
	domain.RoleProjectAdmin: {
		domain.PermissionResourcesRead,
		domain.PermissionResourcesWrite,
		domain.PermissionConfigRead,
		domain.PermissionConfigWrite,
		domain.PermissionOperationsRead,
		domain.PermissionMetricsRead,
		domain.PermissionLogsRead,
		domain.PermissionSecretsRead,
		domain.PermissionSecretsBind,
		domain.PermissionSecretsCreate,
		domain.PermissionSecretsRotate,
		domain.PermissionSecretsDelete,
		domain.PermissionCertificatesRead,
		domain.PermissionCertificatesCreate,
		domain.PermissionCertificatesRotate,
		domain.PermissionCertificatesDelete,
		domain.PermissionBuildsRead,
		domain.PermissionBuildsManage,
		domain.PermissionBuildsCancel,
		domain.PermissionBuildsRetry,
		domain.PermissionHelmRead,
		domain.PermissionHelmDeploy,
		domain.PermissionHelmRetry,
		domain.PermissionHelmRollback,
		domain.PermissionRegistryRead,
		domain.PermissionRegistryPolicyWrite,
		domain.PermissionRegistryCleanupPreview,
		domain.PermissionRegistryCleanupExecute,
		domain.PermissionExternalDNSRead,
		domain.PermissionGrantsRead,
		domain.PermissionGrantsManage,
	},
	domain.RoleOrganizationAdmin: {
		domain.PermissionResourcesRead,
		domain.PermissionResourcesWrite,
		domain.PermissionConfigRead,
		domain.PermissionConfigWrite,
		domain.PermissionOperationsRead,
		domain.PermissionMetricsRead,
		domain.PermissionLogsRead,
		domain.PermissionSecretsRead,
		domain.PermissionSecretsBind,
		domain.PermissionSecretsCreate,
		domain.PermissionSecretsRotate,
		domain.PermissionSecretsDelete,
		domain.PermissionCertificatesRead,
		domain.PermissionCertificatesCreate,
		domain.PermissionCertificatesRotate,
		domain.PermissionCertificatesDelete,
		domain.PermissionBuildsRead,
		domain.PermissionBuildsManage,
		domain.PermissionBuildsCancel,
		domain.PermissionBuildsRetry,
		domain.PermissionHelmRead,
		domain.PermissionHelmDeploy,
		domain.PermissionHelmRetry,
		domain.PermissionHelmRollback,
		domain.PermissionRegistryRead,
		domain.PermissionRegistryPolicyWrite,
		domain.PermissionRegistryCleanupPreview,
		domain.PermissionRegistryCleanupExecute,
		domain.PermissionExternalDNSRead,
		domain.PermissionGrantsRead,
		domain.PermissionGrantsManage,
	},
	domain.RolePlatformAdmin: {
		domain.PermissionResourcesRead,
		domain.PermissionResourcesWrite,
		domain.PermissionConfigRead,
		domain.PermissionConfigWrite,
		domain.PermissionOperationsRead,
		domain.PermissionMetricsRead,
		domain.PermissionLogsRead,
		domain.PermissionSecretsRead,
		domain.PermissionSecretsBind,
		domain.PermissionSecretsCreate,
		domain.PermissionSecretsRotate,
		domain.PermissionSecretsDelete,
		domain.PermissionCertificatesRead,
		domain.PermissionCertificatesCreate,
		domain.PermissionCertificatesRotate,
		domain.PermissionCertificatesDelete,
		domain.PermissionBuildsRead,
		domain.PermissionBuildsManage,
		domain.PermissionBuildsCancel,
		domain.PermissionBuildsRetry,
		domain.PermissionHelmRead,
		domain.PermissionHelmDeploy,
		domain.PermissionHelmRetry,
		domain.PermissionHelmRollback,
		domain.PermissionRegistryRead,
		domain.PermissionRegistryPolicyWrite,
		domain.PermissionRegistryCleanupPreview,
		domain.PermissionRegistryCleanupExecute,
		domain.PermissionRegistryTargetsManage,
		domain.PermissionExternalDNSRead,
		domain.PermissionExternalDNSManage,
		domain.PermissionGrantsRead,
		domain.PermissionGrantsManage,
		domain.PermissionPlatformAdmin,
	},
}

func ValidRole(role domain.AccessRole) bool { _, ok := roleRank[role]; return ok }

func RoleRank(role domain.AccessRole) int { return roleRank[role] }

func ValidScope(scope domain.AccessScopeType) bool {
	switch scope {
	case domain.ScopePlatform, domain.ScopeTeam, domain.ScopeProject, domain.ScopeEnvironment, domain.ScopeNamespace, domain.ScopeApplication:
		return true
	default:
		return false
	}
}

// ValidExtraPermissions deliberately allows only logs.read. Viewer log access
// is opt-in; all higher operational roles already receive it from their role.
func ValidExtraPermissions(permissions []domain.Permission) bool {
	seen := map[domain.Permission]bool{}
	for _, permission := range permissions {
		if permission != domain.PermissionLogsRead || seen[permission] {
			return false
		}
		seen[permission] = true
	}
	return true
}

func BindingMatches(binding domain.AccessBinding, target domain.AccessTarget) bool {
	switch binding.ScopeType {
	case domain.ScopePlatform:
		return binding.ScopeID == "platform"
	case domain.ScopeTeam:
		return binding.ScopeID != "" && binding.ScopeID == target.TeamID
	case domain.ScopeProject:
		return binding.ScopeID != "" && binding.ScopeID == target.ProjectID
	case domain.ScopeEnvironment:
		return binding.ScopeID != "" && binding.ScopeID == target.EnvironmentID
	case domain.ScopeNamespace:
		return binding.ScopeID != "" && binding.ScopeID == target.Namespace
	case domain.ScopeApplication:
		return binding.ScopeID != "" && binding.ScopeID == target.ApplicationID
	default:
		return false
	}
}

func HasPermission(bindings []domain.AccessBinding, target domain.AccessTarget, permission domain.Permission) bool {
	for _, binding := range bindings {
		if !BindingMatches(binding, target) {
			continue
		}
		for _, granted := range rolePermissions[binding.Role] {
			if granted == permission {
				return true
			}
		}
		for _, granted := range binding.Permissions {
			if granted == permission {
				return true
			}
		}
	}
	return false
}

// CanManageGrant applies both scope containment and role delegation bounds.
// Platform grants are bootstrap/internal only and are never managed through a
// project endpoint.
func CanManageGrant(bindings []domain.AccessBinding, target domain.AccessTarget, requestedRole domain.AccessRole) bool {
	for _, binding := range bindings {
		if !BindingMatches(binding, target) || !bindingHas(binding, domain.PermissionGrantsManage) {
			continue
		}
		if RoleRank(binding.Role) >= RoleRank(requestedRole) {
			return true
		}
	}
	return false
}

func bindingHas(binding domain.AccessBinding, permission domain.Permission) bool {
	for _, granted := range rolePermissions[binding.Role] {
		if granted == permission {
			return true
		}
	}
	for _, granted := range binding.Permissions {
		if granted == permission {
			return true
		}
	}
	return false
}

func Actions(binding domain.AccessBinding) []string {
	set := map[string]struct{}{}
	add := func(permission domain.Permission) {
		for _, action := range permissionActions(permission, binding.ScopeType) {
			set[action] = struct{}{}
		}
	}
	for _, permission := range rolePermissions[binding.Role] {
		add(permission)
	}
	for _, permission := range binding.Permissions {
		add(permission)
	}
	out := make([]string, 0, len(set))
	for action := range set {
		out = append(out, action)
	}
	sort.Strings(out)
	return out
}

func permissionActions(permission domain.Permission, scope domain.AccessScopeType) []string {
	switch permission {
	case domain.PermissionResourcesRead:
		switch scope {
		case domain.ScopePlatform, domain.ScopeTeam:
			return []string{"applications:read", "deployments:read", "environments:read", "projects:read", "team-members:read", "teams:read"}
		case domain.ScopeEnvironment, domain.ScopeNamespace:
			return []string{"deployments:read", "environments:read", "projects:read"}
		case domain.ScopeApplication:
			return []string{"applications:read", "deployments:read", "projects:read"}
		default:
			return []string{"applications:read", "deployments:read", "environments:read", "projects:read"}
		}
	case domain.PermissionResourcesWrite:
		switch scope {
		case domain.ScopePlatform, domain.ScopeTeam:
			return []string{"applications:create", "deployments:create", "deployments:update", "environments:create", "projects:create"}
		case domain.ScopeProject:
			return []string{"applications:create", "deployments:create", "deployments:update", "environments:create"}
		default:
			return []string{"deployments:create", "deployments:update"}
		}
	case domain.PermissionConfigRead:
		return []string{"deployment-config:read", "deployment-config:validate"}
	case domain.PermissionConfigWrite:
		return []string{"deployment-config:preview", "deployment-config:write"}
	case domain.PermissionOperationsRead:
		return []string{"operations:read"}
	case domain.PermissionMetricsRead:
		return []string{"metrics:read"}
	case domain.PermissionLogsRead:
		return []string{"logs:read"}
	case domain.PermissionSecretsRead:
		return []string{"secret-bindings:read"}
	case domain.PermissionSecretsBind:
		return []string{"secret-bindings:bind"}
	case domain.PermissionSecretsCreate:
		return []string{"secret-bindings:create"}
	case domain.PermissionSecretsRotate:
		return []string{"secret-bindings:rotate"}
	case domain.PermissionSecretsDelete:
		return []string{"secret-bindings:delete"}
	case domain.PermissionCertificatesRead:
		return []string{"certificate-bindings:read"}
	case domain.PermissionCertificatesCreate:
		return []string{"certificate-bindings:create"}
	case domain.PermissionCertificatesRotate:
		return []string{"certificate-bindings:rotate"}
	case domain.PermissionCertificatesDelete:
		return []string{"certificate-bindings:delete"}
	case domain.PermissionBuildsRead:
		return []string{"build-definitions:read", "builds:read"}
	case domain.PermissionBuildsManage:
		return []string{"build-definitions:write"}
	case domain.PermissionBuildsCancel:
		return []string{"builds:cancel"}
	case domain.PermissionBuildsRetry:
		return []string{"builds:retry"}
	case domain.PermissionHelmRead:
		return []string{"helm-approvals:read", "helm-releases:read", "helm-values:preview"}
	case domain.PermissionHelmDeploy:
		return []string{"helm-releases:deploy", "helm-releases:disable"}
	case domain.PermissionHelmRetry:
		return []string{"helm-releases:retry"}
	case domain.PermissionHelmRollback:
		return []string{"helm-releases:rollback"}
	case domain.PermissionRegistryRead:
		return []string{"registry:read"}
	case domain.PermissionRegistryPolicyWrite:
		return []string{"registry-policies:write"}
	case domain.PermissionRegistryCleanupPreview:
		return []string{"registry-cleanup:preview"}
	case domain.PermissionRegistryCleanupExecute:
		return []string{"registry-cleanup:execute"}
	case domain.PermissionRegistryTargetsManage:
		return []string{"registry-targets:read", "registry-targets:write"}
	case domain.PermissionExternalDNSRead:
		return []string{"external-dns-integrations:read"}
	case domain.PermissionExternalDNSManage:
		return []string{"external-dns-integrations:read", "external-dns-integrations:write"}
	case domain.PermissionGrantsRead:
		if scope == domain.ScopePlatform || scope == domain.ScopeTeam {
			return []string{"access-grants:read", "team-members:read", "users:read"}
		}
		return []string{"access-grants:read"}
	case domain.PermissionGrantsManage:
		if scope == domain.ScopePlatform || scope == domain.ScopeTeam {
			return []string{"access-grants:create", "access-grants:delete", "team-members:write"}
		}
		return []string{"access-grants:create", "access-grants:delete"}
	case domain.PermissionPlatformAdmin:
		return []string{"github-installations:setup", "helm-approvals:manage", "platform-releases:read", "team-members:write", "teams:create", "teams:read", "user-invitations:create", "users:read"}
	default:
		return nil
	}
}

func Capabilities(bindings []domain.AccessBinding) []domain.AccessCapability {
	out := make([]domain.AccessCapability, 0, len(bindings))
	seen := map[string]struct{}{}
	for _, binding := range bindings {
		key := string(binding.Role) + "\x00" + string(binding.ScopeType) + "\x00" + binding.ScopeID + "\x00" + binding.Source
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, domain.AccessCapability{Role: binding.Role, ScopeType: binding.ScopeType, ScopeID: binding.ScopeID, Source: binding.Source, Actions: Actions(binding)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ScopeType != out[j].ScopeType {
			return out[i].ScopeType < out[j].ScopeType
		}
		if out[i].ScopeID != out[j].ScopeID {
			return out[i].ScopeID < out[j].ScopeID
		}
		return RoleRank(out[i].Role) > RoleRank(out[j].Role)
	})
	return out
}
