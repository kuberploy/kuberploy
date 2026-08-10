package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildPromotionContractIsHumanOnlyAndServerDerived(t *testing.T) {
	var document struct {
		Paths      map[string]map[string]githubBuildOperation `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	operation := document.Paths["/v1/builds/{id}/promote"]["post"]
	if operation.OperationID != "promoteBuildAttempt" || operation.AutomationScope != "" ||
		!equalStrings(operation.Audience, []string{"human"}) || len(operation.Security) != 1 ||
		len(operation.Security[0]) != 1 || operation.Security[0]["cookieAuth"] == nil {
		t.Fatalf("promotion is not exact cookie-only human operation: %#v", operation)
	}
	properties := decodeSchemaProperties(t, document.Components.Schemas["PromoteBuildAttempt"])
	if len(properties) != 3 || properties["environmentId"]["format"] != "uuid" ||
		properties["runtime"]["$ref"] != "#/components/schemas/WorkloadRuntime" ||
		properties["route"]["$ref"] != "#/components/schemas/CreateRoute" {
		t.Fatalf("promotion request is not the closed deployment intent: %#v", properties)
	}
	for _, forbidden := range []string{"applicationId", "projectId", "image", "releaseId", "registryTargetId", "repository", "digest", "namespace"} {
		if _, leaked := properties[forbidden]; leaked {
			t.Fatalf("promotion request leaks caller authority field %q", forbidden)
		}
	}
	profile, err := BuildAgentProfile(OpenAPIJSON)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(profile), "promoteBuildAttempt") {
		t.Fatal("human-only promotion leaked into the agent profile")
	}
}
