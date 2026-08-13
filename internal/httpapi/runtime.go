package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/runtimeview"
	"github.com/kuberploy/kuberploy/internal/store"
)

// NewRuntimeViewService binds the read-only Kubernetes client to Kuberploy's
// object authorization model. API callers supply opaque deployment IDs only;
// namespaces, Kubernetes names, selectors, and UIDs are resolved server-side.
func NewRuntimeViewService(st store.Store, client runtimeview.KubernetesClient) (RuntimeViewService, error) {
	if st == nil || client == nil {
		return nil, runtimeview.ErrInvalidRequest
	}
	service, err := runtimeview.NewService(&runtimeResolver{store: st, client: client}, client, runtimeview.NewDefenseInDepthRedactor(), runtimeview.DefaultConfig())
	if err != nil {
		return nil, err
	}
	return &runtimeViewAdapter{service: service, store: st, client: client}, nil
}

type runtimeViewAdapter struct {
	service *runtimeview.Service
	store   store.Store
	client  runtimeview.KubernetesClient
}

func (a *runtimeViewAdapter) Snapshot(ctx context.Context, request runtimeview.SnapshotRequest) (runtimeview.LogSnapshot, error) {
	return a.service.Snapshot(ctx, request)
}

func (a *runtimeViewAdapter) Events(ctx context.Context, request runtimeview.EventRequest) (runtimeview.EventSnapshot, error) {
	return a.service.Events(ctx, request)
}

func (a *runtimeViewAdapter) Follow(ctx context.Context, request runtimeview.FollowRequest) (RuntimeLogStream, error) {
	stream, err := a.service.Follow(ctx, request)
	if err != nil {
		return nil, err
	}
	return runtimeLogStream{stream}, nil
}

func (a *runtimeViewAdapter) Rollout(ctx context.Context, deploymentID string) (runtimeview.RolloutStatus, error) {
	if !validUUID(deploymentID) {
		return runtimeview.RolloutStatus{}, runtimeview.ErrNotFound
	}
	actor := currentUser(ctx).ID
	deployment, err := a.store.GetDeploymentForActor(ctx, actor, deploymentID)
	if err != nil {
		return runtimeview.RolloutStatus{}, collapseRuntimeAuthorization(err)
	}
	environment, err := a.store.GetEnvironment(ctx, deployment.EnvironmentID)
	if err != nil {
		return runtimeview.RolloutStatus{}, collapseRuntimeAuthorization(err)
	}
	application, err := a.store.GetApplication(ctx, deployment.ApplicationID)
	if err != nil || application.ProjectID != environment.ProjectID {
		return runtimeview.RolloutStatus{}, runtimeview.ErrNotFound
	}
	workloadType := deployment.Runtime.WorkloadType
	if workloadType == "" {
		workloadType = "Deployment"
	}
	if workloadType == "StatefulSet" {
		reader, ok := a.client.(runtimeview.StatefulSetReader)
		if !ok {
			return runtimeview.RolloutStatus{}, runtimeview.ErrNotFound
		}
		live, readErr := reader.GetStatefulSet(ctx, environment.Namespace, runtimeDeploymentName(application.ID))
		if readErr != nil {
			return runtimeview.RolloutStatus{}, readErr
		}
		return runtimeview.RolloutStatus{DesiredReplicas: live.DesiredReplicas, ReadyReplicas: live.ReadyReplicas,
			Conditions: append([]runtimeview.DeploymentCondition(nil), live.Conditions...), CurrentRevision: live.CurrentRevision,
			UpdateRevision: live.UpdateRevision, ObservedAt: time.Now().UTC()}, nil
	}
	live, err := a.client.GetDeployment(ctx, environment.Namespace, runtimeDeploymentName(application.ID))
	if err != nil {
		return runtimeview.RolloutStatus{}, err
	}
	return runtimeview.RolloutStatus{DesiredReplicas: live.DesiredReplicas, ReadyReplicas: live.ReadyReplicas,
		Conditions: append([]runtimeview.DeploymentCondition(nil), live.Conditions...), ObservedAt: time.Now().UTC()}, nil
}

type runtimeLogStream struct{ stream *runtimeview.Stream }

func (s runtimeLogStream) Channel() <-chan runtimeview.StreamEvent { return s.stream.Events }
func (s runtimeLogStream) Close()                                  { s.stream.Close() }

type runtimeResolver struct {
	store  store.Store
	client runtimeview.KubernetesClient
}

func (r *runtimeResolver) Resolve(ctx context.Context, target runtimeview.OpaqueTarget) (runtimeview.AuthorizedTarget, error) {
	if target.Kind != runtimeview.TargetDeployment || !validUUID(target.ID) {
		return runtimeview.AuthorizedTarget{}, runtimeview.ErrNotFound
	}
	actor := currentUser(ctx).ID
	deployment, err := r.store.GetDeploymentForActor(ctx, actor, target.ID)
	if err != nil {
		return runtimeview.AuthorizedTarget{}, collapseRuntimeAuthorization(err)
	}
	if err = r.store.Authorize(ctx, actor, domain.PermissionLogsRead, domain.AccessTarget{Type: "deployment", ID: deployment.ID}); err != nil {
		return runtimeview.AuthorizedTarget{}, collapseRuntimeAuthorization(err)
	}
	environment, err := r.store.GetEnvironment(ctx, deployment.EnvironmentID)
	if err != nil {
		return runtimeview.AuthorizedTarget{}, collapseRuntimeAuthorization(err)
	}
	application, err := r.store.GetApplication(ctx, deployment.ApplicationID)
	if err != nil || application.ProjectID != environment.ProjectID {
		return runtimeview.AuthorizedTarget{}, runtimeview.ErrNotFound
	}
	name := runtimeDeploymentName(application.ID)
	workloadType := deployment.Runtime.WorkloadType
	if workloadType == "" {
		workloadType = "Deployment"
	}
	if workloadType == "StatefulSet" {
		reader, ok := r.client.(runtimeview.StatefulSetReader)
		if !ok {
			return runtimeview.AuthorizedTarget{}, runtimeview.ErrNotFound
		}
		live, readErr := reader.GetStatefulSet(ctx, environment.Namespace, name)
		if readErr != nil {
			return runtimeview.AuthorizedTarget{}, readErr
		}
		return runtimeview.AuthorizedTarget{
			Reference: target, ApplicationID: application.ID, Namespace: environment.Namespace,
			Deployments: []runtimeview.DeploymentRef{{Kind: "StatefulSet", Name: name, UID: live.UID}},
		}, nil
	}
	live, err := r.client.GetDeployment(ctx, environment.Namespace, name)
	if err != nil {
		return runtimeview.AuthorizedTarget{}, err
	}
	return runtimeview.AuthorizedTarget{
		Reference:     target,
		ApplicationID: application.ID,
		Namespace:     environment.Namespace,
		Deployments:   []runtimeview.DeploymentRef{{Kind: "Deployment", Name: name, UID: live.UID}},
	}, nil
}

func (r *runtimeResolver) Revalidate(ctx context.Context, target runtimeview.OpaqueTarget) error {
	if target.Kind != runtimeview.TargetDeployment || !validUUID(target.ID) {
		return runtimeview.ErrUnauthorized
	}
	actor := currentUser(ctx).ID
	if _, err := r.store.GetDeploymentForActor(ctx, actor, target.ID); err != nil {
		return runtimeview.ErrUnauthorized
	}
	if err := r.store.Authorize(ctx, actor, domain.PermissionLogsRead, domain.AccessTarget{Type: "deployment", ID: target.ID}); err != nil {
		return runtimeview.ErrUnauthorized
	}
	return nil
}

func collapseRuntimeAuthorization(err error) error {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrForbidden) {
		return runtimeview.ErrNotFound
	}
	return err
}

func runtimeDeploymentName(applicationID string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(applicationID)))
	return "kp-a-" + hex.EncodeToString(digest[:8])
}

type workloadView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Replicas  int    `json:"replicas"`
	Revision  string `json:"revision,omitempty"`
	State     string `json:"state"`
}

func (s *Server) applicationWorkloads(w http.ResponseWriter, r *http.Request) {
	applicationID := r.PathValue("id")
	actor := currentUser(r.Context()).ID
	application, err := s.store.GetApplicationForActor(r.Context(), actor, applicationID)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	deployments, err := s.store.ListDeploymentsForActor(r.Context(), actor)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	items := make([]workloadView, 0)
	for _, deployment := range deployments {
		if deployment.ApplicationID != application.ID {
			continue
		}
		environment, getErr := s.store.GetEnvironment(r.Context(), deployment.EnvironmentID)
		if getErr != nil || environment.ProjectID != application.ProjectID {
			writeProblem(w, r, http.StatusInternalServerError, "RuntimeProjectionInvalid", "Runtime view unavailable", "The workload ownership projection is inconsistent.")
			return
		}
		kind := deployment.Runtime.WorkloadType
		if kind == "" {
			kind = "Deployment"
		}
		items = append(items, workloadView{ID: deployment.ID, Name: runtimeDeploymentName(application.ID), Kind: kind, Namespace: environment.Namespace, Replicas: deployment.Runtime.Replicas, Revision: deployment.DesiredRevision, State: deployment.State})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace != items[j].Namespace {
			return items[i].Namespace < items[j].Namespace
		}
		return items[i].ID < items[j].ID
	})
	w.Header().Set("Cache-Control", "private, no-store")
	collection(w, items)
}

func (s *Server) workloadLogs(w http.ResponseWriter, r *http.Request) {
	if s.runtime == nil {
		runtimeUnavailable(w, r)
		return
	}
	options, _, ok := parseRuntimeLogQuery(w, r, false)
	if !ok {
		return
	}
	deploymentID := r.PathValue("id")
	if !validUUID(deploymentID) {
		writeProblem(w, r, http.StatusNotFound, "NotFound", "Not found", "The requested resource was not found.")
		return
	}
	if err := s.store.AuditRuntimeAccess(r.Context(), currentUser(r.Context()).ID, deploymentID, "runtime.logs.snapshot", requestID(r.Context())); err != nil {
		mappedError(w, r, err)
		return
	}
	snapshot, err := s.runtime.Snapshot(r.Context(), runtimeview.SnapshotRequest{Target: runtimeview.OpaqueTarget{Kind: runtimeview.TargetDeployment, ID: deploymentID}, Options: options})
	if err != nil {
		writeRuntimeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) workloadEvents(w http.ResponseWriter, r *http.Request) {
	if s.runtime == nil {
		runtimeUnavailable(w, r)
		return
	}
	limit, ok := parseEventQuery(w, r)
	if !ok {
		return
	}
	deploymentID := r.PathValue("id")
	if !validUUID(deploymentID) {
		writeProblem(w, r, http.StatusNotFound, "NotFound", "Not found", "The requested resource was not found.")
		return
	}
	if err := s.store.AuditRuntimeAccess(r.Context(), currentUser(r.Context()).ID, deploymentID, "runtime.events.snapshot", requestID(r.Context())); err != nil {
		mappedError(w, r, err)
		return
	}
	snapshot, err := s.runtime.Events(r.Context(), runtimeview.EventRequest{Target: runtimeview.OpaqueTarget{Kind: runtimeview.TargetDeployment, ID: deploymentID}, Limit: limit})
	if err != nil {
		writeRuntimeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) followWorkloadLogs(w http.ResponseWriter, r *http.Request) {
	if s.runtime == nil {
		runtimeUnavailable(w, r)
		return
	}
	options, cursors, ok := parseRuntimeLogQuery(w, r, true)
	if !ok {
		return
	}
	deploymentID := r.PathValue("id")
	if !validUUID(deploymentID) {
		writeProblem(w, r, http.StatusNotFound, "NotFound", "Not found", "The requested resource was not found.")
		return
	}
	if err := s.store.AuditRuntimeAccess(r.Context(), currentUser(r.Context()).ID, deploymentID, "runtime.logs.follow", requestID(r.Context())); err != nil {
		mappedError(w, r, err)
		return
	}
	stream, err := s.runtime.Follow(r.Context(), runtimeview.FollowRequest{Target: runtimeview.OpaqueTarget{Kind: runtimeview.TargetDeployment, ID: deploymentID}, Options: options, Cursors: cursors})
	if err != nil {
		writeRuntimeError(w, r, err)
		return
	}
	defer stream.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Time{})
	encoder := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-stream.Channel():
			if !open {
				return
			}
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err = encoder.Encode(event); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			_ = controller.SetWriteDeadline(time.Time{})
		}
	}
}

var runtimeLogQueryParameters = map[string]struct{}{
	"pod": {}, "revision": {}, "container": {}, "tailLines": {}, "since": {}, "previous": {}, "limitBytes": {}, "cursor": {},
}

var (
	runtimePodPattern       = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$`)
	runtimeContainerPattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	runtimeRevisionPattern  = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
)

func parseRuntimeLogQuery(w http.ResponseWriter, r *http.Request, follow bool) (runtimeview.LogOptions, []runtimeview.ReconnectCursor, bool) {
	query := r.URL.Query()
	for key, values := range query {
		_, allowed := runtimeLogQueryParameters[key]
		if !allowed || key != "cursor" && len(values) != 1 || key == "cursor" && (!follow || len(values) > 50) {
			writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The runtime query contains an unknown or repeated parameter.", FieldError{Pointer: "/query/" + key, Code: "InvalidQueryParameter", Detail: "Use only documented bounded parameters."})
			return runtimeview.LogOptions{}, nil, false
		}
		for _, value := range values {
			if value == "" || strings.TrimSpace(value) != value || len(value) > 2048 {
				writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The runtime query contains an empty, non-canonical, or oversized value.", FieldError{Pointer: "/query/" + key, Code: "InvalidQueryParameter", Detail: "Use one canonical bounded value."})
				return runtimeview.LogOptions{}, nil, false
			}
		}
	}
	options := runtimeview.LogOptions{TailLines: 200, LimitBytes: 1 << 20, Timestamps: true, Follow: follow}
	options.Pod = query.Get("pod")
	options.Revision = query.Get("revision")
	options.Container = query.Get("container")
	if options.Pod != "" && !runtimePodPattern.MatchString(options.Pod) {
		return invalidRuntimeQuery(w, r, "pod", "Use one exact Pod name returned by the workload log source list.")
	}
	if options.Revision != "" && !runtimeRevisionPattern.MatchString(options.Revision) {
		return invalidRuntimeQuery(w, r, "revision", "Use one exact numeric workload revision returned by the log source list.")
	}
	if options.Container != "" && !runtimeContainerPattern.MatchString(options.Container) {
		return invalidRuntimeQuery(w, r, "container", "Use one exact container name returned by the workload log source list.")
	}
	if value := query.Get("tailLines"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 2_000 || strconv.Itoa(parsed) != value {
			return invalidRuntimeQuery(w, r, "tailLines", "Use an integer from 1 through 2000.")
		}
		options.TailLines = int64(parsed)
	}
	if value := query.Get("limitBytes"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 1 || parsed > 5<<20 || strconv.FormatInt(parsed, 10) != value {
			return invalidRuntimeQuery(w, r, "limitBytes", "Use an integer from 1 through 5242880.")
		}
		options.LimitBytes = parsed
	}
	if value := query.Get("since"); value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || parsed.Format(time.RFC3339Nano) != value {
			return invalidRuntimeQuery(w, r, "since", "Use one canonical RFC 3339 timestamp within the allowed lookback.")
		}
		options.SinceTime = &parsed
	}
	if value := query.Get("previous"); value != "" {
		if value != "true" && value != "false" {
			return invalidRuntimeQuery(w, r, "previous", "Use true or false.")
		}
		options.Previous = value == "true"
	}
	if follow && options.Previous {
		return invalidRuntimeQuery(w, r, "previous", "Previous-container logs are snapshot-only.")
	}
	var cursors []runtimeview.ReconnectCursor
	for _, encoded := range query["cursor"] {
		cursor, err := decodeRuntimeCursor(encoded)
		if err != nil {
			return invalidRuntimeQuery(w, r, "cursor", "Use a base64url-encoded cursor returned by a previous stream line.")
		}
		cursors = append(cursors, cursor)
	}
	return options, cursors, true
}

func invalidRuntimeQuery(w http.ResponseWriter, r *http.Request, field, detail string) (runtimeview.LogOptions, []runtimeview.ReconnectCursor, bool) {
	writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The runtime query is invalid.", FieldError{Pointer: "/query/" + field, Code: "InvalidQueryParameter", Detail: detail})
	return runtimeview.LogOptions{}, nil, false
}

func decodeRuntimeCursor(encoded string) (runtimeview.ReconnectCursor, error) {
	if len(encoded) < 16 || len(encoded) > 1024 {
		return runtimeview.ReconnectCursor{}, runtimeview.ErrInvalidRequest
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) > 768 {
		clear(raw)
		return runtimeview.ReconnectCursor{}, runtimeview.ErrInvalidRequest
	}
	defer clear(raw)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var cursor runtimeview.ReconnectCursor
	if err = decoder.Decode(&cursor); err != nil {
		return runtimeview.ReconnectCursor{}, runtimeview.ErrInvalidRequest
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		return runtimeview.ReconnectCursor{}, runtimeview.ErrInvalidRequest
	}
	return cursor, nil
}

func parseEventQuery(w http.ResponseWriter, r *http.Request) (int, bool) {
	query := r.URL.Query()
	for key, values := range query {
		if key != "limit" || len(values) != 1 || values[0] == "" || strings.TrimSpace(values[0]) != values[0] {
			writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The event query contains an unsupported or repeated parameter.")
			return 0, false
		}
	}
	limit := 50
	if value := query.Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 200 || strconv.Itoa(parsed) != value {
			writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "Event limit must be an integer from 1 through 200.")
			return 0, false
		}
		limit = parsed
	}
	return limit, true
}

func runtimeUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusServiceUnavailable, "RuntimeViewUnavailable", "Runtime view unavailable", "No verified Kubernetes runtime-view boundary is configured. The workload continues running.")
}

func writeRuntimeError(w http.ResponseWriter, r *http.Request, err error) {
	var tooMany *runtimeview.TooManySourcesError
	switch {
	case errors.As(err, &tooMany):
		writeProblem(w, r, http.StatusUnprocessableEntity, "TooManySources", "Too many log sources", fmt.Sprintf("The deployment has %d Pod/container sources; narrow the request below the limit of %d.", tooMany.Count, tooMany.Limit))
	case errors.Is(err, runtimeview.ErrInvalidRequest), errors.Is(err, runtimeview.ErrContainerRequired), errors.Is(err, runtimeview.ErrContainerNotFound), errors.Is(err, runtimeview.ErrSourceNotFound), errors.Is(err, runtimeview.ErrPreviousUnavailable):
		writeProblem(w, r, http.StatusUnprocessableEntity, "RuntimeQueryInvalid", "Runtime query invalid", "The requested Pod/container log or event view is invalid or unavailable.")
	case errors.Is(err, runtimeview.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "NotFound", "Not found", "The requested runtime target was not found.")
	case errors.Is(err, runtimeview.ErrUnauthorized):
		writeProblem(w, r, http.StatusForbidden, "RuntimeAuthorizationRevoked", "Permission denied", "Runtime-view authorization was revoked.")
	case errors.Is(err, runtimeview.ErrGone):
		writeProblem(w, r, http.StatusGone, "RuntimeObjectReplaced", "Runtime object replaced", "The resolved Kubernetes object was replaced; reload the workload view.")
	case errors.Is(err, runtimeview.ErrTooManySources), errors.Is(err, runtimeview.ErrResponseLimitReached):
		writeProblem(w, r, http.StatusUnprocessableEntity, "RuntimeLimitReached", "Runtime view limit reached", "Narrow the Pod, container, time, line, or byte range.")
	case errors.Is(err, runtimeview.ErrInsecureTransport):
		writeProblem(w, r, http.StatusServiceUnavailable, "RuntimeTransportRejected", "Runtime view unavailable", "The Kubernetes API transport is not verified.")
	case errors.Is(err, runtimeview.ErrScopeViolation), errors.Is(err, runtimeview.ErrSelectorNotAllowed):
		writeProblem(w, r, http.StatusBadGateway, "RuntimeResponseRejected", "Runtime response rejected", "Kubernetes returned an object outside the authorized runtime scope.")
	default:
		writeProblem(w, r, http.StatusServiceUnavailable, "RuntimeViewUnavailable", "Runtime view unavailable", "The Kubernetes runtime view could not be read. The workload continues running.")
	}
}
