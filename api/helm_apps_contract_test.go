package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestApprovedHelmApplicationContractIsClosedScopedAndAgentReadable(t *testing.T) {
	type operation struct {
		OperationID string   `json:"operationId"`
		Summary     string   `json:"summary"`
		Permission  string   `json:"x-kuberploy-permission"`
		Effect      string   `json:"x-kuberploy-effect"`
		Automation  string   `json:"x-kuberploy-automation-scope"`
		Audience    []string `json:"x-kuberploy-audience"`
		Description string   `json:"description"`
	}
	var document struct {
		Paths      map[string]map[string]operation `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}

	expected := map[string]struct {
		method, permission, effect, automation string
	}{
		"/v1/applications/{id}/environments/{environmentId}/helm/approvals":        {"get", "helm.read", "read", "app.read"},
		"/v1/applications/{id}/environments/{environmentId}/helm/values-preview":   {"post", "helm.read", "validate", "app.read"},
		"/v1/applications/{id}/environments/{environmentId}/helm/release":          {"get", "helm.read", "read", "app.read"},
		"/v1/applications/{id}/environments/{environmentId}/helm/releases":         {"get", "helm.read", "read", "app.read"},
		"/v1/applications/{id}/environments/{environmentId}/helm/rendered-preview": {"get", "helm.read", "read", "app.read"},
		"/v1/applications/{id}/environments/{environmentId}/helm/release#upsert":   {"put", "helm.deploy", "git-write", "app.edit"},
		"/v1/applications/{id}/environments/{environmentId}/helm/release/retry":    {"post", "helm.retry", "git-write", "app.edit"},
		"/v1/applications/{id}/environments/{environmentId}/helm/release/disable":  {"post", "helm.deploy", "git-delete", "app.edit"},
		"/v1/applications/{id}/environments/{environmentId}/helm/release/rollback": {"post", "helm.rollback", "git-write", "app.edit"},
	}
	operationIDs := make(map[string]struct{}, len(expected))
	for key, want := range expected {
		path := strings.TrimSuffix(key, "#upsert")
		got, ok := document.Paths[path][want.method]
		if !ok {
			t.Fatalf("missing %s %s", want.method, path)
		}
		if got.Permission != want.permission || got.Effect != want.effect || got.Automation != want.automation ||
			!contains(got.Audience, "human") || !contains(got.Audience, "agent") {
			t.Fatalf("%s %s safety metadata=%#v", want.method, path, got)
		}
		operationIDs[got.OperationID] = struct{}{}
	}
	rollback := document.Paths["/v1/applications/{id}/environments/{environmentId}/helm/release/rollback"]["post"]
	if !strings.Contains(rollback.Summary, "new Git intent") || !strings.Contains(rollback.Description, "never rewritten") || !strings.Contains(rollback.Description, "no imperative") {
		t.Fatalf("rollback is not documented as immutable rollback-as-new-intent: summary=%q description=%q", rollback.Summary, rollback.Description)
	}

	assertClosedFields := func(name string, want []string) {
		t.Helper()
		var schema struct {
			Additional bool                       `json:"additionalProperties"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(document.Components.Schemas[name], &schema); err != nil {
			t.Fatal(err)
		}
		fields := make([]string, 0, len(schema.Properties))
		for field := range schema.Properties {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		sort.Strings(want)
		if schema.Additional || strings.Join(fields, ",") != strings.Join(want, ",") {
			t.Fatalf("%s fields=%#v additional=%t", name, fields, schema.Additional)
		}
	}
	assertClosedFields("HelmValuesInput", []string{"approvalId", "approvalRevision", "valuesYaml"})
	assertClosedFields("HelmRollbackInput", []string{"sourceRevisionId"})
	assertClosedFields("CreateHelmApproval", []string{"repository", "version", "manifestDigest", "packageDigest", "valuesSchemaDigest"})
	assertClosedFields("HelmRenderedResource", []string{"apiVersion", "kind", "namespace", "name", "sanitizedYaml", "previewOmitted"})
	assertClosedFields("HelmRenderedManifestPreview", []string{"releaseRevisionId", "generation", "manifestDigest", "inventoryDigest", "resourceCount", "previewBytes", "resources"})

	platformGet := document.Paths["/v1/platform/helm/approvals"]["get"]
	platformPost := document.Paths["/v1/platform/helm/approvals"]["post"]
	if platformGet.OperationID != "listPlatformHelmApprovals" || platformPost.OperationID != "admitPlatformHelmApproval" ||
		platformGet.Permission != "platform.admin" || platformPost.Permission != "platform.admin" ||
		contains(platformGet.Audience, "agent") || contains(platformPost.Audience, "agent") ||
		platformPost.Effect != "helm-approval-admit" || platformPost.Automation != "" {
		t.Fatalf("platform Helm approval boundary drifted: get=%#v post=%#v", platformGet, platformPost)
	}

	var approval struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["HelmApproval"], &approval); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"credential", "credentials", "credentialRef", "password", "token", "username", "secret", "registryAuth"} {
		if _, exposed := approval.Properties[forbidden]; exposed {
			t.Fatalf("approval catalog exposes %q", forbidden)
		}
	}

	profileBytes, err := BuildAgentProfile(OpenAPIJSON)
	if err != nil {
		t.Fatal(err)
	}
	var profile decodedAgentProfile
	if err = json.Unmarshal(profileBytes, &profile); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range profile.Operations {
		delete(operationIDs, candidate.OperationID)
	}
	if len(operationIDs) != 0 {
		t.Fatalf("Helm operations omitted from agent profile: %#v", operationIDs)
	}
}
