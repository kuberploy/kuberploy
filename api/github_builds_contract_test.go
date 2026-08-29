package api

import (
	"encoding/json"
	"strings"
	"testing"
)

type githubBuildOperation struct {
	OperationID     string                `json:"operationId"`
	Audience        []string              `json:"x-kuberploy-audience"`
	Permission      string                `json:"x-kuberploy-permission"`
	AutomationScope string                `json:"x-kuberploy-automation-scope"`
	Effect          string                `json:"x-kuberploy-effect"`
	Idempotency     string                `json:"x-kuberploy-idempotency"`
	Confirmation    string                `json:"x-kuberploy-confirmation"`
	Security        []map[string][]string `json:"security"`
	Parameters      []struct {
		Name string `json:"name"`
		Ref  string `json:"$ref"`
	} `json:"parameters"`
	Responses map[string]struct {
		Ref string `json:"$ref"`
	} `json:"responses"`
}

func TestGitHubSetupWebhookAndBuildOpenAPIContract(t *testing.T) {
	var document struct {
		OpenAPI    string                                     `json:"openapi"`
		Paths      map[string]map[string]githubBuildOperation `json:"paths"`
		Components struct {
			SecuritySchemes map[string]json.RawMessage `json:"securitySchemes"`
			Schemas         map[string]json.RawMessage `json:"schemas"`
			Responses       map[string]json.RawMessage `json:"responses"`
			Parameters      map[string]json.RawMessage `json:"parameters"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	if document.OpenAPI != "3.2.0" {
		t.Fatalf("OpenAPI version=%q", document.OpenAPI)
	}

	humanSetup := map[string]struct {
		method string
		auth   string
	}{
		"/v1/github/installations/authorize": {method: "post", auth: "cookieAuth"},
		"/v1/github/installations/setup":     {method: "get", auth: "githubSetupSession"},
		"/v1/github/installations/callback":  {method: "get", auth: "githubSetupSession"},
		"/v1/github/installations/link":      {method: "post", auth: "cookieAuth"},
	}
	for path, expected := range humanSetup {
		operation := document.Paths[path][expected.method]
		expectedSchemes := 1
		if path == "/v1/github/installations/link" {
			expectedSchemes = 2
		}
		if len(operation.Security) != 1 || len(operation.Security[0]) != expectedSchemes || operation.Security[0][expected.auth] == nil ||
			!equalStrings(operation.Audience, []string{"human"}) || operation.Permission != "platform.admin" {
			t.Fatalf("setup operation %s %s is not exact cookie-only human: %#v", expected.method, path, operation)
		}
		if path == "/v1/github/installations/link" && operation.Security[0]["githubSetupHandoff"] == nil {
			t.Fatalf("link operation does not require both human session and one-time handoff: %#v", operation.Security)
		}
	}
	if _, ok := document.Components.SecuritySchemes["githubSetupSession"]; !ok {
		t.Fatal("dedicated GitHub browser-flow session scheme is missing")
	}
	if _, ok := document.Components.SecuritySchemes["githubSetupHandoff"]; !ok {
		t.Fatal("dedicated HttpOnly GitHub setup handoff scheme is missing")
	}
	setupReturn := document.Paths["/v1/github/installations/setup"]["get"]
	if _, ok := setupReturn.Responses["303"]; !ok || !equalStrings(parameterNames(setupReturn.Parameters), []string{"state", "installation_id", "setup_action"}) {
		t.Fatalf("setup return is not the exact documented GitHub split: %#v", setupReturn)
	}
	oauthCallback := document.Paths["/v1/github/installations/callback"]["get"]
	if _, redirects := oauthCallback.Responses["303"]; !equalStrings(parameterNames(oauthCallback.Parameters), []string{"state", "code"}) || !redirects {
		t.Fatalf("OAuth callback accepts caller installation identity: %#v", oauthCallback.Parameters)
	}
	if _, leaked := document.Components.Schemas["GitHubSetupCallback"]; leaked {
		t.Fatal("OAuth callback still exposes a JSON handoff schema")
	}
	if _, leaked := document.Components.Schemas["LinkGitHubSetup"]; leaked {
		t.Fatal("link endpoint still accepts a JavaScript-visible handoff body")
	}
	authorizationSchema := decodeSchemaProperties(t, document.Components.Schemas["GitHubSetupAuthorization"])
	if authorizationSchema["authorizationUrl"]["x-kuberploy-sensitive"] != true || authorizationSchema["state"]["x-kuberploy-sensitive"] != true {
		t.Fatalf("one-time setup authorization is not marked sensitive: %#v", authorizationSchema)
	}

	webhook := document.Paths["/v1/webhooks/github"]["post"]
	if len(webhook.Security) != 1 || len(webhook.Security[0]) != 1 || webhook.Security[0]["githubWebhookHMAC"] == nil || webhook.Responses["202"].Ref != "" {
		t.Fatalf("webhook provider-auth contract=%#v", webhook)
	}
	if _, exists := webhook.Responses["429"]; exists {
		t.Fatal("provider-authenticated webhook must not use transport-peer rate limiting")
	}
	requiredWebhookHeaders := map[string]bool{"X-GitHub-Delivery": false, "X-GitHub-Event": false}
	for _, parameter := range webhook.Parameters {
		if _, expected := requiredWebhookHeaders[parameter.Name]; expected {
			requiredWebhookHeaders[parameter.Name] = true
		}
	}
	for header, present := range requiredWebhookHeaders {
		if !present {
			t.Fatalf("webhook missing required header %q", header)
		}
	}
	if _, ok := document.Components.SecuritySchemes["githubWebhookHMAC"]; !ok {
		t.Fatal("GitHub webhook HMAC security scheme is missing")
	}

	expectedBuildScopes := map[string]string{
		"getApplicationBuildSource":    "app.read",
		"listApplicationBuildProfiles": "app.read",
		"putApplicationBuildSource":    "build.create",
		"listApplicationBuilds":        "app.read",
		"getAppBuildSource":            "app.read",
		"createManualBuildAttempt":     "build.create",
		"getBuildAttempt":              "app.read",
		"getBuildAttemptLogs":          "logs.read",
		"cancelBuildAttempt":           "build.create",
		"retryBuildAttempt":            "build.create",
	}
	found := make(map[string]bool, len(expectedBuildScopes))
	for _, pathItem := range document.Paths {
		for _, operation := range pathItem {
			expectedScope, expected := expectedBuildScopes[operation.OperationID]
			if !expected {
				continue
			}
			found[operation.OperationID] = true
			if operation.AutomationScope != expectedScope || !containsString(operation.Audience, "agent") || !containsString(operation.Audience, "human") {
				t.Fatalf("build operation %q scope/audience=%q/%#v", operation.OperationID, operation.AutomationScope, operation.Audience)
			}
		}
	}
	for operationID := range expectedBuildScopes {
		if !found[operationID] {
			t.Fatalf("build operation %q missing", operationID)
		}
	}
	disconnect := document.Paths["/v1/applications/{id}/source/{sourceId}"]["delete"]
	if disconnect.OperationID != "disconnectApplicationBuildSource" || disconnect.Permission != "builds.manage" ||
		disconnect.Effect != "app-source" || disconnect.Idempotency != "required" || disconnect.Confirmation != "dialog" ||
		!equalStrings(disconnect.Audience, []string{"human"}) || disconnect.AutomationScope != "" {
		t.Fatalf("source disconnect contract=%#v", disconnect)
	}
	if _, ok := disconnect.Responses["204"]; !ok {
		t.Fatalf("source disconnect lacks success response: %#v", disconnect.Responses)
	}
	for schemaName, forbidden := range map[string][]string{
		"AppBuildSource":   {"credentialSecret", "execution", "namespace", "podServiceAccount", "builderAgentImage"},
		"BuildAttempt":     {"planRequest", "checkoutRequest", "deliveryClaimKey", "inputDigest", "jobNamespace", "jobName", "cacheCandidate", "logReference", "token", "credentials"},
		"BuildLogSource":   {"namespace", "jobName", "podName", "container", "selector", "uid", "logReference"},
		"BuildLogSnapshot": {"namespace", "jobName", "podName", "container", "selector", "uid", "logReference"},
	} {
		properties := decodeSchemaProperties(t, document.Components.Schemas[schemaName])
		for _, name := range forbidden {
			if _, leaked := properties[name]; leaked {
				t.Fatalf("safe schema %s leaks %q", schemaName, name)
			}
		}
	}
	profileProperties := decodeSchemaProperties(t, document.Components.Schemas["BuildSecretProfile"])
	for _, forbidden := range []string{"path", "secretName", "secretKey", "credential"} {
		if _, leaked := profileProperties[forbidden]; leaked {
			t.Fatalf("build profile schema leaks %q", forbidden)
		}
	}
	createProperties := decodeSchemaProperties(t, document.Components.Schemas["PutAppBuildSource"])
	for _, forbidden := range []string{"secretFiles", "sshFiles"} {
		if _, leaked := createProperties[forbidden]; leaked {
			t.Fatalf("create build schema accepts arbitrary %q", forbidden)
		}
	}

	profileBytes, err := BuildAgentProfile(OpenAPIJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"authorizeGitHubAppInstallation", "continueGitHubAppInstallationSetup", "completeGitHubAppInstallationCallback", "linkVerifiedGitHubAppInstallation", "acceptGitHubWebhook"} {
		if strings.Contains(string(profileBytes), forbidden) {
			t.Fatalf("human/provider operation leaked into agent profile: %s", forbidden)
		}
	}
}

func parameterNames(parameters []struct {
	Name string `json:"name"`
	Ref  string `json:"$ref"`
}) []string {
	names := make([]string, 0, len(parameters))
	for _, parameter := range parameters {
		if parameter.Name != "" {
			names = append(names, parameter.Name)
		}
	}
	return names
}

func TestEveryHighRiskLimitedOperationReferencesStableProblems(t *testing.T) {
	var document struct {
		Paths map[string]map[string]githubBuildOperation `json:"paths"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{}
	for _, operationID := range []string{
		"bootstrapAdministrator", "acceptUserInvitation", "createUserInvitation", "createTeam", "addOrUpdateTeamMember", "removeTeamMember",
		"registerGitHubInstallationMetadata", "updateGitHubInstallationSharing", "authorizeGitHubAppInstallation", "linkVerifiedGitHubAppInstallation",
		"createEnvironmentGitBinding", "createPlatformArgoGitBinding",
		"createProjectAccessGrant", "deleteProjectAccessGrant", "createProjectServiceAccount", "disableServiceAccount", "createServiceAccountToken", "revokeServiceAccountToken",
		"createRuntimeSecretBinding", "rotateRuntimeSecretBinding", "deleteRuntimeSecretBinding", "putApplicationBuildSource", "createManualBuildAttempt", "cancelBuildAttempt", "retryBuildAttempt",
	} {
		expected[operationID] = false
	}
	for _, pathItem := range document.Paths {
		for _, operation := range pathItem {
			if _, tracked := expected[operation.OperationID]; !tracked {
				continue
			}
			expected[operation.OperationID] = true
			if operation.Responses["429"].Ref != "#/components/responses/RateLimitExceeded" || operation.Responses["503"].Ref != "#/components/responses/RateLimitUnavailable" {
				t.Fatalf("high-risk operation %q has unstable limiter responses: %#v", operation.OperationID, operation.Responses)
			}
		}
	}
	for operationID, found := range expected {
		if !found {
			t.Fatalf("high-risk operation %q missing", operationID)
		}
	}
}

func decodeSchemaProperties(t *testing.T, raw json.RawMessage) map[string]map[string]any {
	t.Helper()
	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	return schema.Properties
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
