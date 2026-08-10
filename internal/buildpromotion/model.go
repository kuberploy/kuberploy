// Package buildpromotion resolves one verified successful source-build into a
// server-derived deployment image for one environment. Caller-controlled
// application IDs, image references, registry coordinates, and project IDs are
// intentionally absent from its request contract.
package buildpromotion

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

var (
	ErrInvalid             = errors.New("build promotion input is invalid")
	ErrNotFound            = errors.New("build promotion source was not found")
	ErrNotReady            = errors.New("build promotion source is not ready")
	ErrArtifactUnavailable = errors.New("build promotion artifact is unavailable")
	ErrConflict            = errors.New("build promotion source conflicts with durable state")

	uuidRE       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	digestRE     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitRE     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	serverRE     = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?::[1-9][0-9]{0,4})?$`)
	repositoryRE = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
)

// ProjectedBuild is produced only by the build state-machine store after it
// revalidates the immutable attempt, definition, and successful projection.
type ProjectedBuild struct {
	AttemptID, DefinitionID, ProjectID, ApplicationID string
	Generation                                        int64
	CommitSHA, DefinitionDigest                       string
	RegistryTargetID, RegistryServer, Repository      string
	ImageReference, ImageDigest, ReleaseID            string
	CreatedAt, CompletedAt, ProjectionCompletedAt     time.Time
}

func (s ProjectedBuild) Validate() error {
	if !uuidRE.MatchString(s.AttemptID) || !uuidRE.MatchString(s.DefinitionID) ||
		!uuidRE.MatchString(s.ProjectID) || !uuidRE.MatchString(s.ApplicationID) || s.Generation < 1 ||
		!commitRE.MatchString(s.CommitSHA) || !digestRE.MatchString(s.DefinitionDigest) ||
		!uuidRE.MatchString(s.RegistryTargetID) || !serverRE.MatchString(s.RegistryServer) ||
		strings.ToLower(s.RegistryServer) != s.RegistryServer || !repositoryRE.MatchString(s.Repository) ||
		!digestRE.MatchString(s.ImageDigest) || s.ReleaseID != s.AttemptID ||
		s.ImageReference != s.RegistryServer+"/"+s.Repository+"@"+s.ImageDigest ||
		s.CreatedAt.IsZero() || s.CompletedAt.Before(s.CreatedAt) ||
		s.ProjectionCompletedAt.Before(s.CompletedAt) {
		return ErrInvalid
	}
	return nil
}

// Request is the entire caller-selectable identity surface. Application,
// project, image, registry, namespace, and release IDs are never accepted.
type Request struct {
	ActorID, AttemptID, EnvironmentID string
}

func (r Request) Validate() error {
	if !uuidRE.MatchString(r.ActorID) || !uuidRE.MatchString(r.AttemptID) || !uuidRE.MatchString(r.EnvironmentID) {
		return ErrInvalid
	}
	return nil
}

type Source struct {
	ProjectedBuild
	EnvironmentID, Namespace string
	Release                  domain.RegistryRelease
}

func (s Source) Validate() error {
	if s.ProjectedBuild.Validate() != nil || !uuidRE.MatchString(s.EnvironmentID) || s.Namespace == "" ||
		s.Release.ID != s.ReleaseID || s.Release.RegistryTargetID != s.RegistryTargetID ||
		s.Release.ServiceID != s.ApplicationID || s.Release.Repository != s.Repository ||
		s.Release.RootDigest != s.ImageDigest || s.Release.SucceededAt == nil ||
		!s.Release.SucceededAt.Equal(s.CompletedAt) || !s.Release.CreatedAt.Equal(s.CreatedAt) ||
		s.Release.Availability != domain.RegistryArtifactPresent || s.Release.AvailabilityObservedAt != nil {
		return ErrInvalid
	}
	return nil
}
