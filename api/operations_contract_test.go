package api

import (
	"encoding/json"
	"testing"
)

func TestOperationsContractIsBounded(t *testing.T) {
	var document struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	var operation struct {
		Parameters []struct {
			Name   string `json:"name"`
			Schema struct {
				Minimum int `json:"minimum"`
				Maximum int `json:"maximum"`
				Default int `json:"default"`
			} `json:"schema"`
		} `json:"parameters"`
		Responses map[string]json.RawMessage `json:"responses"`
	}
	if err := json.Unmarshal(document.Paths["/v1/operations"]["get"], &operation); err != nil {
		t.Fatal(err)
	}
	if len(operation.Parameters) != 1 || operation.Parameters[0].Name != "limit" ||
		operation.Parameters[0].Schema.Minimum != 1 || operation.Parameters[0].Schema.Maximum != 100 ||
		operation.Parameters[0].Schema.Default != 50 || operation.Responses["422"] == nil {
		t.Fatalf("operations query contract is not bounded: %#v", operation)
	}
	var list struct {
		Required   []string `json:"required"`
		Properties struct {
			Items struct {
				MaxItems int `json:"maxItems"`
			} `json:"items"`
			Truncated struct {
				Type string `json:"type"`
			} `json:"truncated"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["OperationList"], &list); err != nil {
		t.Fatal(err)
	}
	if list.Properties.Items.MaxItems != 100 || list.Properties.Truncated.Type != "boolean" ||
		len(list.Required) != 2 || list.Required[0] != "items" || list.Required[1] != "truncated" {
		t.Fatalf("operations list contract is not bounded: %#v", list)
	}
}
