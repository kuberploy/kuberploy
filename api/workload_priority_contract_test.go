package api

import (
	"encoding/json"
	"testing"
)

func TestWorkloadPriorityClassContractRejectsKubernetesSystemClasses(t *testing.T) {
	var document struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	properties := decodeSchemaProperties(t, document.Components.Schemas["WorkloadRuntime"])
	priority, ok := properties["priorityClassName"]
	if !ok {
		t.Fatal("WorkloadRuntime priorityClassName is missing")
	}
	allOf, ok := priority["allOf"].([]any)
	if !ok || len(allOf) != 2 {
		t.Fatalf("priorityClassName does not compose DNS and reserved-name policy: %#v", priority)
	}
	policy, ok := allOf[1].(map[string]any)
	if !ok {
		t.Fatalf("priorityClassName reserved-name policy is malformed: %#v", allOf[1])
	}
	not, ok := policy["not"].(map[string]any)
	if !ok || not["pattern"] != "^system-" {
		t.Fatalf("priorityClassName accepts Kubernetes system classes: %#v", policy)
	}
}
