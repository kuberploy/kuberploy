package httpapi

import (
	"errors"
	"net/http"

	"github.com/kuberploy/kuberploy/internal/deploymentrollback"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/store"
)

type deploymentRollbackRequest struct {
	SourceOperationID string `json:"sourceOperationId"`
}

type deploymentRollbackFingerprint struct {
	DeploymentID      string `json:"deploymentId"`
	SourceOperationID string `json:"sourceOperationId"`
}

// deploymentRollbackSources is parameterized so its transport and adversarial
// tests stay isolated from Server option registration while shared surfaces
// are being composed.
func (s *Server) deploymentRollbackSources(resolver *deploymentrollback.Resolver, w http.ResponseWriter, r *http.Request) {
	if resolver == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "DeploymentRollbackUnavailable", "Deployment rollback unavailable", "The protected rollback history resolver is not configured.")
		return
	}
	limit, ok := parseHelmLimit(w, r, 25)
	if !ok {
		return
	}
	actor := currentUser(r.Context()).ID
	items, err := resolver.Catalog(r.Context(), actor, r.PathValue("id"), 100)
	if err != nil {
		mappedDeploymentRollbackError(w, r, err)
		return
	}
	eligible := make([]deploymentrollback.Candidate, 0, min(limit, len(items)))
	for _, item := range items {
		source, resolveErr := resolver.ResolveAuthorized(r.Context(), deploymentrollback.Request{
			ActorID: actor, DeploymentID: r.PathValue("id"), SourceOperationID: item.SourceOperationID,
		})
		if resolveErr != nil {
			continue
		}
		candidate, parsed, valid, available := s.resolveRetainedConfigDependencies(r.Context(), actor, source.Create)
		if !valid || !available {
			continue
		}
		runtime, schedulingErr := s.resolveSchedulingRuntime(r.Context(), actor, domain.Deployment{
			EnvironmentID: source.Create.EnvironmentID, ApplicationID: source.Create.ApplicationID,
		}, candidate.Runtime, true)
		if schedulingErr != nil {
			continue
		}
		if _, referenceErr := s.resolveAppConfigReferencePlan(r.Context(), actor, domain.Deployment{
			EnvironmentID: source.Create.EnvironmentID, ApplicationID: source.Create.ApplicationID,
		}, runtime, parsed); referenceErr != nil {
			continue
		}
		eligible = append(eligible, item)
		if len(eligible) == limit {
			break
		}
	}
	w.Header().Set("Cache-Control", "private, no-store")
	collection(w, eligible)
}

func (s *Server) rollbackDeployment(resolver *deploymentrollback.Resolver, w http.ResponseWriter, r *http.Request) {
	if resolver == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "DeploymentRollbackUnavailable", "Deployment rollback unavailable", "The protected rollback history resolver is not configured.")
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input deploymentRollbackRequest
	if !decode(w, r, &input) {
		return
	}
	actor := currentUser(r.Context()).ID
	request := deploymentrollback.Request{ActorID: actor, DeploymentID: r.PathValue("id"), SourceOperationID: input.SourceOperationID}
	source, err := resolver.ResolveAuthorized(r.Context(), request)
	if err != nil {
		mappedDeploymentRollbackError(w, r, err)
		return
	}
	fp := fingerprint(deploymentRollbackFingerprint{DeploymentID: request.DeploymentID, SourceOperationID: request.SourceOperationID})

	// Recover an accepted command before mutable registry, scheduling, secret,
	// pull-profile, sslip, Git, or Argo checks. The invalid plan cannot authorize
	// a new write because the store validates it after idempotency lookup.
	replayed, replayOperation, replayErr := s.store.CreateDeployment(r.Context(), actor, key, fp,
		requestID(r.Context()), source.Create, &gitprojection.WritePlan{})
	if replayErr == nil && replayed.Replay {
		writeDeploymentAccepted(w, replayed, replayOperation)
		return
	}
	if errors.Is(replayErr, store.ErrIdempotencyConflict) || errors.Is(replayErr, store.ErrForbidden) || errors.Is(replayErr, store.ErrNotFound) {
		mappedError(w, r, replayErr)
		return
	}
	source, err = resolver.VerifyArtifact(r.Context(), source)
	if err != nil {
		mappedDeploymentRollbackError(w, r, err)
		return
	}
	create := source.Create
	if create.Route != nil && create.Route.DNSMode == "sslip" {
		create.Route.Hostname, err = s.resolveSSLIPDeploymentHostname(r.Context(), actor, create.ApplicationID, create.EnvironmentID)
		if err != nil {
			mappedSSLIPDeploymentError(w, r, err)
			return
		}
	}
	s.submitDeployment(w, r, actor, key, fp, create, false)
}

func mappedDeploymentRollbackError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, deploymentrollback.ErrInvalid):
		writeProblem(w, r, http.StatusUnprocessableEntity, "DeploymentRollbackInvalid", "Deployment rollback invalid", "Select one exact prior deployment operation UUID.")
	case errors.Is(err, deploymentrollback.ErrNotFound), errors.Is(err, store.ErrNotFound):
		mappedError(w, r, store.ErrNotFound)
	case errors.Is(err, store.ErrForbidden):
		mappedError(w, r, store.ErrForbidden)
	case errors.Is(err, deploymentrollback.ErrSourceNotEligible):
		writeProblem(w, r, http.StatusConflict, "DeploymentRollbackSourceNotEligible", "Rollback source not eligible", "The source must be an older successful direct Git revision or a merge-verified protected pull request.")
	case errors.Is(err, deploymentrollback.ErrArtifactUnavailable):
		writeProblem(w, r, http.StatusServiceUnavailable, "DeploymentRollbackArtifactUnavailable", "Rollback artifact unavailable", "The exact Kuberploy-managed release is no longer retained and observed as present in the registry.")
	case errors.Is(err, deploymentrollback.ErrConflict), errors.Is(err, store.ErrConflict):
		writeProblem(w, r, http.StatusServiceUnavailable, "DeploymentRollbackHistoryMismatch", "Rollback history unavailable", "The durable operation, deployment snapshot, publication, and release identities do not match exactly.")
	default:
		writeProblem(w, r, http.StatusServiceUnavailable, "DeploymentRollbackUnavailable", "Deployment rollback unavailable", "The server could not safely resolve the protected rollback source.")
	}
}
