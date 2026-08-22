package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

type decodedAgentProfile struct {
	SchemaVersion string `json:"schemaVersion"`
	Contract      struct {
		OpenAPI string `json:"openapi"`
		SHA256  string `json:"sha256"`
	} `json:"contract"`
	Authentication struct {
		ServiceAccountsAvailable bool   `json:"serviceAccountsAvailable"`
		Mode                     string `json:"mode"`
		CSRFRequiredWith         string `json:"csrfRequiredWith"`
		CSRFNotRequiredWith      string `json:"csrfNotRequiredWith"`
	} `json:"authentication"`
	Safety struct {
		UnknownOperationPolicy string `json:"unknownOperationPolicy"`
	} `json:"safety"`
	Operations []AgentOperation `json:"operations"`
}

func TestAgentProfileIsDeterministicAndFailClosed(t *testing.T) {
	first, err := BuildAgentProfile(OpenAPIJSON)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildAgentProfile(OpenAPIJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("agent profile is not deterministic")
	}

	var profile decodedAgentProfile
	if err := json.Unmarshal(first, &profile); err != nil {
		t.Fatal(err)
	}
	if profile.SchemaVersion != "1.0.0" || profile.Contract.OpenAPI != "/openapi.json" {
		t.Fatalf("unexpected profile identity: %#v", profile)
	}
	digest := sha256.Sum256(OpenAPIJSON)
	if profile.Contract.SHA256 != "sha256:"+hex.EncodeToString(digest[:]) {
		t.Fatalf("profile contract digest=%q", profile.Contract.SHA256)
	}
	if !profile.Authentication.ServiceAccountsAvailable || profile.Authentication.Mode != "cookie-session-or-bearer" {
		t.Fatalf("profile does not advertise implemented authentication alternatives: %#v", profile.Authentication)
	}
	if !strings.Contains(profile.Authentication.CSRFRequiredWith, "cookieAuth") || profile.Authentication.CSRFNotRequiredWith != "bearerAuth" {
		t.Fatalf("profile does not express conditional CSRF semantics: %#v", profile.Authentication)
	}
	if profile.Safety.UnknownOperationPolicy != "deny" {
		t.Fatalf("unknown-operation policy=%q", profile.Safety.UnknownOperationPolicy)
	}
	if len(profile.Operations) < 20 {
		t.Fatalf("agent operation projection is unexpectedly small: %d", len(profile.Operations))
	}

	previous := ""
	operationScopes := make(map[string]string, len(profile.Operations))
	for _, operation := range profile.Operations {
		operationScopes[operation.OperationID] = operation.AutomationScope
		key := operation.Path + "\x00" + operation.Method + "\x00" + operation.OperationID
		if key <= previous {
			t.Fatalf("agent operations are not strictly sorted: %q after %q", key, previous)
		}
		previous = key
		if operation.Permission == "" || operation.Effect == "" || operation.Risk == "" || operation.Idempotency == "" || operation.Confirmation == "" {
			t.Fatalf("incomplete safety metadata for %#v", operation)
		}
		if !validAutomationScope(operation.AutomationScope) {
			t.Fatalf("invalid automation scope for %#v", operation)
		}
		lower := strings.ToLower(operation.OperationID)
		for _, denied := range []string{"bootstrap", "invitation", "grant", "upgrade", "github", "secret", "teammember", "serviceaccount"} {
			if strings.Contains(lower, denied) {
				t.Fatalf("sensitive operation leaked into agent allowlist: %s", operation.OperationID)
			}
		}
	}
	if _, advertised := operationScopes["createProject"]; advertised {
		t.Fatal("project-bound automation credential was allowed to create a project")
	}
	if _, advertised := operationScopes["getAppConfigSchema"]; !advertised {
		t.Fatal("scope-free AppConfig schema discovery is missing from the agent profile")
	}
	expectedScopes := map[string]string{
		"getCurrentUser": "none", "getCapabilities": "none", "getMonitoringStatus": "none", "getAppConfigSchema": "none",
		"listAuditEvents":  "app.read",
		"queryMetricRange": "app.read", "listProjects": "app.read", "getProject": "app.read",
		"listEnvironments": "app.read", "getEnvironment": "app.read", "listEnvironmentApps": "app.read", "listApplications": "app.read", "getApplication": "app.read",
		"listAssignedMiddlewareProfiles":  "app.read",
		"previewApplicationSSLIPHostname": "app.read",
		"listApprovedCertificateIssuers":  "app.read",
		"listApprovedHelmCharts":          "app.read", "previewApprovedHelmValues": "app.read",
		"previewApprovedHelmRenderedResources": "app.read",
		"getDesiredHelmRelease":                "app.read", "listDesiredHelmReleaseHistory": "app.read",
		"getEnvironmentGitBinding": "app.read",
		"listDeployments":          "app.read", "getDeployment": "app.read", "getDeploymentStatus": "app.read", "getDeploymentConfig": "app.read",
		"listDeploymentRollbackSources": "app.read", "rollbackDeployment": "app.edit",
		"validateDeploymentConfig": "app.read", "listOperations": "app.read", "getOperation": "app.read", "listApplicationWorkloads": "app.read",
		"listEnvironmentExternalDNSIntegrations": "app.read", "listApplicationExternalDNSIntegrations": "app.read",
		"listApplicationBuildDefinitions": "app.read", "listApplicationBuildProfiles": "app.read", "listApplicationBuilds": "app.read", "getBuildDefinition": "app.read", "getBuildAttempt": "app.read",
		"listApplicationAutoDeployPolicies": "app.read", "getAutoDeployPolicy": "app.read",
		"listAutoDeployPolicyRevisions": "app.read", "listAutoDeployPolicyRuns": "app.read",
		"getBuildAttemptLogs":             "logs.read",
		"getApplicationRegistryInventory": "app.read", "getRegistryCleanupPlan": "app.read",
		"getWorkloadLogSnapshot": "logs.read", "getWorkloadEvents": "logs.read",
		"createEnvironment": "app.edit", "cloneEnvironment": "app.edit", "createApplication": "app.edit", "createImageDeployment": "app.edit", "previewImageResolution": "app.edit",
		"upsertDesiredHelmRelease": "app.edit", "retryDesiredHelmRelease": "app.edit",
		"disableDesiredHelmRelease": "app.edit", "rollbackDesiredHelmRelease": "app.edit",
		"previewDeploymentConfig": "app.edit", "saveDeploymentConfig": "app.edit",
		"createApplicationBuildDefinition": "build.create", "createManualBuildAttempt": "build.create", "cancelBuildAttempt": "build.create", "retryBuildAttempt": "build.create",
	}
	if len(operationScopes) != len(expectedScopes) {
		t.Fatalf("agent operation set drifted: got=%d expected=%d", len(operationScopes), len(expectedScopes))
	}
	for operationID, expectedScope := range expectedScopes {
		if actualScope, ok := operationScopes[operationID]; !ok || actualScope != expectedScope {
			t.Fatalf("operation %q automation scope=%q present=%t, expected %q", operationID, actualScope, ok, expectedScope)
		}
	}

	mutated := bytes.Replace(OpenAPIJSON, []byte(`"x-kuberploy-confirmation":"none",`), nil, 1)
	if bytes.Equal(mutated, OpenAPIJSON) {
		t.Fatal("test mutation did not change OpenAPI")
	}
	if _, err := BuildAgentProfile(mutated); err == nil || !strings.Contains(err.Error(), "incomplete safety metadata") {
		t.Fatalf("missing safety metadata was not rejected: %v", err)
	}

	missingScope := bytes.Replace(OpenAPIJSON, []byte(`"x-kuberploy-automation-scope":"none",`), nil, 1)
	if bytes.Equal(missingScope, OpenAPIJSON) {
		t.Fatal("automation-scope removal did not change OpenAPI")
	}
	if _, err := BuildAgentProfile(missingScope); err == nil || !strings.Contains(err.Error(), "missing or invalid automation scope") {
		t.Fatalf("missing automation scope was not rejected: %v", err)
	}
	unknownScope := bytes.Replace(OpenAPIJSON, []byte(`"x-kuberploy-automation-scope":"none"`), []byte(`"x-kuberploy-automation-scope":"platform.admin"`), 1)
	if bytes.Equal(unknownScope, OpenAPIJSON) {
		t.Fatal("automation-scope replacement did not change OpenAPI")
	}
	if _, err := BuildAgentProfile(unknownScope); err == nil || !strings.Contains(err.Error(), "missing or invalid automation scope") {
		t.Fatalf("unknown automation scope was not rejected: %v", err)
	}
}

func TestArazzoReferencesOnlyAgentAllowlistedOpenAPIOperations(t *testing.T) {
	var document struct {
		Arazzo             string `json:"arazzo"`
		Self               string `json:"$self"`
		SourceDescriptions []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
			Type string `json:"type"`
		} `json:"sourceDescriptions"`
		Workflows []struct {
			WorkflowID string `json:"workflowId"`
			Steps      []struct {
				StepID      string `json:"stepId"`
				OperationID string `json:"operationId"`
			} `json:"steps"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal(ArazzoYAML, &document); err != nil {
		t.Fatal(err)
	}
	if document.Arazzo != "1.1.0" || document.Self != "/arazzo.yaml" {
		t.Fatalf("unexpected Arazzo identity: version=%q self=%q", document.Arazzo, document.Self)
	}
	if len(document.SourceDescriptions) != 1 || document.SourceDescriptions[0].Name != "kuberploy" || document.SourceDescriptions[0].URL != "/openapi.json" || document.SourceDescriptions[0].Type != "openapi" {
		t.Fatalf("unexpected Arazzo source: %#v", document.SourceDescriptions)
	}
	if len(document.Workflows) != 3 {
		t.Fatalf("workflow count=%d", len(document.Workflows))
	}

	profileBytes, err := BuildAgentProfile(OpenAPIJSON)
	if err != nil {
		t.Fatal(err)
	}
	var profile decodedAgentProfile
	if err := json.Unmarshal(profileBytes, &profile); err != nil {
		t.Fatal(err)
	}
	allowed := make(map[string]struct{}, len(profile.Operations))
	for _, operation := range profile.Operations {
		allowed[operation.OperationID] = struct{}{}
	}

	prefix := "$sourceDescriptions.kuberploy."
	workflowIDs := map[string]struct{}{}
	for _, workflow := range document.Workflows {
		if workflow.WorkflowID == "" {
			t.Fatal("workflow has no workflowId")
		}
		if _, duplicate := workflowIDs[workflow.WorkflowID]; duplicate {
			t.Fatalf("duplicate workflowId %q", workflow.WorkflowID)
		}
		workflowIDs[workflow.WorkflowID] = struct{}{}
		if len(workflow.Steps) == 0 {
			t.Fatalf("workflow %q has no steps", workflow.WorkflowID)
		}
		stepIDs := map[string]struct{}{}
		for _, step := range workflow.Steps {
			if step.StepID == "" {
				t.Fatalf("workflow %q contains a step without stepId", workflow.WorkflowID)
			}
			if _, duplicate := stepIDs[step.StepID]; duplicate {
				t.Fatalf("workflow %q has duplicate stepId %q", workflow.WorkflowID, step.StepID)
			}
			stepIDs[step.StepID] = struct{}{}
			if !strings.HasPrefix(step.OperationID, prefix) {
				t.Fatalf("workflow step does not bind the declared source: %q", step.OperationID)
			}
			operationID := strings.TrimPrefix(step.OperationID, prefix)
			if _, ok := allowed[operationID]; !ok {
				t.Fatalf("workflow references non-agent operation %q", operationID)
			}
		}
	}
	if bytes.Contains(ArazzoYAML, []byte(`"csrfToken"`)) || bytes.Contains(ArazzoYAML, []byte(`"X-CSRF-Token"`)) {
		t.Fatal("Arazzo hard-codes cookie CSRF into workflows that can use bearerAuth")
	}
}

func TestServiceAccountAuthenticationContract(t *testing.T) {
	type operation struct {
		OperationID string                `json:"operationId"`
		Security    []map[string][]string `json:"security"`
		Audience    []string              `json:"x-kuberploy-audience"`
		Parameters  []struct {
			Ref string `json:"$ref"`
		} `json:"parameters"`
	}
	var document struct {
		Security   []map[string][]string           `json:"security"`
		Paths      map[string]map[string]operation `json:"paths"`
		Components struct {
			SecuritySchemes map[string]struct {
				Type   string `json:"type"`
				Scheme string `json:"scheme"`
			} `json:"securitySchemes"`
			Parameters map[string]struct {
				Required    bool   `json:"required"`
				Description string `json:"description"`
			} `json:"parameters"`
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Security) != 2 || len(document.Security[0]) != 1 || len(document.Security[1]) != 1 {
		t.Fatalf("global authentication is not an explicit OR: %#v", document.Security)
	}
	if _, ok := document.Security[0]["cookieAuth"]; !ok {
		t.Fatalf("first global authentication alternative is not cookieAuth: %#v", document.Security)
	}
	if _, ok := document.Security[1]["bearerAuth"]; !ok {
		t.Fatalf("second global authentication alternative is not bearerAuth: %#v", document.Security)
	}
	if bearer := document.Components.SecuritySchemes["bearerAuth"]; bearer.Type != "http" || bearer.Scheme != "bearer" {
		t.Fatalf("unexpected bearer security scheme: %#v", bearer)
	}
	if document.Components.Parameters["CSRFToken"].Required || !strings.Contains(document.Components.Parameters["CSRFToken"].Description, "bearerAuth") {
		t.Fatalf("conditional CSRF parameter is not bearer-aware: %#v", document.Components.Parameters["CSRFToken"])
	}
	if !document.Components.Parameters["SessionCSRFToken"].Required {
		t.Fatal("cookie-only management mutation does not have a required CSRF parameter")
	}

	management := []struct {
		path, method, id string
		mutation         bool
	}{
		{"/v1/workloads/{id}/logs/follow", "get", "followWorkloadLogs", false},
		{"/v1/projects/{id}/service-accounts", "get", "listProjectServiceAccounts", false},
		{"/v1/projects/{id}/service-accounts", "post", "createProjectServiceAccount", true},
		{"/v1/service-accounts/{id}", "delete", "disableServiceAccount", true},
		{"/v1/service-accounts/{id}/tokens", "get", "listServiceAccountTokens", false},
		{"/v1/service-accounts/{id}/tokens", "post", "createServiceAccountToken", true},
		{"/v1/service-accounts/{serviceAccountId}/tokens/{tokenId}", "delete", "revokeServiceAccountToken", true},
	}
	for _, expected := range management {
		actual, ok := document.Paths[expected.path][expected.method]
		if !ok || actual.OperationID != expected.id {
			t.Fatalf("missing management operation %s %s: %#v", expected.method, expected.path, actual)
		}
		if len(actual.Security) != 1 || len(actual.Security[0]) != 1 {
			t.Fatalf("management operation %q is not cookie-only: %#v", expected.id, actual.Security)
		}
		if _, ok := actual.Security[0]["cookieAuth"]; !ok {
			t.Fatalf("management operation %q accepts a non-cookie credential: %#v", expected.id, actual.Security)
		}
		if len(actual.Audience) != 1 || actual.Audience[0] != "human" {
			t.Fatalf("management operation %q leaked beyond human audience: %#v", expected.id, actual.Audience)
		}
		if expected.mutation {
			hasSessionCSRF := false
			for _, parameter := range actual.Parameters {
				hasSessionCSRF = hasSessionCSRF || parameter.Ref == "#/components/parameters/SessionCSRFToken"
			}
			if !hasSessionCSRF {
				t.Fatalf("management mutation %q has no required session CSRF parameter", expected.id)
			}
		}
	}

	for _, schema := range []string{"Principal", "AuthenticationContext", "AutomationName", "AutomationRole", "AutomationScope", "CreateServiceAccount", "ServiceAccount", "CreateServiceAccountToken", "ServiceAccountToken", "ServiceAccountTokenIssue", "ServiceAccountList", "ServiceAccountTokenList"} {
		if len(document.Components.Schemas[schema]) == 0 {
			t.Fatalf("missing service-account schema %q", schema)
		}
	}
	var role struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(document.Components.Schemas["AutomationRole"], &role); err != nil {
		t.Fatal(err)
	}
	if strings.Join(role.Enum, ",") != "viewer,developer,project-admin" {
		t.Fatalf("automation role set is not closed: %#v", role.Enum)
	}
	var scope struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(document.Components.Schemas["AutomationScope"], &scope); err != nil {
		t.Fatal(err)
	}
	if strings.Join(scope.Enum, ",") != "app.read,app.edit,build.create,logs.read" {
		t.Fatalf("automation scope set is not closed: %#v", scope.Enum)
	}
	var name struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(document.Components.Schemas["AutomationName"], &name); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(name.Description, "1-100 UTF-8 bytes") || !strings.Contains(name.Description, "control characters") {
		t.Fatalf("automation name byte/control policy is ambiguous: %q", name.Description)
	}
	var issue struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["ServiceAccountTokenIssue"], &issue); err != nil {
		t.Fatal(err)
	}
	if strings.Join(issue.Required, ",") != "tokenRecord" || len(issue.Properties["token"]) == 0 {
		t.Fatalf("one-time token is not optional on replay: required=%#v properties=%#v", issue.Required, issue.Properties)
	}
}
