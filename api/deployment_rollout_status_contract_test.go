package api

import (
	"encoding/json"
	"testing"
)

func TestDeploymentStatusSeparatesArgoRolloutObservation(t *testing.T) {
	var document struct {
		Paths map[string]map[string]struct {
			Responses map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	properties := decodeSchemaProperties(t, document.Components.Schemas["DeploymentStatus"])
	for _, field := range []string{"argoSyncStatus", "rolloutHealth", "argoObservedRevision", "argoObservedAt", "desiredReplicas", "readyReplicas", "rolloutConditions", "rolloutObservedAt"} {
		if _, ok := properties[field]; !ok {
			t.Fatalf("DeploymentStatus missing %s", field)
		}
	}
	if _, exists := properties["argoApplicationId"]; exists {
		t.Fatal("rollout status exposes internal Argo identity")
	}
	if _, exists := document.Paths["/v1/deployments/{id}/status"]["get"].Responses["200"]; !exists {
		t.Fatal("deployment status response missing")
	}
}

func TestCapabilitiesExposeExplicitRuntimeFeatureStates(t *testing.T) {
	var document struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	properties := decodeSchemaProperties(t, document.Components.Schemas["Capabilities"])
	if _, ok := properties["featureStates"]; !ok {
		t.Fatal("Capabilities missing explicit disabled/unavailable/healthy feature states")
	}
}
