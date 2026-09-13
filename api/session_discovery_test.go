package api

import (
	"encoding/json"
	"testing"
)

func TestBrowserSessionDiscoveryContractIsHumanOnlyAndProtectedMeUnchanged(t *testing.T) {
	var contract map[string]any
	if err := json.Unmarshal(OpenAPIJSON, &contract); err != nil {
		t.Fatal(err)
	}
	paths := contract["paths"].(map[string]any)
	op := paths["/v1/auth/session"].(map[string]any)["get"].(map[string]any)
	if len(op["security"].([]any)) != 0 || op["operationId"] != "discoverBrowserSession" {
		t.Fatal("discovery must be public and optional")
	}
	audience := op["x-kuberploy-audience"].([]any)
	if len(audience) != 1 || audience[0] != "human" {
		t.Fatal("discovery must be browser-only")
	}
	responses := op["responses"].(map[string]any)
	if responses["200"] == nil || responses["401"] == nil || responses["503"] == nil {
		t.Fatal("discovery response cases missing")
	}
	me := paths["/v1/me"].(map[string]any)["get"].(map[string]any)
	if _, public := me["security"]; public || me["responses"].(map[string]any)["401"] == nil {
		t.Fatal("protected me authentication changed")
	}
	schemas := contract["components"].(map[string]any)["schemas"].(map[string]any)
	if schemas["Principal"].(map[string]any)["properties"].(map[string]any)["email"] == nil {
		t.Fatal("principal must describe its existing email field")
	}
	response := schemas["SessionDiscoveryResponse"].(map[string]any)
	variants := response["properties"].(map[string]any)["principal"].(map[string]any)["oneOf"].([]any)
	if variants[0].(map[string]any)["type"] != "null" {
		t.Fatal("anonymous principal must be nullable")
	}
	human := variants[1].(map[string]any)["allOf"].([]any)[1].(map[string]any)
	authentication := human["properties"].(map[string]any)["authentication"].(map[string]any)
	if authentication["properties"].(map[string]any)["kind"].(map[string]any)["const"] != "session" {
		t.Fatal("discovery must not advertise a service-account principal")
	}
	raw, err := BuildAgentProfile(OpenAPIJSON)
	if err != nil {
		t.Fatal(err)
	}
	var profile decodedAgentProfile
	if err := json.Unmarshal(raw, &profile); err != nil {
		t.Fatal(err)
	}
	for _, operation := range profile.Operations {
		if operation.OperationID == "discoverBrowserSession" {
			t.Fatal("browser discovery leaked into agent profile")
		}
	}
}
