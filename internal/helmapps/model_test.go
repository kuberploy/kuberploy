package helmapps

import (
	"errors"
	"strings"
	"testing"
)

func TestApprovalDescriptorAndDesiredRenderAreClosedAndContentAddressed(t *testing.T) {
	files := testChartFiles()
	packageBytes := packageChart(t, files)
	approval := testApproval(t, packageBytes, files)
	desired := testDesired(t, approval, []byte("replicas: 2\n"))
	if err := desired.Validate(); err != nil {
		t.Fatalf("valid desired render rejected: %v", err)
	}
	descriptorYAML, err := desired.Descriptor.YAML()
	if err != nil || !equalBytes(descriptorYAML, desired.DescriptorYAML) {
		t.Fatalf("descriptor is not deterministic: %v", err)
	}
	for _, forbidden := range []string{"password", "token", "credential", "postRenderer", "passCredentials"} {
		if strings.Contains(string(descriptorYAML), forbidden) {
			t.Fatalf("descriptor unexpectedly contains %q", forbidden)
		}
	}

	mutated := desired
	mutated.InputDigest = digestBytes([]byte("caller-chosen"))
	if err = mutated.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("caller-chosen input digest accepted: %v", err)
	}
	mutated = desired
	mutated.ValuesYAML = []byte("replicas: 3\n")
	mutated.ValuesDigest = digestBytes(mutated.ValuesYAML)
	if err = mutated.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mutated values accepted without recomputed server input: %v", err)
	}
}

func TestApprovalRejectsFloatingOrNonCanonicalIdentity(t *testing.T) {
	files := testChartFiles()
	approval := testApproval(t, packageChart(t, files), files)
	tests := []func(*Approval){
		func(value *Approval) { value.OCIRepository = "oci://registry.example.com/platform/sample:latest" },
		func(value *Approval) { value.OCIRepository = "oci://Registry.example.com/platform/sample" },
		func(value *Approval) { value.OCIRepository = "https://registry.example.com/platform/sample" },
		func(value *Approval) { value.ChartVersion = "latest" },
		func(value *Approval) { value.ManifestDigest = strings.Repeat("a", 64) },
		func(value *Approval) { value.RendererImage = "docker.io/alpine/helm:4.2.4" },
		func(value *Approval) { value.RendererVersion = "4.2.4" },
		func(value *Approval) { value.PolicyVersion = "external-helm-p0.v2" },
	}
	for index, mutate := range tests {
		candidate := approval
		mutate(&candidate)
		if candidate.Validate() == nil {
			t.Fatalf("mutation %d was accepted", index)
		}
	}
}

func TestParseValuesRejectsAmbiguousCredentialsAndRendererControls(t *testing.T) {
	invalid := []string{
		"replicas: 1\nreplicas: 2\n",
		"base: &base\n  replicas: 1\ncopy: *base\n",
		"value: !custom foo\n",
		"replicas: 1\n---\nreplicas: 2\n",
		"password: hunter2\n",
		"password:\n  value: hunter2\n",
		"auth:\n  token: bearer\n",
		"namespace: ''\n",
		"namespace: attacker\n",
		"postRenderer: ./evil\n",
		"skipSchemaValidation: true\n",
		"passCredentials: true\n",
		"dependencyUpdate: true\n",
		"existingSecret: Not_DNS\n",
	}
	for _, raw := range invalid {
		if _, err := ParseValues([]byte(raw)); !errors.Is(err, ErrUnsafeYAML) {
			t.Fatalf("unsafe values accepted (%q): %v", raw, err)
		}
	}
	parsed, err := ParseValues([]byte("replicas: 2\nexistingSecret: runtime-secret\n"))
	if err != nil || string(parsed.Raw) != "replicas: 2\nexistingSecret: runtime-secret\n" {
		t.Fatalf("safe values rejected: %v", err)
	}
}
