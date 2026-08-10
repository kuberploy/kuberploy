package helmapps

import (
	"errors"
	"strings"
	"testing"
)

func TestInspectChartPackageAndMergedValuesSchema(t *testing.T) {
	files := testChartFiles()
	packageBytes := packageChart(t, files)
	approval := testApproval(t, packageBytes, files)
	bundle, err := InspectChartPackage(approval, packageBytes)
	if err != nil {
		t.Fatalf("inspect valid chart: %v", err)
	}
	if bundle.ChartName != "sample" || bundle.ChartVersion != "1.2.3" || bundle.FileCount != len(files) {
		t.Fatalf("unexpected chart bundle: %+v", bundle)
	}
	// The required field is supplied by values.yaml. Helm validates the merged
	// values tree, not only the caller override document.
	desired := testDesired(t, approval, []byte("{}\n"))
	if _, err = NewRenderPlan(approval, desired, bundle); err != nil {
		t.Fatalf("defaults plus empty override should satisfy schema: %v", err)
	}
	invalid := testDesired(t, approval, []byte("replicas: 0\n"))
	if _, err = NewRenderPlan(approval, invalid, bundle); !errors.Is(err, ErrUnsafeYAML) {
		t.Fatalf("schema-invalid merged values accepted: %v", err)
	}
}

func TestRequiredSchemaValueMayBeSuppliedByTheSingleOverrideDocument(t *testing.T) {
	files := testChartFiles()
	files["values.yaml"] = []byte("{}\n")
	packageBytes := packageChart(t, files)
	approval := testApproval(t, packageBytes, files)
	bundle, err := InspectChartPackage(approval, packageBytes)
	if err != nil {
		t.Fatalf("chart with caller-required value rejected at approval: %v", err)
	}
	desired := testDesired(t, approval, []byte("replicas: 2\n"))
	if _, err = NewRenderPlan(approval, desired, bundle); err != nil {
		t.Fatalf("required override did not satisfy merged schema: %v", err)
	}
}

func TestInspectChartPackageRejectsUnsafeHelmFeatures(t *testing.T) {
	tests := map[string]func(map[string][]byte){
		"dependencies": func(files map[string][]byte) {
			files["Chart.yaml"] = []byte("apiVersion: v2\nname: sample\nversion: 1.2.3\ndependencies: []\n")
		},
		"crds":     func(files map[string][]byte) { files["crds/widget.yaml"] = []byte("kind: CustomResourceDefinition\n") },
		"subchart": func(files map[string][]byte) { files["charts/child.tgz"] = []byte("not-used") },
		"lookup": func(files map[string][]byte) {
			files["templates/deployment.yaml"] = []byte(`{{ lookup "v1" "Secret" "default" "x" }}`)
		},
		"caller template evaluation": func(files map[string][]byte) {
			files["templates/deployment.yaml"] = []byte(`{{ tpl .Values.template . }}`)
		},
		"network function": func(files map[string][]byte) {
			files["templates/deployment.yaml"] = []byte(`{{ getHostByName "example.com" }}`)
		},
		"random function": func(files map[string][]byte) { files["templates/deployment.yaml"] = []byte(`{{ randBytes 8 }}`) },
		"remote schema ref": func(files map[string][]byte) {
			files["values.schema.json"] = []byte(`{"type":"object","additionalProperties":false,"properties":{"x":{"$ref":"https://example.com/schema.json"}}}`)
		},
		"open schema": func(files map[string][]byte) {
			files["values.schema.json"] = []byte(`{"type":"object","additionalProperties":true}`)
		},
		"duplicate schema key": func(files map[string][]byte) {
			files["values.schema.json"] = []byte(`{"type":"object","type":"object","additionalProperties":false}`)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			files := testChartFiles()
			mutate(files)
			packageBytes := packageChart(t, files)
			approval := testApproval(t, packageBytes, files)
			if _, err := InspectChartPackage(approval, packageBytes); !errors.Is(err, ErrUnsafeChart) {
				t.Fatalf("unsafe chart accepted: %v", err)
			}
		})
	}
}

func TestInspectChartPackageRequiresExactPackageSchemaNameAndVersion(t *testing.T) {
	files := testChartFiles()
	packageBytes := packageChart(t, files)
	approval := testApproval(t, packageBytes, files)
	mutations := []func(*Approval){
		func(value *Approval) { value.PackageDigest = digestBytes([]byte("another-package")) },
		func(value *Approval) { value.ValuesSchemaDigest = digestBytes([]byte("another-schema")) },
		func(value *Approval) { value.ChartVersion = "1.2.4" },
		func(value *Approval) {
			value.OCIRepository = strings.Replace(value.OCIRepository, "/sample", "/other", 1)
		},
	}
	for index, mutate := range mutations {
		candidate := approval
		mutate(&candidate)
		if _, err := InspectChartPackage(candidate, packageBytes); !errors.Is(err, ErrUnsafeChart) {
			t.Fatalf("identity mutation %d accepted: %v", index, err)
		}
	}
}
