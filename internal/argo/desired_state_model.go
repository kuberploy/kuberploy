package argo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

const (
	DesiredStateContract = "argo-desired-state-runtime-v1"

	minimumDesiredStateLease = 30 * time.Second
	maximumDesiredStateLease = 2 * time.Minute
)

var (
	ErrDesiredStateNotReady       = errors.New("matching Argo desired-state worker is not ready")
	ErrNoDesiredStateChange       = errors.New("Argo desired state did not change")
	ErrRegistryReferencesNotReady = errors.New("exact registry pull artifacts are not ready")
	// ErrDesiredStateProjectionSuperseded means a claimed command lost its
	// exact active projection before any durable Git write-base was recorded.
	// The command is safe to retire and replace with a freshly materialized one.
	ErrDesiredStateProjectionSuperseded = errors.New("Argo desired-state projection was superseded")
	// ErrDesiredStateWriteNotFound means the durable write-base exists, but
	// the exact operation trailer is absent from provider history. The command
	// cannot be recovered safely; its environment must be replanned.
	ErrDesiredStateWriteNotFound = errors.New("exact Argo desired-state write commit was not found")

	desiredStateOwnerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)
	strongETagRE        = regexp.MustCompile(`^"sha256:[0-9a-f]{64}"$`)
)

type ChartDigestEnforcement string

const (
	ChartDigestUnavailable ChartDigestEnforcement = "unavailable"
	ChartDigestNativeOCI   ChartDigestEnforcement = "native-oci-digest-v1"
)

// DesiredStateTarget joins the tenant environment values repository to the
// operator-owned protected platform repository. Every path, ref, Argo project,
// and Kubernetes destination is derived from these durable bindings.
type DesiredStateTarget struct {
	Environment     EnvironmentTarget
	PlatformBinding gitprojection.Binding
}

func (t DesiredStateTarget) Validate() error {
	platform := t.PlatformBinding
	if t.Environment.Validate() != nil || platform.Validate() != nil || platform.Kind != gitprojection.BindingPlatform ||
		t.Environment.Binding.CredentialMode != gitprojection.CredentialGitHubApp ||
		platform.CredentialMode != gitprojection.CredentialGitHubApp || platform.ClusterID == "" ||
		platform.TargetHeadRevision == "" || platform.TargetHeadObservedAt.IsZero() ||
		(platform.State != gitprojection.BindingReady && platform.State != gitprojection.BindingIndexing) {
		return ErrInvalid
	}
	return nil
}

func DesiredStatePath(clusterID, environmentID string) (string, error) {
	if !uuidRE.MatchString(clusterID) || !uuidRE.MatchString(environmentID) {
		return "", ErrInvalid
	}
	return path.Join(gitprojection.PlatformPrefix(clusterID), "argocd", "environments", environmentID+".yaml"), nil
}

type DesiredStateCommandState string

const (
	DesiredStatePending             DesiredStateCommandState = "pending"
	DesiredStateClaimed             DesiredStateCommandState = "claimed"
	DesiredStateGitCommitted        DesiredStateCommandState = "git-committed"
	DesiredStateVerified            DesiredStateCommandState = "verified"
	DesiredStateBlockedPrerequisite DesiredStateCommandState = "blocked-prerequisite"
	DesiredStateFailed              DesiredStateCommandState = "failed"
	DesiredStateSuperseded          DesiredStateCommandState = "superseded"
)

type DesiredStateLease struct {
	CommandID    string    `json:"commandId"`
	Owner        string    `json:"owner"`
	Epoch        int64     `json:"epoch"`
	Until        time.Time `json:"until"`
	Contract     string    `json:"contract"`
	ConfigDigest string    `json:"configDigest"`
}

func (l DesiredStateLease) Validate() error {
	if !uuidRE.MatchString(l.CommandID) || !desiredStateOwnerRE.MatchString(l.Owner) || l.Epoch <= 0 || l.Until.IsZero() ||
		l.Contract != DesiredStateContract || !digestRE.MatchString(l.ConfigDigest) {
		return ErrInvalid
	}
	return nil
}

// DesiredStateCommand contains the exact accepted bytes for one environment.
// It deliberately contains no repository credential, Kubernetes Secret data,
// tenant-selected path, destination, or imperative Argo sync instruction.
type DesiredStateCommand struct {
	ID                    string `json:"id"`
	Generation            int64  `json:"generation"`
	ProjectID             string `json:"projectId"`
	EnvironmentID         string `json:"environmentId"`
	PlatformBindingID     string `json:"platformBindingId"`
	EnvironmentBindingID  string `json:"environmentBindingId"`
	ClusterID             string `json:"clusterId"`
	PlatformTargetRef     string `json:"platformTargetRef"`
	EnvironmentTargetRef  string `json:"environmentTargetRef"`
	EnvironmentRevision   string `json:"environmentRevision"`
	EnvironmentGeneration int64  `json:"environmentGeneration"`
	Path                  string `json:"path"`
	ArgoNamespace         string `json:"argoNamespace"`
	DestinationNamespace  string `json:"destinationNamespace"`
	ArgoProject           string `json:"argoProject"`
	// BaseRevision is the immutable provider-verified platform head observed
	// when this command was accepted. WriteBaseRevision is a later, once-only,
	// lease-fenced receipt proving an allowed descendant and the same protected
	// path precondition immediately before Git mutation.
	BaseRevision        string                             `json:"baseRevision"`
	WriteBaseRevision   string                             `json:"writeBaseRevision,omitempty"`
	WriteBaseObservedAt *time.Time                         `json:"writeBaseObservedAt,omitempty"`
	Precondition        gitprojection.MutationPrecondition `json:"precondition"`
	ExpectedETag        string                             `json:"expectedETag,omitempty"`
	PolicyDigest        string                             `json:"policyDigest,omitempty"`
	CatalogDigest       string                             `json:"catalogDigest"`
	Runtime             RuntimeLock                        `json:"runtime"`
	DigestEnforcement   ChartDigestEnforcement             `json:"chartDigestEnforcement"`
	AppProjectContent   []byte                             `json:"-"`
	Content             []byte                             `json:"-"`
	ContentSHA256       string                             `json:"contentSha256"`
	Message             string                             `json:"message"`
	State               DesiredStateCommandState           `json:"state"`
	CommittedRevision   string                             `json:"committedRevision,omitempty"`
	CommittedAt         *time.Time                         `json:"committedAt,omitempty"`
	VerifiedAt          *time.Time                         `json:"verifiedAt,omitempty"`
	NextAttemptAt       time.Time                          `json:"nextAttemptAt"`
	ConsecutiveFailures int                                `json:"consecutiveFailures"`
	LastFailureCode     string                             `json:"lastFailureCode,omitempty"`
	LeaseEpoch          int64                              `json:"-"`
	Lease               *DesiredStateLease                 `json:"-"`
	CreatedAt           time.Time                          `json:"createdAt"`
	UpdatedAt           time.Time                          `json:"updatedAt"`
	CompletedAt         *time.Time                         `json:"completedAt,omitempty"`
}

func newDesiredStateCommand(id string, target DesiredStateTarget, approval DesiredStateProjectionApproval, previous *DesiredStateCommand, now time.Time) (DesiredStateCommand, error) {
	if !uuidRE.MatchString(id) || target.Validate() != nil || now.IsZero() {
		return DesiredStateCommand{}, ErrInvalid
	}
	appProjectContent, err := RenderAppProject(target)
	if err != nil {
		return DesiredStateCommand{}, ErrInvalid
	}
	content, err := RenderEnvironment(target, approval.Applications, approval.Deployments)
	if err != nil || len(content) > gitprojection.MaxDocumentBytes {
		return DesiredStateCommand{}, ErrInvalid
	}
	commandPath, err := DesiredStatePath(target.PlatformBinding.ClusterID, target.Environment.Environment.ID)
	if err != nil {
		return DesiredStateCommand{}, err
	}
	generation := int64(1)
	precondition := gitprojection.MutationCreateIfAbsent
	expectedETag := ""
	unchanged := false
	if previous != nil {
		if previous.Validate() != nil || previous.State != DesiredStateVerified || previous.ProjectID != target.Environment.Project.ID ||
			previous.EnvironmentID != target.Environment.Environment.ID || previous.PlatformBindingID != target.PlatformBinding.ID ||
			previous.EnvironmentBindingID != target.Environment.Binding.ID || previous.Generation == int64(^uint64(0)>>1) {
			return DesiredStateCommand{}, ErrInvalid
		}
		unchanged = previous.ContentSHA256 == contentDigest(content)
		generation = previous.Generation + 1
		precondition = gitprojection.MutationMatchETag
		expectedETag = `"` + previous.ContentSHA256 + `"`
	}
	createdAt := now.UTC()
	contentSHA := contentDigest(content)
	command := DesiredStateCommand{
		ID: id, Generation: generation, ProjectID: target.Environment.Project.ID, EnvironmentID: target.Environment.Environment.ID,
		PlatformBindingID: target.PlatformBinding.ID, EnvironmentBindingID: target.Environment.Binding.ID, ClusterID: target.PlatformBinding.ClusterID,
		PlatformTargetRef: target.PlatformBinding.TargetRef, EnvironmentTargetRef: target.Environment.Binding.TargetRef,
		EnvironmentRevision: target.Environment.Binding.IndexedRevision, EnvironmentGeneration: target.Environment.Binding.ProjectionGeneration, Path: commandPath,
		ArgoNamespace: target.Environment.ArgoNamespace, DestinationNamespace: target.Environment.Environment.Namespace,
		ArgoProject: target.Environment.Environment.ArgoProject, BaseRevision: target.PlatformBinding.TargetHeadRevision,
		Precondition: precondition, ExpectedETag: expectedETag, PolicyDigest: approval.PolicyDigest,
		CatalogDigest: approval.CatalogDigest, Runtime: target.Environment.Runtime,
		DigestEnforcement: ChartDigestNativeOCI, AppProjectContent: append([]byte(nil), appProjectContent...),
		Content: append([]byte(nil), content...), ContentSHA256: contentSHA,
		Message: fmt.Sprintf("Reconcile Argo desired state for environment %s generation %d", target.Environment.Environment.ID, generation),
		State:   DesiredStatePending, NextAttemptAt: createdAt, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	if err = command.ValidateFor(target); err != nil {
		return DesiredStateCommand{}, err
	}
	if unchanged {
		return command, ErrNoDesiredStateChange
	}
	return command, nil
}

func (c DesiredStateCommand) Validate() error {
	commandPath, pathErr := DesiredStatePath(c.ClusterID, c.EnvironmentID)
	validPrecondition := c.Precondition == gitprojection.MutationCreateIfAbsent && c.ExpectedETag == "" ||
		c.Precondition == gitprojection.MutationMatchETag && strongETagRE.MatchString(c.ExpectedETag)
	if !uuidRE.MatchString(c.ID) || c.Generation <= 0 || !uuidRE.MatchString(c.ProjectID) || !uuidRE.MatchString(c.EnvironmentID) ||
		!uuidRE.MatchString(c.PlatformBindingID) || !uuidRE.MatchString(c.EnvironmentBindingID) || !commitRE.MatchString(c.EnvironmentRevision) ||
		c.EnvironmentGeneration <= 0 || pathErr != nil || c.Path != commandPath ||
		!kubeRE.MatchString(c.ArgoNamespace) || !kubeRE.MatchString(c.DestinationNamespace) ||
		(c.ArgoProject != c.DestinationNamespace && c.ArgoProject != ProjectName(c.ProjectID)) ||
		!commitRE.MatchString(c.BaseRevision) || !validPrecondition ||
		(c.PolicyDigest != "" && !digestRE.MatchString(c.PolicyDigest)) || !digestRE.MatchString(c.CatalogDigest) || c.Runtime.Validate() != nil ||
		len(c.Content) == 0 || len(c.Content) > gitprojection.MaxDocumentBytes || c.ContentSHA256 != contentDigest(c.Content) ||
		(len(c.AppProjectContent) > 0 && !bytes.HasPrefix(c.Content, append(append([]byte(nil), c.AppProjectContent...), []byte("---\n")...))) ||
		len(c.Message) == 0 || len(c.Message) > 512 || !utf8.ValidString(c.Message) || strings.ContainsAny(c.Message, "\x00\r") ||
		c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() || c.NextAttemptAt.IsZero() || c.UpdatedAt.Before(c.CreatedAt) ||
		c.NextAttemptAt.Before(c.CreatedAt) || c.ConsecutiveFailures < 0 || c.ConsecutiveFailures > 30 || c.LeaseEpoch < 0 ||
		(c.ConsecutiveFailures == 0) != (c.LastFailureCode == "") || c.LastFailureCode != "" && !failureCodeRE.MatchString(c.LastFailureCode) {
		return ErrInvalid
	}
	if (c.WriteBaseRevision == "") != (c.WriteBaseObservedAt == nil) || c.WriteBaseRevision != "" &&
		(!commitRE.MatchString(c.WriteBaseRevision) || c.WriteBaseObservedAt.Before(c.CreatedAt) || c.WriteBaseObservedAt.After(c.UpdatedAt)) {
		return ErrInvalid
	}
	if c.DigestEnforcement != ChartDigestNativeOCI && c.DigestEnforcement != ChartDigestUnavailable {
		return ErrInvalid
	}
	if c.Lease != nil {
		if c.Lease.Validate() != nil || c.Lease.CommandID != c.ID || c.Lease.Epoch != c.LeaseEpoch || !c.Lease.Until.After(c.UpdatedAt) {
			return ErrInvalid
		}
	}
	if !validDesiredStateShape(c) {
		return ErrInvalid
	}
	return nil
}

func (c DesiredStateCommand) ValidateFor(target DesiredStateTarget) error {
	if c.Validate() != nil || target.Validate() != nil || c.ProjectID != target.Environment.Project.ID ||
		c.EnvironmentID != target.Environment.Environment.ID || c.PlatformBindingID != target.PlatformBinding.ID ||
		c.EnvironmentBindingID != target.Environment.Binding.ID || c.ClusterID != target.PlatformBinding.ClusterID ||
		c.PlatformTargetRef != target.PlatformBinding.TargetRef || c.EnvironmentTargetRef != target.Environment.Binding.TargetRef ||
		c.EnvironmentRevision != target.Environment.Binding.IndexedRevision || c.EnvironmentGeneration != target.Environment.Binding.ProjectionGeneration ||
		c.BaseRevision != target.PlatformBinding.TargetHeadRevision ||
		c.ArgoNamespace != target.Environment.ArgoNamespace || c.DestinationNamespace != target.Environment.Environment.Namespace ||
		c.ArgoProject != target.Environment.Environment.ArgoProject || c.Runtime != target.Environment.Runtime {
		return ErrInvalid
	}
	return nil
}

func validDesiredStateShape(c DesiredStateCommand) bool {
	terminal := c.CompletedAt != nil
	lease := c.Lease != nil
	switch c.State {
	case DesiredStatePending:
		return !lease && !terminal && c.CommittedRevision == "" && c.CommittedAt == nil && c.VerifiedAt == nil && c.DigestEnforcement == ChartDigestNativeOCI
	case DesiredStateClaimed:
		return lease && !terminal && c.CommittedRevision == "" && c.CommittedAt == nil && c.VerifiedAt == nil && c.DigestEnforcement == ChartDigestNativeOCI
	case DesiredStateGitCommitted:
		return c.WriteBaseRevision != "" && !terminal && commitRE.MatchString(c.CommittedRevision) && c.CommittedAt != nil && !c.CommittedAt.Before(c.CreatedAt) && c.VerifiedAt == nil && c.DigestEnforcement == ChartDigestNativeOCI
	case DesiredStateVerified:
		return c.WriteBaseRevision != "" && !lease && terminal && commitRE.MatchString(c.CommittedRevision) && c.CommittedAt != nil && c.VerifiedAt != nil &&
			!c.VerifiedAt.Before(*c.CommittedAt) && c.CompletedAt.Equal(*c.VerifiedAt) && c.DigestEnforcement == ChartDigestNativeOCI
	case DesiredStateBlockedPrerequisite:
		return c.WriteBaseRevision == "" && !lease && terminal && c.CommittedRevision == "" && c.CommittedAt == nil && c.VerifiedAt == nil && c.DigestEnforcement == ChartDigestUnavailable
	case DesiredStateSuperseded:
		return c.WriteBaseRevision == "" && !lease && terminal && c.CommittedRevision == "" && c.CommittedAt == nil && c.VerifiedAt == nil && c.DigestEnforcement == ChartDigestNativeOCI
	case DesiredStateFailed:
		return !lease && terminal && c.CommittedRevision == "" && c.CommittedAt == nil && c.VerifiedAt == nil && c.DigestEnforcement == ChartDigestNativeOCI
	default:
		return false
	}
}

func (c DesiredStateCommand) Mutation() gitprojection.Mutation {
	baseRevision := c.BaseRevision
	if c.WriteBaseRevision != "" {
		baseRevision = c.WriteBaseRevision
	}
	return gitprojection.Mutation{
		BindingID: c.PlatformBindingID, OperationID: c.ID, Path: c.Path, BaseRevision: baseRevision,
		Precondition: c.Precondition, ExpectedETag: c.ExpectedETag, Content: append([]byte(nil), c.Content...), Message: c.Message,
	}
}

func (c DesiredStateCommand) Status() DesiredStateStatus {
	return DesiredStateStatus{
		CommandID: c.ID, Generation: c.Generation, ProjectID: c.ProjectID, EnvironmentID: c.EnvironmentID,
		EnvironmentRevision: c.EnvironmentRevision, EnvironmentGeneration: c.EnvironmentGeneration,
		PlannedBaseRevision: c.BaseRevision, WriteBaseRevision: c.WriteBaseRevision, WriteBaseObservedAt: cloneTimePointer(c.WriteBaseObservedAt),
		State: c.State, CatalogDigest: c.CatalogDigest, ContentSHA256: c.ContentSHA256,
		ChartVersion: c.Runtime.ChartVersion, ChartDigest: c.Runtime.ChartDigest, RendererImage: c.Runtime.RendererImage,
		CommittedRevision: c.CommittedRevision, ConsecutiveFailures: c.ConsecutiveFailures, LastFailureCode: c.LastFailureCode,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, CommittedAt: cloneTimePointer(c.CommittedAt),
		VerifiedAt: cloneTimePointer(c.VerifiedAt), CompletedAt: cloneTimePointer(c.CompletedAt),
	}
}

// DesiredStateStatus is safe for API reads. It excludes manifest bytes,
// repository credentials, worker lease ownership, and mutable Git internals.
type DesiredStateStatus struct {
	CommandID             string                   `json:"commandId"`
	Generation            int64                    `json:"generation"`
	ProjectID             string                   `json:"projectId"`
	EnvironmentID         string                   `json:"environmentId"`
	EnvironmentRevision   string                   `json:"environmentRevision"`
	EnvironmentGeneration int64                    `json:"environmentGeneration"`
	PlannedBaseRevision   string                   `json:"plannedBaseRevision"`
	WriteBaseRevision     string                   `json:"writeBaseRevision,omitempty"`
	WriteBaseObservedAt   *time.Time               `json:"writeBaseObservedAt,omitempty"`
	State                 DesiredStateCommandState `json:"state"`
	CatalogDigest         string                   `json:"catalogDigest"`
	ContentSHA256         string                   `json:"contentSha256"`
	ChartVersion          string                   `json:"chartVersion"`
	ChartDigest           string                   `json:"chartDigest"`
	RendererImage         string                   `json:"rendererImage"`
	CommittedRevision     string                   `json:"committedRevision,omitempty"`
	ConsecutiveFailures   int                      `json:"consecutiveFailures"`
	LastFailureCode       string                   `json:"lastFailureCode,omitempty"`
	CreatedAt             time.Time                `json:"createdAt"`
	UpdatedAt             time.Time                `json:"updatedAt"`
	CommittedAt           *time.Time               `json:"committedAt,omitempty"`
	VerifiedAt            *time.Time               `json:"verifiedAt,omitempty"`
	CompletedAt           *time.Time               `json:"completedAt,omitempty"`
}

func contentDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
