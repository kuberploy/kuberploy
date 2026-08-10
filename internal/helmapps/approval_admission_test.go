package helmapps

import (
	"context"
	"errors"
	"testing"
	"time"
)

type admissionPackageSource struct {
	artifact ChartArtifact
	err      error
	calls    int
}

func (s *admissionPackageSource) Fetch(context.Context, Approval) (ChartArtifact, error) {
	s.calls++
	if s.err != nil {
		return ChartArtifact{}, s.err
	}
	return ChartArtifact{ManifestDigest: s.artifact.ManifestDigest,
		PackageDigest: s.artifact.PackageDigest,
		PackageBytes:  append([]byte(nil), s.artifact.PackageBytes...)}, nil
}

func TestApprovalAdmissionDerivesDocumentsAndReplaysWithoutRegistry(t *testing.T) {
	files := testChartFiles()
	packageBytes := packageChart(t, files)
	approval := testApproval(t, packageBytes, files)
	request := ApprovalAdmissionRequest{ActorID: approval.CreatedBy,
		IdempotencyKey: approval.IdempotencyKey, OCIRepository: approval.OCIRepository,
		ChartVersion: approval.ChartVersion, ManifestDigest: approval.ManifestDigest,
		PackageDigest: approval.PackageDigest, ValuesSchemaDigest: approval.ValuesSchemaDigest}
	source := &admissionPackageSource{artifact: ChartArtifact{ManifestDigest: approval.ManifestDigest,
		PackageDigest: approval.PackageDigest, PackageBytes: packageBytes}}
	store := NewMemoryStore()
	service := ApprovalAdmissionService{Store: store, Packages: source,
		Now: func() time.Time { return testTime }, NewID: func() string { return approval.ID }}
	document, replay, err := service.Admit(context.Background(), request)
	if err != nil || replay || source.calls != 1 || document.Validate() != nil ||
		!equalBytes(document.ValuesSchemaJSON, files["values.schema.json"]) ||
		!equalBytes(document.DefaultValuesYAML, files["values.yaml"]) {
		t.Fatalf("admission document=%+v replay=%v calls=%d err=%v", document, replay, source.calls, err)
	}
	catalog, err := service.Catalog(context.Background(), 1)
	if err != nil || len(catalog) != 1 || catalog[0].Approval.ID != document.Approval.ID {
		t.Fatalf("catalog=%+v err=%v", catalog, err)
	}
	configOnlyCatalog, err := (ApprovalAdmissionService{Store: store}).Catalog(context.Background(), 1)
	if err != nil || len(configOnlyCatalog) != 1 {
		t.Fatalf("config-only catalog=%+v err=%v", configOnlyCatalog, err)
	}
	if _, err = service.Catalog(context.Background(), 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbounded catalog accepted: %v", err)
	}
	source.err = errors.New("registry unavailable")
	replayed, replay, err := service.Admit(context.Background(), request)
	if err != nil || !replay || source.calls != 1 || replayed.Approval.ID != document.Approval.ID {
		t.Fatalf("durable replay=%+v replay=%v calls=%d err=%v", replayed, replay, source.calls, err)
	}
	conflict := request
	conflict.ChartVersion = "1.2.4"
	if _, _, err = service.Admit(context.Background(), conflict); !errors.Is(err, ErrConflict) || source.calls != 1 {
		t.Fatalf("conflicting replay fetched or was accepted: calls=%d err=%v", source.calls, err)
	}
}

func TestApprovalAdmissionRejectsArtifactSubstitutionWithoutPersistence(t *testing.T) {
	files := testChartFiles()
	packageBytes := packageChart(t, files)
	approval := testApproval(t, packageBytes, files)
	request := ApprovalAdmissionRequest{ActorID: approval.CreatedBy,
		IdempotencyKey: approval.IdempotencyKey, OCIRepository: approval.OCIRepository,
		ChartVersion: approval.ChartVersion, ManifestDigest: approval.ManifestDigest,
		PackageDigest: approval.PackageDigest, ValuesSchemaDigest: approval.ValuesSchemaDigest}
	source := &admissionPackageSource{artifact: ChartArtifact{ManifestDigest: approval.ManifestDigest,
		PackageDigest: digestBytes([]byte("substituted")), PackageBytes: packageBytes}}
	store := NewMemoryStore()
	service := ApprovalAdmissionService{Store: store, Packages: source,
		Now: func() time.Time { return testTime }, NewID: func() string { return approval.ID }}
	if _, _, err := service.Admit(context.Background(), request); !errors.Is(err, ErrUnsafeChart) {
		t.Fatalf("substituted artifact accepted: %v", err)
	}
	if _, err := store.ApprovalAdmission(context.Background(), request.ActorID,
		request.IdempotencyKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed admission persisted state: %v", err)
	}
}
