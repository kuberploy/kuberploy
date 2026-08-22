package httpapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/imagepull"
	"github.com/kuberploy/kuberploy/internal/imageresolution"
	"github.com/kuberploy/kuberploy/internal/operationcache"
	"github.com/kuberploy/kuberploy/internal/store"
)

type projectRequest struct {
	Name   string `json:"name"`
	Slug   string `json:"slug,omitempty"`
	TeamID string `json:"teamId,omitempty"`
}

func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		items, err := s.store.ListProjectsForActor(r.Context(), currentUser(r.Context()).ID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		collection(w, items)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var in projectRequest
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.TeamID = strings.TrimSpace(in.TeamID)
	if in.Slug == "" {
		in.Slug = slugify(in.Name)
	} else {
		in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	}
	if in.Name == "" || len(in.Name) > 100 || !validSlug(in.Slug) {
		writeProblem(w, r, 422, "ValidationFailed", "Validation failed", "The project name or slug is invalid.", FieldError{Pointer: "/slug", Code: "InvalidSlug", Detail: "Use 1-63 lowercase letters, digits, or hyphens."})
		return
	}
	u := currentUser(r.Context())
	result, err := s.store.CreateProject(r.Context(), u.ID, key, fingerprint(in), domain.CreateProject{Name: in.Name, Slug: in.Slug, TeamID: in.TeamID})
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/projects/"+result.Value.ID)
	writeJSON(w, 201, result.Value)
}
func (s *Server) project(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.GetProjectForActor(r.Context(), currentUser(r.Context()).ID, r.PathValue("id"))
	if err != nil {
		mappedError(w, r, err)
		return
	}
	writeJSON(w, 200, v)
}

type environmentRequest struct {
	ProjectID        string                             `json:"projectId"`
	Name             string                             `json:"name"`
	Slug             string                             `json:"slug,omitempty"`
	Namespace        string                             `json:"namespace,omitempty"`
	ArgoProject      string                             `json:"argoProject,omitempty"`
	ProtectionPolicy domain.EnvironmentProtectionPolicy `json:"protectionPolicy,omitempty"`
}

func (s *Server) environments(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		items, err := s.store.ListEnvironmentsForActor(r.Context(), currentUser(r.Context()).ID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		collection(w, items)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var in environmentRequest
	if !decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Namespace) != "" || strings.TrimSpace(in.ArgoProject) != "" {
		errors := make([]FieldError, 0, 2)
		if strings.TrimSpace(in.Namespace) != "" {
			errors = append(errors, FieldError{Pointer: "/namespace", Code: "LockedField", Detail: "Kuberploy derives the destination namespace from the project and environment identity."})
		}
		if strings.TrimSpace(in.ArgoProject) != "" {
			errors = append(errors, FieldError{Pointer: "/argoProject", Code: "LockedField", Detail: "Kuberploy derives the Argo CD project from the owning project identity."})
		}
		writeProblem(w, r, 422, "ValidationFailed", "Validation failed", "Namespace and Argo CD project are platform-owned destination fields.", errors...)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Slug == "" {
		in.Slug = slugify(in.Name)
	} else {
		in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	}
	project, err := s.store.GetProjectForActor(r.Context(), currentUser(r.Context()).ID, in.ProjectID)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	in.Namespace, in.ArgoProject = domain.DeriveEnvironmentDestination(project, in.Slug)
	if in.Name == "" || len(in.Name) > 100 || !validSlug(in.Slug) || !validSlug(in.Namespace) || !validSlug(in.ArgoProject) {
		writeProblem(w, r, 422, "ValidationFailed", "Validation failed", "The environment name, slug, namespace, or Argo project is invalid.")
		return
	}
	if in.ProtectionPolicy == "" {
		in.ProtectionPolicy = domain.EnvironmentProtected
	}
	if in.ProtectionPolicy != domain.EnvironmentDevelopment && in.ProtectionPolicy != domain.EnvironmentProtected {
		writeProblem(w, r, 422, "ValidationFailed", "Validation failed", "protectionPolicy must be exactly development or protected.",
			FieldError{Pointer: "/protectionPolicy", Code: "InvalidEnvironmentProtectionPolicy", Detail: "Choose development for direct Git publication or protected for pull-request publication."})
		return
	}
	u := currentUser(r.Context())
	result, err := s.store.CreateEnvironment(r.Context(), u.ID, key, fingerprint(in), domain.CreateEnvironment{ProjectID: in.ProjectID, Name: in.Name, Slug: in.Slug, Namespace: in.Namespace, ArgoProject: in.ArgoProject, ProtectionPolicy: in.ProtectionPolicy})
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/environments/"+result.Value.ID)
	writeJSON(w, 201, result.Value)
}
func (s *Server) environment(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.GetEnvironmentForActor(r.Context(), currentUser(r.Context()).ID, r.PathValue("id"))
	if err != nil {
		mappedError(w, r, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) environmentApps(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListEnvironmentAppPlacementsForActor(r.Context(), currentUser(r.Context()).ID, r.PathValue("id"))
	if err != nil {
		mappedError(w, r, err)
		return
	}
	collection(w, items)
}

type environmentCloneRequest struct {
	Name             string                             `json:"name"`
	Slug             string                             `json:"slug,omitempty"`
	ProtectionPolicy domain.EnvironmentProtectionPolicy `json:"protectionPolicy,omitempty"`
}

func (s *Server) cloneEnvironment(w http.ResponseWriter, r *http.Request) {
	sourceEnvironmentID := strings.TrimSpace(r.PathValue("id"))
	if !validUUID(sourceEnvironmentID) {
		writeProblem(w, r, http.StatusNotFound, "NotFound", "Not found", "The requested resource was not found.")
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var in environmentCloneRequest
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Slug == "" {
		in.Slug = slugify(in.Name)
	} else {
		in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	}
	if in.Name == "" || len(in.Name) > 100 || !validSlug(in.Slug) {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The cloned environment name or slug is invalid.",
			FieldError{Pointer: "/slug", Code: "InvalidSlug", Detail: "Use 1-63 lowercase letters, digits, or hyphens."})
		return
	}
	if in.ProtectionPolicy != "" && in.ProtectionPolicy != domain.EnvironmentDevelopment && in.ProtectionPolicy != domain.EnvironmentProtected {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "protectionPolicy must be exactly development or protected.",
			FieldError{Pointer: "/protectionPolicy", Code: "InvalidEnvironmentProtectionPolicy", Detail: "Omit to inherit the source policy, or choose development or protected."})
		return
	}
	result, err := s.store.CloneEnvironment(r.Context(), currentUser(r.Context()).ID, sourceEnvironmentID, key, fingerprint(in), domain.CloneEnvironment{
		Name: in.Name, Slug: in.Slug, ProtectionPolicy: in.ProtectionPolicy,
	})
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/environments/"+result.Value.Environment.ID)
	writeJSON(w, http.StatusCreated, result.Value)
}

type applicationRequest struct {
	ProjectID     string `json:"projectId"`
	EnvironmentID string `json:"environmentId,omitempty"`
	Name          string `json:"name"`
	Slug          string `json:"slug,omitempty"`
}

func (s *Server) applications(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		items, err := s.store.ListApplicationsForActor(r.Context(), currentUser(r.Context()).ID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		collection(w, items)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var in applicationRequest
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Slug == "" {
		in.Slug = slugify(in.Name)
	} else {
		in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	}
	if in.ProjectID == "" || in.Name == "" || len(in.Name) > 100 || !validSlug(in.Slug) ||
		(in.EnvironmentID != "" && !validUUID(in.EnvironmentID)) {
		writeProblem(w, r, 422, "ValidationFailed", "Validation failed", "The application project, name, or slug is invalid.")
		return
	}
	u := currentUser(r.Context())
	result, err := s.store.CreateApplication(r.Context(), u.ID, key, fingerprint(in), domain.CreateApplication{
		ProjectID: in.ProjectID, EnvironmentID: in.EnvironmentID, Name: in.Name, Slug: in.Slug,
	})
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/applications/"+result.Value.ID)
	writeJSON(w, 201, result.Value)
}
func (s *Server) application(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.GetApplicationForActor(r.Context(), currentUser(r.Context()).ID, r.PathValue("id"))
	if err != nil {
		mappedError(w, r, err)
		return
	}
	writeJSON(w, 200, v)
}

type routeRequest struct {
	Hostname   string `json:"hostname"`
	PathPrefix string `json:"pathPrefix,omitempty"`
	TLSMode    string `json:"tlsMode,omitempty"`
	DNSMode    string `json:"dnsMode,omitempty"`
}
type deploymentRequest struct {
	EnvironmentID          string                  `json:"environmentId"`
	ApplicationID          string                  `json:"applicationId"`
	Image                  string                  `json:"image"`
	ExpectedImmutableImage string                  `json:"expectedImmutableImage,omitempty"`
	Replicas               int                     `json:"replicas,omitempty"`
	Port                   int                     `json:"port"`
	Environment            map[string]string       `json:"environment,omitempty"`
	Route                  *routeRequest           `json:"route,omitempty"`
	Runtime                *domain.WorkloadRuntime `json:"runtime,omitempty"`
}

func (s *Server) deployments(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		items, err := s.store.ListDeploymentsForActor(r.Context(), currentUser(r.Context()).ID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		collection(w, items)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var in deploymentRequest
	if !decode(w, r, &in) {
		return
	}
	if in.Runtime != nil && (in.Replicas != 0 || in.Port != 0 || len(in.Environment) != 0) {
		writeProblem(w, r, 422, "ValidationFailed", "Validation failed", "Use runtime for workload fields; do not combine it with deprecated replicas, port, or environment fields.", FieldError{Pointer: "/runtime", Code: "ConflictingFields", Detail: "runtime is the canonical workload contract."})
		return
	}
	var runtime domain.WorkloadRuntime
	if in.Runtime == nil {
		runtime = domain.DefaultWorkloadRuntime(in.Port, in.Environment)
		runtime.Replicas = in.Replicas
	} else {
		runtime = *in.Runtime
	}
	runtime = domain.NormalizeWorkloadRuntime(runtime)
	var route *domain.Route
	if in.Route != nil {
		in.Route.Hostname = strings.ToLower(strings.TrimSpace(in.Route.Hostname))
		in.Route.DNSMode = strings.TrimSpace(in.Route.DNSMode)
		if in.Route.DNSMode == "" {
			in.Route.DNSMode = "manual"
		}
		if in.Route.PathPrefix == "" {
			in.Route.PathPrefix = "/"
		}
		if in.Route.TLSMode == "" {
			in.Route.TLSMode = "httpOnly"
		}
		if in.Route.PathPrefix != "/" || in.Route.TLSMode != "httpOnly" ||
			(in.Route.DNSMode == "manual" && !validHostname(in.Route.Hostname)) ||
			(in.Route.DNSMode == "sslip" && in.Route.Hostname != "") ||
			(in.Route.DNSMode != "manual" && in.Route.DNSMode != "sslip") {
			writeProblem(w, r, 422, "ValidationFailed", "Validation failed", "Use an HTTP-only manual hostname or request the server-derived sslip DNS mode.", FieldError{Pointer: "/route", Code: "InvalidRoute", Detail: "manual requires hostname; sslip forbids caller hostname and accepts only dnsMode."})
			return
		}
		route = &domain.Route{Hostname: in.Route.Hostname, PathPrefix: in.Route.PathPrefix, TLSMode: in.Route.TLSMode, DNSMode: in.Route.DNSMode}
	}
	if in.EnvironmentID == "" || in.ApplicationID == "" {
		writeProblem(w, r, 422, "ValidationFailed", "Validation failed", "Deployment requires valid application and environment IDs.")
		return
	}
	if problems := domain.ValidateApplicationScheduling(runtime, in.ApplicationID); len(problems) > 0 {
		limit := len(problems)
		if limit > 50 {
			limit = 50
		}
		fields := make([]FieldError, 0, limit)
		for _, problem := range problems[:limit] {
			fields = append(fields, FieldError{Pointer: problem.Pointer, Code: problem.Code, Detail: problem.Detail})
		}
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The workload scheduling configuration is invalid.", fields...)
		return
	}
	immutableInput := imageresolution.IsImmutableImage(in.Image)
	if immutableInput {
		if in.ExpectedImmutableImage != "" {
			writeProblem(w, r, 422, "ValidationFailed", "Validation failed", "An immutable image forbids the tag-resolution precondition.", FieldError{Pointer: "/expectedImmutableImage", Code: "ForbiddenField", Detail: "expectedImmutableImage is accepted only with a tag"})
			return
		}
	} else {
		if _, imageErr := imageresolution.ParseTagReference(in.Image); imageErr != nil {
			mappedImageResolutionError(w, r, imageErr)
			return
		}
		if !imageresolution.IsImmutableImage(in.ExpectedImmutableImage) {
			writeProblem(w, r, 422, "ValidationFailed", "Validation failed", "A tag requires the exact immutable image from the current server preview.", FieldError{Pointer: "/expectedImmutableImage", Code: "RequiredPrecondition", Detail: "provide the previewed repository@sha256 image"})
			return
		}
	}
	if problems := domain.ValidateWorkloadRuntime(runtime); len(problems) > 0 {
		limit := len(problems)
		if limit > 50 {
			limit = 50
		}
		errors := make([]FieldError, 0, limit)
		for _, problem := range problems[:limit] {
			errors = append(errors, FieldError{Pointer: problem.Pointer, Code: problem.Code, Detail: problem.Detail})
		}
		writeProblem(w, r, 422, "ValidationFailed", "Validation failed", "The workload runtime configuration is invalid.", errors...)
		return
	}
	u := currentUser(r.Context())
	replicas, port, ordinary := domain.LegacyWorkloadFields(runtime)
	create := domain.CreateDeployment{EnvironmentID: in.EnvironmentID, ApplicationID: in.ApplicationID, Image: in.Image, Replicas: replicas, Port: port, Environment: ordinary, Route: route, Runtime: runtime}
	fp := fingerprint(in)
	// Recover the exact accepted receipt before any mutable registry or sslip
	// observation. A new write cannot pass the deliberately invalid Git plan.
	replayed, replayOperation, replayErr := s.store.CreateDeployment(r.Context(), u.ID, key, fp, requestID(r.Context()), create, &gitprojection.WritePlan{})
	if replayErr == nil && replayed.Replay {
		writeDeploymentAccepted(w, replayed, replayOperation)
		return
	}
	if errors.Is(replayErr, store.ErrIdempotencyConflict) || errors.Is(replayErr, store.ErrForbidden) || errors.Is(replayErr, store.ErrNotFound) {
		mappedError(w, r, replayErr)
		return
	}
	if !immutableInput {
		if s.imageResolution == nil {
			mappedImageResolutionError(w, r, imageresolution.ErrUnavailable)
			return
		}
		resolution, resolveErr := s.imageResolution.Resolve(r.Context(), u.ID, in.ApplicationID, in.EnvironmentID, in.Image)
		if resolveErr != nil {
			mappedImageResolutionError(w, r, resolveErr)
			return
		}
		if resolution.ImmutableImage != in.ExpectedImmutableImage {
			writeProblem(w, r, http.StatusConflict, "ImageTagMoved", "Image tag moved", "The tag no longer resolves to the immutable image that was previewed. Resolve it again before deploying.", FieldError{Pointer: "/expectedImmutableImage", Code: "PreconditionMismatch", Detail: "the preview is stale"})
			return
		}
		create.Image = resolution.ImmutableImage
	}
	if route != nil && route.DNSMode == "sslip" {
		resolved, sslipErr := s.resolveSSLIPDeploymentHostname(r.Context(), u.ID, in.ApplicationID, in.EnvironmentID)
		if sslipErr != nil {
			mappedSSLIPDeploymentError(w, r, sslipErr)
			return
		}
		route.Hostname = resolved
	}
	s.submitDeployment(w, r, u.ID, key, fp, create, false)
}

// submitDeployment is the single policy-governed Git deployment mutation path used
// by both existing-image deployment and verified source-build promotion. The
// caller must already have constructed a fully server-authoritative create
// command and a fingerprint of only the caller-selectable intent.
func (s *Server) submitDeployment(w http.ResponseWriter, r *http.Request, actor, key, fp string, create domain.CreateDeployment, promotionCAS bool) {
	// Recover an accepted command before re-resolving mutable profile state.
	// The invalid plan cannot authorize a new write, while the store resolves a
	// matching idempotency record before inspecting it.
	replayed, replayOperation, replayErr := s.store.CreateDeployment(r.Context(), actor, key, fp, requestID(r.Context()), create, &gitprojection.WritePlan{})
	if replayErr == nil && replayed.Replay {
		writeDeploymentAccepted(w, replayed, replayOperation)
		return
	}
	if errors.Is(replayErr, store.ErrIdempotencyConflict) || errors.Is(replayErr, store.ErrForbidden) || errors.Is(replayErr, store.ErrNotFound) {
		mappedError(w, r, replayErr)
		return
	}
	// Every deployment source, including existing-image, build promotion, and
	// rollback, crosses the exact actor/application/environment/repository
	// catalog fence. Immutable syntax proves bytes, never repository authority.
	if s.imageResolutionCatalog == nil || !imageresolution.IsImmutableImage(create.Image) {
		mappedImageResolutionError(w, r, imageresolution.ErrUnavailable)
		return
	}
	resolution, resolutionErr := (&imageresolution.Resolver{Catalog: s.imageResolutionCatalog}).Resolve(
		r.Context(), actor, create.ApplicationID, create.EnvironmentID, create.Image,
	)
	if resolutionErr != nil {
		mappedImageResolutionError(w, r, resolutionErr)
		return
	}
	if resolution.ImmutableImage != create.Image || resolution.Resolved {
		mappedImageResolutionError(w, r, imageresolution.ErrConflict)
		return
	}
	var exactConfigParsed map[string]any
	if len(create.ConfigRaw) != 0 {
		parsed, runtime, diagnostics := appconfig.ParseAndValidate(create.ConfigRaw)
		exactImage, imageOK := appconfig.MaterializedImage(parsed)
		if len(diagnostics) != 0 || !imageOK || exactImage != create.Image {
			writeProblem(w, r, http.StatusServiceUnavailable, "DeploymentRollbackHistoryMismatch", "Rollback history unavailable", "The retained AppConfig snapshot is invalid or does not match the selected immutable release.")
			return
		}
		hash := sha256.Sum256(create.ConfigRaw)
		candidate := appconfig.Candidate{Raw: append([]byte(nil), create.ConfigRaw...), Parsed: parsed, Runtime: runtime, Hash: hash[:]}
		candidate = s.materializeMiddlewareCandidate(r.Context(), actor, domain.Deployment{EnvironmentID: create.EnvironmentID, ApplicationID: create.ApplicationID}, create.ConfigRaw, candidate)
		candidate.Diagnostics = append(candidate.Diagnostics, s.externalDNSRouteDiagnostics(r.Context(), actor,
			domain.Deployment{EnvironmentID: create.EnvironmentID, ApplicationID: create.ApplicationID}, candidate)...)
		sslipDiagnostics, _ := s.sslipRouteDiagnostics(r.Context(), actor,
			domain.Deployment{EnvironmentID: create.EnvironmentID, ApplicationID: create.ApplicationID}, candidate)
		candidate.Diagnostics = append(candidate.Diagnostics, sslipDiagnostics...)
		if len(candidate.Diagnostics) != 0 {
			writeProblem(w, r, http.StatusConflict, "DeploymentRollbackDependencyUnavailable", "Rollback dependency unavailable", "The retained AppConfig no longer matches a current middleware, DNS, route, or edge dependency.")
			return
		}
		create.Runtime = candidate.Runtime
		exactConfigParsed = candidate.Parsed
	}
	resolvedRuntime, schedulingErr := s.resolveSchedulingRuntime(r.Context(), actor, domain.Deployment{EnvironmentID: create.EnvironmentID, ApplicationID: create.ApplicationID}, create.Runtime, true)
	if schedulingErr != nil {
		mappedSchedulingRuntimeError(w, r, schedulingErr)
		return
	}
	create.Runtime = resolvedRuntime
	create.Replicas, create.Port, create.Environment = domain.LegacyWorkloadFields(resolvedRuntime)
	if runtimeProblem := s.deploymentMutationRuntimeProblem(r.Context()); runtimeProblem != "" {
		invalidPlan := &gitprojection.WritePlan{}
		result, operation, replayErr := s.store.CreateDeployment(r.Context(), actor, key, fp, requestID(r.Context()), create, invalidPlan)
		if replayErr == nil && result.Replay {
			writeDeploymentAccepted(w, result, operation)
			return
		}
		if errors.Is(replayErr, store.ErrIdempotencyConflict) || errors.Is(replayErr, store.ErrForbidden) || errors.Is(replayErr, store.ErrNotFound) {
			mappedError(w, r, replayErr)
			return
		}
		writeDeploymentMutationRuntimeProblem(w, r, runtimeProblem)
		return
	}
	var projection *gitprojection.WritePlan
	if s.gitProjection != nil {
		expectedETag := strings.TrimSpace(r.Header.Get("If-Match"))
		if expectedETag != "" && !gitProjectionETagPattern.MatchString(expectedETag) {
			writeProblem(w, r, 400, "InvalidPrecondition", "Invalid precondition", "If-Match must contain the exact strong Git bundle ETag.")
			return
		}
		plan, planErr := s.gitProjection.PlanMutation(r.Context(), actor, create.EnvironmentID, create.ApplicationID, expectedETag)
		if planErr != nil {
			// The store resolves a matching idempotency replay before inspecting
			// this deliberately invalid plan. This recovers a lost first response
			// even after the path was created or the binding began indexing, while
			// making a new mutation fail closed.
			invalidPlan := &gitprojection.WritePlan{}
			result, op, replayErr := s.store.CreateDeployment(r.Context(), actor, key, fp, requestID(r.Context()), create, invalidPlan)
			if replayErr == nil && result.Replay {
				writeDeploymentAccepted(w, result, op)
				return
			}
			if errors.Is(replayErr, store.ErrIdempotencyConflict) || errors.Is(replayErr, store.ErrForbidden) || errors.Is(replayErr, store.ErrNotFound) {
				mappedError(w, r, replayErr)
				return
			}
			mappedDeploymentGitError(w, r, planErr, promotionCAS)
			return
		}
		projection = &plan
	}
	if s.registryPulls != nil {
		repository, _, _ := strings.Cut(create.Image, "@")
		reference, present, resolveErr := s.registryPulls.ResolveRegistryPull(r.Context(), s.registryPullConfig, create.ApplicationID, create.EnvironmentID, repository)
		if resolveErr != nil {
			if s.replayDeployment(w, r, actor, key, fp, create) {
				return
			}
			mappedRegistryPullResolutionError(w, r, resolveErr)
			return
		}
		if present {
			if projection == nil || s.registryPullReadiness == nil {
				if s.replayDeployment(w, r, actor, key, fp, create) {
					return
				}
				mappedRegistryPullResolutionError(w, r, imagepull.ErrUnavailable)
				return
			}
			readinessContext, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			readyErr := s.registryPullReadiness.Probe(readinessContext)
			cancel()
			if readyErr != nil {
				if s.replayDeployment(w, r, actor, key, fp, create) {
					return
				}
				mappedRegistryPullResolutionError(w, r, imagepull.ErrUnavailable)
				return
			}
			create.RegistryPull = &reference
		}
	}
	references, referenceErr := s.resolveAppConfigReferencePlan(r.Context(), actor, domain.Deployment{
		EnvironmentID: create.EnvironmentID, ApplicationID: create.ApplicationID,
	}, create.Runtime, exactConfigParsed)
	if referenceErr != nil {
		if runtimeSecretReferenceUnavailable(referenceErr) {
			mappedSecretError(w, r, referenceErr)
			return
		}
		writeProblem(w, r, 422, "RuntimeSecretReferenceUnresolved", "Runtime-secret reference unresolved", runtimeSecretReferenceDiagnostic(referenceErr).Detail)
		return
	}
	result, op, err := s.store.CreateDeployment(r.Context(), actor, key, fp, requestID(r.Context()), create, projection, references)
	if err != nil {
		mappedDeploymentGitError(w, r, err, promotionCAS)
		return
	}
	writeDeploymentAccepted(w, result, op)
}

func mappedDeploymentGitError(w http.ResponseWriter, r *http.Request, err error, promotionCAS bool) {
	if promotionCAS && (errors.Is(err, gitprojection.ErrConflict) || errors.Is(err, gitprojection.ErrPreconditionRequired) || errors.Is(err, store.ErrPreconditionFailed)) {
		writeProblem(w, r, http.StatusConflict, "PromotionCASConflict", "Promotion conflict", "The protected Git bundle changed or requires its exact current ETag. Reload and retry the promotion.")
		return
	}
	mappedGitProjectionError(w, r, err)
}

func (s *Server) deploymentMutationRuntimeProblem(requestContext context.Context) string {
	// Preserve the legacy development/test store when the complete projection
	// runtime is intentionally absent. Once any production Git projection seam
	// is configured, however, mutations fail closed unless both the writer and
	// the protected Argo desired-state runtime are freshly observed.
	if s.gitProjection == nil && s.gitReadiness == nil && s.argoReadiness == nil {
		return ""
	}
	if s.gitProjection == nil || s.gitReadiness == nil {
		return "GitProjectionRuntimeUnavailable"
	}
	ctx, cancel := context.WithTimeout(requestContext, 2*time.Second)
	defer cancel()
	if err := s.gitReadiness.Probe(ctx); err != nil {
		return "GitProjectionRuntimeUnavailable"
	}
	if s.argoReadiness == nil {
		return "ArgoDesiredStateRuntimeUnavailable"
	}
	if err := s.argoReadiness.Probe(ctx); err != nil {
		return "ArgoDesiredStateRuntimeUnavailable"
	}
	return ""
}

func writeDeploymentMutationRuntimeProblem(w http.ResponseWriter, r *http.Request, code string) {
	if code == "GitProjectionRuntimeUnavailable" {
		writeProblem(w, r, 503, code, "Git projection unavailable", "No matching Git projection worker has reported a fresh exact runtime observation.")
		return
	}
	writeProblem(w, r, 503, "ArgoDesiredStateRuntimeUnavailable", "Protected rollout unavailable", "No matching protected Argo desired-state runtime has reported a fresh exact prerequisite proof.")
}

func (s *Server) replayDeployment(w http.ResponseWriter, r *http.Request, actor, key, fingerprint string, create domain.CreateDeployment) bool {
	invalidPlan := &gitprojection.WritePlan{}
	result, operation, err := s.store.CreateDeployment(r.Context(), actor, key, fingerprint, requestID(r.Context()), create, invalidPlan)
	if err != nil || !result.Replay {
		return false
	}
	writeDeploymentAccepted(w, result, operation)
	return true
}

func mappedRegistryPullResolutionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, imagepull.ErrConflict), errors.Is(err, imagepull.ErrInvalid):
		writeProblem(w, r, 503, "RegistryPullProfileMismatch", "Private registry pull unavailable", "The registry target, service repository, and operator-owned pull profile do not match exactly.")
	case errors.Is(err, imagepull.ErrUnavailable):
		writeProblem(w, r, 503, "RegistryPullRuntimeUnavailable", "Private registry pull unavailable", "No matching private-image-pull worker has reported a fresh exact runtime observation.")
	default:
		writeProblem(w, r, 503, "RegistryPullResolutionUnavailable", "Private registry pull unavailable", "The server could not safely resolve the image repository against the current registry pull policy.")
	}
}

func writeDeploymentAccepted(w http.ResponseWriter, result store.Result[domain.Deployment], op domain.Operation) {
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/operations/"+op.ID)
	w.Header().Set("X-Kuberploy-Resource-Location", "/v1/deployments/"+result.Value.ID)
	writeJSON(w, 202, op)
}

func (s *Server) deployment(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.GetDeploymentForActor(r.Context(), currentUser(r.Context()).ID, r.PathValue("id"))
	if err != nil {
		mappedError(w, r, err)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) deploymentStatus(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.DeploymentStatusForActor(r.Context(), currentUser(r.Context()).ID, r.PathValue("id"))
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if s.runtime != nil && s.runtimeReadiness != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if s.runtimeReadiness.Probe(ctx) == nil {
			if rollout, rolloutErr := s.runtime.Rollout(ctx, v.DeploymentID); rolloutErr == nil {
				desired, ready := rollout.DesiredReplicas, rollout.ReadyReplicas
				v.DesiredReplicas, v.ReadyReplicas = &desired, &ready
				observedAt := rollout.ObservedAt.UTC()
				v.RolloutObservedAt = &observedAt
				v.RolloutConditions = make([]domain.RolloutCondition, 0, len(rollout.Conditions))
				for _, condition := range rollout.Conditions {
					v.RolloutConditions = append(v.RolloutConditions, domain.RolloutCondition{Type: condition.Type, Status: condition.Status,
						Reason: condition.Reason, LastTransitionTime: condition.LastTransitionTime})
				}
			}
		}
	}
	writeJSON(w, 200, v)
}
func (s *Server) operations(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListOperationsForActor(r.Context(), currentUser(r.Context()).ID)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	collection(w, items)
}
func (s *Server) operation(w http.ResponseWriter, r *http.Request) {
	actor := currentUser(r.Context()).ID
	operationID := r.PathValue("id")
	identityStore, cacheable := s.store.(interface {
		OperationCacheIdentityForActor(context.Context, string, string) (operationcache.Identity, error)
	})
	if cacheable && s.operationCache != nil {
		identity, err := identityStore.OperationCacheIdentityForActor(r.Context(), actor, operationID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		if cached, hit, cacheErr := s.operationCache.Load(r.Context(), identity); cacheErr == nil && hit && identity.MatchesOperation(cached) {
			writeJSON(w, 200, cached)
			return
		}
		v, err := s.store.GetOperationForActor(r.Context(), actor, operationID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		if identity.MatchesOperation(v) {
			// Cache and hint delivery are both disposable. Failure here cannot
			// turn a successful authoritative PostgreSQL read into an API error.
			_ = s.operationCache.Store(r.Context(), identity, v)
		}
		writeJSON(w, 200, v)
		return
	}
	v, err := s.store.GetOperationForActor(r.Context(), actor, operationID)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	writeJSON(w, 200, v)
}
