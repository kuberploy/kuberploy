package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDeploymentRedeployContractUsesOnlySavedConfiguration(t *testing.T) {
	var document struct {
		Paths map[string]map[string]struct {
			OperationID string                     `json:"operationId"`
			Description string                     `json:"description"`
			Permission  string                     `json:"x-kuberploy-permission"`
			Effect      string                     `json:"x-kuberploy-effect"`
			Idempotency string                     `json:"x-kuberploy-idempotency"`
			Automation  string                     `json:"x-kuberploy-automation-scope"`
			Audience    []string                   `json:"x-kuberploy-audience"`
			RequestBody json.RawMessage            `json:"requestBody"`
			Responses   map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	operation := document.Paths["/v1/deployments/{id}/redeploy"]["post"]
	if operation.OperationID != "redeployDeployment" || operation.Permission != "resources.write" || operation.Effect != "git-write" ||
		operation.Idempotency != "required" || operation.Automation != "app.edit" || len(operation.RequestBody) != 0 ||
		operation.Responses["202"] == nil || !contains(operation.Audience, "agent") {
		t.Fatalf("redeploy operation drifted: %#v", operation)
	}
	for _, phrase := range []string{"exact server-owned AppConfig", "stopped draft", "without changing configuration"} {
		if !strings.Contains(operation.Description, phrase) {
			t.Fatalf("redeploy description omits %q: %s", phrase, operation.Description)
		}
	}
}
