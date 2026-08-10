package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestImageResolutionContractIsClosedReplayFirstAndDigestOnly(t *testing.T) {
	var document struct {
		Paths map[string]map[string]struct {
			OperationID     string         `json:"operationId"`
			Description     string         `json:"description"`
			AutomationScope string         `json:"x-kuberploy-automation-scope"`
			Audience        []string       `json:"x-kuberploy-audience"`
			Responses       map[string]any `json:"responses"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Required             []string                   `json:"required"`
				Properties           map[string]json.RawMessage `json:"properties"`
				AdditionalProperties any                        `json:"additionalProperties"`
				OneOf                []struct {
					Ref string `json:"$ref"`
				} `json:"oneOf"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	preview := document.Paths["/v1/deployments/image-resolution-preview"]["post"]
	if preview.OperationID != "previewImageResolution" || preview.AutomationScope != "app.edit" || len(preview.Audience) != 2 {
		t.Fatalf("preview metadata=%+v", preview)
	}
	create := document.Paths["/v1/deployments"]["post"]
	for _, truth := range []string{"idempotent receipt", "server selects", "persists/Git-projects only repository@sha256", "direct-or-pull-request"} {
		if !strings.Contains(create.Description, truth) {
			t.Fatalf("create description omits %q: %s", truth, create.Description)
		}
	}
	input := document.Components.Schemas["ImageResolutionPreviewInput"]
	if input.AdditionalProperties != false || len(input.Properties) != 3 || len(input.Required) != 3 {
		t.Fatalf("input is not closed: %+v", input)
	}
	output := document.Components.Schemas["ImageResolutionPreview"]
	if output.AdditionalProperties != false || len(output.Properties) != 3 || len(output.Required) != 3 {
		t.Fatalf("unsafe preview projection: %+v", output)
	}
	createInput := document.Components.Schemas["CreateDeployment"]
	if len(createInput.OneOf) != 2 || createInput.OneOf[0].Ref != "#/components/schemas/CreateImmutableImageDeployment" || createInput.OneOf[1].Ref != "#/components/schemas/CreateTaggedImageDeployment" {
		t.Fatalf("deployment input is not a closed image-mode union: %+v", createInput.OneOf)
	}
	immutableInput := document.Components.Schemas["CreateImmutableImageDeployment"]
	if immutableInput.AdditionalProperties != false || len(immutableInput.Properties) != 5 {
		t.Fatalf("immutable deployment input is not closed: %+v", immutableInput)
	}
	if _, accepted := immutableInput.Properties["expectedImmutableImage"]; accepted {
		t.Fatal("immutable deployment input accepts a tag precondition")
	}
	taggedInput := document.Components.Schemas["CreateTaggedImageDeployment"]
	if taggedInput.AdditionalProperties != false || len(taggedInput.Properties) != 6 || len(taggedInput.Required) != 5 {
		t.Fatalf("tagged deployment input is not closed: %+v", taggedInput)
	}
	if _, required := taggedInput.Properties["expectedImmutableImage"]; !required {
		t.Fatal("tagged deployment input omits the moved-tag precondition")
	}
	for _, forbidden := range []string{"registryTargetId", "profile", "credential", "realm", "challenge", "service"} {
		if _, present := input.Properties[forbidden]; present {
			t.Fatalf("caller authority field %q accepted", forbidden)
		}
		if _, present := output.Properties[forbidden]; present {
			t.Fatalf("private provider field %q projected", forbidden)
		}
	}
	middleware := document.Paths["/v1/middlewares/catalog"]["get"]
	if middleware.OperationID != "listManageableMiddlewareProfiles" || len(middleware.Audience) != 1 || middleware.Audience[0] != "human" || middleware.AutomationScope != "none" {
		t.Fatalf("middleware management catalog metadata=%+v", middleware)
	}
}
