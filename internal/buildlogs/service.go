package buildlogs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/runtimeview"
)

type Service struct {
	resolver Resolver
	auditor  Auditor
	client   KubernetesClient
	redactor Redactor
	config   Config
	now      func() time.Time
}

func NewService(resolver Resolver, auditor Auditor, client KubernetesClient, redactor Redactor, config Config) (*Service, error) {
	if resolver == nil || auditor == nil || client == nil {
		return nil, ErrInvalidRequest
	}
	security := client.Security()
	if !security.TLSVerified || security.InsecureSkipTLSVerify {
		return nil, ErrInsecureTransport
	}
	if redactor == nil {
		redactor = runtimeview.NewDefenseInDepthRedactor()
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return &Service{resolver: resolver, auditor: auditor, client: client, redactor: redactor, config: config, now: func() time.Time { return time.Now().UTC() }}, nil
}

func validateConfig(c Config) error {
	if c.DefaultTailLines < 1 || c.MaxTailLines < c.DefaultTailLines || c.DefaultLimitBytes < 1 ||
		c.MaxSnapshotBytes < c.DefaultLimitBytes || c.MaxLineBytes < 1 || int64(c.MaxLineBytes) > c.MaxSnapshotBytes ||
		c.MaxLookback <= 0 || c.SnapshotTimeout <= 0 || c.FollowBuffer < 1 || c.MaxFollowBytes < c.DefaultLimitBytes ||
		c.MaxFollowDuration <= 0 || c.RevalidateInterval <= 0 || c.HeartbeatInterval <= 0 ||
		c.RediscoverInterval <= 0 || c.ReconnectDelay <= 0 || c.ReconnectOverlap < 0 || c.DedupeEntries < 1 {
		return ErrInvalidRequest
	}
	if c.MaxTailLines > 2_000 || c.MaxSnapshotBytes > 5<<20 || c.MaxLineBytes > 1<<20 ||
		c.MaxLookback > 7*24*time.Hour || c.SnapshotTimeout > 2*time.Minute || c.FollowBuffer > 4_096 || c.MaxFollowBytes > 100<<20 ||
		c.MaxFollowDuration > time.Hour || c.ReconnectOverlap > c.MaxLookback || c.DedupeEntries > 65_536 {
		return ErrInvalidRequest
	}
	return nil
}

func (s *Service) ensureSecure() error {
	security := s.client.Security()
	if !security.TLSVerified || security.InsecureSkipTLSVerify {
		return ErrInsecureTransport
	}
	return nil
}

func (s *Service) normalizeOptions(options LogOptions, follow bool) (LogOptions, error) {
	if err := s.ensureSecure(); err != nil {
		return LogOptions{}, err
	}
	if options.Follow != follow || follow && options.Previous {
		return LogOptions{}, ErrInvalidRequest
	}
	if options.TailLines == 0 {
		options.TailLines = s.config.DefaultTailLines
	}
	if options.TailLines < 1 || options.TailLines > s.config.MaxTailLines {
		return LogOptions{}, ErrInvalidRequest
	}
	if options.LimitBytes == 0 {
		options.LimitBytes = s.config.DefaultLimitBytes
	}
	if options.LimitBytes < 1 || options.LimitBytes > s.config.MaxSnapshotBytes {
		return LogOptions{}, ErrInvalidRequest
	}
	if options.SinceTime != nil {
		since := options.SinceTime.UTC()
		now := s.now()
		if since.After(now.Add(time.Minute)) || since.Before(now.Add(-s.config.MaxLookback)) {
			return LogOptions{}, ErrInvalidRequest
		}
		options.SinceTime = &since
	}
	return options, nil
}

func validAccess(access AccessRequest) bool {
	return uuidPattern.MatchString(access.ActorID) && uuidPattern.MatchString(access.AttemptID)
}

func (s *Service) resolve(ctx context.Context, access AccessRequest) (AuthorizedAttempt, error) {
	if !validAccess(access) {
		return AuthorizedAttempt{}, ErrInvalidRequest
	}
	authorized, err := s.resolver.Resolve(ctx, access)
	if err != nil {
		return AuthorizedAttempt{}, err
	}
	if authorized.Access != access || !uuidPattern.MatchString(authorized.ApplicationID) || !uuidPattern.MatchString(authorized.ProjectID) ||
		authorized.Attempt.ID != access.AttemptID || authorized.Attempt.ServiceID != authorized.ApplicationID ||
		authorized.Attempt.ProjectID != authorized.ProjectID || !kubeNamePattern.MatchString(authorized.Attempt.JobNamespace) ||
		!kubeNamePattern.MatchString(authorized.Attempt.JobName) || authorized.Attempt.PlanRequest.Namespace != authorized.Attempt.JobNamespace ||
		authorized.Attempt.PlanRequest.Build.OperationID != authorized.Attempt.ID || authorized.Attempt.PlanRequest.Build.Generation != authorized.Attempt.Generation ||
		authorized.Attempt.PlanRequest.Build.ProjectID != authorized.ProjectID || authorized.Attempt.PlanRequest.Build.ServiceID != authorized.ApplicationID ||
		authorized.Attempt.PlanRequest.Build.Commit != authorized.Attempt.CommitSHA || authorized.Attempt.CheckoutRequest.Commit != authorized.Attempt.CommitSHA {
		return AuthorizedAttempt{}, scopeStage("resolve attempt binding")
	}
	plan, err := builder.PlanJob(authorized.Attempt.PlanRequest)
	if err != nil || !builder.CanAdoptJob(plan.Job, authorized.Attempt.PlanRequest) {
		return AuthorizedAttempt{}, scopeStage("resolve build plan")
	}
	metadata, _ := plan.Job["metadata"].(map[string]any)
	if metadata["name"] != authorized.Attempt.JobName || metadata["namespace"] != authorized.Attempt.JobNamespace {
		return AuthorizedAttempt{}, scopeStage("resolve Job identity")
	}
	// Resolver results are documented immutable, but deep-copy the durable
	// attempt anyway so a follow session cannot race a mutable fake/cache.
	encoded, err := json.Marshal(authorized.Attempt)
	if err != nil {
		return AuthorizedAttempt{}, scopeStage("clone authorized attempt")
	}
	var cloned builds.BuildAttempt
	if err = json.Unmarshal(encoded, &cloned); err != nil {
		return AuthorizedAttempt{}, scopeStage("decode authorized attempt")
	}
	authorized.Attempt = cloned
	return authorized, nil
}

func (s *Service) audit(ctx context.Context, access AccessRequest, requestID, action string) error {
	if !requestIDPattern.MatchString(requestID) {
		return ErrInvalidRequest
	}
	return s.auditor.AuditBuildLogAccess(ctx, AuditEvent{ActorID: access.ActorID, AttemptID: access.AttemptID, Action: action, RequestID: requestID, At: s.now()})
}

type sourceBinding struct {
	authorized AuthorizedAttempt
	job        builds.ObservedBuildJob
	pod        builds.ObservedBuildPod
	source     Source
	redactions []string
}

func wrapScopeStage(stage string, err error) error {
	if errors.Is(err, ErrScopeViolation) {
		return fmt.Errorf("%s: %w", stage, ErrScopeViolation)
	}
	return err
}

func scopeStage(stage string) error {
	return fmt.Errorf("%s: %w", stage, ErrScopeViolation)
}

func (s *Service) discover(ctx context.Context, authorized AuthorizedAttempt, previous bool) (sourceBinding, error) {
	liveJob, err := s.client.GetBuildJob(ctx, authorized.Attempt.JobNamespace, authorized.Attempt.JobName)
	if err != nil {
		return sourceBinding{}, wrapScopeStage("get build Job", err)
	}
	job, err := builds.VerifyObservedBuildJob(authorized.Attempt, liveJob)
	if err != nil {
		return sourceBinding{}, fmt.Errorf("verify build Job: %v: %w", err, ErrScopeViolation)
	}
	pods, err := s.client.ListBuildJobPods(ctx, JobPodQuery{
		Namespace: job.Namespace, JobName: job.Name, JobUID: job.UID,
		OperationLabel: job.OperationLabel, GenerationLabel: job.GenerationLabel,
	})
	if err != nil {
		return sourceBinding{}, wrapScopeStage("list build Pods", err)
	}
	if len(pods) == 0 {
		return sourceBinding{}, ErrNotFound
	}
	if len(pods) != 1 {
		return sourceBinding{}, fmt.Errorf("build Pod count: %w", ErrScopeViolation)
	}
	pod, err := builds.VerifyObservedBuildPod(job, pods[0])
	if err != nil {
		return sourceBinding{}, fmt.Errorf("verify build Pod: %v: %w", err, ErrScopeViolation)
	}
	if previous && pod.AgentRestarts < 1 {
		return sourceBinding{}, ErrPreviousUnavailable
	}
	sum := sha256.Sum256([]byte(authorized.Attempt.ID + "\x00" + job.UID + "\x00" + pod.UID + "\x00" + boolString(previous)))
	source := Source{ID: "build_" + hex.EncodeToString(sum[:])[:32], Ready: pod.Ready, Previous: previous}
	return sourceBinding{
		authorized: authorized, job: job, pod: pod, source: source,
		redactions: []string{job.Namespace, job.Name, job.UID, pod.Name, pod.UID},
	}, nil
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func (s *Service) open(ctx context.Context, binding sourceBinding, options LogOptions) (io.ReadCloser, error) {
	// Re-read both owner and Pod immediately before logs. The adapter performs a
	// second Pod UID read at the subresource boundary as defense in depth.
	liveJob, err := s.client.GetBuildJob(ctx, binding.job.Namespace, binding.job.Name)
	if err != nil {
		return nil, wrapScopeStage("reget build Job", err)
	}
	job, err := builds.VerifyObservedBuildJob(binding.authorized.Attempt, liveJob)
	if err != nil {
		return nil, fmt.Errorf("reverify build Job: %v: %w", err, ErrScopeViolation)
	}
	if job.UID != binding.job.UID {
		return nil, ErrGone
	}
	livePod, err := s.client.GetBuildPod(ctx, binding.pod.Namespace, binding.pod.Name)
	if err != nil {
		return nil, wrapScopeStage("reget build Pod", err)
	}
	pod, err := builds.VerifyObservedBuildPod(job, livePod)
	if err != nil {
		return nil, fmt.Errorf("reverify build Pod: %v: %w", err, ErrScopeViolation)
	}
	if pod.UID != binding.pod.UID {
		return nil, ErrGone
	}
	if options.Previous && pod.AgentRestarts < 1 {
		return nil, ErrPreviousUnavailable
	}
	reader, err := s.client.OpenBuilderAgentLogs(ctx, ExactPodRef{Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID}, PodLogOptions{
		TailLines: options.TailLines, SinceTime: options.SinceTime, Previous: options.Previous,
		Timestamps: options.Timestamps, LimitBytes: options.LimitBytes, Follow: options.Follow,
	})
	if err != nil {
		return nil, wrapScopeStage("open builder-agent logs", err)
	}
	return reader, nil
}

func sameBuildIdentity(left, right AuthorizedAttempt) bool {
	return left.Access == right.Access && left.ApplicationID == right.ApplicationID && left.ProjectID == right.ProjectID &&
		left.Attempt.ID == right.Attempt.ID && left.Attempt.JobNamespace == right.Attempt.JobNamespace &&
		left.Attempt.JobName == right.Attempt.JobName && left.Attempt.Generation == right.Attempt.Generation &&
		left.Attempt.InputDigest == right.Attempt.InputDigest && left.Attempt.DefinitionID == right.Attempt.DefinitionID &&
		left.Attempt.CacheCandidate == right.Attempt.CacheCandidate && reflect.DeepEqual(left.Attempt.PlanRequest, right.Attempt.PlanRequest) &&
		reflect.DeepEqual(left.Attempt.CheckoutRequest, right.Attempt.CheckoutRequest)
}

func (s *Service) Snapshot(ctx context.Context, request SnapshotRequest) (Snapshot, error) {
	options, err := s.normalizeOptions(request.Options, false)
	if err != nil {
		return Snapshot{}, err
	}
	snapshotCtx, cancel := context.WithTimeout(ctx, s.config.SnapshotTimeout)
	defer cancel()
	authorized, err := s.resolve(snapshotCtx, request.Access)
	if err != nil {
		return Snapshot{}, err
	}
	if err = s.audit(snapshotCtx, request.Access, request.RequestID, "build.logs.snapshot"); err != nil {
		return Snapshot{}, err
	}
	binding, err := s.discover(snapshotCtx, authorized, options.Previous)
	if err != nil {
		return Snapshot{}, err
	}
	reader, err := s.open(snapshotCtx, binding, options)
	if err != nil {
		return Snapshot{}, err
	}
	defer reader.Close()
	lines, bytesRead, truncated, err := s.readLines(reader, binding.source, binding.redactions, options, false, options.TailLines)
	if err != nil && !errors.Is(err, io.EOF) {
		return Snapshot{}, err
	}
	return Snapshot{Source: binding.source, Lines: lines, Bytes: bytesRead, Truncated: truncated, ObservedAt: s.now()}, nil
}
