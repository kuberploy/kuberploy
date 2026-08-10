package edge

import (
	"context"
	"fmt"
	"slices"
	"sync"
)

const testExternalDNSIntegrationID = "11111111-1111-4111-8111-111111111111"

func testDigest(label string) string { return digestBytes([]byte(label)) }

func expectations(names []string, prefix string) []ObjectExpectation {
	result := make([]ObjectExpectation, len(names))
	for index, name := range names {
		result[index] = ObjectExpectation{Name: name, SpecDigest: testDigest(prefix + "/" + name)}
	}
	return result
}

func testRuntimeConfig() RuntimeConfig {
	traefik := TraefikProfile{
		Revision: 1, Mode: ModeManaged, Namespace: "traefik-system", Version: "v3.5.3",
		Deployment:   DeploymentExpectation{Name: "traefik", ContainerName: "traefik", Image: "docker.io/traefik:v3.5.3", SpecDigest: testDigest("traefik-deployment")},
		Service:      ObjectExpectation{Name: "traefik", SpecDigest: testDigest("traefik-service")},
		IngressClass: ObjectExpectation{Name: "traefik", SpecDigest: testDigest("traefik-ingress-class")},
		CRDs:         expectations(RequiredTraefikCRDs, "traefik-crd"), ProfileConfigMap: "kuberploy-edge-profile", RequireLoadBalancerReady: true,
	}
	certDeployments := []DeploymentExpectation{
		{Name: "cert-manager", ContainerName: "cert-manager-controller", Image: "quay.io/jetstack/cert-manager-controller:v1.18.2", SpecDigest: testDigest("cert-manager")},
		{Name: "cert-manager-cainjector", ContainerName: "cert-manager-cainjector", Image: "quay.io/jetstack/cert-manager-cainjector:v1.18.2", SpecDigest: testDigest("cert-manager-cainjector")},
		{Name: "cert-manager-webhook", ContainerName: "cert-manager-webhook", Image: "quay.io/jetstack/cert-manager-webhook:v1.18.2", SpecDigest: testDigest("cert-manager-webhook")},
	}
	cert := CertManagerProfile{
		Revision: 1, Mode: ModeManaged, Namespace: "cert-manager", Version: "v1.18.2", Deployments: certDeployments,
		CRDs: expectations(RequiredCertManagerCRDs, "cert-crd"), ProfileConfigMap: "kuberploy-certificate-profile",
		IngressClassName: "traefik",
		ProductionIssuer: "letsencrypt-production", ProductionServerClass: "letsencrypt-production", ProductionSolverTypes: []string{"http01"},
		StagingIssuer: "letsencrypt-staging", StagingServerClass: "letsencrypt-staging", StagingSolverTypes: []string{"http01"},
	}
	external := ExternalDNSProfile{
		IntegrationID: testExternalDNSIntegrationID, Revision: 1, Mode: ModeAdopted, Namespace: "external-dns", Version: "v0.18.0",
		Deployment:       DeploymentExpectation{Name: "external-dns-primary", ContainerName: "external-dns", Image: "registry.k8s.io/external-dns/external-dns:v0.18.0", SpecDigest: testDigest("external-dns")},
		ProfileConfigMap: "external-dns-profile", LabelFilter: "kuberploy.io/dns-integration=primary", AnnotationFilter: "",
		ProviderKind: "cloudflare",
		TXTOwnerID:   "kuberploy.primary", Policy: "upsert-only", DomainFilters: []string{"example.com", "prod.example.com"},
	}
	return RuntimeConfig{
		Enabled: true, Profiles: RuntimeProfiles{Traefik: &traefik, CertManager: &cert, ExternalDNS: []ExternalDNSProfile{external}},
		PollInterval: 30_000_000_000, WorkLease: 120_000_000_000, HeartbeatInterval: 20_000_000_000,
		ReadinessMaxAge: 90_000_000_000, MinimumBackoff: 5_000_000_000, MaximumBackoff: 300_000_000_000,
	}
}

type fakeKubernetesReader struct {
	mu          sync.Mutex
	deployments map[string]DeploymentSnapshot
	services    map[string]ServiceSnapshot
	ingresses   map[string]ObjectSnapshot
	crds        map[string]ObjectSnapshot
	configMaps  map[string]ConfigMapSnapshot
	issuers     map[string]IssuerSnapshot
	err         error
}

func newFakeKubernetesReader(config RuntimeConfig) *fakeKubernetesReader {
	reader := &fakeKubernetesReader{deployments: map[string]DeploymentSnapshot{}, services: map[string]ServiceSnapshot{},
		ingresses: map[string]ObjectSnapshot{}, crds: map[string]ObjectSnapshot{}, configMaps: map[string]ConfigMapSnapshot{}, issuers: map[string]IssuerSnapshot{}}
	counter := 1
	object := func(name, namespace, spec string) ObjectSnapshot {
		value := ObjectSnapshot{Name: name, Namespace: namespace, UID: fmt.Sprintf("00000000-0000-4000-8000-%012x", counter),
			ResourceVersion: fmt.Sprintf("rv-%d", counter), Generation: 1, SpecDigest: spec}
		counter++
		return value
	}
	addDeployment := func(namespace, version string, expected DeploymentExpectation, args []string) {
		reader.deployments[namespace+"/"+expected.Name+"/"+expected.ContainerName] = DeploymentSnapshot{
			ObjectSnapshot: object(expected.Name, namespace, expected.SpecDigest), ObservedGeneration: 1, Version: version,
			DesiredReplicas: 1, AvailableReplicas: 1, ContainerName: expected.ContainerName, ContainerImage: expected.Image,
			ContainerArguments: slices.Clone(args),
		}
	}
	if profile := config.Profiles.Traefik; profile != nil {
		addDeployment(profile.Namespace, profile.Version, profile.Deployment, nil)
		reader.services[profile.Namespace+"/"+profile.Service.Name] = ServiceSnapshot{ObjectSnapshot: object(profile.Service.Name, profile.Namespace, profile.Service.SpecDigest), Type: "LoadBalancer", LoadBalancerReady: true}
		reader.ingresses[profile.IngressClass.Name] = object(profile.IngressClass.Name, "", profile.IngressClass.SpecDigest)
		for _, expected := range profile.CRDs {
			reader.crds[expected.Name] = object(expected.Name, "", expected.SpecDigest)
		}
		data := profile.ProfileData()
		reader.configMaps[profile.Namespace+"/"+profile.ProfileConfigMap] = ConfigMapSnapshot{ObjectSnapshot: object(profile.ProfileConfigMap, profile.Namespace, digestStringMap(data)), Data: cloneStringMap(data)}
	}
	if profile := config.Profiles.CertManager; profile != nil {
		for _, expected := range profile.Deployments {
			addDeployment(profile.Namespace, profile.Version, expected, nil)
		}
		for _, expected := range profile.CRDs {
			reader.crds[expected.Name] = object(expected.Name, "", expected.SpecDigest)
		}
		for _, name := range profile.ApprovedIssuers() {
			base := object(name, "", testDigest("issuer/"+name))
			reader.issuers[name] = IssuerSnapshot{Name: name, UID: base.UID, ResourceVersion: base.ResourceVersion, Generation: 1, ObservedGeneration: 1, Ready: true}
		}
		data := profile.ProfileData()
		reader.configMaps[profile.Namespace+"/"+profile.ProfileConfigMap] = ConfigMapSnapshot{ObjectSnapshot: object(profile.ProfileConfigMap, profile.Namespace, digestStringMap(data)), Data: cloneStringMap(data)}
	}
	for _, profile := range config.Profiles.ExternalDNS {
		addDeployment(profile.Namespace, profile.Version, profile.Deployment, profile.RequiredArguments())
		data := profile.ProfileData()
		reader.configMaps[profile.Namespace+"/"+profile.ProfileConfigMap] = ConfigMapSnapshot{ObjectSnapshot: object(profile.ProfileConfigMap, profile.Namespace, digestStringMap(data)), Data: cloneStringMap(data)}
	}
	return reader
}

func (r *fakeKubernetesReader) Deployment(_ context.Context, namespace, name, container string) (DeploymentSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return DeploymentSnapshot{}, r.err
	}
	value, ok := r.deployments[namespace+"/"+name+"/"+container]
	if !ok {
		return DeploymentSnapshot{}, ErrNotFound
	}
	value.ContainerArguments = slices.Clone(value.ContainerArguments)
	value.ContainerSecretRefs = slices.Clone(value.ContainerSecretRefs)
	return value, nil
}
func (r *fakeKubernetesReader) Service(_ context.Context, namespace, name string) (ServiceSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.services[namespace+"/"+name]
	if r.err != nil {
		return ServiceSnapshot{}, r.err
	}
	if !ok {
		return ServiceSnapshot{}, ErrNotFound
	}
	value.LoadBalancerIngress = slices.Clone(value.LoadBalancerIngress)
	return value, nil
}
func (r *fakeKubernetesReader) IngressClass(_ context.Context, name string) (ObjectSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.ingresses[name]
	if r.err != nil {
		return ObjectSnapshot{}, r.err
	}
	if !ok {
		return ObjectSnapshot{}, ErrNotFound
	}
	return value, nil
}
func (r *fakeKubernetesReader) CustomResourceDefinition(_ context.Context, name string) (ObjectSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.crds[name]
	if r.err != nil {
		return ObjectSnapshot{}, r.err
	}
	if !ok {
		return ObjectSnapshot{}, ErrNotFound
	}
	return value, nil
}
func (r *fakeKubernetesReader) ConfigMap(_ context.Context, namespace, name string) (ConfigMapSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.configMaps[namespace+"/"+name]
	if r.err != nil {
		return ConfigMapSnapshot{}, r.err
	}
	if !ok {
		return ConfigMapSnapshot{}, ErrNotFound
	}
	value.Data = cloneStringMap(value.Data)
	return value, nil
}

func (r *fakeKubernetesReader) NetworkPolicy(_ context.Context, namespace, name string) (ObjectSnapshot, error) {
	return ObjectSnapshot{Name: name, Namespace: namespace, UID: "77777777-7777-4777-8777-777777777777", ResourceVersion: "1", Generation: 1, SpecDigest: digestBytes([]byte("network-policy"))}, nil
}
func (r *fakeKubernetesReader) ClusterIssuer(_ context.Context, name string) (IssuerSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.issuers[name]
	if r.err != nil {
		return IssuerSnapshot{}, r.err
	}
	if !ok {
		return IssuerSnapshot{}, ErrNotFound
	}
	return value, nil
}

var _ KubernetesReader = (*fakeKubernetesReader)(nil)
