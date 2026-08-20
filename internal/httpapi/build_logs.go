package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/buildlogs"
	"github.com/kuberploy/kuberploy/internal/store"
)

// NewBuildLogService joins the immutable build-attempt catalog to the central
// application/project authorization store and the narrow Kubernetes reader.
// Callers can only address an opaque build attempt ID.
func NewBuildLogService(st store.Store, attempts buildlogs.AttemptCatalog, client buildlogs.KubernetesClient) (BuildLogService, error) {
	if st == nil || attempts == nil || client == nil {
		return nil, buildlogs.ErrInvalidRequest
	}
	resolver, err := buildlogs.NewRecordResolver(attempts, st)
	if err != nil {
		return nil, err
	}
	auditor, err := buildlogs.NewStoreAuditor(st)
	if err != nil {
		return nil, err
	}
	service, err := buildlogs.NewService(resolver, auditor, client, nil, buildlogs.DefaultConfig())
	if err != nil {
		return nil, err
	}
	// PostgreSQL resolves build_attempts inside AuditBuildLogAccess. The memory
	// store keeps builds in a separate catalog, so bind only its narrow immutable
	// ownership adapter without teaching the central store about build payloads.
	if binder, ok := st.(interface {
		BindBuildAttemptAuditCatalog(store.BuildLogAttemptCatalog)
	}); ok {
		ownership, ownershipErr := buildlogs.NewAttemptOwnershipCatalog(attempts)
		if ownershipErr != nil {
			return nil, ownershipErr
		}
		binder.BindBuildAttemptAuditCatalog(ownership)
	}
	return &buildLogServiceAdapter{service: service}, nil
}

type buildLogServiceAdapter struct{ service *buildlogs.Service }

func (a *buildLogServiceAdapter) Snapshot(ctx context.Context, request buildlogs.SnapshotRequest) (buildlogs.Snapshot, error) {
	return a.service.Snapshot(ctx, request)
}

func (a *buildLogServiceAdapter) Follow(ctx context.Context, request buildlogs.FollowRequest) (BuildLogStream, error) {
	stream, err := a.service.Follow(ctx, request)
	if err != nil {
		return nil, err
	}
	return buildLogStream{stream: stream}, nil
}

type buildLogStream struct{ stream *buildlogs.Stream }

func (s buildLogStream) Channel() <-chan buildlogs.StreamEvent { return s.stream.Events }
func (s buildLogStream) Close()                                { s.stream.Close() }

var buildLogQueryParameters = map[string]struct{}{
	"follow": {}, "tailLines": {}, "since": {}, "previous": {}, "limitBytes": {}, "cursor": {},
}

func (s *Server) buildAttemptLogs(w http.ResponseWriter, r *http.Request) {
	if !s.buildLogsAvailable(r.Context()) {
		buildLogsUnavailable(w, r)
		return
	}
	attemptID := r.PathValue("id")
	if !validUUID(attemptID) {
		writeProblem(w, r, http.StatusNotFound, "NotFound", "Not found", "The requested build was not found.")
		return
	}
	options, cursor, follow, ok := parseBuildLogQuery(w, r)
	if !ok {
		return
	}
	access := buildlogs.AccessRequest{ActorID: currentUser(r.Context()).ID, AttemptID: attemptID}
	if !follow {
		snapshot, err := s.buildLogs.Snapshot(r.Context(), buildlogs.SnapshotRequest{Access: access, RequestID: requestID(r.Context()), Options: options})
		if err != nil {
			writeBuildLogError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		writeJSON(w, http.StatusOK, snapshot)
		return
	}
	stream, err := s.buildLogs.Follow(r.Context(), buildlogs.FollowRequest{Access: access, RequestID: requestID(r.Context()), Options: options, Cursor: cursor})
	if err != nil {
		writeBuildLogError(w, r, err)
		return
	}
	defer stream.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Time{})
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
			if err = writeBuildLogSSE(w, event); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			_ = controller.SetWriteDeadline(time.Time{})
		}
	}
}

func (s *Server) buildLogsAvailable(ctx context.Context) bool {
	if s.buildLogs == nil || s.buildLogReadiness == nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.buildLogReadiness.Probe(probeCtx) == nil
}

func parseBuildLogQuery(w http.ResponseWriter, r *http.Request) (buildlogs.LogOptions, *buildlogs.ReconnectCursor, bool, bool) {
	query := r.URL.Query()
	for key, values := range query {
		if _, allowed := buildLogQueryParameters[key]; !allowed || len(values) != 1 || values[0] == "" || strings.TrimSpace(values[0]) != values[0] || len(values[0]) > 2048 {
			return invalidBuildLogQuery(w, r, key, "Use only one canonical value for each documented bounded parameter.")
		}
	}
	follow := false
	if value := query.Get("follow"); value != "" {
		if value != "true" && value != "false" {
			return invalidBuildLogQuery(w, r, "follow", "Use true or false.")
		}
		follow = value == "true"
	}
	options := buildlogs.LogOptions{TailLines: 200, LimitBytes: 1 << 20, Timestamps: true, Follow: follow}
	if value := query.Get("tailLines"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 1 || parsed > 2_000 || strconv.FormatInt(parsed, 10) != value {
			return invalidBuildLogQuery(w, r, "tailLines", "Use an integer from 1 through 2000.")
		}
		options.TailLines = parsed
	}
	if value := query.Get("limitBytes"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 1 || parsed > 5<<20 || strconv.FormatInt(parsed, 10) != value {
			return invalidBuildLogQuery(w, r, "limitBytes", "Use an integer from 1 through 5242880.")
		}
		options.LimitBytes = parsed
	}
	if value := query.Get("since"); value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || parsed.Format(time.RFC3339Nano) != value {
			return invalidBuildLogQuery(w, r, "since", "Use one canonical RFC 3339 timestamp within the bounded lookback.")
		}
		options.SinceTime = &parsed
	}
	if value := query.Get("previous"); value != "" {
		if value != "true" && value != "false" {
			return invalidBuildLogQuery(w, r, "previous", "Use true or false.")
		}
		options.Previous = value == "true"
	}
	if follow && options.Previous {
		return invalidBuildLogQuery(w, r, "previous", "Previous builder logs are snapshot-only.")
	}
	encodedCursor := query.Get("cursor")
	headerCursor := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if encodedCursor != "" && headerCursor != "" {
		return invalidBuildLogQuery(w, r, "cursor", "Use either cursor or Last-Event-ID, not both.")
	}
	if !follow && (encodedCursor != "" || headerCursor != "") {
		return invalidBuildLogQuery(w, r, "cursor", "Reconnect cursors are valid only when follow=true.")
	}
	if encodedCursor == "" {
		encodedCursor = headerCursor
	}
	var cursor *buildlogs.ReconnectCursor
	if encodedCursor != "" {
		decoded, err := decodeBuildLogCursor(encodedCursor)
		if err != nil {
			return invalidBuildLogQuery(w, r, "cursor", "Use the opaque cursor returned in a previous stream event.")
		}
		cursor = &decoded
	}
	return options, cursor, follow, true
}

func invalidBuildLogQuery(w http.ResponseWriter, r *http.Request, field, detail string) (buildlogs.LogOptions, *buildlogs.ReconnectCursor, bool, bool) {
	if field == "" {
		field = "query"
	}
	writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The build log query is invalid.", FieldError{Pointer: "/query/" + field, Code: "InvalidQueryParameter", Detail: detail})
	return buildlogs.LogOptions{}, nil, false, false
}

type buildLogCursorDocument struct {
	SourceID    string    `json:"sourceId"`
	Timestamp   time.Time `json:"timestamp"`
	Fingerprint string    `json:"fingerprint"`
}

func encodeBuildLogCursor(cursor *buildlogs.LineCursor) string {
	if cursor == nil {
		return ""
	}
	raw, err := json.Marshal(buildLogCursorDocument{SourceID: cursor.SourceID, Timestamp: cursor.Timestamp.UTC(), Fingerprint: cursor.Fingerprint})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeBuildLogCursor(encoded string) (buildlogs.ReconnectCursor, error) {
	if len(encoded) < 16 || len(encoded) > 1024 || strings.TrimSpace(encoded) != encoded {
		return buildlogs.ReconnectCursor{}, buildlogs.ErrInvalidRequest
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) > 768 {
		clear(raw)
		return buildlogs.ReconnectCursor{}, buildlogs.ErrInvalidRequest
	}
	defer clear(raw)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document buildLogCursorDocument
	if err = decoder.Decode(&document); err != nil {
		return buildlogs.ReconnectCursor{}, buildlogs.ErrInvalidRequest
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF || document.SourceID == "" || document.Timestamp.IsZero() || document.Fingerprint == "" {
		return buildlogs.ReconnectCursor{}, buildlogs.ErrInvalidRequest
	}
	return buildlogs.ReconnectCursor{SourceID: document.SourceID, Timestamp: document.Timestamp.UTC(), Fingerprint: document.Fingerprint}, nil
}

func writeBuildLogSSE(w io.Writer, event buildlogs.StreamEvent) error {
	switch event.Type {
	case buildlogs.StreamLine, buildlogs.StreamStatus, buildlogs.StreamGap, buildlogs.StreamHeartbeat, buildlogs.StreamTerminal:
	default:
		return buildlogs.ErrScopeViolation
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(w, "event: %s\n", event.Type); err != nil {
		return err
	}
	if event.Line != nil && event.Line.Cursor != nil {
		if cursor := encodeBuildLogCursor(event.Line.Cursor); cursor != "" {
			if _, err = fmt.Fprintf(w, "id: %s\n", cursor); err != nil {
				return err
			}
		}
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", payload)
	return err
}

func buildLogsUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusServiceUnavailable, "BuildLogRuntimeUnavailable", "Build logs unavailable", "No verified Kubernetes source-build log boundary is available. Build metadata remains available.")
}

func writeBuildLogError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, buildlogs.ErrInvalidRequest), errors.Is(err, buildlogs.ErrPreviousUnavailable):
		writeProblem(w, r, http.StatusUnprocessableEntity, "BuildLogQueryInvalid", "Build log query invalid", "The requested bounded build log view is invalid or unavailable.")
	case errors.Is(err, buildlogs.ErrNotFound), errors.Is(err, buildlogs.ErrUnauthorized):
		writeProblem(w, r, http.StatusNotFound, "NotFound", "Not found", "The requested build was not found.")
	case errors.Is(err, buildlogs.ErrGone):
		writeProblem(w, r, http.StatusGone, "BuildLogSourceReplaced", "Build log source replaced", "The exact build log source was replaced or deleted; reload the build view.")
	case errors.Is(err, buildlogs.ErrResponseLimitReached):
		writeProblem(w, r, http.StatusUnprocessableEntity, "BuildLogLimitReached", "Build log limit reached", "Narrow the line, time, or byte range.")
	case errors.Is(err, buildlogs.ErrInsecureTransport):
		writeProblem(w, r, http.StatusServiceUnavailable, "BuildLogTransportRejected", "Build logs unavailable", "The Kubernetes API transport is not verified.")
	case errors.Is(err, buildlogs.ErrScopeViolation):
		slog.ErrorContext(r.Context(), "build log Kubernetes scope rejected", "error", err.Error())
		writeProblem(w, r, http.StatusBadGateway, "BuildLogResponseRejected", "Build log response rejected", "Kubernetes returned an object outside the authorized build scope.")
	default:
		writeProblem(w, r, http.StatusServiceUnavailable, "BuildLogRuntimeUnavailable", "Build logs unavailable", "The Kubernetes build log boundary could not be read. Build metadata remains available.")
	}
}
