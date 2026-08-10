package helmapps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"sort"
	"testing"
	"time"
)

const (
	testAdminID       = "11111111-1111-4111-8111-111111111111"
	testApprovalID    = "22222222-2222-4222-8222-222222222222"
	testProjectID     = "33333333-3333-4333-8333-333333333333"
	testEnvironmentID = "44444444-4444-4444-8444-444444444444"
	testApplicationID = "55555555-5555-4555-8555-555555555555"
	testCommandID     = "66666666-6666-4666-8666-666666666666"
	testScopeID       = "77777777-7777-4777-8777-777777777777"
)

var testTime = time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC)

func testChartFiles() map[string][]byte {
	return map[string][]byte{
		"Chart.yaml":                []byte("apiVersion: v2\nname: sample\nversion: 1.2.3\ntype: application\n"),
		"values.yaml":               []byte("replicas: 1\n"),
		"values.schema.json":        []byte(`{"type":"object","additionalProperties":false,"properties":{"replicas":{"type":"integer","minimum":1}},"required":["replicas"]}`),
		"templates/deployment.yaml": []byte("{{ .Values.replicas }}\n"),
	}
}

func packageChart(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		content := files[name]
		fullName := "sample/" + name
		if err := tarWriter.WriteHeader(&tar.Header{Name: fullName, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func testApproval(t *testing.T, packageBytes []byte, files map[string][]byte) Approval {
	t.Helper()
	approval := Approval{
		ApprovalKey:   ApprovalKey{ID: testApprovalID, Revision: 1},
		OCIRepository: "oci://registry.example.com/platform/sample", ChartVersion: "1.2.3",
		ManifestDigest: digestBytes([]byte("oci-manifest")), PackageDigest: digestBytes(packageBytes),
		ValuesSchemaDigest: digestBytes(files["values.schema.json"]), RendererImage: RendererImage,
		RendererVersion: HelmVersion, PolicyVersion: PolicyVersion, CreatedBy: testAdminID,
		IdempotencyKey: "approval-create-0001", CreatedAt: testTime,
	}
	if err := approval.Validate(); err != nil {
		t.Fatalf("invalid test approval: %v", err)
	}
	return approval
}

func testDestination() DestinationIdentity {
	return DestinationIdentity{ProjectID: testProjectID, EnvironmentID: testEnvironmentID,
		ApplicationID: testApplicationID, ApplicationSlug: "sample", Namespace: "project-sample"}
}

func testDesired(t *testing.T, approval Approval, values []byte) DesiredRender {
	t.Helper()
	desired, err := NewDesiredRender(testCommandID, testScopeID, "render-create-0001", approval, testDestination(), values)
	if err != nil {
		t.Fatalf("new desired render: %v", err)
	}
	return desired
}

func validConfigMapManifest(descriptor Descriptor) []byte {
	labels := descriptor.RequiredLabels()
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: sample-config
  namespace: %s
  labels:
    app.kubernetes.io/instance: %s
    app.kubernetes.io/name: %s
    kuberploy.io/application: %s
    kuberploy.io/environment: %s
    kuberploy.io/project: %s
data:
  hello: world
`, descriptor.Destination.Namespace, labels["app.kubernetes.io/instance"], labels["app.kubernetes.io/name"],
		labels["kuberploy.io/application"], labels["kuberploy.io/environment"], labels["kuberploy.io/project"]))
}
