// Package environmentfoundation builds and publishes the fixed, cluster-level
// Kubernetes resources that make a Kuberploy environment namespace usable.
// Tenant input can select an environment, but can never supply resource names,
// Kubernetes objects, manifest bytes, Git paths, or Git publisher authority.
package environmentfoundation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	Contract                = "environment-foundation.v1"
	PublisherContract       = "environment-foundation-protected-git.v1"
	ProtectedGitPolicy      = "platform-protected-git.v1"
	MaximumAttempts         = 30
	MinimumLease            = 15 * time.Second
	MaximumLease            = 5 * time.Minute
	MaximumManifestBytes    = 256 * 1024
	FoundationResourceCount = 7
)

var (
	ErrInvalid     = errors.New("environment foundation value is invalid")
	ErrNotFound    = errors.New("environment foundation was not found")
	ErrConflict    = errors.New("environment foundation conflicts with durable state")
	ErrLeaseLost   = errors.New("environment foundation lease was lost")
	ErrUnavailable = errors.New("environment foundation runtime is unavailable")

	uuidRE       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	dnsLabelRE   = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	psaVersionRE = regexp.MustCompile(`^v1\.(?:2[5-9]|[3-9][0-9])$`)
	digestRE     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	gitCommitRE  = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	gitRefRE     = regexp.MustCompile(`^refs/heads/[A-Za-z0-9](?:[A-Za-z0-9._/-]{0,198}[A-Za-z0-9])?$`)
	workerIDRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)
	failureRE    = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,62}$`)
	requestRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$`)
)

type Quota struct {
	Pods, Services, ConfigMaps, Secrets, PersistentVolumeClaims int
	RequestCPUMilli, LimitCPUMilli                              int
	RequestMemoryMiB, LimitMemoryMiB, RequestStorageGiB         int
}

func (q Quota) Validate() error {
	if q.Pods < 1 || q.Pods > 500 || q.Services < 1 || q.Services > 200 ||
		q.ConfigMaps < 1 || q.ConfigMaps > 1000 || q.Secrets < 1 || q.Secrets > 1000 ||
		q.PersistentVolumeClaims < 0 || q.PersistentVolumeClaims > 100 ||
		q.RequestCPUMilli < 100 || q.RequestCPUMilli > 200000 ||
		q.LimitCPUMilli < q.RequestCPUMilli || q.LimitCPUMilli > 400000 ||
		q.RequestMemoryMiB < 64 || q.RequestMemoryMiB > 1048576 ||
		q.LimitMemoryMiB < q.RequestMemoryMiB || q.LimitMemoryMiB > 2097152 ||
		q.RequestStorageGiB < 1 || q.RequestStorageGiB > 65536 {
		return ErrInvalid
	}
	return nil
}

type Limits struct {
	DefaultRequestCPUMilli, DefaultLimitCPUMilli   int
	MinimumCPUMilli, MaximumCPUMilli               int
	DefaultRequestMemoryMiB, DefaultLimitMemoryMiB int
	MinimumMemoryMiB, MaximumMemoryMiB             int
}

func (l Limits) Validate() error {
	if l.MinimumCPUMilli < 1 || l.DefaultRequestCPUMilli < l.MinimumCPUMilli ||
		l.DefaultLimitCPUMilli < l.DefaultRequestCPUMilli || l.MaximumCPUMilli < l.DefaultLimitCPUMilli ||
		l.MaximumCPUMilli > 400000 || l.MinimumMemoryMiB < 1 ||
		l.DefaultRequestMemoryMiB < l.MinimumMemoryMiB ||
		l.DefaultLimitMemoryMiB < l.DefaultRequestMemoryMiB ||
		l.MaximumMemoryMiB < l.DefaultLimitMemoryMiB || l.MaximumMemoryMiB > 2097152 {
		return ErrInvalid
	}
	return nil
}

// Profile is operator-owned configuration. It deliberately contains numeric
// resource bounds and exact platform workload selectors, never arbitrary YAML.
type Profile struct {
	PlatformBindingID, PublisherConfigDigest string
	PSAVersion                               string
	Quota                                    Quota
	Limits                                   Limits
	ControlPlaneNamespace                    string
	ObserverServiceAccount                   string
}

func DefaultProfile(platformBindingID, publisherConfigDigest, psaVersion string) Profile {
	return Profile{
		PlatformBindingID: platformBindingID, PublisherConfigDigest: publisherConfigDigest, PSAVersion: psaVersion,
		Quota: Quota{Pods: 100, Services: 40, ConfigMaps: 200, Secrets: 200, PersistentVolumeClaims: 20,
			RequestCPUMilli: 16000, LimitCPUMilli: 32000, RequestMemoryMiB: 32768,
			LimitMemoryMiB: 65536, RequestStorageGiB: 1024},
		Limits: Limits{DefaultRequestCPUMilli: 100, DefaultLimitCPUMilli: 1000, MinimumCPUMilli: 10,
			MaximumCPUMilli: 8000, DefaultRequestMemoryMiB: 128, DefaultLimitMemoryMiB: 512,
			MinimumMemoryMiB: 16, MaximumMemoryMiB: 16384},
		ControlPlaneNamespace: "kuberploy-system", ObserverServiceAccount: "kuberploy-api",
	}
}

func (p Profile) Validate() error {
	if !uuidRE.MatchString(p.PlatformBindingID) || !digestRE.MatchString(p.PublisherConfigDigest) ||
		!psaVersionRE.MatchString(p.PSAVersion) || p.Quota.Validate() != nil || p.Limits.Validate() != nil ||
		!dnsLabelRE.MatchString(p.ControlPlaneNamespace) || !dnsLabelRE.MatchString(p.ObserverServiceAccount) {
		return ErrInvalid
	}
	return nil
}

func (p Profile) Digest() (string, error) {
	if p.Validate() != nil {
		return "", ErrInvalid
	}
	b, err := json.Marshal(struct {
		Contract string  `json:"contract"`
		Profile  Profile `json:"profile"`
	}{Contract, p})
	if err != nil {
		return "", ErrInvalid
	}
	return digest(b), nil
}

type EnvironmentIdentity struct {
	EnvironmentID, ProjectID, Namespace, ArgoProject string
}

func (i EnvironmentIdentity) Validate() error {
	if !uuidRE.MatchString(i.EnvironmentID) || !uuidRE.MatchString(i.ProjectID) ||
		!dnsLabelRE.MatchString(i.Namespace) || !dnsLabelRE.MatchString(i.ArgoProject) {
		return ErrInvalid
	}
	return nil
}

type GitAuthority struct {
	BindingID, TargetRef, PlannedHead string
	Generation                        int64
}

func (a GitAuthority) Validate() error {
	if !uuidRE.MatchString(a.BindingID) || !gitRefRE.MatchString(a.TargetRef) ||
		strings.Contains(a.TargetRef, "..") || strings.Contains(a.TargetRef, "//") ||
		!gitCommitRE.MatchString(a.PlannedHead) || a.Generation < 1 {
		return ErrInvalid
	}
	return nil
}

type IntentState string

const (
	StatePending    IntentState = "pending"
	StateClaimed    IntentState = "claimed"
	StateReady      IntentState = "ready"
	StateFailed     IntentState = "failed"
	StateSuperseded IntentState = "superseded"
)

type Intent struct {
	ID string
	EnvironmentIdentity
	Authority                GitAuthority
	ProfileDigest            string
	PublisherConfigDigest    string
	PublisherContractVersion string
	PublisherPolicy          string
	Path                     string
	Manifest                 []byte
	ManifestDigest           string
	IntentDigest             string
	CommitTrailer            string
	State                    IntentState
	Active                   bool
	NextAttemptAt            time.Time
	Attempts                 int
	ConsecutiveFailures      int
	LastFailureCode          string
	LeaseOwner               string
	LeaseEpoch               int64
	LeaseUntil               *time.Time
	WriteBaseRevision        string
	WriteBaseObservedAt      *time.Time
	CommittedRevision        string
	CommittedParentRevision  string
	ProviderRequest          string
	PublishedAt, CompletedAt *time.Time
	CreatedAt, UpdatedAt     time.Time
}

func (i Intent) Validate() error {
	if !uuidRE.MatchString(i.ID) || i.EnvironmentIdentity.Validate() != nil || i.Authority.Validate() != nil ||
		!digestRE.MatchString(i.ProfileDigest) || !digestRE.MatchString(i.PublisherConfigDigest) ||
		i.PublisherContractVersion != PublisherContract || i.PublisherPolicy != ProtectedGitPolicy ||
		i.Path != ManifestPath(i.EnvironmentID) || len(i.Manifest) < 1 || len(i.Manifest) > MaximumManifestBytes ||
		digest(i.Manifest) != i.ManifestDigest || !digestRE.MatchString(i.IntentDigest) ||
		i.CommitTrailer != "Kuberploy-Environment-Foundation-Intent: "+i.ID || i.CreatedAt.IsZero() ||
		i.UpdatedAt.Before(i.CreatedAt) || i.NextAttemptAt.Before(i.CreatedAt) || i.Attempts < 0 || i.Attempts > MaximumAttempts ||
		i.ConsecutiveFailures < 0 || i.ConsecutiveFailures > MaximumAttempts ||
		(i.LastFailureCode == "") != (i.ConsecutiveFailures == 0) ||
		(i.LastFailureCode != "" && !failureRE.MatchString(i.LastFailureCode)) ||
		(i.LeaseUntil != nil && !i.LeaseUntil.After(i.UpdatedAt)) ||
		(i.ProviderRequest != "" && !requestRE.MatchString(i.ProviderRequest)) {
		return ErrInvalid
	}
	lease := workerIDRE.MatchString(i.LeaseOwner) && i.LeaseEpoch > 0 && i.LeaseUntil != nil
	if (i.LeaseOwner != "" || i.LeaseUntil != nil) != lease {
		return ErrInvalid
	}
	writeBase := gitCommitRE.MatchString(i.WriteBaseRevision) && i.WriteBaseObservedAt != nil &&
		!i.WriteBaseObservedAt.Before(i.CreatedAt) && !i.WriteBaseObservedAt.After(i.UpdatedAt)
	if (i.WriteBaseRevision != "" || i.WriteBaseObservedAt != nil) != writeBase {
		return ErrInvalid
	}
	receipt := writeBase && gitCommitRE.MatchString(i.CommittedRevision) && i.CommittedParentRevision == i.WriteBaseRevision &&
		i.PublishedAt != nil && i.CompletedAt != nil && i.ProviderRequest != ""
	if (i.CommittedRevision != "" || i.CommittedParentRevision != "" || i.PublishedAt != nil || i.ProviderRequest != "") != receipt {
		return ErrInvalid
	}
	switch i.State {
	case StatePending:
		if !i.Active || lease || receipt || i.CompletedAt != nil {
			return ErrInvalid
		}
	case StateClaimed:
		if !i.Active || !lease || receipt || i.CompletedAt != nil || i.Attempts < 1 {
			return ErrInvalid
		}
	case StateReady:
		if !i.Active || lease || !receipt || !i.CompletedAt.Equal(*i.PublishedAt) {
			return ErrInvalid
		}
	case StateFailed:
		if i.Active || lease || receipt || i.CompletedAt == nil || i.ConsecutiveFailures < 1 {
			return ErrInvalid
		}
	case StateSuperseded:
		if i.Active || lease || i.CompletedAt == nil {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

type EnsureRequest struct {
	IntentID, EnvironmentID string
	Profile                 Profile
	Now                     time.Time
}

type Lease struct {
	Intent Intent
	Owner  string
	Epoch  int64
	Until  time.Time
}

func (l Lease) Validate(at time.Time) error {
	if l.Intent.Validate() != nil || l.Intent.State != StateClaimed || l.Owner != l.Intent.LeaseOwner ||
		l.Epoch != l.Intent.LeaseEpoch || l.Intent.LeaseUntil == nil || !l.Until.Equal(*l.Intent.LeaseUntil) || !l.Until.After(at) {
		return ErrInvalid
	}
	return nil
}

type Readiness struct {
	WorkerID                                       string
	WorkerEpoch                                    int64
	Contract, ProfileDigest, PublisherConfigDigest string
	ActiveIntentCount                              int
	StartedAt, ObservedAt, LeaseUntil              time.Time
}

func (r Readiness) Validate() error {
	if !workerIDRE.MatchString(r.WorkerID) || r.WorkerEpoch < 1 || r.Contract != Contract ||
		!digestRE.MatchString(r.ProfileDigest) || !digestRE.MatchString(r.PublisherConfigDigest) ||
		r.ActiveIntentCount < 0 || r.ActiveIntentCount > 10000 || r.StartedAt.IsZero() ||
		r.ObservedAt.Before(r.StartedAt) || !r.LeaseUntil.After(r.ObservedAt) {
		return ErrInvalid
	}
	return nil
}

func ManifestPath(environmentID string) string {
	if !uuidRE.MatchString(environmentID) {
		return ""
	}
	return fmt.Sprintf("platform/argocd/foundations/%s.yaml", environmentID)
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}
