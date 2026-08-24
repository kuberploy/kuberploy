package edge

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
)

type ObjectSnapshot struct {
	Name            string
	Namespace       string
	UID             string
	ResourceVersion string
	Generation      int64
	SpecDigest      string
}

func (o ObjectSnapshot) validate(name, namespace, expectedDigest string) error {
	if o.Name != name || o.Namespace != namespace || !uuidPattern.MatchString(o.UID) || !resourceVersionPattern.MatchString(o.ResourceVersion) ||
		o.Generation < 0 || o.SpecDigest != expectedDigest || !validDigest(o.SpecDigest) {
		return mismatch("resource-identity-mismatch")
	}
	return nil
}

type DeploymentSnapshot struct {
	ObjectSnapshot
	ObservedGeneration     int64
	Version                string
	DesiredReplicas        int32
	AvailableReplicas      int32
	ContainerName          string
	ContainerImage         string
	ContainerArguments     []string
	ContainerSecretRefs    []string
	ContainerConfigMapRefs []string
}

func (d DeploymentSnapshot) validate(namespace, version string, expected DeploymentExpectation) error {
	if d.ObjectSnapshot.validate(expected.Name, namespace, expected.SpecDigest) != nil {
		return mismatch("deployment-spec-mismatch")
	}
	if d.ObservedGeneration != d.Generation || d.Version != version || d.DesiredReplicas < 1 || d.AvailableReplicas < d.DesiredReplicas ||
		d.ContainerName != expected.ContainerName || d.ContainerImage != expected.Image {
		return mismatch("deployment-not-ready")
	}
	if len(d.ContainerArguments) > 256 {
		return mismatch("deployment-arguments-invalid")
	}
	if len(d.ContainerSecretRefs) > 32 || !slices.IsSorted(d.ContainerSecretRefs) {
		return mismatch("deployment-secret-references-invalid")
	}
	if len(d.ContainerConfigMapRefs) > 32 || !slices.IsSorted(d.ContainerConfigMapRefs) {
		return mismatch("deployment-configmap-references-invalid")
	}
	for index, name := range d.ContainerConfigMapRefs {
		if !validObjectName(name) || index > 0 && d.ContainerConfigMapRefs[index-1] == name {
			return mismatch("deployment-configmap-references-invalid")
		}
	}
	for index, name := range d.ContainerSecretRefs {
		if !validObjectName(name) || index > 0 && d.ContainerSecretRefs[index-1] == name {
			return mismatch("deployment-secret-references-invalid")
		}
	}
	return nil
}

type ServiceSnapshot struct {
	ObjectSnapshot
	Type                string
	LoadBalancerReady   bool
	LoadBalancerIngress []LoadBalancerIngress
}

type ConfigMapSnapshot struct {
	ObjectSnapshot
	Data       map[string]string
	BinaryData bool
}

type IssuerSnapshot struct {
	Name               string
	UID                string
	ResourceVersion    string
	Generation         int64
	ObservedGeneration int64
	Ready              bool
}

type KubernetesReader interface {
	Deployment(context.Context, string, string, string) (DeploymentSnapshot, error)
	Service(context.Context, string, string) (ServiceSnapshot, error)
	IngressClass(context.Context, string) (ObjectSnapshot, error)
	CustomResourceDefinition(context.Context, string) (ObjectSnapshot, error)
	ConfigMap(context.Context, string, string) (ConfigMapSnapshot, error)
	ClusterIssuer(context.Context, string) (IssuerSnapshot, error)
	NetworkPolicy(context.Context, string, string) (ObjectSnapshot, error)
}

type TargetObserver interface {
	ObserveTraefik(context.Context, TraefikProfile) (ObservationReceipt, error)
	ObserveCertManager(context.Context, CertManagerProfile) (ObservationReceipt, error)
	ObserveExternalDNS(context.Context, ExternalDNSProfile) (ObservationReceipt, error)
}

type KubernetesTargetObserver struct {
	Reader      KubernetesReader
	Resolver    HostnameResolver
	CallTimeout time.Duration
}

func (o *KubernetesTargetObserver) callTimeout() time.Duration {
	if o.CallTimeout == 0 {
		return 10 * time.Second
	}
	return o.CallTimeout
}

func (o *KubernetesTargetObserver) validate() error {
	if o == nil || o.Reader == nil || o.callTimeout() < time.Second || o.callTimeout() > 30*time.Second {
		return ErrInvalid
	}
	return nil
}

func (o *KubernetesTargetObserver) ObserveTraefik(ctx context.Context, profile TraefikProfile) (ObservationReceipt, error) {
	if o.validate() != nil || profile.Validate() != nil {
		return ObservationReceipt{}, ErrInvalid
	}
	identities, versions := []string{}, []string{}
	deployment, err := observeCall(ctx, o.callTimeout(), func(callContext context.Context) (DeploymentSnapshot, error) {
		return o.Reader.Deployment(callContext, profile.Namespace, profile.Deployment.Name, profile.Deployment.ContainerName)
	})
	if err != nil {
		return ObservationReceipt{}, err
	}
	if err = deployment.validate(profile.Namespace, profile.Version, profile.Deployment); err != nil {
		return ObservationReceipt{}, err
	}
	appendObjectReceipt(&identities, &versions, "Deployment", deployment.ObjectSnapshot)
	service, err := observeCall(ctx, o.callTimeout(), func(callContext context.Context) (ServiceSnapshot, error) {
		return o.Reader.Service(callContext, profile.Namespace, profile.Service.Name)
	})
	if err != nil {
		return ObservationReceipt{}, err
	}
	if service.ObjectSnapshot.validate(profile.Service.Name, profile.Namespace, profile.Service.SpecDigest) != nil ||
		profile.RequireLoadBalancerReady && (service.Type != "LoadBalancer" || !service.LoadBalancerReady) {
		return ObservationReceipt{}, mismatch("service-not-ready")
	}
	appendObjectReceipt(&identities, &versions, "Service", service.ObjectSnapshot)
	var sslipEndpoint *SSLIPIngressEndpoint
	if profile.SSLIP != nil {
		selected, selectErr := selectSSLIPIngress(ctx, *profile.SSLIP, service, o.Resolver, o.callTimeout())
		if selectErr != nil {
			return ObservationReceipt{}, selectErr
		}
		sslipEndpoint = &selected
		identities = append(identities, "SSLIP\x00"+selected.PublicIPv4+"\x00"+selected.Source)
		versions = append(versions, "SSLIP\x00"+selected.ServiceResourceVersion)
	}
	ingressClass, err := observeCall(ctx, o.callTimeout(), func(callContext context.Context) (ObjectSnapshot, error) {
		return o.Reader.IngressClass(callContext, profile.IngressClass.Name)
	})
	if err != nil {
		return ObservationReceipt{}, err
	}
	if ingressClass.validate(profile.IngressClass.Name, "", profile.IngressClass.SpecDigest) != nil {
		return ObservationReceipt{}, mismatch("ingress-class-mismatch")
	}
	appendObjectReceipt(&identities, &versions, "IngressClass", ingressClass)
	for _, expectation := range profile.CRDs {
		crd, readErr := observeCall(ctx, o.callTimeout(), func(callContext context.Context) (ObjectSnapshot, error) {
			return o.Reader.CustomResourceDefinition(callContext, expectation.Name)
		})
		if readErr != nil {
			return ObservationReceipt{}, readErr
		}
		if crd.validate(expectation.Name, "", expectation.SpecDigest) != nil {
			return ObservationReceipt{}, mismatch("traefik-crd-mismatch")
		}
		appendObjectReceipt(&identities, &versions, "CustomResourceDefinition", crd)
	}
	profileMap, err := o.observeProfile(ctx, profile.Namespace, profile.ProfileConfigMap, profile.ProfileData())
	if err != nil {
		return ObservationReceipt{}, err
	}
	appendObjectReceipt(&identities, &versions, "ConfigMap", profileMap.ObjectSnapshot)
	desiredDigest, _ := profile.Digest()
	result := receipt("traefik", desiredDigest, identities, versions)
	result.SSLIP = sslipEndpoint
	return result, nil
}

func (o *KubernetesTargetObserver) ObserveCertManager(ctx context.Context, profile CertManagerProfile) (ObservationReceipt, error) {
	if o.validate() != nil || profile.Validate() != nil {
		return ObservationReceipt{}, ErrInvalid
	}
	identities, versions := []string{}, []string{}
	for _, expectation := range profile.Deployments {
		deployment, err := observeCall(ctx, o.callTimeout(), func(callContext context.Context) (DeploymentSnapshot, error) {
			return o.Reader.Deployment(callContext, profile.Namespace, expectation.Name, expectation.ContainerName)
		})
		if err != nil {
			return ObservationReceipt{}, err
		}
		if err = deployment.validate(profile.Namespace, profile.Version, expectation); err != nil {
			return ObservationReceipt{}, err
		}
		appendObjectReceipt(&identities, &versions, "Deployment", deployment.ObjectSnapshot)
	}
	for _, expectation := range profile.CRDs {
		crd, err := observeCall(ctx, o.callTimeout(), func(callContext context.Context) (ObjectSnapshot, error) {
			return o.Reader.CustomResourceDefinition(callContext, expectation.Name)
		})
		if err != nil {
			return ObservationReceipt{}, err
		}
		if crd.validate(expectation.Name, "", expectation.SpecDigest) != nil {
			return ObservationReceipt{}, mismatch("cert-manager-crd-mismatch")
		}
		appendObjectReceipt(&identities, &versions, "CustomResourceDefinition", crd)
	}
	for _, name := range profile.ApprovedIssuers() {
		issuer, err := observeCall(ctx, o.callTimeout(), func(callContext context.Context) (IssuerSnapshot, error) {
			return o.Reader.ClusterIssuer(callContext, name)
		})
		if err != nil {
			return ObservationReceipt{}, err
		}
		if issuer.Name != name || !uuidPattern.MatchString(issuer.UID) || !resourceVersionPattern.MatchString(issuer.ResourceVersion) ||
			issuer.Generation < 1 || issuer.ObservedGeneration != issuer.Generation || !issuer.Ready {
			return ObservationReceipt{}, mismatch("approved-issuer-not-ready")
		}
		identities = append(identities, "ClusterIssuer\x00\x00"+name+"\x00"+issuer.UID+"\x00"+strconv.FormatInt(issuer.Generation, 10))
		versions = append(versions, "ClusterIssuer\x00"+name+"\x00"+issuer.ResourceVersion)
	}
	profileMap, err := o.observeProfile(ctx, profile.Namespace, profile.ProfileConfigMap, profile.ProfileData())
	if err != nil {
		return ObservationReceipt{}, err
	}
	appendObjectReceipt(&identities, &versions, "ConfigMap", profileMap.ObjectSnapshot)
	desiredDigest, _ := profile.Digest()
	return receipt("cert-manager", desiredDigest, identities, versions), nil
}

func (o *KubernetesTargetObserver) ObserveExternalDNS(ctx context.Context, profile ExternalDNSProfile) (ObservationReceipt, error) {
	if o.validate() != nil || profile.Validate() != nil {
		return ObservationReceipt{}, ErrInvalid
	}
	deployment, err := observeCall(ctx, o.callTimeout(), func(callContext context.Context) (DeploymentSnapshot, error) {
		return o.Reader.Deployment(callContext, profile.Namespace, profile.Deployment.Name, profile.Deployment.ContainerName)
	})
	if err != nil {
		return ObservationReceipt{}, err
	}
	if err = deployment.validate(profile.Namespace, profile.Version, profile.Deployment); err != nil {
		return ObservationReceipt{}, err
	}
	for _, required := range profile.RequiredArguments() {
		separator := strings.IndexByte(required, '=')
		if separator < 1 {
			return ObservationReceipt{}, mismatch("external-dns-arguments-mismatch")
		}
		prefix := required[:separator+1]
		if prefix == "--managed-record-types=" {
			continue
		}
		if !hasExactArgument(deployment.ContainerArguments, prefix, required) {
			return ObservationReceipt{}, mismatch("external-dns-arguments-mismatch")
		}
	}
	if !hasExactArgumentSet(deployment.ContainerArguments, "--managed-record-types=", []string{
		"--managed-record-types=A", "--managed-record-types=AAAA", "--managed-record-types=CNAME", "--managed-record-types=TXT",
	}) {
		return ObservationReceipt{}, mismatch("external-dns-arguments-mismatch")
	}
	if profile.Mode == ModeManaged {
		if !hasExactArgument(deployment.ContainerArguments, "--label-filter=", "--label-filter="+profile.LabelFilter) {
			return ObservationReceipt{}, mismatch("external-dns-label-filter-mismatch")
		}
		expectedDomains := make([]string, len(profile.DomainFilters))
		for index, domain := range profile.DomainFilters {
			expectedDomains[index] = "--domain-filter=" + domain
		}
		if !hasExactArgumentSet(deployment.ContainerArguments, "--domain-filter=", expectedDomains) {
			return ObservationReceipt{}, mismatch("external-dns-domain-filter-mismatch")
		}
	}
	if profile.Mode == ModeManaged && !slices.Equal(deployment.ContainerSecretRefs, []string{profile.CredentialSecretRef}) {
		return ObservationReceipt{}, mismatch("external-dns-credential-reference-mismatch")
	}
	if profile.Mode == ModeManaged && !slices.Equal(deployment.ContainerConfigMapRefs, []string{profile.ProviderConfigRef}) {
		return ObservationReceipt{}, mismatch("external-dns-provider-config-reference-mismatch")
	}
	identities, versions := []string{}, []string{}
	appendObjectReceipt(&identities, &versions, "Deployment", deployment.ObjectSnapshot)
	profileMap, err := o.observeProfile(ctx, profile.Namespace, profile.ProfileConfigMap, profile.ProfileData())
	if err != nil {
		return ObservationReceipt{}, err
	}
	appendObjectReceipt(&identities, &versions, "ConfigMap", profileMap.ObjectSnapshot)
	if profile.Mode == ModeManaged {
		egress, egressErr := observeCall(ctx, o.callTimeout(), func(callContext context.Context) (ObjectSnapshot, error) {
			return o.Reader.NetworkPolicy(callContext, profile.Namespace, profile.EgressConfigRef)
		})
		if egressErr != nil {
			return ObservationReceipt{}, egressErr
		}
		if egress.Name != profile.EgressConfigRef || egress.Namespace != profile.Namespace || !uuidPattern.MatchString(egress.UID) || !resourceVersionPattern.MatchString(egress.ResourceVersion) {
			return ObservationReceipt{}, mismatch("external-dns-egress-reference-mismatch")
		}
		appendObjectReceipt(&identities, &versions, "NetworkPolicy", egress)
	}
	desiredDigest, _ := profile.Digest()
	return receipt("external-dns/"+profile.IntegrationID, desiredDigest, identities, versions), nil
}

func (o *KubernetesTargetObserver) observeProfile(ctx context.Context, namespace, name string, expected map[string]string) (ConfigMapSnapshot, error) {
	profile, err := observeCall(ctx, o.callTimeout(), func(callContext context.Context) (ConfigMapSnapshot, error) {
		return o.Reader.ConfigMap(callContext, namespace, name)
	})
	if err != nil {
		return ConfigMapSnapshot{}, err
	}
	expectedDigest := digestStringMap(expected)
	if profile.ObjectSnapshot.validate(name, namespace, expectedDigest) != nil || profile.BinaryData || !equalStringMap(profile.Data, expected) {
		return ConfigMapSnapshot{}, mismatch("profile-configmap-mismatch")
	}
	return profile, nil
}

func observeCall[T any](ctx context.Context, timeout time.Duration, call func(context.Context) (T, error)) (T, error) {
	callContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	value, err := call(callContext)
	if err != nil && callContext.Err() != nil {
		return value, callContext.Err()
	}
	return value, err
}

func appendObjectReceipt(identities, versions *[]string, kind string, object ObjectSnapshot) {
	*identities = append(*identities, kind+"\x00"+object.Namespace+"\x00"+object.Name+"\x00"+object.UID+"\x00"+object.SpecDigest)
	*versions = append(*versions, kind+"\x00"+object.Namespace+"\x00"+object.Name+"\x00"+object.ResourceVersion)
}

func receipt(key, desiredDigest string, identities, versions []string) ObservationReceipt {
	return ObservationReceipt{TargetKey: key, DesiredDigest: desiredDigest, IdentityDigest: digestStrings(identities), ResourceVersionDigest: digestStrings(versions)}
}

func digestStringMap(value map[string]string) string {
	entries := make([]string, 0, len(value))
	for key, item := range value {
		entries = append(entries, key+"\x00"+item)
	}
	return digestStrings(entries)
}

func equalStringMap(left, right map[string]string) bool {
	return len(left) == len(right) && slices.EqualFunc(sortedMapEntries(left), sortedMapEntries(right), func(left, right string) bool { return left == right })
}

func sortedMapEntries(value map[string]string) []string {
	entries := make([]string, 0, len(value))
	for key, item := range value {
		entries = append(entries, key+"\x00"+item)
	}
	slices.Sort(entries)
	return entries
}

func hasExactArgument(values []string, prefix, expected string) bool {
	count, exact := 0, false
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			count++
			exact = value == expected
		}
	}
	return count == 1 && exact
}

func hasExactArgumentSet(values []string, prefix string, expected []string) bool {
	actual := make([]string, 0, len(expected))
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			actual = append(actual, value)
		}
	}
	slices.Sort(actual)
	wanted := slices.Clone(expected)
	slices.Sort(wanted)
	return slices.Equal(actual, wanted)
}

var _ TargetObserver = (*KubernetesTargetObserver)(nil)
