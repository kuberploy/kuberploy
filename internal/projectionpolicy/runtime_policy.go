package projectionpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/imagepull"
	"github.com/kuberploy/kuberploy/internal/middlewareprofiles"
	"github.com/kuberploy/kuberploy/internal/scheduling"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

const runtimePolicyContract = "appconfig-dynamic-policy.v5"

// RuntimePolicyDigest fences Git projection readiness to every dynamic policy
// input used during activation. It contains only safe configuration digests,
// never key material or registry credential bytes.
func RuntimePolicyDigest(secretConfig secrets.RuntimeConfig, certificateConfig certificates.ObservationConfig, issuerConfig certissuers.ObserverConfig, registryPullConfig imagepull.RuntimeConfig, edgeConfig edge.RuntimeConfig) (string, error) {
	secretDigest, err := secrets.RuntimePolicyDigest(secretConfig)
	if err != nil {
		return "", err
	}
	certificateDigest, err := certificates.ObservationPolicyDigest(certificateConfig)
	if err != nil {
		return "", certificates.ErrObservationUnavailable
	}
	issuerDigest, err := certissuers.ObserverPolicyDigest(issuerConfig)
	if err != nil {
		return "", certissuers.ErrObservationUnavailable
	}
	pullDigest, err := registryPullConfig.Digest()
	if err != nil {
		return "", imagepull.ErrInvalid
	}
	edgeDigest, err := edgeRoutePolicyDigest(edgeConfig)
	if err != nil {
		return "", edge.ErrInvalid
	}
	encoded, err := json.Marshal(struct {
		Contract           string `json:"contract"`
		RuntimeSecrets     string `json:"runtimeSecrets"`
		Certificates       string `json:"certificates"`
		CertificateIssuers string `json:"certificateIssuers"`
		RuntimeImagePulls  string `json:"runtimeImagePulls"`
		EdgeRoutes         string `json:"edgeRoutes"`
		Scheduling         string `json:"scheduling"`
		Middleware         string `json:"middleware"`
	}{runtimePolicyContract, secretDigest, certificateDigest, issuerDigest, pullDigest, edgeDigest, scheduling.Contract, middlewareprofiles.Contract})
	if err != nil {
		return "", imagepull.ErrInvalid
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func edgeRoutePolicyDigest(config edge.RuntimeConfig) (string, error) {
	if config.Validate() != nil {
		return "", edge.ErrInvalid
	}
	if config.Enabled {
		return config.Digest()
	}
	encoded, err := json.Marshal(struct {
		Contract string `json:"contract"`
		Enabled  bool   `json:"enabled"`
	}{edge.RuntimeContract, false})
	if err != nil {
		return "", edge.ErrInvalid
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
