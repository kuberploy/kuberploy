// Package argo compiles trusted Kuberploy Git bindings into deterministic Argo
// CD manifests and stores Argo's observed state. Desired-state mutations,
// including rollbacks, are represented only as new Git commits.
package argo

import (
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

var (
	ErrInvalid                = errors.New("invalid Argo reconciliation input")
	ErrNotFound               = errors.New("Argo observation not found")
	ErrConflict               = errors.New("Argo observation conflict")
	ErrLeaseHeld              = errors.New("Argo observation lease is held")
	ErrLeaseLost              = errors.New("Argo observation lease was lost")
	ErrApplicationSetNotReady = errors.New("Argo ApplicationSet refresh is not ready")

	uuidRE    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	digestRE  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitRE  = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	kubeRE    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$`)
	semverRE  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	groupRE   = regexp.MustCompile(`^(?:|[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?)$`)
	kindRE    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,127}$`)
	versionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,63}$`)
)

const InClusterServer = "https://kubernetes.default.svc"

type RuntimeLock struct {
	ChartRepository string `json:"chartRepository"`
	ChartName       string `json:"chartName"`
	ChartVersion    string `json:"chartVersion"`
	ChartDigest     string `json:"chartDigest"`
	RendererImage   string `json:"rendererImage"`
}

func (l RuntimeLock) Validate() error {
	repository, parseErr := url.Parse(l.ChartRepository)
	if parseErr != nil || repository.Scheme != "oci" || repository.Host == "" || repository.Path == "" || repository.User != nil || repository.RawQuery != "" || repository.Fragment != "" ||
		len(l.ChartRepository) > 512 || strings.ContainsAny(l.ChartRepository, "?#@\r\n\x00") ||
		l.ChartName != "kuberploy-runtime" || !semverRE.MatchString(l.ChartVersion) || len(l.ChartVersion) > 64 || !digestRE.MatchString(l.ChartDigest) ||
		len(l.RendererImage) > 512 || !strings.Contains(l.RendererImage, "@sha256:") {
		return ErrInvalid
	}
	parts := strings.Split(l.RendererImage, "@")
	if len(parts) != 2 || parts[0] == "" || strings.ContainsAny(parts[0], " \t\r\n\x00") || !digestRE.MatchString(parts[1]) {
		return ErrInvalid
	}
	return nil
}

type EnvironmentTarget struct {
	Project       domain.Project
	Environment   domain.Environment
	Binding       gitprojection.Binding
	ArgoNamespace string
	Runtime       RuntimeLock
}

func (t EnvironmentTarget) Validate() error {
	if t.Binding.Validate() != nil || t.Binding.Kind != gitprojection.BindingEnvironment || t.Binding.ProjectID != t.Project.ID ||
		t.Binding.EnvironmentID != t.Environment.ID || t.Binding.State != gitprojection.BindingReady || t.Binding.IndexedRevision == "" ||
		t.Binding.TargetHeadRevision != t.Binding.IndexedRevision || t.Binding.ProjectionGeneration <= 0 || t.Binding.IndexedAt.IsZero() ||
		t.Environment.ProjectID != t.Project.ID || !kubeRE.MatchString(t.ArgoNamespace) || t.Runtime.Validate() != nil {
		return ErrInvalid
	}
	namespace, argoProject := domain.DeriveEnvironmentDestination(t.Project, t.Environment.Slug)
	if t.Environment.Namespace != namespace || t.Environment.ArgoProject != argoProject {
		return ErrInvalid
	}
	return nil
}

func ProjectName(projectID string) string {
	return "kp-p-" + strings.ReplaceAll(strings.ToLower(projectID), "-", "")
}

func ApplicationName(deploymentID string) string {
	return "kp-d-" + strings.ReplaceAll(strings.ToLower(deploymentID), "-", "")
}

func ApplicationSetName(environmentID string) string {
	return "kp-e-" + strings.ReplaceAll(strings.ToLower(environmentID), "-", "")
}

type SyncStatus string

const (
	SyncUnknown   SyncStatus = "unknown"
	SyncSynced    SyncStatus = "synced"
	SyncOutOfSync SyncStatus = "out-of-sync"
)

type HealthStatus string

const (
	HealthUnknown     HealthStatus = "unknown"
	HealthProgressing HealthStatus = "progressing"
	HealthHealthy     HealthStatus = "healthy"
	HealthDegraded    HealthStatus = "degraded"
	HealthSuspended   HealthStatus = "suspended"
	HealthMissing     HealthStatus = "missing"
)

type ResourceIdentity struct {
	Group     string       `json:"group"`
	Version   string       `json:"version"`
	Kind      string       `json:"kind"`
	Namespace string       `json:"namespace"`
	Name      string       `json:"name"`
	UID       string       `json:"uid"`
	Health    HealthStatus `json:"health"`
}

type Observation struct {
	DeploymentID         string             `json:"deploymentId"`
	ApplicationID        string             `json:"applicationId"`
	ProjectID            string             `json:"projectId"`
	EnvironmentID        string             `json:"environmentId"`
	ArgoUID              string             `json:"argoUid"`
	ArgoNamespace        string             `json:"argoNamespace"`
	ArgoName             string             `json:"argoName"`
	DestinationNamespace string             `json:"destinationNamespace"`
	DesiredRevision      string             `json:"desiredRevision"`
	ObservedRevision     string             `json:"observedRevision"`
	Sync                 SyncStatus         `json:"sync"`
	Health               HealthStatus       `json:"health"`
	OperationPhase       string             `json:"operationPhase,omitempty"`
	Message              string             `json:"message,omitempty"`
	Resources            []ResourceIdentity `json:"resources"`
	ObservedAt           time.Time          `json:"observedAt"`
	UpdatedAt            time.Time          `json:"updatedAt"`
}

func (o Observation) Validate() error {
	if !uuidRE.MatchString(o.DeploymentID) || !uuidRE.MatchString(o.ApplicationID) || !uuidRE.MatchString(o.ProjectID) || !uuidRE.MatchString(o.EnvironmentID) || !uuidRE.MatchString(o.ArgoUID) || !kubeRE.MatchString(o.ArgoNamespace) ||
		o.ArgoName != ApplicationName(o.DeploymentID) || !kubeRE.MatchString(o.DestinationNamespace) || !commitRE.MatchString(o.DesiredRevision) ||
		!commitRE.MatchString(o.ObservedRevision) || !validSync(o.Sync) || !validHealth(o.Health) || len(o.Message) > 1024 ||
		!validOperationPhase(o.OperationPhase) || len(o.Resources) > 512 || o.ObservedAt.IsZero() || o.UpdatedAt.IsZero() || o.UpdatedAt.Before(o.ObservedAt) {
		return ErrInvalid
	}
	seen := map[string]struct{}{}
	for _, resource := range o.Resources {
		if !groupRE.MatchString(resource.Group) || !versionRE.MatchString(resource.Version) || !kindRE.MatchString(resource.Kind) || resource.Namespace != o.DestinationNamespace ||
			!kubeRE.MatchString(resource.Name) || resource.UID == "" || len(resource.UID) > 128 || strings.ContainsAny(resource.UID, "\x00\r\n") || !validHealth(resource.Health) {
			return ErrInvalid
		}
		key := resource.Group + "\x00" + resource.Version + "\x00" + resource.Kind + "\x00" + resource.Namespace + "\x00" + resource.Name
		if _, duplicate := seen[key]; duplicate {
			return ErrInvalid
		}
		seen[key] = struct{}{}
	}
	return nil
}

func sameObservation(left, right Observation) bool {
	return left.DeploymentID == right.DeploymentID && left.ApplicationID == right.ApplicationID && left.ProjectID == right.ProjectID && left.EnvironmentID == right.EnvironmentID &&
		left.ArgoUID == right.ArgoUID && left.ArgoNamespace == right.ArgoNamespace && left.ArgoName == right.ArgoName &&
		left.DestinationNamespace == right.DestinationNamespace && left.DesiredRevision == right.DesiredRevision &&
		left.ObservedRevision == right.ObservedRevision && left.Sync == right.Sync && left.Health == right.Health &&
		left.OperationPhase == right.OperationPhase && left.Message == right.Message && slices.Equal(left.Resources, right.Resources) &&
		left.ObservedAt.Equal(right.ObservedAt) && left.UpdatedAt.Equal(right.UpdatedAt)
}

func (o Observation) Reconciled() bool {
	return o.Sync == SyncSynced && o.Health == HealthHealthy && o.DesiredRevision == o.ObservedRevision
}

func validSync(value SyncStatus) bool {
	return value == SyncUnknown || value == SyncSynced || value == SyncOutOfSync
}
func validHealth(value HealthStatus) bool {
	return value == HealthUnknown || value == HealthProgressing || value == HealthHealthy || value == HealthDegraded || value == HealthSuspended || value == HealthMissing
}
func validOperationPhase(value string) bool {
	switch value {
	case "", "running", "succeeded", "failed", "error", "terminating":
		return true
	default:
		return false
	}
}
