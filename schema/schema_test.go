package schema

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestAppConfigSchemaIsValidAndMatchesRuntimeChart(t *testing.T) {
	if !json.Valid(AppConfig) {
		t.Fatal("embedded AppConfig schema is not valid JSON")
	}
	chartSchema, err := os.ReadFile("../charts/kuberploy-runtime/values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err = json.Unmarshal(AppConfig, &schema); err != nil {
		t.Fatal(err)
	}
	var chart map[string]any
	if err = json.Unmarshal(chartSchema, &chart); err != nil {
		t.Fatal(err)
	}
	chartProperties := chart["properties"].(map[string]any)
	for _, operatorOnly := range []string{"values", "kuberployExpectedIdentity"} {
		if _, exists := chartProperties[operatorOnly]; !exists {
			t.Fatalf("runtime chart is missing operator-only %s", operatorOnly)
		}
		delete(chartProperties, operatorOnly)
	}
	required := chart["required"].([]any)
	filtered := required[:0]
	for _, value := range required {
		if value != "kuberployExpectedIdentity" {
			filtered = append(filtered, value)
		}
	}
	chart["required"] = filtered
	if !reflect.DeepEqual(schema, chart) {
		t.Fatal("AppConfig subset and runtime chart schema drifted")
	}
	defs := schema["$defs"].(map[string]any)
	runtime := defs["runtime"].(map[string]any)
	required = runtime["required"].([]any)
	if !contains(required, "resources") {
		t.Fatal("runtime resources must be explicit")
	}
	properties := runtime["properties"].(map[string]any)
	for _, name := range []string{"nodeSelector", "affinity", "topologySpreadConstraints", "tolerations"} {
		if _, exists := properties[name]; !exists {
			t.Fatalf("runtime schema is missing %s", name)
		}
	}
}

func contains(values []any, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
