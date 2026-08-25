package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/helmdirect"
	"github.com/kuberploy/kuberploy/internal/store"
)

type HelmApplicationBackend interface {
	Deploy(context.Context, helmdirect.DeployRequest, time.Time) (helmdirect.Revision, bool, error)
	Retry(context.Context, helmdirect.MutationRequest, time.Time) (helmdirect.Revision, bool, error)
	Disable(context.Context, helmdirect.MutationRequest, time.Time) (helmdirect.Revision, bool, error)
	Rollback(context.Context, helmdirect.MutationRequest, time.Time) (helmdirect.Revision, bool, error)
	Head(context.Context, helmdirect.Target) (helmdirect.Revision, error)
	History(context.Context, helmdirect.Target, int) ([]helmdirect.Revision, error)
	Capabilities(context.Context) (helmdirect.Capabilities, error)
}

type helmSourceView struct {
	Kind           string `json:"kind"`
	RepositoryURL  string `json:"repositoryUrl"`
	Chart          string `json:"chart,omitempty"`
	TargetRevision string `json:"targetRevision"`
	Path           string `json:"path,omitempty"`
}

type helmReleaseRevisionView struct {
	ID                       string         `json:"id"`
	Generation               int64          `json:"generation"`
	ReleaseName              string         `json:"releaseName"`
	Action                   string         `json:"action"`
	DesiredEnabled           bool           `json:"desiredEnabled"`
	ParentRevisionID         string         `json:"parentRevisionId,omitempty"`
	RollbackSourceRevisionID string         `json:"rollbackSourceRevisionId,omitempty"`
	Source                   helmSourceView `json:"source"`
	ValuesYAML               string         `json:"valuesYaml"`
	ValuesDigest             string         `json:"valuesDigest"`
	State                    string         `json:"state"`
	FailureCode              string         `json:"failureCode,omitempty"`
	RequestID                string         `json:"requestId"`
	CreatedAt                time.Time      `json:"createdAt"`
	UpdatedAt                time.Time      `json:"updatedAt"`
}

func helmReleaseView(value helmdirect.Revision) helmReleaseRevisionView {
	return helmReleaseRevisionView{ID: value.ID, Generation: value.Generation, ReleaseName: value.ReleaseName,
		Action: string(value.Action), DesiredEnabled: value.DesiredEnabled, ParentRevisionID: value.ParentRevisionID,
		RollbackSourceRevisionID: value.RollbackSourceRevisionID,
		Source: helmSourceView{Kind: string(value.Source.Kind), RepositoryURL: value.Source.RepositoryURL,
			Chart: value.Source.Chart, TargetRevision: value.Source.TargetRevision, Path: value.Source.Path}, ValuesYAML: string(value.ValuesYAML),
		ValuesDigest: value.ValuesDigest, State: string(value.State), FailureCode: value.FailureCode,
		RequestID: value.RequestID, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
}

type helmValuesInput struct {
	Source struct {
		Kind           string `json:"kind"`
		RepositoryURL  string `json:"repositoryUrl"`
		Chart          string `json:"chart"`
		TargetRevision string `json:"targetRevision"`
		Path           string `json:"path"`
	} `json:"source"`
	ValuesYAML string `json:"valuesYaml"`
}

func (s *Server) helmUpsert(w http.ResponseWriter, r *http.Request) {
	target, _, ok := s.authorizedHelmTarget(w, r, domain.PermissionHelmDeploy)
	if !ok || !s.helmMutationReady(w, r, false) {
		return
	}
	key, ok := helmIdempotencyKey(w, r)
	if !ok {
		return
	}
	var input helmValuesInput
	if !decodeHelmRequest(w, r, &input) {
		return
	}
	value, replay, err := s.helmApplications.Deploy(r.Context(), helmdirect.DeployRequest{Target: target,
		Actor: helmActor(r, key), Source: helmdirect.Source{Kind: helmdirect.SourceKind(input.Source.Kind),
			RepositoryURL: input.Source.RepositoryURL, Chart: input.Source.Chart, TargetRevision: input.Source.TargetRevision,
			Path: input.Source.Path}, Values: []byte(input.ValuesYAML)}, time.Now().UTC())
	s.writeHelmMutation(w, r, value, replay, err)
}

func (s *Server) helmHead(w http.ResponseWriter, r *http.Request) {
	target, _, ok := s.authorizedHelmTarget(w, r, domain.PermissionHelmRead)
	if !ok || !s.helmConfigured(w, r) {
		return
	}
	value, err := s.helmApplications.Head(r.Context(), target)
	if err != nil {
		writeHelmError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, helmReleaseView(value))
}

func (s *Server) helmHistory(w http.ResponseWriter, r *http.Request) {
	target, _, ok := s.authorizedHelmTarget(w, r, domain.PermissionHelmRead)
	if !ok || !s.helmConfigured(w, r) {
		return
	}
	limit, ok := parseHelmLimit(w, r, 25)
	if !ok {
		return
	}
	history, err := s.helmApplications.History(r.Context(), target, limit)
	if err != nil {
		writeHelmError(w, r, err)
		return
	}
	items := make([]helmReleaseRevisionView, len(history))
	for index := range history {
		items[index] = helmReleaseView(history[index])
	}
	w.Header().Set("Cache-Control", "private, no-store")
	collection(w, items)
}

func (s *Server) helmRetry(w http.ResponseWriter, r *http.Request) {
	s.helmMutation(w, r, domain.PermissionHelmRetry, false, func(target helmdirect.Target, actor helmdirect.Actor) (helmdirect.Revision, bool, error) {
		return s.helmApplications.Retry(r.Context(), helmdirect.MutationRequest{Target: target, Actor: actor}, time.Now().UTC())
	})
}

func (s *Server) helmDisable(w http.ResponseWriter, r *http.Request) {
	s.helmMutation(w, r, domain.PermissionHelmDeploy, false, func(target helmdirect.Target, actor helmdirect.Actor) (helmdirect.Revision, bool, error) {
		return s.helmApplications.Disable(r.Context(), helmdirect.MutationRequest{Target: target, Actor: actor}, time.Now().UTC())
	})
}

func (s *Server) helmMutation(w http.ResponseWriter, r *http.Request, permission domain.Permission, rollback bool,
	mutation func(helmdirect.Target, helmdirect.Actor) (helmdirect.Revision, bool, error)) {
	target, _, ok := s.authorizedHelmTarget(w, r, permission)
	if !ok || !s.helmMutationReady(w, r, rollback) {
		return
	}
	key, ok := helmIdempotencyKey(w, r)
	if !ok || !decodeHelmEmptyRequest(w, r) {
		return
	}
	value, replay, err := mutation(target, helmActor(r, key))
	s.writeHelmMutation(w, r, value, replay, err)
}

func (s *Server) helmRollback(w http.ResponseWriter, r *http.Request) {
	target, _, ok := s.authorizedHelmTarget(w, r, domain.PermissionHelmRollback)
	if !ok || !s.helmMutationReady(w, r, true) {
		return
	}
	key, ok := helmIdempotencyKey(w, r)
	if !ok {
		return
	}
	var input struct {
		SourceRevisionID string `json:"sourceRevisionId"`
	}
	if !decodeHelmRequest(w, r, &input) {
		return
	}
	value, replay, err := s.helmApplications.Rollback(r.Context(), helmdirect.MutationRequest{Target: target,
		Actor: helmActor(r, key), RollbackSourceID: strings.TrimSpace(input.SourceRevisionID)}, time.Now().UTC())
	s.writeHelmMutation(w, r, value, replay, err)
}

func (s *Server) authorizedHelmTarget(w http.ResponseWriter, r *http.Request, permission domain.Permission) (helmdirect.Target, domain.AccessTarget, bool) {
	applicationID, environmentID := strings.TrimSpace(r.PathValue("id")), strings.TrimSpace(r.PathValue("environmentId"))
	if !validUUID(applicationID) || !validUUID(environmentID) {
		mappedError(w, r, store.ErrNotFound)
		return helmdirect.Target{}, domain.AccessTarget{}, false
	}
	application, err := s.store.GetApplication(r.Context(), applicationID)
	if err != nil {
		mappedError(w, r, err)
		return helmdirect.Target{}, domain.AccessTarget{}, false
	}
	environment, err := s.store.GetEnvironment(r.Context(), environmentID)
	if err != nil || application.ProjectID == "" || application.ProjectID != environment.ProjectID || application.SourceKind != domain.ApplicationSourceHelm {
		mappedError(w, r, store.ErrNotFound)
		return helmdirect.Target{}, domain.AccessTarget{}, false
	}
	project, err := s.store.GetProject(r.Context(), application.ProjectID)
	if err != nil {
		mappedError(w, r, err)
		return helmdirect.Target{}, domain.AccessTarget{}, false
	}
	accessTarget := domain.AccessTarget{Type: "application", ID: application.ID, TeamID: project.TeamID,
		ProjectID: project.ID, EnvironmentID: environment.ID, Namespace: environment.Namespace, ApplicationID: application.ID}
	if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, permission, accessTarget); err != nil {
		mappedError(w, r, err)
		return helmdirect.Target{}, domain.AccessTarget{}, false
	}
	return helmdirect.Target{ProjectID: project.ID, EnvironmentID: environment.ID, ApplicationID: application.ID}, accessTarget, true
}

func (s *Server) helmConfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.helmApplications == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "HelmApplicationsUnavailable", "Helm Apps unavailable", "Direct Argo CD Helm App reconciliation is not configured.")
		return false
	}
	return true
}

func (s *Server) helmMutationReady(w http.ResponseWriter, r *http.Request, rollback bool) bool {
	if !s.helmConfigured(w, r) {
		return false
	}
	capabilities, err := s.helmApplications.Capabilities(r.Context())
	if err != nil || !capabilities.HelmDeployments || rollback && !capabilities.HelmRollbacks {
		writeProblem(w, r, http.StatusServiceUnavailable, "HelmRuntimeNotReady", "Helm Apps not ready", "Direct Argo CD Helm App reconciliation is unavailable.")
		return false
	}
	return true
}

func helmActor(r *http.Request, key string) helmdirect.Actor {
	return helmdirect.Actor{ID: currentUser(r.Context()).ID, IdempotencyKey: key, RequestID: requestID(r.Context())}
}

func (s *Server) writeHelmMutation(w http.ResponseWriter, r *http.Request, value helmdirect.Revision, replay bool, err error) {
	if err != nil {
		writeHelmError(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Location", "/v1/applications/"+value.Target.ApplicationID+"/environments/"+value.Target.EnvironmentID+"/helm/release")
	writeJSON(w, http.StatusAccepted, helmReleaseView(value))
}

func parseHelmLimit(w http.ResponseWriter, r *http.Request, fallback int) (int, bool) {
	query := r.URL.Query()
	for key, values := range query {
		if key != "limit" || len(values) != 1 || values[0] == "" {
			writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "Use only one bounded limit query parameter.")
			return 0, false
		}
	}
	if query.Get("limit") == "" {
		return fallback, true
	}
	value, err := strconv.Atoi(query.Get("limit"))
	if err != nil || value < 1 || value > 100 || strconv.Itoa(value) != query.Get("limit") {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "limit must be a canonical integer from 1 through 100.")
		return 0, false
	}
	return value, true
}

func helmIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 || !setupHTTPIdempotencyRE.MatchString(values[0]) {
		writeProblem(w, r, http.StatusBadRequest, "IdempotencyKeyRequired", "Idempotency key required", "Provide one Idempotency-Key header containing 16 to 128 safe ASCII characters.")
		return "", false
	}
	return values[0], true
}

func decodeHelmEmptyRequest(w http.ResponseWriter, r *http.Request) bool {
	var input struct{}
	return decodeHelmRequest(w, r, &input)
}

func decodeHelmRequest(w http.ResponseWriter, r *http.Request, output any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeProblem(w, r, http.StatusUnsupportedMediaType, "UnsupportedMediaType", "Unsupported media type", "Helm mutations require application/json.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaximumHelmHTTPRequestBytes)
	raw, err := io.ReadAll(r.Body)
	defer clear(raw)
	if err != nil || len(raw) < 2 || !utf8.Valid(raw) || !uniqueJSONObject(raw) {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body must be one bounded UTF-8 JSON object without duplicate fields.")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(output); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body contains an unknown or invalid field.")
		return false
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body must contain exactly one object.")
		return false
	}
	return true
}

const MaximumHelmHTTPRequestBytes = helmdirect.MaximumValuesBytes + 16<<10

func writeHelmError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, helmdirect.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "HelmAppNotFound", "Helm App not found", "The requested Helm App was not found.")
	case errors.Is(err, helmdirect.ErrConflict):
		writeProblem(w, r, http.StatusConflict, "HelmAppConflict", "Helm App conflict", "The requested change conflicts with current Helm App history.")
	case errors.Is(err, helmdirect.ErrInvalid):
		writeProblem(w, r, http.StatusUnprocessableEntity, "HelmValidationFailed", "Helm validation failed", "The source, revision, chart path, credential selection, or values YAML is invalid.")
	case errors.Is(err, helmdirect.ErrUnavailable):
		writeProblem(w, r, http.StatusServiceUnavailable, "HelmRuntimeNotReady", "Helm Apps not ready", "Direct Argo CD Helm App reconciliation is unavailable.")
	default:
		writeProblem(w, r, http.StatusInternalServerError, "HelmPersistenceFailed", "Helm request failed", "The Helm App request could not be completed.")
	}
}
