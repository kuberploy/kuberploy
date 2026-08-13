package domain

import "time"

// AccessRole is a closed, ordered set. Roles grant permissions; a grant's
// scope determines where those permissions apply.
type AccessRole string

const (
	RoleViewer            AccessRole = "viewer"
	RoleDeveloper         AccessRole = "developer"
	RoleProjectAdmin      AccessRole = "project-admin"
	RoleOrganizationAdmin AccessRole = "organization-admin"
	RolePlatformAdmin     AccessRole = "platform-admin"
)

// AccessScopeType is the durable grant boundary exposed by the API.
type AccessScopeType string

const (
	ScopePlatform    AccessScopeType = "platform"
	ScopeTeam        AccessScopeType = "team"
	ScopeProject     AccessScopeType = "project"
	ScopeEnvironment AccessScopeType = "environment"
	ScopeNamespace   AccessScopeType = "namespace"
	ScopeApplication AccessScopeType = "application"
)

// Permission is intentionally small and composable. API action names are
// derived from these permissions rather than persisted as authorization data.
type Permission string

const (
	PermissionResourcesRead          Permission = "resources.read"
	PermissionResourcesWrite         Permission = "resources.write"
	PermissionConfigRead             Permission = "config.read"
	PermissionConfigWrite            Permission = "config.write"
	PermissionOperationsRead         Permission = "operations.read"
	PermissionMetricsRead            Permission = "metrics.read"
	PermissionLogsRead               Permission = "logs.read"
	PermissionSecretsRead            Permission = "secrets.read"
	PermissionSecretsBind            Permission = "secrets.bind"
	PermissionSecretsCreate          Permission = "secrets.create"
	PermissionSecretsRotate          Permission = "secrets.rotate"
	PermissionSecretsDelete          Permission = "secrets.delete"
	PermissionCertificatesRead       Permission = "certificates.read"
	PermissionCertificatesCreate     Permission = "certificates.create"
	PermissionCertificatesRotate     Permission = "certificates.rotate"
	PermissionCertificatesDelete     Permission = "certificates.delete"
	PermissionBuildsRead             Permission = "builds.read"
	PermissionBuildsManage           Permission = "builds.manage"
	PermissionBuildsCancel           Permission = "builds.cancel"
	PermissionBuildsRetry            Permission = "builds.retry"
	PermissionHelmRead               Permission = "helm.read"
	PermissionHelmDeploy             Permission = "helm.deploy"
	PermissionHelmRetry              Permission = "helm.retry"
	PermissionHelmRollback           Permission = "helm.rollback"
	PermissionRegistryRead           Permission = "registry.read"
	PermissionRegistryPolicyWrite    Permission = "registry.policy.write"
	PermissionRegistryCleanupPreview Permission = "registry.cleanup.preview"
	PermissionRegistryCleanupExecute Permission = "registry.cleanup.execute"
	PermissionRegistryTargetsManage  Permission = "registry.targets.manage"
	PermissionExternalDNSRead        Permission = "external-dns.read"
	PermissionExternalDNSManage      Permission = "external-dns.manage"
	PermissionGrantsRead             Permission = "grants.read"
	PermissionGrantsManage           Permission = "grants.manage"
	PermissionPlatformAdmin          Permission = "platform.admin"
)

// AccessGrant is an explicit, durable assignment. Team owner/member access is
// evaluated as an implicit binding and is not duplicated in this table.
type AccessGrant struct {
	ID            string          `json:"id"`
	SubjectUserID string          `json:"subjectUserId,omitempty"`
	SubjectTeamID string          `json:"subjectTeamId,omitempty"`
	Role          AccessRole      `json:"role"`
	ScopeType     AccessScopeType `json:"scopeType"`
	ScopeID       string          `json:"scopeId"`
	Permissions   []Permission    `json:"permissions"`
	Source        string          `json:"source"`
	CreatedBy     string          `json:"createdBy"`
	CreatedAt     time.Time       `json:"createdAt"`
}

type CreateAccessGrant struct {
	ProjectID     string
	SubjectUserID string
	SubjectTeamID string
	Role          AccessRole
	ScopeType     AccessScopeType
	ScopeID       string
	Permissions   []Permission
}

// AccessTarget contains the fully resolved ownership chain for one resource.
// ID is the concrete resource ID and Namespace is the exact Kubernetes
// namespace bound to EnvironmentID.
type AccessTarget struct {
	Type          string
	ID            string
	TeamID        string
	ProjectID     string
	EnvironmentID string
	Namespace     string
	ApplicationID string
}

// AccessBinding is an explicit grant or an implicit team membership after it
// has been normalized for the shared evaluator.
type AccessBinding struct {
	Role        AccessRole
	ScopeType   AccessScopeType
	ScopeID     string
	Permissions []Permission
	Source      string
}

type AccessCapability struct {
	Role      AccessRole      `json:"role"`
	ScopeType AccessScopeType `json:"scopeType"`
	ScopeID   string          `json:"scopeId"`
	Source    string          `json:"source"`
	Actions   []string        `json:"actions"`
}
