// Package imagepull reconciles operator-projected registry credentials into
// revisioned namespace-local imagePullSecrets. Its durable models contain only
// metadata; credential bytes and hashes of credential bytes are never stored.
package imagepull

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	RuntimeContract = "registry-pull.v1"
	MaximumProfiles = 32
	SourceRoot      = "/var/run/secrets/kuberploy/registry-pulls"
)

var (
	ErrInvalid     = errors.New("runtime registry pull metadata is invalid")
	ErrNotFound    = errors.New("runtime registry pull metadata was not found")
	ErrUnavailable = errors.New("runtime registry pull controller is unavailable")
	ErrConflict    = errors.New("runtime registry pull metadata conflicts with durable state")
	ErrLeaseLost   = errors.New("runtime registry pull lease was lost")

	uuidPattern           = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	kubernetesUIDPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	dnsLabelPattern       = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	credentialRefPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$`)
	secretKeyPattern      = regexp.MustCompile(`^[A-Za-z0-9._-]{1,253}$`)
	registryServerPattern = regexp.MustCompile(
		`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?::[1-9][0-9]{0,4})?$`,
	)
	resourceVersionPattern = regexp.MustCompile(`^[A-Za-z0-9._:/+-]{1,128}$`)
	namespacePrefixPattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,60}[a-z0-9])?-$`)
	failureCodePattern     = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,62}$`)
	workerIDPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)
)

type Profile struct {
	Name            string `json:"name"`
	TargetID        string `json:"targetId"`
	RegistryServer  string `json:"registryServer"`
	CredentialRef   string `json:"credentialRef"`
	Revision        int64  `json:"revision"`
	SourceSecretRef string `json:"sourceSecretRef"`
	SourceSecretKey string `json:"sourceSecretKey"`
}

func (p Profile) Validate() error {
	if !dnsLabelPattern.MatchString(p.Name) || !uuidPattern.MatchString(p.TargetID) ||
		len(p.RegistryServer) > 253 || !registryServerPattern.MatchString(p.RegistryServer) ||
		!credentialRefPattern.MatchString(p.CredentialRef) || p.Revision <= 0 ||
		!dnsLabelPattern.MatchString(p.SourceSecretRef) || !secretKeyPattern.MatchString(p.SourceSecretKey) {
		return ErrInvalid
	}
	if strings.TrimSpace(p.RegistryServer) != p.RegistryServer || strings.ToLower(p.RegistryServer) != p.RegistryServer {
		return ErrInvalid
	}
	if separator := strings.LastIndexByte(p.RegistryServer, ':'); separator >= 0 {
		port, err := strconv.ParseUint(p.RegistryServer[separator+1:], 10, 16)
		if err != nil || port == 0 {
			return ErrInvalid
		}
	}
	return nil
}

func (p Profile) SourcePath() string {
	if p.Validate() != nil {
		return ""
	}
	return SourceRoot + "/" + p.Name + "/dockerconfigjson"
}

type RuntimeConfig struct {
	Enabled           bool
	Namespaces        []string
	NamespacePrefixes []string
	Profiles          []Profile
	PollInterval      time.Duration
	WorkLease         time.Duration
	HeartbeatInterval time.Duration
	ReadinessMaxAge   time.Duration
	MinimumBackoff    time.Duration
	MaximumBackoff    time.Duration
}

func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		PollInterval: 30 * time.Second, WorkLease: 2 * time.Minute,
		HeartbeatInterval: 20 * time.Second, ReadinessMaxAge: 90 * time.Second,
		MinimumBackoff: 5 * time.Second, MaximumBackoff: 5 * time.Minute,
	}
}

func (c RuntimeConfig) Validate() error {
	if !c.Enabled {
		if len(c.Namespaces) != 0 || len(c.NamespacePrefixes) != 0 || len(c.Profiles) != 0 || c.PollInterval != 0 || c.WorkLease != 0 ||
			c.HeartbeatInterval != 0 || c.ReadinessMaxAge != 0 || c.MinimumBackoff != 0 || c.MaximumBackoff != 0 {
			return ErrInvalid
		}
		return nil
	}
	if len(c.Namespaces)+len(c.NamespacePrefixes) == 0 || len(c.Namespaces) > 256 || len(c.NamespacePrefixes) > 16 || len(c.Profiles) == 0 || len(c.Profiles) > MaximumProfiles ||
		c.PollInterval < 5*time.Second || c.PollInterval > time.Hour ||
		c.WorkLease < 30*time.Second || c.WorkLease > 15*time.Minute ||
		c.HeartbeatInterval < 5*time.Second || c.HeartbeatInterval*2 >= c.WorkLease ||
		c.ReadinessMaxAge < c.HeartbeatInterval*2 || c.ReadinessMaxAge > 15*time.Minute ||
		c.MinimumBackoff < time.Second || c.MaximumBackoff < c.MinimumBackoff || c.MaximumBackoff > time.Hour {
		return ErrInvalid
	}
	for index, namespace := range c.Namespaces {
		if !dnsLabelPattern.MatchString(namespace) || index > 0 && c.Namespaces[index-1] >= namespace {
			return ErrInvalid
		}
	}
	for index, prefix := range c.NamespacePrefixes {
		if !namespacePrefixPattern.MatchString(prefix) || index > 0 && c.NamespacePrefixes[index-1] >= prefix {
			return ErrInvalid
		}
	}
	for index, profile := range c.Profiles {
		if profile.Validate() != nil {
			return ErrInvalid
		}
		if index > 0 && compareProfiles(c.Profiles[index-1], profile) >= 0 {
			return ErrInvalid
		}
		for previous := 0; previous < index; previous++ {
			other := c.Profiles[previous]
			if other.TargetID == profile.TargetID || other.Name == profile.Name || other.CredentialRef == profile.CredentialRef ||
				other.SourceSecretRef == profile.SourceSecretRef {
				return ErrInvalid
			}
		}
	}
	return nil
}

func compareProfiles(left, right Profile) int {
	if compared := strings.Compare(left.TargetID, right.TargetID); compared != 0 {
		return compared
	}
	return strings.Compare(left.Name, right.Name)
}

func (c RuntimeConfig) Digest() (string, error) {
	if c.Validate() != nil {
		return "", ErrInvalid
	}
	encoded, err := json.Marshal(struct {
		Contract          string    `json:"contract"`
		Namespaces        []string  `json:"namespaces"`
		NamespacePrefixes []string  `json:"namespacePrefixes"`
		Profiles          []Profile `json:"profiles"`
		PollSeconds       int64     `json:"pollSeconds"`
		WorkLeaseSeconds  int64     `json:"workLeaseSeconds"`
		HeartbeatSeconds  int64     `json:"heartbeatSeconds"`
		ReadinessSeconds  int64     `json:"readinessSeconds"`
		MinimumBackoffSec int64     `json:"minimumBackoffSeconds"`
		MaximumBackoffSec int64     `json:"maximumBackoffSeconds"`
	}{RuntimeContract, slices.Clone(c.Namespaces), slices.Clone(c.NamespacePrefixes), slices.Clone(c.Profiles), int64(c.PollInterval.Seconds()),
		int64(c.WorkLease.Seconds()), int64(c.HeartbeatInterval.Seconds()), int64(c.ReadinessMaxAge.Seconds()),
		int64(c.MinimumBackoff.Seconds()), int64(c.MaximumBackoff.Seconds())})
	if err != nil {
		return "", ErrInvalid
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (c RuntimeConfig) ProfileForTarget(targetID string) (Profile, bool) {
	if c.Validate() != nil {
		return Profile{}, false
	}
	index, found := slices.BinarySearchFunc(c.Profiles, Profile{TargetID: targetID}, func(profile, target Profile) int {
		return strings.Compare(profile.TargetID, target.TargetID)
	})
	if !found {
		return Profile{}, false
	}
	return c.Profiles[index], true
}

func (c RuntimeConfig) AllowsNamespace(namespace string) bool {
	if c.Validate() != nil {
		return false
	}
	_, found := slices.BinarySearch(c.Namespaces, namespace)
	if found {
		return true
	}
	for _, prefix := range c.NamespacePrefixes {
		if strings.HasPrefix(namespace, prefix) {
			return true
		}
	}
	return false
}

type ArtifactKey struct {
	EnvironmentID    string
	RegistryTargetID string
	ProfileRevision  int64
}

type DesiredArtifact struct {
	ArtifactKey
	Namespace         string
	PullCredentialRef string
	ProfileName       string
	SecretName        string
}

func Desired(config RuntimeConfig, environmentID, namespace, targetID string) (DesiredArtifact, error) {
	profile, found := config.ProfileForTarget(targetID)
	if !found || !uuidPattern.MatchString(environmentID) || !config.AllowsNamespace(namespace) {
		return DesiredArtifact{}, ErrInvalid
	}
	desired := DesiredArtifact{
		ArtifactKey: ArtifactKey{EnvironmentID: environmentID, RegistryTargetID: targetID, ProfileRevision: profile.Revision},
		Namespace:   namespace, PullCredentialRef: profile.CredentialRef,
		ProfileName: profile.Name, SecretName: SecretName(namespace, targetID, profile.Revision),
	}
	if desired.Validate() != nil {
		return DesiredArtifact{}, ErrInvalid
	}
	return desired, nil
}

func (d DesiredArtifact) Validate() error {
	if !uuidPattern.MatchString(d.EnvironmentID) || !uuidPattern.MatchString(d.RegistryTargetID) || d.ProfileRevision <= 0 ||
		!dnsLabelPattern.MatchString(d.Namespace) || !credentialRefPattern.MatchString(d.PullCredentialRef) ||
		!dnsLabelPattern.MatchString(d.ProfileName) || d.SecretName != SecretName(d.Namespace, d.RegistryTargetID, d.ProfileRevision) {
		return ErrInvalid
	}
	return nil
}

// SecretName derives one exact resource name from operator-owned target profile
// identity. Kubernetes namespaces already scope names; keeping the same name
// in every managed Environment lets Helm emit resourceNames-restricted RBAC
// before dynamically created Environment namespaces exist.
func SecretName(namespace, targetID string, revision int64) string {
	if !dnsLabelPattern.MatchString(namespace) || !uuidPattern.MatchString(targetID) || revision <= 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("kuberploy-runtime-pull-v2\x00%s\x00%d", targetID, revision)))
	return "kuberploy-pull-" + hex.EncodeToString(sum[:12])
}

type RuntimeState string

const (
	StateAwaiting RuntimeState = "awaiting"
	StateReady    RuntimeState = "ready"
	StateFailed   RuntimeState = "failed"

	// profileMismatchFailureCode is the sole failed state that can be
	// reclaimed. The controller revalidates the complete operator profile and
	// derived Secret identity before reading credentials or mutating Kubernetes,
	// so correcting operator configuration recovers without weakening other
	// permanent-failure fences.
	profileMismatchFailureCode = "profile-mismatch"
)

type Artifact struct {
	DesiredArtifact
	Active                  bool
	State                   RuntimeState
	NextObservationAt       time.Time
	LastObservedAt          *time.Time
	ConsecutiveFailures     int
	LastFailureCode         string
	ObservedUID             string
	ObservedResourceVersion string
	LeaseOwner              string
	LeaseEpoch              int64
	LeaseUntil              *time.Time
	WorkerContract          string
	WorkerConfigDigest      string
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

func (a Artifact) Validate() error {
	if a.DesiredArtifact.Validate() != nil || (a.State != StateAwaiting && a.State != StateReady && a.State != StateFailed) ||
		a.NextObservationAt.Before(a.CreatedAt) || a.UpdatedAt.Before(a.CreatedAt) || a.ConsecutiveFailures < 0 || a.ConsecutiveFailures > 30 {
		return ErrInvalid
	}
	if (a.LastFailureCode == "") != (a.ConsecutiveFailures == 0) || a.LastFailureCode != "" && !failureCodePattern.MatchString(a.LastFailureCode) {
		return ErrInvalid
	}
	observed := a.LastObservedAt != nil
	if observed != (a.ObservedUID != "") || observed != (a.ObservedResourceVersion != "") ||
		a.ObservedUID != "" && !kubernetesUIDPattern.MatchString(a.ObservedUID) ||
		a.ObservedResourceVersion != "" && !resourceVersionPattern.MatchString(a.ObservedResourceVersion) ||
		a.State == StateAwaiting && observed || a.State == StateReady && !observed {
		return ErrInvalid
	}
	leased := a.LeaseOwner != ""
	if leased != (a.LeaseUntil != nil) || leased != (a.WorkerContract != "") || leased != (a.WorkerConfigDigest != "") {
		return ErrInvalid
	}
	if leased && (!workerIDPattern.MatchString(a.LeaseOwner) || a.LeaseEpoch <= 0 || a.LeaseUntil == nil || !a.LeaseUntil.After(a.UpdatedAt) ||
		a.WorkerContract != RuntimeContract || !digestPattern(a.WorkerConfigDigest)) {
		return ErrInvalid
	}
	return nil
}

func digestPattern(value string) bool {
	return len(value) == 71 && strings.HasPrefix(value, "sha256:") && strings.Trim(value[7:], "0123456789abcdef") == ""
}

type Lease struct {
	Artifact Artifact
	Owner    string
	Epoch    int64
	Until    time.Time
}

func (l Lease) Validate(now time.Time) error {
	if l.Artifact.Validate() != nil || !l.Artifact.Active || !workerIDPattern.MatchString(l.Owner) || l.Epoch <= 0 || !l.Until.After(now) ||
		l.Artifact.LeaseOwner != l.Owner || l.Artifact.LeaseEpoch != l.Epoch || l.Artifact.LeaseUntil == nil || !l.Artifact.LeaseUntil.Equal(l.Until) {
		return ErrInvalid
	}
	return nil
}

type Readiness struct {
	WorkerID     string
	WorkerEpoch  int64
	Contract     string
	ConfigDigest string
	ProfileCount int
	StartedAt    time.Time
	ObservedAt   time.Time
	LeaseUntil   time.Time
}

func (r Readiness) Validate() error {
	if !workerIDPattern.MatchString(r.WorkerID) || r.WorkerEpoch <= 0 || r.Contract != RuntimeContract || !digestPattern(r.ConfigDigest) ||
		r.ProfileCount < 1 || r.ProfileCount > MaximumProfiles || r.StartedAt.IsZero() || r.ObservedAt.Before(r.StartedAt) || !r.LeaseUntil.After(r.ObservedAt) {
		return ErrInvalid
	}
	return nil
}
