package helmapps

import (
	"context"
	"path"
	"time"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

const ProtectedArgoRootCompatibilityContract = "helm-protected-argo-root.v1"

// ProductionProtectedArgoReadinessConfig joins independently configured Helm
// and Argo authorities. Paths are deliberately absent: the adapter derives the
// only compatible recursive root and protected Application directory.
type ProductionProtectedArgoReadinessConfig struct {
	PlatformBindingID string
	Application       ProtectedApplicationRuntime
	Publisher         ProtectedPublisherIdentity
}

func (c ProductionProtectedArgoReadinessConfig) Validate() error {
	if !uuidRE.MatchString(c.PlatformBindingID) ||
		c.Application.Validate() != nil || c.Publisher.Validate() != nil {
		return ErrInvalid
	}
	return nil
}

// ProductionProtectedArgoReadiness is the sole Helm adapter for Argo's
// production readiness proof. It snapshots the concrete probe at construction
// so a caller cannot later substitute its identity, while retaining the
// readiness store that receives normal lease heartbeats.
type ProductionProtectedArgoReadiness struct {
	probe               argo.ProductionDesiredStateReadinessProbe
	config              ProductionProtectedArgoReadinessConfig
	compatibilityDigest string
}

func NewProductionProtectedArgoReadiness(probe *argo.ProductionDesiredStateReadinessProbe,
	config ProductionProtectedArgoReadinessConfig) (*ProductionProtectedArgoReadiness, error) {
	if probe == nil || probe.Store == nil || config.Validate() != nil ||
		validateProductionArgoProbe(*probe, config) != nil {
		return nil, ErrInvalid
	}
	probeCopy := *probe
	// Every capability evaluation supplies its own exact timestamp. Avoid
	// retaining a separately mutable or divergent probe clock.
	probeCopy.Now = nil
	result := &ProductionProtectedArgoReadiness{probe: probeCopy, config: config}
	digest, err := result.expectedCompatibilityDigest()
	if err != nil {
		return nil, err
	}
	result.compatibilityDigest = digest
	return result, nil
}

func validateProductionArgoProbe(probe argo.ProductionDesiredStateReadinessProbe,
	config ProductionProtectedArgoReadinessConfig) error {
	identity := probe.Identity
	repositoryCredential, credentialErr := argo.RepositoryCredentialName(config.PlatformBindingID)
	if identity.Validate() != nil || identity.ContractVersion != argo.DesiredStateContract ||
		identity.PlatformBindingID != config.PlatformBindingID ||
		identity.ArgoNamespace != config.Application.ArgoNamespace ||
		identity.RootApplicationName != argo.PlatformRootApplicationName ||
		credentialErr != nil || identity.RepositorySecretName != repositoryCredential ||
		identity.Runtime.ChartName != argo.RuntimeChartName ||
		identity.DigestEnforcement != argo.ChartDigestNativeOCI ||
		(probe.MaxAge != 0 && (probe.MaxAge < 2*argo.DesiredStateHeartbeatInterval || probe.MaxAge > 5*time.Minute)) {
		return ErrInvalid
	}
	return nil
}

func (r *ProductionProtectedArgoReadiness) expectedCompatibilityDigest() (string, error) {
	if r == nil || r.config.Validate() != nil || r.probe.Store == nil ||
		validateProductionArgoProbe(r.probe, r.config) != nil {
		return "", ErrInvalid
	}
	rootPath := path.Join(gitprojection.PlatformPrefix(), "argocd")
	applicationDirectory := path.Join(rootPath, "helm-applications")
	return digestJSON(struct {
		Contract                 string                     `json:"contract"`
		ArgoContract             string                     `json:"argoContract"`
		ArgoConfigDigest         string                     `json:"argoConfigDigest"`
		PlatformBindingID        string                     `json:"platformBindingId"`
		ArgoNamespace            string                     `json:"argoNamespace"`
		RootApplicationName      string                     `json:"rootApplicationName"`
		RootPath                 string                     `json:"rootPath"`
		RootRecursive            bool                       `json:"rootRecursive"`
		ProtectedApplicationPath string                     `json:"protectedApplicationPath"`
		Publisher                ProtectedPublisherIdentity `json:"publisher"`
	}{ProtectedArgoRootCompatibilityContract, r.probe.Identity.ContractVersion,
		r.probe.Identity.ConfigDigest, r.config.PlatformBindingID,
		r.config.Application.ArgoNamespace, r.probe.Identity.RootApplicationName, rootPath, true,
		applicationDirectory, r.config.Publisher})
}

func (r *ProductionProtectedArgoReadiness) ProtectedHelmApplicationsReady(ctx context.Context,
	publisher ProtectedPublisherIdentity, now time.Time) (bool, error) {
	if r == nil || ctx == nil || now.IsZero() || publisher.Validate() != nil || publisher != r.config.Publisher {
		return false, ErrInvalid
	}
	expectedDigest, err := r.expectedCompatibilityDigest()
	if err != nil || expectedDigest != r.compatibilityDigest {
		return false, ErrInvalid
	}
	probe := r.probe
	probe.Now = func() time.Time { return now.UTC() }
	if probe.Probe(ctx) != nil {
		return false, nil
	}
	return true, nil
}

var _ ProtectedArgoReadiness = (*ProductionProtectedArgoReadiness)(nil)
