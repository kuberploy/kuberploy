package projectionpolicy_test

import (
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/imagepull"
	"github.com/kuberploy/kuberploy/internal/projectionpolicy"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

func TestRuntimePolicyDigestFencesRegistryPullProfiles(t *testing.T) {
	disabled, err := projectionpolicy.RuntimePolicyDigest(secrets.RuntimeConfig{}, certificates.ObservationConfig{}, certissuers.ObserverConfig{}, imagepull.RuntimeConfig{}, edge.RuntimeConfig{}, externaldns.OperationalConfig{})
	if err != nil || !strings.HasPrefix(disabled, "sha256:") {
		t.Fatalf("disabled digest=%q err=%v", disabled, err)
	}
	config := imagepull.DefaultRuntimeConfig()
	config.Enabled = true
	config.Namespaces = []string{"apps-production"}
	config.Profiles = []imagepull.Profile{{Name: "external-main", TargetID: "11111111-1111-4111-8111-111111111111",
		RegistryServer: "registry.example.test", CredentialRef: "operator/external-main", Revision: 1,
		SourceSecretRef: "registry-pull-external-main", SourceSecretKey: "dockerconfigjson"}}
	first, err := projectionpolicy.RuntimePolicyDigest(secrets.RuntimeConfig{}, certificates.ObservationConfig{}, certissuers.ObserverConfig{}, config, edge.RuntimeConfig{}, externaldns.OperationalConfig{})
	if err != nil {
		t.Fatal(err)
	}
	config.Profiles[0].Revision = 2
	second, err := projectionpolicy.RuntimePolicyDigest(secrets.RuntimeConfig{}, certificates.ObservationConfig{}, certissuers.ObserverConfig{}, config, edge.RuntimeConfig{}, externaldns.OperationalConfig{})
	if err != nil || first == second || disabled == first {
		t.Fatalf("digests disabled=%q first=%q second=%q err=%v", disabled, first, second, err)
	}
}
