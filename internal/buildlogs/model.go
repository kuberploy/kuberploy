package buildlogs

import (
	"context"
	"errors"
	"io"
	"regexp"
	"time"

	"github.com/kuberploy/kuberploy/internal/builds"
)

var (
	ErrInvalidRequest       = errors.New("build log request is invalid")
	ErrNotFound             = errors.New("build log source was not found")
	ErrUnauthorized         = errors.New("build log authorization was revoked")
	ErrGone                 = errors.New("build log source was replaced")
	ErrScopeViolation       = errors.New("Kubernetes response escaped the authorized build scope")
	ErrInsecureTransport    = errors.New("Kubernetes client must verify TLS")
	ErrPreviousUnavailable  = errors.New("previous builder logs are unavailable")
	ErrResponseLimitReached = errors.New("build log response limit reached")
)

var (
	uuidPattern        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	kubeNamePattern    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$`)
	uidPattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
	requestIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	sourceIDPattern    = regexp.MustCompile(`^build_[a-f0-9]{32}$`)
	fingerprintPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// AccessRequest is constructed by the authenticated HTTP boundary. The only
// caller-originating resource value is the opaque attempt ID; it has no
// namespace, Job, Pod, selector, container, URL, or stored log-reference field.
type AccessRequest struct {
	ActorID   string
	AttemptID string
}

// AuthorizedAttempt is an immutable result from Resolver. Implementations
// must load the existing attempt, application, and project records, prove the
// attempt belongs to that application/project, require both builds.read and
// logs.read visibility, and collapse missing/unauthorized resources.
type AuthorizedAttempt struct {
	Access        AccessRequest
	Attempt       builds.BuildAttempt
	ApplicationID string
	ProjectID     string
}

type Resolver interface {
	Resolve(context.Context, AccessRequest) (AuthorizedAttempt, error)
	Revalidate(context.Context, AccessRequest) error
}

type AuditEvent struct {
	ActorID   string
	AttemptID string
	Action    string
	RequestID string
	At        time.Time
}

// Auditor must transactionally re-resolve the attempt/application/project,
// recheck builds.read plus logs.read, and durably record access without
// Kubernetes object names or stored log references. Log access fails closed
// when that authorization/audit transaction fails.
type Auditor interface {
	AuditBuildLogAccess(context.Context, AuditEvent) error
}

type ClientSecurity struct {
	TLSVerified           bool
	InsecureSkipTLSVerify bool
}

// JobPodQuery is generated only after exact attempt-to-Job verification. It is
// not an arbitrary selector surface; the Kubernetes adapter encodes exactly
// the two immutable builder labels and a fixed limit of two.
type JobPodQuery struct {
	Namespace       string
	JobName         string
	JobUID          string
	OperationLabel  string
	GenerationLabel string
}

type PodLogOptions struct {
	TailLines  int64
	SinceTime  *time.Time
	Previous   bool
	Timestamps bool
	LimitBytes int64
	Follow     bool
}

type ExactPodRef struct {
	Namespace string
	Name      string
	UID       string
}

// KubernetesClient is intentionally incapable of generic REST calls,
// mutations, Secret reads, proxying, exec, attach, or port-forward. The log
// method always targets the fixed builder-agent container.
type KubernetesClient interface {
	Security() ClientSecurity
	GetBuildJob(context.Context, string, string) (map[string]any, error)
	ListBuildJobPods(context.Context, JobPodQuery) ([]map[string]any, error)
	GetBuildPod(context.Context, string, string) (map[string]any, error)
	OpenBuilderAgentLogs(context.Context, ExactPodRef, PodLogOptions) (io.ReadCloser, error)
}

type Redactor interface {
	Redact(string) string
}

type Config struct {
	DefaultTailLines   int64
	MaxTailLines       int64
	DefaultLimitBytes  int64
	MaxSnapshotBytes   int64
	MaxLineBytes       int
	MaxLookback        time.Duration
	SnapshotTimeout    time.Duration
	FollowBuffer       int
	MaxFollowBytes     int64
	MaxFollowDuration  time.Duration
	RevalidateInterval time.Duration
	HeartbeatInterval  time.Duration
	RediscoverInterval time.Duration
	ReconnectDelay     time.Duration
	ReconnectOverlap   time.Duration
	DedupeEntries      int
}

func DefaultConfig() Config {
	return Config{
		DefaultTailLines: 200, MaxTailLines: 2_000, DefaultLimitBytes: 1 << 20,
		MaxSnapshotBytes: 5 << 20, MaxLineBytes: 256 << 10, MaxLookback: 24 * time.Hour,
		SnapshotTimeout: 30 * time.Second, FollowBuffer: 128, MaxFollowBytes: 20 << 20, MaxFollowDuration: 15 * time.Minute,
		RevalidateInterval: 30 * time.Second, HeartbeatInterval: 15 * time.Second,
		RediscoverInterval: 5 * time.Second, ReconnectDelay: time.Second,
		ReconnectOverlap: 2 * time.Second, DedupeEntries: 4_096,
	}
}

type LogOptions struct {
	TailLines  int64
	SinceTime  *time.Time
	Previous   bool
	Timestamps bool
	LimitBytes int64
	Follow     bool
}

type SnapshotRequest struct {
	Access    AccessRequest
	RequestID string
	Options   LogOptions
}

type FollowRequest struct {
	Access    AccessRequest
	RequestID string
	Options   LogOptions
	Cursor    *ReconnectCursor
}

// Source is deliberately opaque. Kubernetes namespaces, Job/Pod/container
// names, selectors, UIDs, and stored log references never enter API output.
type Source struct {
	ID       string `json:"id"`
	Ready    bool   `json:"ready"`
	Previous bool   `json:"previous"`
}

type LineCursor struct {
	SourceID    string    `json:"sourceId"`
	Timestamp   time.Time `json:"timestamp"`
	Fingerprint string    `json:"fingerprint"`
}

type LogLine struct {
	Type      string      `json:"type"`
	Timestamp *time.Time  `json:"timestamp,omitempty"`
	Source    Source      `json:"source"`
	Message   string      `json:"message"`
	Truncated bool        `json:"truncated"`
	Cursor    *LineCursor `json:"cursor,omitempty"`
}

type Snapshot struct {
	Source     Source    `json:"source"`
	Lines      []LogLine `json:"lines"`
	Bytes      int64     `json:"bytes"`
	Truncated  bool      `json:"truncated"`
	ObservedAt time.Time `json:"observedAt"`
}

type ReconnectCursor struct {
	SourceID    string
	Timestamp   time.Time
	Fingerprint string
}

type StreamEventType string

const (
	StreamLine      StreamEventType = "line"
	StreamStatus    StreamEventType = "status"
	StreamGap       StreamEventType = "gap"
	StreamHeartbeat StreamEventType = "heartbeat"
	StreamTerminal  StreamEventType = "terminal"
)

type StatusPayload struct {
	Source Source `json:"source"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type GapPayload struct {
	Source       Source `json:"source"`
	DroppedLines int64  `json:"droppedLines"`
}

type TerminalPayload struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type StreamEvent struct {
	Type     StreamEventType  `json:"type"`
	Line     *LogLine         `json:"line,omitempty"`
	Status   *StatusPayload   `json:"status,omitempty"`
	Gap      *GapPayload      `json:"gap,omitempty"`
	Terminal *TerminalPayload `json:"terminal,omitempty"`
	At       time.Time        `json:"at"`
}

type Stream struct {
	Events <-chan StreamEvent
	cancel context.CancelFunc
	done   <-chan struct{}
}

func (s *Stream) Close() {
	if s != nil && s.cancel != nil {
		s.cancel()
	}
}

func (s *Stream) Done() <-chan struct{} {
	if s == nil || s.done == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return s.done
}
