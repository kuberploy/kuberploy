package api

import (
	"encoding/json"
	"testing"
)

func TestProjectApplicationAndEnvironmentDeletionContract(t *testing.T) {
	type operation struct {
		OperationID     string `json:"operationId"`
		AutomationScope string `json:"x-kuberploy-automation-scope"`
		Idempotency     string `json:"x-kuberploy-idempotency"`
		Parameters      []struct {
			Ref string `json:"$ref"`
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
		Paths map[string]map[string]operation `json:"paths"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	for path, operationID := range map[string]string{
		"/v1/projects/{id}":     "deleteProject",
		"/v1/applications/{id}": "deleteApplication",
		"/v1/environments/{id}": "deleteEnvironment",
	} {
		operation := document.Paths[path]["delete"]
		if operation.OperationID != operationID || operation.AutomationScope != "app.edit" || operation.Idempotency != "required" {
			t.Fatalf("%s deletion contract is incomplete: %#v", path, operation)
		}
		refs := map[string]bool{}
		for _, parameter := range operation.Parameters {
			refs[parameter.Ref] = true
		}
		if !refs["#/components/parameters/ResourceId"] || !refs["#/components/parameters/IdempotencyKey"] || !refs["#/components/parameters/CSRFToken"] {
			t.Fatalf("%s deletion omits command protections: %#v", path, refs)
		}
		if got := operation.RequestBody.Content["application/json"].Schema.Ref; got != "#/components/schemas/DeleteNamedResource" {
			t.Fatalf("%s confirmation schema=%q", path, got)
		}
	}

	profile, err := BuildAgentProfile(OpenAPIJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, operationID := range []string{"deleteProject", "deleteApplication", "deleteEnvironment"} {
		if !containsJSONOperation(profile, operationID) {
			t.Fatalf("automation operation %q missing from agent allowlist", operationID)
		}
	}
}

func TestDeploymentStopContract(t *testing.T) {
	type operation struct {
		OperationID     string `json:"operationId"`
		AutomationScope string `json:"x-kuberploy-automation-scope"`
		Effect          string `json:"x-kuberploy-effect"`
		Idempotency     string `json:"x-kuberploy-idempotency"`
		Parameters      []struct {
			Ref string `json:"$ref"`
		} `json:"parameters"`
		Responses map[string]json.RawMessage `json:"responses"`
	}
	var document struct {
		Paths map[string]map[string]operation `json:"paths"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	stop := document.Paths["/v1/deployments/{id}"]["delete"]
	if stop.OperationID != "stopDeployment" || stop.AutomationScope != "app.edit" || stop.Effect != "git-write" || stop.Idempotency != "required" || stop.Responses["202"] == nil {
		t.Fatalf("deployment stop contract=%#v", stop)
	}
	refs := map[string]bool{}
	for _, parameter := range stop.Parameters {
		refs[parameter.Ref] = true
	}
	if !refs["#/components/parameters/ResourceId"] || !refs["#/components/parameters/IdempotencyKey"] || !refs["#/components/parameters/CSRFToken"] {
		t.Fatalf("deployment stop protections=%#v", refs)
	}
	profile, err := BuildAgentProfile(OpenAPIJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !containsJSONOperation(profile, "stopDeployment") {
		t.Fatal("deployment stop missing from agent allowlist")
	}
}
