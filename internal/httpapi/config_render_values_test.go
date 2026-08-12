package httpapi

import (
	"encoding/json"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestEffectiveRenderValuesPreservesNumericScalars(t *testing.T) {
	parsed := map[string]any{
		"spec": map[string]any{
			"replicas": json.Number("2"),
			"weight":   json.Number("12.5"),
			"ports": []any{
				map[string]any{"containerPort": json.Number("8080")},
			},
		},
	}
	raw, err := effectiveRenderValues(parsed, nil)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err = yaml.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	spec, _ := decoded["spec"].(map[string]any)
	ports, _ := spec["ports"].([]any)
	port, _ := ports[0].(map[string]any)
	if spec["replicas"] != 2 || spec["weight"] != 12.5 || port["containerPort"] != 8080 {
		t.Fatalf("render values changed numeric types: %#v", decoded)
	}
}
