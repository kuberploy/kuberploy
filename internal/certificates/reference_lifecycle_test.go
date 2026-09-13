package certificates

import (
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

func TestRetainedCertificateReferenceIsNotReadyRatherThanCorrupt(t *testing.T) {
	binding, version, certificate := activeCertificateObservationTarget(t)
	ref := Reference{BindingID: binding.ID, Name: binding.Name, Version: version.Number}
	rotatedAt := testNow.Add(time.Minute)
	binding.ActiveVersion++
	binding.UpdatedAt = rotatedAt
	version.State = secrets.VersionRetained
	version.RetainedAt = rotatedAt
	version.UpdatedAt = rotatedAt
	candidate := referenceCandidate{Binding: binding, Version: version, Certificate: certificate,
		Resolved: ResolvedReference{BindingID: binding.ID, SecretVersionID: version.ID, Name: binding.Name,
			Version: version.Number, Namespace: binding.Scope.Namespace, TargetSecretName: secrets.TargetSecretName(binding, version.Number),
			LeafFingerprint: certificate.LeafFingerprint, PublicKeyFingerprint: certificate.PublicKeyFingerprint,
			NotBefore: certificate.NotBefore, NotAfter: certificate.NotAfter}}
	if err := candidate.validate(binding.Scope, ref, "api.example.test", rotatedAt); !errors.Is(err, ErrNotReady) {
		t.Fatalf("retained version must become a repairable policy diagnostic, got %v", err)
	}
	if err := validateActiveCertificateTarget(binding, version, certificate); !errors.Is(err, ErrInvalid) {
		t.Fatalf("retained version must not enter active observation, got %v", err)
	}
	wrongScope := binding.Scope
	wrongScope.ApplicationID = "20000000-0000-4000-8000-000000000001"
	if err := candidate.validate(wrongScope, ref, "api.example.test", rotatedAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retained version crossed application boundary: %v", err)
	}
	corrupt := candidate
	corrupt.Resolved.TargetSecretName = "substituted-target"
	if err := corrupt.validate(binding.Scope, ref, "api.example.test", rotatedAt); !errors.Is(err, ErrConflict) {
		t.Fatalf("retained version corruption became a readiness diagnostic: %v", err)
	}
}
