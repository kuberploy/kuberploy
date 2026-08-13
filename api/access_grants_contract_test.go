package api

import (
	"encoding/json"
	"testing"
)

func TestAccessGrantSubjectsAreExactlyOneUserOrTeam(t *testing.T) {
	var document struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"AccessGrant", "CreateAccessGrant"} {
		var schema struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
			OneOf      []struct {
				Required []string `json:"required"`
				Not      struct {
					Required []string `json:"required"`
				} `json:"not"`
			} `json:"oneOf"`
		}
		if err := json.Unmarshal(document.Components.Schemas[name], &schema); err != nil {
			t.Fatal(err)
		}
		if _, ok := schema.Properties["subjectUserId"]; !ok {
			t.Fatalf("%s omits user subject", name)
		}
		if _, ok := schema.Properties["subjectTeamId"]; !ok {
			t.Fatalf("%s omits team subject", name)
		}
		if len(schema.OneOf) != 2 || schema.OneOf[0].Required[0] != "subjectUserId" || schema.OneOf[0].Not.Required[0] != "subjectTeamId" || schema.OneOf[1].Required[0] != "subjectTeamId" || schema.OneOf[1].Not.Required[0] != "subjectUserId" {
			t.Fatalf("%s does not require exactly one closed subject: %#v", name, schema.OneOf)
		}
	}
}
