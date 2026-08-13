package deploymentrollback

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
	platformstore "github.com/kuberploy/kuberploy/internal/store"
)

type History interface {
	GetDeploymentForActor(context.Context, string, string) (domain.Deployment, error)
	GetDeploymentForOperation(context.Context, string) (domain.Deployment, error)
	GetOperationForActor(context.Context, string, string) (domain.Operation, error)
	ListOperationsForActor(context.Context, string) ([]domain.Operation, error)
	Authorize(context.Context, string, domain.Permission, domain.AccessTarget) error
}

// ArtifactVerifier reports whether the exact image is backed by Kuberploy's
// registry release catalog. Unknown/external images are unmanaged and valid;
// a known release must still be retained and observed as present.
type ArtifactVerifier interface {
	VerifyRetainedDeploymentImage(context.Context, string, string) (managed bool, err error)
}

type PublicationCatalog interface {
	Publication(context.Context, string) (gitpublication.Publication, error)
}

type Resolver struct {
	History      History
	Artifacts    ArtifactVerifier
	Publications PublicationCatalog
}

func (r *Resolver) Resolve(ctx context.Context, request Request) (Source, error) {
	if r == nil || r.History == nil || r.Artifacts == nil || request.Validate() != nil {
		return Source{}, ErrInvalid
	}
	source, err := r.ResolveAuthorized(ctx, request)
	if err != nil {
		return Source{}, err
	}
	return r.VerifyArtifact(ctx, source)
}

// ResolveAuthorized reconstructs immutable history and freshly authorizes the
// exact target without consulting mutable registry availability. HTTP uses
// this phase to recover an already accepted idempotent command first.
func (r *Resolver) ResolveAuthorized(ctx context.Context, request Request) (Source, error) {
	if r == nil || r.History == nil || request.Validate() != nil {
		return Source{}, ErrInvalid
	}
	current, err := r.History.GetDeploymentForActor(ctx, request.ActorID, request.DeploymentID)
	if err != nil {
		return Source{}, classify(err)
	}
	if current.ID != request.DeploymentID {
		return Source{}, ErrConflict
	}
	if err = r.History.Authorize(ctx, request.ActorID, domain.PermissionResourcesWrite,
		domain.AccessTarget{Type: "deployment", ID: current.ID}); err != nil {
		return Source{}, classify(err)
	}
	op, err := r.History.GetOperationForActor(ctx, request.ActorID, request.SourceOperationID)
	if err != nil {
		return Source{}, classify(err)
	}
	snapshot, err := r.History.GetDeploymentForOperation(ctx, request.SourceOperationID)
	if err != nil {
		return Source{}, classify(err)
	}
	// The source must be genuinely prior and bound to the exact current logical
	// deployment. Cross-environment/application and current-head selection are
	// hidden as not-found to avoid turning rollback into a history oracle.
	if snapshot.ID != current.ID || snapshot.EnvironmentID != current.EnvironmentID ||
		snapshot.ApplicationID != current.ApplicationID || snapshot.OperationID != request.SourceOperationID ||
		op.ID != request.SourceOperationID || op.TargetID != current.ID {
		return Source{}, ErrNotFound
	}
	if snapshot.Generation >= current.Generation || op.Generation != snapshot.Generation ||
		op.Kind != "deployment.git-write" || op.TargetType != "deployment" || op.Status != "succeeded" {
		return Source{}, ErrSourceNotEligible
	}
	protectedMergeVerified := false
	if commitRE.MatchString(op.GitRevision) {
		if op.PullRequest != nil {
			return Source{}, ErrConflict
		}
	} else {
		if r.Publications == nil {
			return Source{}, ErrSourceNotEligible
		}
		publication, publicationErr := r.Publications.Publication(ctx, op.ID)
		if publicationErr != nil {
			return Source{}, ErrConflict
		}
		if publication.Validate() != nil || publication.OperationID != op.ID || publication.State != gitpublication.StateMergeVerified ||
			publication.TargetRevision == "" {
			return Source{}, ErrSourceNotEligible
		}
		if op.PullRequest != nil && (publication.PullRequestNumber != op.PullRequest.Number ||
			publication.PullRequestURL != op.PullRequest.URL || publication.CandidateRevision != op.PullRequest.CandidateRevision) {
			return Source{}, ErrConflict
		}
		protectedMergeVerified = true
	}
	runtime := cloneRuntime(snapshot.Runtime)
	replicas, port, ordinary := domain.LegacyWorkloadFields(runtime)
	source := Source{Deployment: cloneDeployment(snapshot), SourceOperation: op, ProtectedMergeVerified: protectedMergeVerified,
		Create: domain.CreateDeployment{EnvironmentID: snapshot.EnvironmentID, ApplicationID: snapshot.ApplicationID,
			Image: snapshot.Image, Replicas: replicas, Port: port, Environment: ordinary,
			Route: cloneRoute(snapshot.Route), Runtime: runtime}}
	if source.Validate() != nil {
		return Source{}, ErrConflict
	}
	return source, nil
}

// VerifyArtifact is intentionally separate from authorization so exact
// idempotent replay remains recoverable after a registry observation changes.
func (r *Resolver) VerifyArtifact(ctx context.Context, source Source) (Source, error) {
	if r == nil || r.Artifacts == nil || source.Validate() != nil {
		return Source{}, ErrInvalid
	}
	managed, err := r.Artifacts.VerifyRetainedDeploymentImage(ctx, source.Deployment.ApplicationID, source.Deployment.Image)
	if err != nil {
		return Source{}, classify(err)
	}
	source.ManagedReleaseVerified = managed
	return source, nil
}

// Catalog returns only currently eligible exact rollback identities. Known
// managed releases that are missing/expired are omitted; external immutable
// digests remain selectable but are explicitly labelled unverified.
func (r *Resolver) Catalog(ctx context.Context, actorID, deploymentID string, limit int) ([]Candidate, error) {
	if r == nil || r.History == nil || r.Artifacts == nil || !uuidRE.MatchString(actorID) ||
		!uuidRE.MatchString(deploymentID) || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	current, err := r.History.GetDeploymentForActor(ctx, actorID, deploymentID)
	if err != nil {
		return nil, classify(err)
	}
	if current.ID != deploymentID {
		return nil, ErrConflict
	}
	operations, err := r.History.ListOperationsForActor(ctx, actorID)
	if err != nil {
		return nil, classify(err)
	}
	items := make([]Candidate, 0, limit)
	for _, operation := range operations {
		if operation.TargetType != "deployment" || operation.TargetID != deploymentID || operation.ID == current.OperationID {
			continue
		}
		source, resolveErr := r.Resolve(ctx, Request{ActorID: actorID, DeploymentID: deploymentID, SourceOperationID: operation.ID})
		if resolveErr != nil {
			if errors.Is(resolveErr, ErrNotFound) || errors.Is(resolveErr, ErrSourceNotEligible) ||
				errors.Is(resolveErr, ErrArtifactUnavailable) {
				continue
			}
			return nil, resolveErr
		}
		items = append(items, source.Candidate())
		if len(items) == limit {
			break
		}
	}
	return items, nil
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrInvalid) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) ||
		errors.Is(err, ErrSourceNotEligible) || errors.Is(err, ErrArtifactUnavailable) {
		return err
	}
	if errors.Is(err, platformstore.ErrNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, platformstore.ErrForbidden) {
		return platformstore.ErrForbidden
	}
	if errors.Is(err, platformstore.ErrConflict) || errors.Is(err, platformstore.ErrPreconditionFailed) {
		return ErrConflict
	}
	return err
}

func cloneDeployment(in domain.Deployment) domain.Deployment {
	in.Environment = cloneMap(in.Environment)
	in.Route = cloneRoute(in.Route)
	in.Runtime = cloneRuntime(in.Runtime)
	in.ConfigRaw = append([]byte(nil), in.ConfigRaw...)
	return in
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneRoute(in *domain.Route) *domain.Route {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneRuntime(in domain.WorkloadRuntime) domain.WorkloadRuntime {
	// WorkloadRuntime contains nested maps and slices. Its canonical JSON form
	// is already the durable operation representation, so use the domain helper
	// pattern without sharing mutable history memory with submission.
	encoded, _ := json.Marshal(in)
	var out domain.WorkloadRuntime
	_ = json.Unmarshal(encoded, &out)
	return out
}
