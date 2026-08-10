// Package deploymentrollback resolves an ordinary application rollback from
// one exact, prior, server-owned deployment operation. It never accepts image,
// AppConfig, registry, environment, application, or Git coordinates from a
// caller.
package deploymentrollback

import (
	"errors"
	"regexp"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

var (
	ErrInvalid             = errors.New("deployment rollback input is invalid")
	ErrNotFound            = errors.New("deployment rollback source was not found")
	ErrConflict            = errors.New("deployment rollback source conflicts with durable state")
	ErrSourceNotEligible   = errors.New("deployment rollback source is not eligible")
	ErrArtifactUnavailable = errors.New("deployment rollback artifact is unavailable")

	uuidRE   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	imageRE  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*(?::[1-9][0-9]{0,4})?)(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+@sha256:[0-9a-f]{64}$`)
	commitRE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

// Request is the complete caller-selectable rollback surface.
type Request struct {
	ActorID           string
	DeploymentID      string
	SourceOperationID string
}

func (r Request) Validate() error {
	if !uuidRE.MatchString(r.ActorID) || !uuidRE.MatchString(r.DeploymentID) ||
		!uuidRE.MatchString(r.SourceOperationID) {
		return ErrInvalid
	}
	return nil
}

// Source contains only server-owned history reconstructed for the ordinary
// deployment submission pipeline. ConfigRaw is retained for integrity checks
// and history diagnostics but is never accepted from, or returned to, callers.
type Source struct {
	Deployment             domain.Deployment
	SourceOperation        domain.Operation
	Create                 domain.CreateDeployment
	ManagedReleaseVerified bool
	ProtectedMergeVerified bool
}

type ArtifactAssurance string

const (
	ArtifactManagedReleaseVerified   ArtifactAssurance = "managed-release-verified"
	ArtifactExternalDigestUnverified ArtifactAssurance = "external-digest-unverified"
)

// Candidate is the bounded, metadata-only history projection. Raw AppConfig,
// environment values, registry coordinates, pull profiles, and Git details are
// deliberately absent.
type Candidate struct {
	SourceOperationID      string            `json:"sourceOperationId"`
	Generation             int64             `json:"generation"`
	Image                  string            `json:"image"`
	ArtifactAssurance      ArtifactAssurance `json:"artifactAssurance"`
	ManagedReleaseVerified bool              `json:"managedReleaseVerified"`
	CreatedAt              time.Time         `json:"createdAt"`
}

func (s Source) Candidate() Candidate {
	assurance := ArtifactExternalDigestUnverified
	if s.ManagedReleaseVerified {
		assurance = ArtifactManagedReleaseVerified
	}
	return Candidate{SourceOperationID: s.SourceOperation.ID, Generation: s.SourceOperation.Generation,
		Image: s.Deployment.Image, ArtifactAssurance: assurance, ManagedReleaseVerified: s.ManagedReleaseVerified,
		CreatedAt: s.SourceOperation.CreatedAt}
}

func (s Source) Validate() error {
	d := s.Deployment
	op := s.SourceOperation
	if !uuidRE.MatchString(d.ID) || !uuidRE.MatchString(d.EnvironmentID) ||
		!uuidRE.MatchString(d.ApplicationID) || !uuidRE.MatchString(d.OperationID) ||
		!imageRE.MatchString(d.Image) || d.Generation < 1 || len(d.ConfigRaw) == 0 ||
		op.ID != d.OperationID || op.Kind != "deployment.git-write" ||
		op.TargetType != "deployment" || op.TargetID != d.ID || op.Generation != d.Generation ||
		op.Status != "succeeded" || (!commitRE.MatchString(op.GitRevision) && !s.ProtectedMergeVerified) ||
		s.Create.EnvironmentID != d.EnvironmentID ||
		s.Create.ApplicationID != d.ApplicationID || s.Create.Image != d.Image {
		return ErrConflict
	}
	return nil
}
