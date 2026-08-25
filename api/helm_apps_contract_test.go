package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestDirectHelmApplicationContractIsClosedScopedAndAgentReadable(t *testing.T) {
	type operation struct {
		OperationID string   `json:"operationId"`
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
		"/v1/applications/{id}/environments/{environmentId}/helm/release":          {"get", "helm.read", "read", "app.read"},
		"/v1/applications/{id}/environments/{environmentId}/helm/releases":         {"get", "helm.read", "read", "app.read"},
		"/v1/applications/{id}/environments/{environmentId}/helm/release#upsert":   {"put", "helm.deploy", "argo-application-write", "app.edit"},
		"/v1/applications/{id}/environments/{environmentId}/helm/release/retry":    {"post", "helm.retry", "argo-application-write", "app.edit"},
		"/v1/applications/{id}/environments/{environmentId}/helm/release/disable":  {"post", "helm.deploy", "argo-application-delete", "app.edit"},
		"/v1/applications/{id}/environments/{environmentId}/helm/release/rollback": {"post", "helm.rollback", "argo-application-write", "app.edit"},
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
	if !strings.Contains(rollback.Description, "new immutable desired revision") || !strings.Contains(rollback.Description, "never rewritten") {
		t.Fatalf("rollback is not documented as immutable rollback-as-new-revision: %q", rollback.Description)
	}

	for _, removed := range []string{
		"/v1/applications/{id}/environments/{environmentId}/helm/approvals",
		"/v1/applications/{id}/environments/{environmentId}/helm/values-preview",
		"/v1/applications/{id}/environments/{environmentId}/helm/rendered-preview",
		"/v1/platform/helm/approvals",
	} {
		if _, exists := document.Paths[removed]; exists {
			t.Fatalf("removed Helm approval path remains: %s", removed)
		}
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
	assertClosedFields("HelmValuesInput", []string{"source", "valuesYaml"})
	assertClosedFields("HelmRollbackInput", []string{"sourceRevisionId"})
	assertClosedFields("HelmReleaseRevision", []string{"action", "createdAt", "desiredEnabled", "failureCode", "generation", "id", "parentRevisionId", "releaseName", "requestId", "rollbackSourceRevisionId", "source", "state", "updatedAt", "valuesDigest", "valuesYaml"})

	var sourceUnion struct {
		OneOf []struct {
			Additional bool                       `json:"additionalProperties"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"oneOf"`
	}
	if err := json.Unmarshal(document.Components.Schemas["HelmSource"], &sourceUnion); err != nil || len(sourceUnion.OneOf) != 3 {
		t.Fatalf("HelmSource union: variants=%d err=%v", len(sourceUnion.OneOf), err)
	}
	wantSourceFields := [][]string{
		{"kind", "repositoryUrl", "chart", "targetRevision"},
		{"kind", "repositoryUrl", "chart", "targetRevision"},
		{"kind", "repositoryUrl", "path", "targetRevision"},
	}
	for index, variant := range sourceUnion.OneOf {
		fields := make([]string, 0, len(variant.Properties))
		for field := range variant.Properties {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		sort.Strings(wantSourceFields[index])
		if variant.Additional || strings.Join(fields, ",") != strings.Join(wantSourceFields[index], ",") {
			t.Fatalf("HelmSource variant %d fields=%#v additional=%t", index, fields, variant.Additional)
		}
	}

	for _, removed := range []string{"CreateHelmApproval", "HelmApproval", "HelmValuesPreview", "HelmRenderedManifestPreview"} {
		if _, exists := document.Components.Schemas[removed]; exists {
			t.Fatalf("removed Helm approval schema remains: %s", removed)
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
