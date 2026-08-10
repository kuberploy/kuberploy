package helmapps

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/id"
)

func TestPreviewApprovalValuesValidatesMergedSchemaAndProducesBoundedSemanticDiff(t *testing.T) {
	document := releaseApprovalDocumentFixture(t)
	current := []byte("image:\n  tag: 1.1.0\nobsolete: true\nreplicaCount: 1\n")
	proposed := []byte("replicaCount: 2\nimage:\n  tag: 1.2.0")
	preview, err := PreviewApprovalValues(document, current, proposed)
	if err != nil {
		t.Fatal(err)
	}
	if preview.NormalizedValuesYAML != string(proposed)+"\n" ||
		preview.ValuesDigest != digestBytes([]byte(preview.NormalizedValuesYAML)) ||
		preview.CurrentValuesDigest != digestBytes(current) {
		t.Fatalf("unexpected normalized preview: %#v", preview)
	}
	wantPaths := []string{"/image/tag", "/obsolete", "/replicaCount"}
	if len(preview.ChangedPaths) != len(wantPaths) {
		t.Fatalf("changed paths=%#v", preview.ChangedPaths)
	}
	for index := range wantPaths {
		if preview.ChangedPaths[index] != wantPaths[index] {
			t.Fatalf("changed paths=%#v want=%#v", preview.ChangedPaths, wantPaths)
		}
	}
	var effective map[string]any
	if err = json.Unmarshal(preview.EffectiveValues, &effective); err != nil {
		t.Fatal(err)
	}
	image, ok := effective["image"].(map[string]any)
	if !ok || image["repository"] != "registry.example.com/platform/sample" || image["tag"] != "1.2.0" ||
		effective["replicaCount"] != float64(2) {
		t.Fatalf("effective merged values=%#v", effective)
	}
}

func TestPreviewApprovalValuesRejectsAmbiguousOrSchemaInvalidAdvancedYAML(t *testing.T) {
	document := releaseApprovalDocumentFixture(t)
	for name, raw := range map[string][]byte{
		"duplicate":  []byte("replicaCount: 1\nreplicaCount: 2\n"),
		"alias":      []byte("image: &image\n  tag: 1.2.0\ncopy: *image\n"),
		"multi-doc":  []byte("replicaCount: 1\n---\nreplicaCount: 2\n"),
		"wrong type": []byte("replicaCount: many\n"),
		"credential": []byte("password: caller-controlled\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PreviewApprovalValues(document, nil, raw); !errors.Is(err, ErrUnsafeYAML) {
				t.Fatalf("unsafe values error=%v", err)
			}
		})
	}
}

func releaseApprovalDocumentFixture(t *testing.T) ApprovalDocument {
	t.Helper()
	now := time.Now().UTC()
	schema := []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"properties":{"replicaCount":{"type":"integer","minimum":1},"image":{"type":"object","additionalProperties":false,"properties":{"repository":{"type":"string"},"tag":{"type":"string"}}}}}`)
	defaults := []byte("replicaCount: 1\nimage:\n  repository: registry.example.com/platform/sample\n  tag: latest\n")
	approval := Approval{ApprovalKey: ApprovalKey{ID: id.New(), Revision: 1},
		OCIRepository: "oci://registry.example.com/platform/sample", ChartVersion: "1.2.3",
		ManifestDigest: digestBytes([]byte("manifest")), PackageDigest: digestBytes([]byte("package")),
		ValuesSchemaDigest: digestBytes(schema), RendererImage: RendererImage,
		RendererVersion: HelmVersion, PolicyVersion: PolicyVersion, CreatedBy: id.New(),
		IdempotencyKey: "approval-document-0001", CreatedAt: now}
	documentsDigest, err := approvalDocumentsDigest(approval.ApprovalKey, schema, defaults)
	if err != nil {
		t.Fatal(err)
	}
	document := ApprovalDocument{Approval: approval, ValuesSchemaJSON: schema,
		DefaultValuesYAML: defaults, DocumentsDigest: documentsDigest, CreatedAt: now}
	if document.Validate() != nil {
		t.Fatal("invalid approval document fixture")
	}
	return document
}
