// Package autodeploy owns the immutable policy and durable handoff from a
// verified source-build release to the canonical deployment command pipeline.
// It never accepts an image, Git revision, credential, or effective runtime
// material from a webhook or tenant at execution time.
package autodeploy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
)

var (
	ErrInvalid      = errors.New("invalid auto-deploy input")
	ErrNotFound     = errors.New("auto-deploy record not found")
	ErrConflict     = errors.New("auto-deploy state conflict")
	ErrUnauthorized = errors.New("auto-deploy service account is unauthorized")
	ErrNotReady     = errors.New("auto-deploy release is not ready")
	ErrLeaseLost    = errors.New("auto-deploy lease was lost")

	uuidRE       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	digestRE     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitRE     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	imageRE      = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
	configETagRE = regexp.MustCompile(`^"(?:sha256:|cfg-sha256-)[0-9a-f]{64}"$`)
)

const RequiredAutomationScope = domain.AutomationScopeAppEdit

type Policy struct {
	ID string `json:"id"`
	// BuildDefinitionID is retained only for source compatibility with older
	// in-process callers. App source selection is no longer part of policy
	// persistence or public JSON.
	BuildDefinitionID string    `json:"-"`
	ProjectID         string    `json:"projectId"`
	ApplicationID     string    `json:"applicationId"`
	EnvironmentID     string    `json:"environmentId"`
	CurrentRevision   int64     `json:"currentRevision"`
	CreatedBy         string    `json:"createdBy"`
	CreatedAt         time.Time `json:"createdAt"`
}

// Template is the immutable caller-selectable intent derived from the exact
// current AppConfig. ConfigIntent is canonical non-runnable JSON: image,
// registry pull metadata, effective scheduling, reusable-profile specs,
// configRevision, and derived sslip hosts are absent and re-resolved per run.
type Template struct {
	SourceDeploymentID         string `json:"sourceDeploymentId"`
	SourceDeploymentGeneration int64  `json:"sourceDeploymentGeneration"`
	SourceConfigETag           string `json:"sourceConfigETag"`
	ConfigIntent               []byte `json:"configIntent"`
}

type Revision struct {
	PolicyID       string    `json:"policyId"`
	Revision       int64     `json:"revision"`
	Enabled        bool      `json:"enabled"`
	Template       Template  `json:"template"`
	TemplateDigest string    `json:"templateDigest"`
	ServiceActorID string    `json:"serviceActorId"`
	CreatedBy      string    `json:"createdBy"`
	CreatedAt      time.Time `json:"createdAt"`
}

type RunState string

const (
	RunPending    RunState = "pending"
	RunProcessing RunState = "processing"
	RunSubmitted  RunState = "submitted"
	RunFailed     RunState = "failed"
)

type Run struct {
	AttemptID                  string     `json:"attemptId"`
	PolicyID                   string     `json:"policyId"`
	PolicyRevision             int64      `json:"policyRevision"`
	DefinitionID               string     `json:"definitionId"`
	DefinitionDigest           string     `json:"definitionDigest"`
	ReleaseID                  string     `json:"releaseId"`
	TemplateDigest             string     `json:"templateDigest"`
	SourceDeploymentID         string     `json:"sourceDeploymentId"`
	SourceDeploymentGeneration int64      `json:"sourceDeploymentGeneration"`
	SourceConfigETag           string     `json:"sourceConfigETag"`
	IdempotencyKey             string     `json:"idempotencyKey"`
	State                      RunState   `json:"state"`
	OperationID                string     `json:"operationId,omitempty"`
	DeploymentID               string     `json:"deploymentId,omitempty"`
	FailureCode                string     `json:"failureCode,omitempty"`
	Attempts                   int        `json:"attempts"`
	AvailableAt                time.Time  `json:"availableAt"`
	CreatedAt                  time.Time  `json:"createdAt"`
	UpdatedAt                  time.Time  `json:"updatedAt"`
	CompletedAt                *time.Time `json:"completedAt,omitempty"`
}

type Lease struct {
	AttemptID string
	PolicyID  string
	Owner     string
	Epoch     int64
	Until     time.Time
}

type Work struct {
	Policy   Policy
	Revision Revision
	Run      Run
	Lease    Lease
}

// VerifiedRelease is returned only after the source attempt and the
// independent registry release projection have both been revalidated.
type VerifiedRelease struct {
	AttemptID        string
	DefinitionID     string
	DefinitionDigest string
	ProjectID        string
	ApplicationID    string
	ReleaseID        string
	Image            string
	CommitSHA        string
	CompletedAt      time.Time
}

type Submission struct {
	ActorID                    string
	IdempotencyKey             string
	RequestID                  string
	AttemptID                  string
	PolicyID                   string
	PolicyRevision             int64
	ProjectID                  string
	ApplicationID              string
	EnvironmentID              string
	Image                      string
	ConfigIntent               []byte
	TemplateDigest             string
	SourceDeploymentID         string
	SourceDeploymentGeneration int64
	SourceConfigETag           string
}

type SubmissionReceipt struct {
	OperationID  string
	DeploymentID string
	Replay       bool
}

func (p Policy) Validate() error {
	if !uuidRE.MatchString(p.ID) || !uuidRE.MatchString(p.ProjectID) ||
		!uuidRE.MatchString(p.ApplicationID) || !uuidRE.MatchString(p.EnvironmentID) || p.CurrentRevision < 1 ||
		!uuidRE.MatchString(p.CreatedBy) || p.CreatedAt.IsZero() {
		return ErrInvalid
	}
	return nil
}

func (t Template) Validate(digest string) error {
	if !uuidRE.MatchString(t.SourceDeploymentID) || t.SourceDeploymentGeneration < 1 ||
		!configETagRE.MatchString(t.SourceConfigETag) || !appconfig.ValidateAutoDeployIntentTemplate(t.ConfigIntent, digest) {
		return ErrInvalid
	}
	return nil
}

func (r Revision) ValidateFor(policy Policy) error {
	if policy.Validate() != nil || r.PolicyID != policy.ID || r.Revision < 1 || r.Revision > policy.CurrentRevision ||
		r.Template.Validate(r.TemplateDigest) != nil || !digestRE.MatchString(r.TemplateDigest) ||
		!uuidRE.MatchString(r.ServiceActorID) || !uuidRE.MatchString(r.CreatedBy) || r.CreatedAt.IsZero() || r.CreatedAt.Before(policy.CreatedAt) {
		return ErrInvalid
	}
	return nil
}

func (r Run) Validate() error {
	if !uuidRE.MatchString(r.AttemptID) || !uuidRE.MatchString(r.PolicyID) || r.PolicyRevision < 1 ||
		!uuidRE.MatchString(r.DefinitionID) || !digestRE.MatchString(r.DefinitionDigest) || !uuidRE.MatchString(r.ReleaseID) ||
		!digestRE.MatchString(r.TemplateDigest) || r.IdempotencyKey != IdempotencyKey(r.PolicyID, r.PolicyRevision, r.AttemptID) ||
		!uuidRE.MatchString(r.SourceDeploymentID) || r.SourceDeploymentGeneration < 1 || !configETagRE.MatchString(r.SourceConfigETag) ||
		r.Attempts < 0 || r.Attempts > 20 || r.AvailableAt.IsZero() || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() ||
		r.UpdatedAt.Before(r.CreatedAt) {
		return ErrInvalid
	}
	switch r.State {
	case RunPending, RunProcessing:
		if r.CompletedAt != nil || r.OperationID != "" || r.DeploymentID != "" {
			return ErrInvalid
		}
	case RunSubmitted:
		if r.CompletedAt == nil || !uuidRE.MatchString(r.OperationID) || !uuidRE.MatchString(r.DeploymentID) || r.FailureCode != "" {
			return ErrInvalid
		}
	case RunFailed:
		if r.CompletedAt == nil || r.FailureCode == "" || len(r.FailureCode) > 63 || r.OperationID != "" || r.DeploymentID != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func (r VerifiedRelease) Validate() error {
	if !uuidRE.MatchString(r.AttemptID) || !uuidRE.MatchString(r.DefinitionID) || !digestRE.MatchString(r.DefinitionDigest) ||
		!uuidRE.MatchString(r.ProjectID) || !uuidRE.MatchString(r.ApplicationID) || !uuidRE.MatchString(r.ReleaseID) ||
		!imageRE.MatchString(r.Image) || !commitRE.MatchString(r.CommitSHA) || r.CompletedAt.IsZero() {
		return ErrInvalid
	}
	return nil
}

func (s Submission) Validate() error {
	if !uuidRE.MatchString(s.ActorID) || s.IdempotencyKey != IdempotencyKey(s.PolicyID, s.PolicyRevision, s.AttemptID) ||
		!uuidRE.MatchString(s.AttemptID) || !uuidRE.MatchString(s.PolicyID) || s.PolicyRevision < 1 || s.RequestID == "" || len(s.RequestID) > 256 ||
		!uuidRE.MatchString(s.ProjectID) || !uuidRE.MatchString(s.EnvironmentID) || !uuidRE.MatchString(s.ApplicationID) || !imageRE.MatchString(s.Image) ||
		!appconfig.ValidateAutoDeployIntentTemplate(s.ConfigIntent, s.TemplateDigest) || !uuidRE.MatchString(s.SourceDeploymentID) ||
		s.SourceDeploymentGeneration < 1 || !configETagRE.MatchString(s.SourceConfigETag) {
		return ErrInvalid
	}
	return nil
}

func TemplateDigest(template Template) string {
	if len(template.ConfigIntent) == 0 {
		return ""
	}
	digest := sha256.Sum256(template.ConfigIntent)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func IdempotencyKey(policyID string, revision int64, attemptID string) string {
	return "auto-deploy/" + policyID + "/" + strconv.FormatInt(revision, 10) + "/" + attemptID
}
