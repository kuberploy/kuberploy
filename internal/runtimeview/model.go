package runtimeview

import (
	"context"
	"errors"
	"io"
	"regexp"
	"time"
)

var (
	ErrInvalidRequest       = errors.New("runtime view request is invalid")
	ErrNotFound             = errors.New("runtime target was not found")
	ErrUnauthorized         = errors.New("runtime view authorization was revoked")
	ErrGone                 = errors.New("resolved Kubernetes object was replaced")
	ErrScopeViolation       = errors.New("Kubernetes response escaped the authorized scope")
	ErrSelectorNotAllowed   = errors.New("Kubernetes label selector is not allowlisted")
	ErrInsecureTransport    = errors.New("Kubernetes client must verify TLS")
	ErrContainerRequired    = errors.New("an exact container is required")
	ErrContainerNotFound    = errors.New("container does not belong to the resolved Pod")
	ErrSourceNotFound       = errors.New("no log source matches the exact Pod or revision filter")
	ErrPreviousUnavailable  = errors.New("previous container logs are unavailable")
	ErrTooManySources       = errors.New("runtime target exceeds the log source limit")
	ErrResponseLimitReached = errors.New("runtime view response limit reached")
)

type TargetKind string

const (
	TargetApplication TargetKind = "application"
	TargetDeployment  TargetKind = "deployment"
)

// OpaqueTarget is the complete API-facing resource input. It intentionally has
// no namespace, Pod name, selector, Kubernetes URL, or credential field.
type OpaqueTarget struct {
	Kind TargetKind
	ID   string
}

type DeploymentRef struct {
	Name string
	UID  string
}

// AuthorizedTarget is produced only by Resolver. Resolver implementations must
// apply visibility and logs.read authorization before returning it and should
// collapse unauthorized and missing objects into ErrNotFound.
type AuthorizedTarget struct {
	Reference     OpaqueTarget
	ApplicationID string
	Namespace     string
	Deployments   []DeploymentRef
}

type Resolver interface {
	Resolve(context.Context, OpaqueTarget) (AuthorizedTarget, error)
	Revalidate(context.Context, OpaqueTarget) error
}

type ClientSecurity struct {
	TLSVerified           bool
	InsecureSkipTLSVerify bool
}

type LabelSelector struct {
	MatchLabels map[string]string
}

type OwnerReference struct {
	UID        string
	Kind       string
	Controller bool
}

type Deployment struct {
	Namespace string
	Name      string
	UID       string
	Selector  LabelSelector
}

type ReplicaSet struct {
	Namespace string
	Name      string
	UID       string
	Owners    []OwnerReference
	Revision  string
	Ready     bool
}

type ContainerKind string

const (
	ContainerRegular ContainerKind = "regular"
	ContainerInit    ContainerKind = "init"
)

type Container struct {
	Name         string
	Kind         ContainerKind
	RestartCount int32
}

type Pod struct {
	Namespace   string
	Name        string
	UID         string
	Owners      []OwnerReference
	Containers  []Container
	Ready       bool
	Terminating bool
}

type PodLogOptions struct {
	Container  string
	TailLines  int64
	SinceTime  *time.Time
	Previous   bool
	Timestamps bool
	LimitBytes int64
	Follow     bool
}

type PodLogRequest struct {
	Namespace string
	PodName   string
	PodUID    string
	Options   PodLogOptions
}

type EventQuery struct {
	// InvolvedUIDs is an exact, core-derived allowlist. Adapters may translate
	// each UID to the Kubernetes involvedObject.uid field selector, but must not
	// add arbitrary selector expressions.
	InvolvedUIDs []string
	Limit        int
}

type KubernetesEvent struct {
	Namespace    string
	UID          string
	InvolvedUID  string
	InvolvedKind string
	InvolvedName string
	Type         string
	Reason       string
	Message      string
	Count        int32
	FirstSeen    time.Time
	LastSeen     time.Time
}

// KubernetesClient is deliberately narrower than client-go. There is no
// generic REST request, arbitrary URL, raw selector string, Secret operation,
// or caller-provided authentication/TLS option.
type KubernetesClient interface {
	Security() ClientSecurity
	GetDeployment(context.Context, string, string) (Deployment, error)
	ListReplicaSets(context.Context, string, LabelSelector) ([]ReplicaSet, error)
	ListPods(context.Context, string, LabelSelector) ([]Pod, error)
	GetPod(context.Context, string, string) (Pod, error)
	OpenPodLogs(context.Context, PodLogRequest) (io.ReadCloser, error)
	ListEvents(context.Context, string, EventQuery) ([]KubernetesEvent, error)
}

type Redactor interface {
	Redact(string) string
}

type Config struct {
	DefaultTailLines     int64
	MaxTailLines         int64
	DefaultLimitBytes    int64
	MaxSourceBytes       int64
	MaxSnapshotBytes     int64
	MaxLineBytes         int
	MaxSources           int
	MaxLookback          time.Duration
	MaxEvents            int
	MaxEventMessageBytes int
	FollowBuffer         int
	MaxFollowBytes       int64
	MaxFollowDuration    time.Duration
	RevalidateInterval   time.Duration
	HeartbeatInterval    time.Duration
	RediscoverInterval   time.Duration
	ReconnectDelay       time.Duration
	ReconnectOverlap     time.Duration
	DedupeEntries        int
}

func DefaultConfig() Config {
	return Config{
		DefaultTailLines:     200,
		MaxTailLines:         2_000,
		DefaultLimitBytes:    1 << 20,
		MaxSourceBytes:       5 << 20,
		MaxSnapshotBytes:     20 << 20,
		MaxLineBytes:         256 << 10,
		MaxSources:           50,
		MaxLookback:          24 * time.Hour,
		MaxEvents:            200,
		MaxEventMessageBytes: 8 << 10,
		FollowBuffer:         128,
		MaxFollowBytes:       20 << 20,
		MaxFollowDuration:    15 * time.Minute,
		RevalidateInterval:   30 * time.Second,
		HeartbeatInterval:    15 * time.Second,
		RediscoverInterval:   5 * time.Second,
		ReconnectDelay:       time.Second,
		ReconnectOverlap:     2 * time.Second,
		DedupeEntries:        4_096,
	}
}

type LogOptions struct {
	Container  string
	Pod        string
	Revision   string
	TailLines  int64
	SinceTime  *time.Time
	Previous   bool
	Timestamps bool
	LimitBytes int64
	Follow     bool
}

type SnapshotRequest struct {
	Target  OpaqueTarget
	Options LogOptions
}

type EventRequest struct {
	Target OpaqueTarget
	Limit  int
}

type ReconnectCursor struct {
	SourceID    string
	Timestamp   time.Time
	Fingerprint string
}

type FollowRequest struct {
	Target  OpaqueTarget
	Options LogOptions
	Cursors []ReconnectCursor
}

type LogSource struct {
	ID            string        `json:"podId"`
	PodName       string        `json:"podName"`
	Container     string        `json:"container"`
	ContainerKind ContainerKind `json:"containerKind"`
	RestartCount  int32         `json:"restartCount"`
	Revision      string        `json:"revision,omitempty"`
	Ready         bool          `json:"ready"`
	Terminating   bool          `json:"terminating"`
	Previous      bool          `json:"previous"`
}

type LineCursor struct {
	SourceID    string    `json:"sourceId"`
	Timestamp   time.Time `json:"timestamp"`
	Fingerprint string    `json:"fingerprint"`
}

type LogLine struct {
	Type      string      `json:"type"`
	Timestamp *time.Time  `json:"timestamp,omitempty"`
	Source    LogSource   `json:"source"`
	Message   string      `json:"message"`
	Truncated bool        `json:"truncated"`
	Cursor    *LineCursor `json:"cursor,omitempty"`
}

type SourceStatus struct {
	Source LogSource `json:"source"`
	State  string    `json:"state"`
	Reason string    `json:"reason,omitempty"`
}

type LogSnapshot struct {
	Lines      []LogLine      `json:"lines"`
	Sources    []LogSource    `json:"sources"`
	Statuses   []SourceStatus `json:"sourceStatuses,omitempty"`
	Bytes      int64          `json:"bytes"`
	Truncated  bool           `json:"truncated"`
	ObservedAt time.Time      `json:"observedAt"`
}

type RuntimeEvent struct {
	ID               string    `json:"id"`
	Type             string    `json:"type"`
	Reason           string    `json:"reason"`
	Message          string    `json:"message"`
	MessageTruncated bool      `json:"messageTruncated"`
	ObjectKind       string    `json:"objectKind"`
	ObjectName       string    `json:"objectName"`
	Count            int32     `json:"count"`
	FirstSeen        time.Time `json:"firstSeen,omitempty"`
	LastSeen         time.Time `json:"lastSeen,omitempty"`
}

type EventSnapshot struct {
	Items      []RuntimeEvent `json:"items"`
	Truncated  bool           `json:"truncated"`
	ObservedAt time.Time      `json:"observedAt"`
}

type StreamEventType string

const (
	StreamLine         StreamEventType = "line"
	StreamSourceStatus StreamEventType = "source-status"
	StreamGap          StreamEventType = "gap"
	StreamHeartbeat    StreamEventType = "heartbeat"
	StreamTerminal     StreamEventType = "terminal"
)

type StreamGapPayload struct {
	Source       LogSource `json:"source"`
	DroppedLines int64     `json:"droppedLines"`
}

type StreamTerminalPayload struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type StreamEvent struct {
	Type     StreamEventType        `json:"type"`
	Line     *LogLine               `json:"line,omitempty"`
	Status   *SourceStatus          `json:"sourceStatus,omitempty"`
	Gap      *StreamGapPayload      `json:"gap,omitempty"`
	Terminal *StreamTerminalPayload `json:"terminal,omitempty"`
	At       time.Time              `json:"at"`
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

type TooManySourcesError struct {
	Count int
	Limit int
}

func (e *TooManySourcesError) Error() string { return ErrTooManySources.Error() }
func (e *TooManySourcesError) Unwrap() error { return ErrTooManySources }

var (
	opaqueIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$`)
	kubeNamePattern    = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$`)
	containerPattern   = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	revisionPattern    = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	uidPattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
	fingerprintPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)
