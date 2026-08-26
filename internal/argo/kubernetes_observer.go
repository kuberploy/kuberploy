package argo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	KuberployApplicationSelector = "app.kubernetes.io/managed-by=kuberploy"
	DefaultApplicationPageSize   = 250
	MaximumObservedApplications  = 10_000
)

var ErrObservationNotReady = errors.New("Argo application observation is not ready")

// ObservationTarget is authoritative control-plane identity used to validate
// a Kubernetes Application before its status can enter the read model. Labels
// on the Application are never sufficient authorization on their own.
type ObservationTarget struct {
	DeploymentID         string
	ApplicationID        string
	ProjectID            string
	EnvironmentID        string
	ArgoProject          string
	DestinationNamespace string
	DesiredRevision      string
}

func (t ObservationTarget) validate() error {
	if !uuidRE.MatchString(t.DeploymentID) || !uuidRE.MatchString(t.ApplicationID) || !uuidRE.MatchString(t.ProjectID) || !uuidRE.MatchString(t.EnvironmentID) ||
		!kubeRE.MatchString(t.ArgoProject) || !kubeRE.MatchString(t.DestinationNamespace) || !commitRE.MatchString(t.DesiredRevision) {
		return ErrInvalid
	}
	return nil
}

// ObservationTargetResolver loads server-owned application/environment/Git
// identities. Implementations must authorize only records managed by this
// Kuberploy installation; the Kubernetes object's labels are untrusted input.
type ObservationTargetResolver interface {
	ResolveArgoObservationTarget(context.Context, string) (ObservationTarget, error)
}

type KubernetesApplication struct {
	UID                  string
	Namespace            string
	Name                 string
	ResourceVersion      string
	Labels               map[string]string
	Project              string
	DestinationServer    string
	DestinationNamespace string
	SyncStatus           string
	SyncRevisions        []string
	HealthStatus         string
	OperationPhase       string
	ReconciledAt         time.Time
}

type KubernetesApplicationPage struct {
	ResourceVersion string
	Continue        string
	Applications    []KubernetesApplication
}

// KubernetesApplicationSource is deliberately list-only. A production watch
// loop may use the same decoder, but a complete, resourceVersion-consistent
// namespace list is the repair boundary after disconnects and compaction.
type KubernetesApplicationSource interface {
	ListKuberployApplications(context.Context, string, string, int) (KubernetesApplicationPage, error)
}

type ObservationSink interface {
	PutObservation(context.Context, Observation) error
}

type ObservationBatch struct {
	Observed        int
	IgnoredUnknown  int
	IgnoredStale    int
	SnapshotVersion string
}

// KubernetesObserver projects a complete namespace list into the durable
// observation store. It deliberately has no Argo mutation or sync method.
// Multi-replica production wiring must put PollOnce behind a fenced namespace
// lease; this package does not pretend an in-process mutex is distributed.
type KubernetesObserver struct {
	Source    KubernetesApplicationSource
	Resolver  ObservationTargetResolver
	Store     ObservationSink
	Namespace string
	PageSize  int
	Now       func() time.Time
}

func (o KubernetesObserver) PollOnce(ctx context.Context) (ObservationBatch, error) {
	if o.Source == nil || o.Resolver == nil || o.Store == nil || !kubeRE.MatchString(o.Namespace) {
		return ObservationBatch{}, ErrInvalid
	}
	pageSize := o.PageSize
	if pageSize == 0 {
		pageSize = DefaultApplicationPageSize
	}
	if pageSize < 1 || pageSize > 500 {
		return ObservationBatch{}, ErrInvalid
	}
	var result ObservationBatch
	continuation := ""
	seenContinuations := map[string]struct{}{}
	seenApplications := map[string]struct{}{}
	scannedApplications := 0
	for {
		page, err := o.Source.ListKuberployApplications(ctx, o.Namespace, continuation, pageSize)
		if err != nil {
			return result, err
		}
		if page.ResourceVersion == "" || strings.ContainsAny(page.ResourceVersion, "\x00\r\n") || len(page.ResourceVersion) > 128 {
			return result, ErrInvalid
		}
		if result.SnapshotVersion == "" {
			result.SnapshotVersion = page.ResourceVersion
		} else if result.SnapshotVersion != page.ResourceVersion {
			return result, errors.New("Argo Application pagination changed resourceVersion")
		}
		for _, application := range page.Applications {
			scannedApplications++
			if scannedApplications > MaximumObservedApplications {
				return result, errors.New("Argo Application observation exceeded its bound")
			}
			deploymentID := strings.TrimSpace(application.Labels["kuberploy.io/deployment-id"])
			if deploymentID == "" {
				if isManagedHelmApplication(application) {
					// Helm Applications deliberately carry application and environment
					// identity but no deployment identity. Accept both the current direct
					// Helm label and the legacy approved-package label during upgrades.
					result.IgnoredUnknown++
					continue
				}
				return result, ErrInvalid
			}
			if _, duplicate := seenApplications[deploymentID]; duplicate {
				return result, ErrInvalid
			}
			seenApplications[deploymentID] = struct{}{}
			target, resolveErr := o.Resolver.ResolveArgoObservationTarget(ctx, deploymentID)
			if errors.Is(resolveErr, ErrNotFound) {
				// A stale or spoofed managed-by label must not become a row or block
				// observations for valid applications in the same Argo namespace.
				result.IgnoredUnknown++
				continue
			}
			if resolveErr != nil {
				return result, resolveErr
			}
			observation, decodeErr := ObservationFromKubernetesApplication(application, target, o.Namespace)
			if errors.Is(decodeErr, ErrObservationNotReady) {
				continue
			}
			if decodeErr != nil {
				return result, decodeErr
			}
			// Argo's status.reconciledAt identifies the desired-state reconcile,
			// but it does not necessarily advance when rollout health changes after
			// that reconcile. Stamp the exact poll time so later health transitions
			// can replace an older durable observation of the same revision.
			if o.Now != nil {
				observedAt := o.Now().UTC()
				if observedAt.IsZero() {
					return result, ErrInvalid
				}
				observation.ObservedAt = observedAt
				observation.UpdatedAt = observedAt
			}
			if err = o.Store.PutObservation(ctx, observation); err != nil {
				if errors.Is(err, ErrConflict) {
					result.IgnoredStale++
					continue
				}
				return result, err
			}
			result.Observed++
		}
		if page.Continue == "" {
			return result, nil
		}
		if len(page.Continue) > 1024 || strings.ContainsAny(page.Continue, "\x00\r\n") {
			return result, ErrInvalid
		}
		if _, duplicate := seenContinuations[page.Continue]; duplicate {
			return result, errors.New("Argo Application pagination repeated its continuation token")
		}
		seenContinuations[page.Continue] = struct{}{}
		continuation = page.Continue
	}
}

func isManagedHelmApplication(application KubernetesApplication) bool {
	labels := application.Labels
	applicationID := strings.TrimSpace(labels["kuberploy.io/application-id"])
	component := labels["app.kubernetes.io/component"]
	return labels["app.kubernetes.io/managed-by"] == "kuberploy" &&
		(component == "helm-application" || component == "approved-helm-application") &&
		uuidRE.MatchString(applicationID) && uuidRE.MatchString(strings.TrimSpace(labels["kuberploy.io/project-id"])) &&
		uuidRE.MatchString(strings.TrimSpace(labels["kuberploy.io/environment-id"])) &&
		application.Name == "kp-h-"+strings.ReplaceAll(applicationID, "-", "")
}

func ObservationFromKubernetesApplication(application KubernetesApplication, target ObservationTarget, argoNamespace string) (Observation, error) {
	if target.validate() != nil || !kubeRE.MatchString(argoNamespace) || application.Namespace != argoNamespace ||
		application.Name != ApplicationName(target.DeploymentID) || application.Project != target.ArgoProject ||
		application.DestinationServer != InClusterServer || application.DestinationNamespace != target.DestinationNamespace ||
		application.Labels["app.kubernetes.io/managed-by"] != "kuberploy" ||
		application.Labels["kuberploy.io/deployment-id"] != target.DeploymentID ||
		application.Labels["kuberploy.io/application-id"] != target.ApplicationID ||
		application.Labels["kuberploy.io/project-id"] != target.ProjectID ||
		application.Labels["kuberploy.io/environment-id"] != target.EnvironmentID ||
		!uuidRE.MatchString(application.UID) || application.ResourceVersion == "" || len(application.ResourceVersion) > 128 ||
		strings.ContainsAny(application.ResourceVersion, "\x00\r\n") {
		return Observation{}, ErrInvalid
	}
	if application.ReconciledAt.IsZero() {
		return Observation{}, ErrObservationNotReady
	}
	observedRevision, err := oneGitRevision(application.SyncRevisions)
	if err != nil {
		return Observation{}, err
	}
	syncStatus, err := decodeSyncStatus(application.SyncStatus)
	if err != nil {
		return Observation{}, err
	}
	healthStatus, err := decodeHealthStatus(application.HealthStatus)
	if err != nil {
		return Observation{}, err
	}
	operationPhase, err := decodeOperationPhase(application.OperationPhase)
	if err != nil {
		return Observation{}, err
	}
	reconciledAt := application.ReconciledAt.UTC()
	observation := Observation{
		DeploymentID: target.DeploymentID, ApplicationID: target.ApplicationID, ProjectID: target.ProjectID, EnvironmentID: target.EnvironmentID,
		ArgoUID: application.UID, ArgoNamespace: argoNamespace, ArgoName: application.Name,
		DestinationNamespace: target.DestinationNamespace, DesiredRevision: target.DesiredRevision, ObservedRevision: observedRevision,
		Sync: syncStatus, Health: healthStatus, OperationPhase: operationPhase, Resources: []ResourceIdentity{},
		ObservedAt: reconciledAt, UpdatedAt: reconciledAt,
	}
	if err = observation.Validate(); err != nil {
		return Observation{}, err
	}
	return observation, nil
}

func oneGitRevision(values []string) (string, error) {
	unique := make([]string, 0, 1)
	for _, value := range values {
		value = strings.TrimSpace(strings.ToLower(value))
		if !commitRE.MatchString(value) || slices.Contains(unique, value) {
			continue
		}
		unique = append(unique, value)
	}
	if len(unique) == 0 {
		return "", ErrObservationNotReady
	}
	if len(unique) != 1 {
		return "", fmt.Errorf("Argo Application status contains ambiguous Git revisions: %w", ErrInvalid)
	}
	return unique[0], nil
}

func decodeSyncStatus(value string) (SyncStatus, error) {
	switch value {
	case "Unknown", "":
		return SyncUnknown, nil
	case "Synced":
		return SyncSynced, nil
	case "OutOfSync":
		return SyncOutOfSync, nil
	default:
		return "", ErrInvalid
	}
}

func decodeHealthStatus(value string) (HealthStatus, error) {
	switch value {
	case "Unknown", "":
		return HealthUnknown, nil
	case "Progressing":
		return HealthProgressing, nil
	case "Healthy":
		return HealthHealthy, nil
	case "Degraded":
		return HealthDegraded, nil
	case "Suspended":
		return HealthSuspended, nil
	case "Missing":
		return HealthMissing, nil
	default:
		return "", ErrInvalid
	}
}

func decodeOperationPhase(value string) (string, error) {
	switch value {
	case "":
		return "", nil
	case "Running":
		return "running", nil
	case "Succeeded":
		return "succeeded", nil
	case "Failed":
		return "failed", nil
	case "Error":
		return "error", nil
	case "Terminating":
		return "terminating", nil
	default:
		return "", ErrInvalid
	}
}
