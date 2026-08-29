package access

import (
	"slices"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func TestScopeMatchingAndAdditiveViewerLogPermission(t *testing.T) {
	target := domain.AccessTarget{TeamID: "team-a", ProjectID: "project-a", EnvironmentID: "env-a", Namespace: "ns-a", ApplicationID: "app-a"}
	viewer := domain.AccessBinding{Role: domain.RoleViewer, ScopeType: domain.ScopeApplication, ScopeID: "app-a"}
	if !HasPermission([]domain.AccessBinding{viewer}, target, domain.PermissionResourcesRead) {
		t.Fatal("application viewer cannot read application descendants")
	}
	if !HasPermission([]domain.AccessBinding{viewer}, target, domain.PermissionMetricsRead) {
		t.Fatal("application viewer cannot read scoped metrics")
	}
	if HasPermission([]domain.AccessBinding{viewer}, target, domain.PermissionLogsRead) {
		t.Fatal("viewer received logs.read without an explicit additive permission")
	}
	viewer.Permissions = []domain.Permission{domain.PermissionLogsRead}
	if !HasPermission([]domain.AccessBinding{viewer}, target, domain.PermissionLogsRead) {
		t.Fatal("explicit viewer logs.read permission was ignored")
	}
	other := target
	other.ApplicationID = "app-b"
	if HasPermission([]domain.AccessBinding{viewer}, other, domain.PermissionResourcesRead) {
		t.Fatal("application grant crossed its exact scope")
	}
}

func TestDelegationIsBoundByRoleAndScope(t *testing.T) {
	target := domain.AccessTarget{TeamID: "team-a", ProjectID: "project-a"}
	projectAdmin := domain.AccessBinding{Role: domain.RoleProjectAdmin, ScopeType: domain.ScopeProject, ScopeID: "project-a"}
	if !CanManageGrant([]domain.AccessBinding{projectAdmin}, target, domain.RoleDeveloper) {
		t.Fatal("project admin could not delegate a narrower role")
	}
	if CanManageGrant([]domain.AccessBinding{projectAdmin}, target, domain.RoleOrganizationAdmin) {
		t.Fatal("project admin delegated a broader role")
	}
	other := target
	other.ProjectID = "project-b"
	if CanManageGrant([]domain.AccessBinding{projectAdmin}, other, domain.RoleViewer) {
		t.Fatal("project admin delegated outside its scope")
	}
}

func TestCapabilityActionsAreNarrowedToBindingScope(t *testing.T) {
	applicationDeveloper := Actions(domain.AccessBinding{Role: domain.RoleDeveloper, ScopeType: domain.ScopeApplication, ScopeID: "app-a"})
	has := func(want string) bool {
		for _, action := range applicationDeveloper {
			if action == want {
				return true
			}
		}
		return false
	}
	if !has("applications:read") || !has("deployments:create") || !has("logs:read") || !has("metrics:read") {
		t.Fatalf("application capability omitted effective actions: %#v", applicationDeveloper)
	}
	if has("projects:create") || has("environments:create") || has("environments:read") {
		t.Fatalf("application capability advertised broader actions: %#v", applicationDeveloper)
	}
}

func TestProjectDeletionCapabilityMatchesWritableProjectScope(t *testing.T) {
	for _, binding := range []domain.AccessBinding{
		{Role: domain.RolePlatformAdmin, ScopeType: domain.ScopePlatform, ScopeID: "platform"},
		{Role: domain.RoleOrganizationAdmin, ScopeType: domain.ScopeTeam, ScopeID: "team-a"},
		{Role: domain.RoleProjectAdmin, ScopeType: domain.ScopeProject, ScopeID: "project-a"},
	} {
		if !slices.Contains(Actions(binding), "projects:delete") {
			t.Fatalf("writable binding omitted projects:delete: %#v", binding)
		}
	}
	if slices.Contains(Actions(domain.AccessBinding{Role: domain.RoleDeveloper, ScopeType: domain.ScopeProject, ScopeID: "project-a"}), "projects:delete") {
		t.Fatal("developer received projects:delete")
	}
}

func TestTeamCapabilityAdvertisesTheMemberListThatTeamReadersCanUse(t *testing.T) {
	actions := Actions(domain.AccessBinding{Role: domain.RoleViewer, ScopeType: domain.ScopeTeam, ScopeID: "team-a"})
	foundMemberRead := false
	for _, action := range actions {
		if action == "team-members:read" {
			foundMemberRead = true
		}
		if action == "team-members:write" {
			t.Fatal("team viewer capability advertised member mutation")
		}
	}
	if !foundMemberRead {
		t.Fatalf("team reader capability omitted member visibility: %#v", actions)
	}
}

func TestRuntimeSecretPermissionsAreClosedByRole(t *testing.T) {
	target := domain.AccessTarget{TeamID: "team-a", ProjectID: "project-a", EnvironmentID: "env-a", Namespace: "ns-a", ApplicationID: "app-a"}
	roles := []struct {
		role                               domain.AccessRole
		read, bind, create, rotate, delete bool
	}{
		{domain.RoleViewer, true, false, false, false, false},
		{domain.RoleDeveloper, true, true, false, false, false},
		{domain.RoleProjectAdmin, true, true, true, true, true},
		{domain.RoleOrganizationAdmin, true, true, true, true, true},
		{domain.RolePlatformAdmin, true, true, true, true, true},
	}
	for _, test := range roles {
		binding := domain.AccessBinding{Role: test.role, ScopeType: domain.ScopeApplication, ScopeID: "app-a"}
		bindings := []domain.AccessBinding{binding}
		for _, permission := range []struct {
			value domain.Permission
			want  bool
		}{
			{domain.PermissionSecretsRead, test.read},
			{domain.PermissionSecretsBind, test.bind},
			{domain.PermissionSecretsCreate, test.create},
			{domain.PermissionSecretsRotate, test.rotate},
			{domain.PermissionSecretsDelete, test.delete},
		} {
			if got := HasPermission(bindings, target, permission.value); got != permission.want {
				t.Fatalf("role %q permission %q=%t, want %t", test.role, permission.value, got, permission.want)
			}
		}
		actions := Actions(binding)
		hasAction := func(value string) bool {
			for _, action := range actions {
				if action == value {
					return true
				}
			}
			return false
		}
		if hasAction("secret-bindings:read") != test.read || hasAction("secret-bindings:bind") != test.bind ||
			hasAction("secret-bindings:create") != test.create || hasAction("secret-bindings:rotate") != test.rotate || hasAction("secret-bindings:delete") != test.delete {
			t.Fatalf("role %q secret actions=%#v", test.role, actions)
		}
	}
}

func TestRuntimeSecretMutationPermissionsRemainIndependentlyAddressable(t *testing.T) {
	for permission, want := range map[domain.Permission]string{
		domain.PermissionSecretsCreate: "secret-bindings:create",
		domain.PermissionSecretsRotate: "secret-bindings:rotate",
		domain.PermissionSecretsDelete: "secret-bindings:delete",
		domain.PermissionSecretsBind:   "secret-bindings:bind",
	} {
		actions := Actions(domain.AccessBinding{Role: domain.AccessRole("no-role"), ScopeType: domain.ScopeApplication, ScopeID: "app-a", Permissions: []domain.Permission{permission}})
		if len(actions) != 1 || actions[0] != want {
			t.Fatalf("permission %q actions=%#v, want only %q", permission, actions, want)
		}
		if ValidExtraPermissions([]domain.Permission{permission}) {
			t.Fatalf("secret permission %q can be injected as an additive grant", permission)
		}
	}
}

func TestCertificatePermissionsAreClosedAndIndependentFromRuntimeSecrets(t *testing.T) {
	target := domain.AccessTarget{TeamID: "team-a", ProjectID: "project-a", EnvironmentID: "env-a", Namespace: "ns-a", ApplicationID: "app-a"}
	roles := []struct {
		role                         domain.AccessRole
		read, create, rotate, delete bool
	}{
		{domain.RoleViewer, true, false, false, false},
		{domain.RoleDeveloper, true, false, false, false},
		{domain.RoleProjectAdmin, true, true, true, true},
		{domain.RoleOrganizationAdmin, true, true, true, true},
		{domain.RolePlatformAdmin, true, true, true, true},
	}
	for _, test := range roles {
		binding := domain.AccessBinding{Role: test.role, ScopeType: domain.ScopeApplication, ScopeID: "app-a"}
		for _, permission := range []struct {
			value domain.Permission
			want  bool
		}{
			{domain.PermissionCertificatesRead, test.read},
			{domain.PermissionCertificatesCreate, test.create},
			{domain.PermissionCertificatesRotate, test.rotate},
			{domain.PermissionCertificatesDelete, test.delete},
		} {
			if got := HasPermission([]domain.AccessBinding{binding}, target, permission.value); got != permission.want {
				t.Fatalf("role %q certificate permission %q=%t, want %t", test.role, permission.value, got, permission.want)
			}
		}
		actions := Actions(binding)
		for action, want := range map[string]bool{
			"certificate-bindings:read": test.read, "certificate-bindings:create": test.create,
			"certificate-bindings:rotate": test.rotate, "certificate-bindings:delete": test.delete,
		} {
			if slices.Contains(actions, action) != want {
				t.Fatalf("role %q certificate actions=%#v", test.role, actions)
			}
		}
	}

	for permission, want := range map[domain.Permission]string{
		domain.PermissionCertificatesRead: "certificate-bindings:read", domain.PermissionCertificatesCreate: "certificate-bindings:create",
		domain.PermissionCertificatesRotate: "certificate-bindings:rotate", domain.PermissionCertificatesDelete: "certificate-bindings:delete",
	} {
		actions := Actions(domain.AccessBinding{Role: domain.AccessRole("no-role"), ScopeType: domain.ScopeApplication, ScopeID: "app-a", Permissions: []domain.Permission{permission}})
		if len(actions) != 1 || actions[0] != want || ValidExtraPermissions([]domain.Permission{permission}) {
			t.Fatalf("certificate permission %q actions=%#v extra=%t", permission, actions, ValidExtraPermissions([]domain.Permission{permission}))
		}
		if strings.HasPrefix(want, "certificate-") && slices.Contains(actions, "secret-bindings:read") {
			t.Fatalf("certificate permission %q also granted a generic secret action", permission)
		}
	}
}

func TestBuildPermissionsAreClosedAndIndependentlyAddressable(t *testing.T) {
	target := domain.AccessTarget{TeamID: "team-a", ProjectID: "project-a", ApplicationID: "app-a"}
	tests := []struct {
		role                        domain.AccessRole
		read, manage, cancel, retry bool
	}{
		{domain.RoleViewer, true, false, false, false},
		{domain.RoleDeveloper, true, false, true, true},
		{domain.RoleProjectAdmin, true, true, true, true},
		{domain.RoleOrganizationAdmin, true, true, true, true},
		{domain.RolePlatformAdmin, true, true, true, true},
	}
	for _, test := range tests {
		binding := domain.AccessBinding{Role: test.role, ScopeType: domain.ScopeApplication, ScopeID: "app-a"}
		for _, permission := range []struct {
			value domain.Permission
			want  bool
		}{
			{domain.PermissionBuildsRead, test.read},
			{domain.PermissionBuildsManage, test.manage},
			{domain.PermissionBuildsCancel, test.cancel},
			{domain.PermissionBuildsRetry, test.retry},
		} {
			if got := HasPermission([]domain.AccessBinding{binding}, target, permission.value); got != permission.want {
				t.Fatalf("role %q permission %q=%t, want %t", test.role, permission.value, got, permission.want)
			}
		}
	}
	for permission, want := range map[domain.Permission][]string{
		domain.PermissionBuildsRead:   {"app-sources:read", "builds:read"},
		domain.PermissionBuildsManage: {"app-sources:write"},
		domain.PermissionBuildsCancel: {"builds:cancel"},
		domain.PermissionBuildsRetry:  {"builds:retry"},
	} {
		actions := Actions(domain.AccessBinding{Role: domain.AccessRole("no-role"), ScopeType: domain.ScopeApplication, ScopeID: "app-a", Permissions: []domain.Permission{permission}})
		if len(actions) != len(want) {
			t.Fatalf("permission %q actions=%#v", permission, actions)
		}
		for index := range want {
			if actions[index] != want[index] {
				t.Fatalf("permission %q actions=%#v want=%#v", permission, actions, want)
			}
		}
		if ValidExtraPermissions([]domain.Permission{permission}) {
			t.Fatalf("build permission %q can be injected as an additive grant", permission)
		}
	}
}

func TestHelmPermissionsAreClosedByRoleAndScope(t *testing.T) {
	target := domain.AccessTarget{TeamID: "team-a", ProjectID: "project-a", EnvironmentID: "env-a", ApplicationID: "app-a"}
	tests := []struct {
		role                          domain.AccessRole
		read, deploy, retry, rollback bool
	}{
		{domain.RoleViewer, true, false, false, false},
		{domain.RoleDeveloper, true, true, true, false},
		{domain.RoleProjectAdmin, true, true, true, true},
		{domain.RoleOrganizationAdmin, true, true, true, true},
		{domain.RolePlatformAdmin, true, true, true, true},
	}
	for _, test := range tests {
		binding := domain.AccessBinding{Role: test.role, ScopeType: domain.ScopeApplication, ScopeID: "app-a"}
		for _, permission := range []struct {
			value domain.Permission
			want  bool
		}{{domain.PermissionHelmRead, test.read}, {domain.PermissionHelmDeploy, test.deploy},
			{domain.PermissionHelmRetry, test.retry}, {domain.PermissionHelmRollback, test.rollback}} {
			if got := HasPermission([]domain.AccessBinding{binding}, target, permission.value); got != permission.want {
				t.Fatalf("role %q permission %q=%t, want %t", test.role, permission.value, got, permission.want)
			}
		}
	}
	other := target
	other.ApplicationID = "app-b"
	if HasPermission([]domain.AccessBinding{{Role: domain.RoleProjectAdmin, ScopeType: domain.ScopeApplication, ScopeID: "app-a"}}, other, domain.PermissionHelmRollback) {
		t.Fatal("Helm rollback crossed an application-scoped grant")
	}
	for permission, want := range map[domain.Permission][]string{
		domain.PermissionHelmRead:     {"helm-releases:read"},
		domain.PermissionHelmDeploy:   {"helm-releases:deploy", "helm-releases:disable"},
		domain.PermissionHelmRetry:    {"helm-releases:retry"},
		domain.PermissionHelmRollback: {"helm-releases:rollback"},
	} {
		actions := Actions(domain.AccessBinding{Role: domain.AccessRole("no-role"), ScopeType: domain.ScopeApplication,
			ScopeID: "app-a", Permissions: []domain.Permission{permission}})
		if !slices.Equal(actions, want) || ValidExtraPermissions([]domain.Permission{permission}) {
			t.Fatalf("permission %q actions=%#v extra=%t", permission, actions, ValidExtraPermissions([]domain.Permission{permission}))
		}
	}
}

func TestRegistryPermissionsAreClosedByRole(t *testing.T) {
	target := domain.AccessTarget{TeamID: "team-a", ProjectID: "project-a", ApplicationID: "app-a"}
	tests := []struct {
		role                                    domain.AccessRole
		read, policy, preview, execute, targets bool
	}{
		{domain.RoleViewer, true, false, false, false, false},
		{domain.RoleDeveloper, true, false, false, false, false},
		{domain.RoleProjectAdmin, true, true, true, true, false},
		{domain.RoleOrganizationAdmin, true, true, true, true, false},
		{domain.RolePlatformAdmin, true, true, true, true, true},
	}
	for _, test := range tests {
		binding := domain.AccessBinding{Role: test.role, ScopeType: domain.ScopeApplication, ScopeID: "app-a"}
		permissions := []struct {
			permission domain.Permission
			want       bool
		}{
			{domain.PermissionRegistryRead, test.read},
			{domain.PermissionRegistryPolicyWrite, test.policy},
			{domain.PermissionRegistryCleanupPreview, test.preview},
			{domain.PermissionRegistryCleanupExecute, test.execute},
			{domain.PermissionRegistryTargetsManage, test.targets},
		}
		for _, permission := range permissions {
			if got := HasPermission([]domain.AccessBinding{binding}, target, permission.permission); got != permission.want {
				t.Fatalf("role %q permission %q=%t, want %t", test.role, permission.permission, got, permission.want)
			}
		}
	}

	platformActions := Actions(domain.AccessBinding{Role: domain.RolePlatformAdmin, ScopeType: domain.ScopePlatform, ScopeID: "platform"})
	for _, want := range []string{"registry:read", "registry-policies:write", "registry-cleanup:preview", "registry-cleanup:execute", "registry-targets:read", "registry-targets:write"} {
		found := false
		for _, action := range platformActions {
			found = found || action == want
		}
		if !found {
			t.Fatalf("platform registry action %q missing from %#v", want, platformActions)
		}
	}
}

func TestExternalDNSReadIsScopedAndManagementIsPlatformOnly(t *testing.T) {
	target := domain.AccessTarget{TeamID: "team-a", ProjectID: "project-a", EnvironmentID: "env-a", ApplicationID: "app-a"}
	for _, role := range []domain.AccessRole{domain.RoleViewer, domain.RoleDeveloper, domain.RoleProjectAdmin, domain.RoleOrganizationAdmin, domain.RolePlatformAdmin} {
		binding := domain.AccessBinding{Role: role, ScopeType: domain.ScopeApplication, ScopeID: "app-a"}
		if !HasPermission([]domain.AccessBinding{binding}, target, domain.PermissionExternalDNSRead) {
			t.Fatalf("role %q cannot read its exact external-dns catalog", role)
		}
		if HasPermission([]domain.AccessBinding{binding}, domain.AccessTarget{Type: "platform", ID: "platform"}, domain.PermissionExternalDNSManage) {
			t.Fatalf("role %q gained external-dns platform management from application scope", role)
		}
	}
	platform := domain.AccessBinding{Role: domain.RolePlatformAdmin, ScopeType: domain.ScopePlatform, ScopeID: "platform"}
	actions := Actions(platform)
	for _, want := range []string{"external-dns-integrations:read", "external-dns-integrations:write"} {
		found := false
		for _, action := range actions {
			found = found || action == want
		}
		if !found {
			t.Fatalf("platform capability omitted %q: %#v", want, actions)
		}
	}
	narrow := domain.AccessBinding{Role: domain.RoleViewer, ScopeType: domain.ScopeApplication, ScopeID: "app-a"}
	if HasPermission([]domain.AccessBinding{narrow}, domain.AccessTarget{ApplicationID: "app-b", ProjectID: "project-a"}, domain.PermissionExternalDNSRead) {
		t.Fatal("application catalog permission crossed scope")
	}
}
