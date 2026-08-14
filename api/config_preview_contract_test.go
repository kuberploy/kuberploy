package api

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/kuberploy/kuberploy/internal/appconfigpreview"
)

func TestAppConfigRenderIdentityNamesExactRendererImage(t *testing.T) {
	var document struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	properties := decodeSchemaProperties(t, document.Components.Schemas["AppConfigRenderIdentity"])
	renderer, ok := properties["rendererImage"]
	if !ok || renderer["const"] != appconfigpreview.RendererImage {
		t.Fatalf("AppConfig renderer identity contract=%#v runtime=%q", renderer, appconfigpreview.RendererImage)
	}
	version, ok := properties["rendererVersion"]
	pattern, patternOK := version["pattern"].(string)
	if !ok || !patternOK || regexp.MustCompile(pattern).FindString(appconfigpreview.RendererVersion) != appconfigpreview.RendererVersion {
		t.Fatalf("AppConfig renderer version contract=%#v runtime=%q", version, appconfigpreview.RendererVersion)
	}
}

func TestTopologySpreadMinDomainsContractRequiresDoNotSchedule(t *testing.T) {
	var document struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	var constraint struct {
		AllOf []struct {
			If struct {
				Required []string `json:"required"`
			} `json:"if"`
			Then struct {
				Properties map[string]map[string]any `json:"properties"`
			} `json:"then"`
		} `json:"allOf"`
	}
	if err := json.Unmarshal(document.Components.Schemas["TopologySpreadConstraint"], &constraint); err != nil {
		t.Fatal(err)
	}
	if len(constraint.AllOf) != 1 || len(constraint.AllOf[0].If.Required) != 1 ||
		constraint.AllOf[0].If.Required[0] != "minDomains" ||
		constraint.AllOf[0].Then.Properties["whenUnsatisfiable"]["const"] != "DoNotSchedule" {
		t.Fatalf("OpenAPI minDomains semantic fence drifted: %#v", constraint.AllOf)
	}
}
