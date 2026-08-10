package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestVariableSetManagementContractIsHumanScopedAndServerDerived(t *testing.T) {
	type operation struct {
		OperationID string                `json:"operationId"`
		Permission  string                `json:"x-kuberploy-permission"`
		Audience    []string              `json:"x-kuberploy-audience"`
		Security    []map[string][]string `json:"security"`
		Parameters  []struct {
			Ref  string `json:"$ref"`
			Name string `json:"name"`
		} `json:"parameters"`
		RequestBody struct {
			Content map[string]struct {
				Schema struct {
					Ref string `json:"$ref"`
				} `json:"schema"`
			} `json:"content"`
		} `json:"requestBody"`
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
	operations := []operation{
		document.Paths["/v1/environments/{id}/variable-sets"]["get"],
		document.Paths["/v1/environments/{id}/variable-sets/{scope}/preview"]["post"],
		document.Paths["/v1/environments/{id}/variable-sets/{scope}"]["put"],
	}
	expectedIDs := []string{"listEnvironmentVariableSets", "previewEnvironmentVariableSet", "saveEnvironmentVariableSet"}
	for index, candidate := range operations {
		if candidate.OperationID != expectedIDs[index] || candidate.Permission != map[int]string{0: "config.read", 1: "config.write", 2: "config.write"}[index] ||
			contains(candidate.Audience, "agent") || !contains(candidate.Audience, "human") || len(candidate.Security) != 1 {
			t.Fatalf("variable operation[%d]=%#v", index, candidate)
		}
		if _, cookieOnly := candidate.Security[0]["cookieAuth"]; !cookieOnly || len(candidate.Security[0]) != 1 {
			t.Fatalf("variable operation accepts non-cookie authority: %#v", candidate.Security)
		}
		for _, parameter := range candidate.Parameters {
			for _, forbidden := range []string{"path", "bindingId", "publicationMode", "targetRef"} {
				if strings.EqualFold(parameter.Name, forbidden) {
					t.Fatalf("%s accepts caller authority %q", candidate.OperationID, parameter.Name)
				}
			}
		}
	}
	for _, candidate := range operations[1:] {
		if candidate.RequestBody.Content["application/json"].Schema.Ref != "#/components/schemas/VariableSetInput" {
			t.Fatalf("%s request=%#v", candidate.OperationID, candidate.RequestBody)
		}
	}
	var input struct {
		Additional bool                       `json:"additionalProperties"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["VariableSetInput"], &input); err != nil {
		t.Fatal(err)
	}
	fields := make([]string, 0, len(input.Properties))
	for name := range input.Properties {
		fields = append(fields, name)
	}
	sort.Strings(fields)
	if input.Additional || strings.Join(fields, ",") != "rawYaml" || strings.Join(input.Required, ",") != "rawYaml" {
		t.Fatalf("VariableSet input fields=%#v required=%#v additional=%t", fields, input.Required, input.Additional)
	}

	profileBytes, err := BuildAgentProfile(OpenAPIJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, operationID := range expectedIDs {
		if strings.Contains(string(profileBytes), `"operationId":"`+operationID+`"`) {
			t.Fatalf("human-only VariableSet operation %q leaked into agent profile", operationID)
		}
	}
}

func TestConfigBundleContractSurfacesSafeVariableSourcesAndPrecedence(t *testing.T) {
	var document struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	var bundle struct {
		Properties map[string]struct {
			Type     string `json:"type"`
			MinItems int    `json:"minItems"`
			MaxItems int    `json:"maxItems"`
			Items    struct {
				Ref string `json:"$ref"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["ConfigBundle"], &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Properties["documents"].MaxItems != 3 || bundle.Properties["variableDependencies"].MinItems != 2 ||
		bundle.Properties["variableDependencies"].MaxItems != 2 || bundle.Properties["effectiveVariables"].MaxItems != 256 {
		t.Fatalf("variable bundle bounds=%#v", bundle.Properties)
	}
	var effective struct {
		Additional bool                       `json:"additionalProperties"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["EffectiveVariable"], &effective); err != nil {
		t.Fatal(err)
	}
	if effective.Additional {
		t.Fatal("effective variable permits unknown fields")
	}
	for _, forbidden := range []string{"secret", "secretValue", "credential", "token", "privateKey"} {
		if _, leaked := effective.Properties[forbidden]; leaked {
			t.Fatalf("effective variable exposes %q", forbidden)
		}
	}
}
