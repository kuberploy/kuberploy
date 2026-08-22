package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnvironmentCloneOpenAPIContractIsDraftOnlyAndProjectScoped(t *testing.T) {
	var document struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Description string `json:"description"`
			Permission  string `json:"x-kuberploy-permission"`
			Idempotency string `json:"x-kuberploy-idempotency"`
			Automation  string `json:"x-kuberploy-automation-scope"`
			RequestBody struct {
				Content map[string]struct {
					Schema map[string]any `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
			Responses map[string]struct {
				Description string `json:"description"`
				Content     map[string]struct {
					Schema map[string]any `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Required   []string                  `json:"required"`
				Properties map[string]map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	operation := document.Paths["/v1/environments/{id}/clone"]["post"]
	if operation.OperationID != "cloneEnvironment" || operation.Permission != "resources.write" || operation.Idempotency != "required" || operation.Automation != "app.edit" {
		t.Fatalf("clone operation=%#v", operation)
	}
	for _, required := range []string{"environments:create", "source project", "no deployment", "copies no secret value"} {
		if !strings.Contains(operation.Description, required) {
			t.Fatalf("clone description omits %q: %q", required, operation.Description)
		}
	}
	if operation.RequestBody.Content["application/json"].Schema["$ref"] != "#/components/schemas/CloneEnvironment" ||
		operation.Responses["201"].Content["application/json"].Schema["$ref"] != "#/components/schemas/EnvironmentCloneResult" {
		t.Fatalf("clone schemas request=%#v response=%#v", operation.RequestBody, operation.Responses["201"])
	}
	request := document.Components.Schemas["CloneEnvironment"]
	if len(request.Required) != 1 || request.Required[0] != "name" || request.Properties["projectId"] != nil || request.Properties["namespace"] != nil {
		t.Fatalf("clone input=%#v", request)
	}
	placement := document.Components.Schemas["EnvironmentAppPlacement"]
	for _, field := range []string{"projectId", "environmentId", "applicationId", "applicationName", "applicationSlug", "state", "desiredState", "createdAt", "updatedAt"} {
		if placement.Properties[field] == nil {
			t.Fatalf("placement omits %s: %#v", field, placement)
		}
	}
	for _, forbidden := range []string{"deploymentId", "image", "runtime", "environment", "secret", "secretValue"} {
		if placement.Properties[forbidden] != nil {
			t.Fatalf("placement exposes %s", forbidden)
		}
	}
	createApplication := document.Components.Schemas["CreateApplication"]
	if createApplication.Properties["environmentId"]["format"] != "uuid" ||
		!strings.Contains(document.Paths["/v1/applications"]["post"].Responses["201"].Description, "stopped draft placement") {
		t.Fatalf("CreateApplication does not document atomic Environment placement: schema=%#v response=%#v",
			createApplication, document.Paths["/v1/applications"]["post"].Responses["201"])
	}
	for _, required := range createApplication.Required {
		if required == "environmentId" {
			t.Fatal("CreateApplication environmentId must remain optional")
		}
	}
}
