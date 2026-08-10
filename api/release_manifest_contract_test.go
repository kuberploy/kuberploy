package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAPIReleaseManifestRequiresCanonicalComponentCharts(t *testing.T) {
	var document struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	var artifacts struct {
		Required   []string `json:"required"`
		Properties struct {
			ComponentCharts struct {
				MinItems    int `json:"minItems"`
				MaxItems    int `json:"maxItems"`
				PrefixItems []struct {
					AllOf []struct {
						Properties struct {
							Name struct {
								Const string `json:"const"`
							} `json:"name"`
						} `json:"properties"`
					} `json:"allOf"`
				} `json:"prefixItems"`
				Items json.RawMessage `json:"items"`
			} `json:"componentCharts"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["ManifestArtifacts"], &artifacts); err != nil {
		t.Fatal(err)
	}
	if strings.Join(artifacts.Required, ",") != "images,chart,componentCharts" {
		t.Fatalf("ManifestArtifacts required=%#v", artifacts.Required)
	}
	charts := artifacts.Properties.ComponentCharts
	if charts.MinItems != 12 || charts.MaxItems != 12 || string(charts.Items) != "false" || len(charts.PrefixItems) != 12 {
		t.Fatalf("componentCharts cardinality/order is not closed: %#v", charts)
	}
	want := []string{"kuberploy-argocd", "kuberploy-builder", "kuberploy-cert-manager", "kuberploy-edge", "kuberploy-external-dns", "kuberploy-external-secrets", "kuberploy-monitoring", "kuberploy-postgresql", "kuberploy-registry", "kuberploy-runtime", "kuberploy-sealed-secrets", "kuberploy-valkey"}
	for index, item := range charts.PrefixItems {
		if len(item.AllOf) != 2 || item.AllOf[1].Properties.Name.Const != want[index] {
			t.Fatalf("component chart index %d=%#v, want %q", index, item, want[index])
		}
	}
}
