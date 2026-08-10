package helmapps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestRenderedResourceInventoryNeverExposesManifestLeaves(t *testing.T) {
	files := testChartFiles()
	approval := testApproval(t, packageChart(t, files), files)
	descriptor, err := NewDescriptor(approval, testDestination())
	if err != nil {
		t.Fatal(err)
	}
	labels := descriptor.RequiredLabels()
	raw := []byte(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: preview-safe
  namespace: %s
  labels:
    app.kubernetes.io/instance: %s
    app.kubernetes.io/name: %s
    kuberploy.io/application: %s
    kuberploy.io/environment: %s
    kuberploy.io/project: %s
  annotations:
    internal.example.com/git-commit: caller-must-not-see-this
data:
  password: caller-must-not-see-this
binaryData:
  token: Y2FsbGVyLW11c3Qtbm90LXNlZS10aGlz
`, descriptor.Destination.Namespace, labels["app.kubernetes.io/instance"],
		labels["app.kubernetes.io/name"], labels["kuberploy.io/application"],
		labels["kuberploy.io/environment"], labels["kuberploy.io/project"]))
	validated, err := ValidateRenderedManifests(raw, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	resources, previewBytes, err := renderedResourceInventory(validated.Raw, descriptor.Destination.Namespace)
	if err != nil || len(resources) != 1 || resources[0].Kind != "ConfigMap" ||
		resources[0].Name != "preview-safe" || previewBytes != len(resources[0].SanitizedYAML) ||
		resources[0].PreviewOmitted {
		t.Fatalf("inventory=%+v err=%v", resources, err)
	}
	encoded, err := json.Marshal(RenderedManifestPreview{ManifestDigest: validated.ManifestDigest,
		InventoryDigest: validated.InventoryDigest, ResourceCount: validated.ResourceCount,
		Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"caller-must-not-see-this", "password", "git-commit", "token"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("preview leaked %q: %s", forbidden, encoded)
		}
	}
	if !strings.Contains(resources[0].SanitizedYAML, "binaryData: '[REDACTED]'") ||
		!strings.Contains(resources[0].SanitizedYAML, "annotations: '[REDACTED]'") {
		t.Fatalf("preview did not clearly mark redactions: %s", resources[0].SanitizedYAML)
	}
}

func TestSanitizedResourceYAMLRedactsCredentialSurfacesDeterministically(t *testing.T) {
	raw := `status:
  password: status-must-not-appear
spec:
  template:
    spec:
      containers:
      - env:
        - name: ORDINARY
          value: env-must-not-appear
        - name: FROM_SECRET
          valueFrom:
            secretKeyRef:
              key: password
              name: safe-reference-name
        args: ["--token=arg-must-not-appear", "--listen=:8080"]
        image: registry.example.test/team/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
metadata:
# comment-must-not-appear
  annotations:
    example.test/credential: annotation-must-not-appear
  labels:
    example.test/raw: label-must-not-appear
  uid: uid-must-not-appear
  namespace: preview
  name: api
kind: Deployment
apiVersion: apps/v1
`
	var document yaml.Node
	if err := yaml.NewDecoder(strings.NewReader(raw)).Decode(&document); err != nil {
		t.Fatal(err)
	}
	first, err := sanitizedResourceYAML(document.Content[0], "Deployment")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sanitizedResourceYAML(document.Content[0], "Deployment")
	if err != nil || first != second {
		t.Fatalf("sanitization is not deterministic: err=%v\nfirst=%s\nsecond=%s", err, first, second)
	}
	for _, forbidden := range []string{"status-must-not-appear", "env-must-not-appear",
		"arg-must-not-appear", "annotation-must-not-appear", "comment-must-not-appear",
		"label-must-not-appear", "uid-must-not-appear"} {
		if strings.Contains(first, forbidden) {
			t.Fatalf("sanitized YAML leaked %q: %s", forbidden, first)
		}
	}
	for _, required := range []string{"apiVersion: apps/v1", "kind: Deployment",
		"image: registry.example.test", "name: safe-reference-name", redactedPreviewValue} {
		if !strings.Contains(first, required) {
			t.Fatalf("sanitized YAML omitted safe/marker %q: %s", required, first)
		}
	}
	if bytes.Index([]byte(first), []byte("apiVersion:")) > bytes.Index([]byte(first), []byte("kind:")) {
		t.Fatalf("mapping keys were not canonicalized: %s", first)
	}
}

func TestSanitizedResourceYAMLOversizeCanBeOmittedWithoutRawFallback(t *testing.T) {
	raw := "apiVersion: example.test/v1\nkind: Large\nmetadata:\n  name: large\n  namespace: preview\nspec:\n  ordinary: " +
		strings.Repeat("x", MaximumSanitizedResourcePreviewBytes+1) + "\n"
	var document yaml.Node
	if err := yaml.NewDecoder(strings.NewReader(raw)).Decode(&document); err != nil {
		t.Fatal(err)
	}
	preview, err := sanitizedResourceYAML(document.Content[0], "Large")
	if err != nil || len(preview) <= MaximumSanitizedResourcePreviewBytes {
		t.Fatalf("preview bytes=%d err=%v", len(preview), err)
	}
	resource := RenderedResourcePreview{SanitizedYAML: preview}
	if len(resource.SanitizedYAML) > MaximumSanitizedResourcePreviewBytes {
		resource.SanitizedYAML = ""
		resource.PreviewOmitted = true
	}
	if !resource.PreviewOmitted || resource.SanitizedYAML != "" {
		t.Fatalf("oversized resource was not fail-closed: %+v", resource)
	}
}
