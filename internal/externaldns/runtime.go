package externaldns

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/edge"
)

var ErrRuntimeUnavailable = errors.New("external-dns operational runtime is unavailable")

// ManagedRuntimeTemplate is the immutable operator-owned half of every
// dynamic managed integration. UI input cannot select an image, namespace,
// ServiceAccount, arbitrary argument, URL, or Kubernetes payload.
type ManagedRuntimeTemplate struct {
	Namespace, Version, Image, ServiceAccount string
}

func (t ManagedRuntimeTemplate) Validate() error {
	probe := edge.ExternalDNSProfile{IntegrationID: "00000000-0000-4000-8000-000000000000", Revision: 1, Mode: edge.ModeManaged,
		Namespace: t.Namespace, Version: t.Version, Deployment: edge.DeploymentExpectation{Name: "external-dns-probe", ContainerName: "external-dns", Image: t.Image, SpecDigest: strings.Repeat("a", 0)},
		ProfileConfigMap: "external-dns-probe-profile", LabelFilter: "x=y", ProviderKind: "cloudflare", CredentialSecretRef: "credential", ProviderConfigRef: "provider", EgressConfigRef: "egress", TXTOwnerID: "owner", Policy: "upsert-only", DomainFilters: []string{"example.com"}}
	probe.Deployment.SpecDigest = digest([]byte("probe"))
	if probe.Validate() != nil || !slugRE.MatchString(t.ServiceAccount) {
		return ErrRuntimeUnavailable
	}
	return nil
}

func ManagedProfile(item domain.ExternalDNSIntegration, t ManagedRuntimeTemplate) (edge.ExternalDNSProfile, error) {
	if t.Validate() != nil || Validate(item) != nil || item.Mode != ModeManaged || item.Lifecycle != "active" || item.RuntimeRevision < 1 {
		return edge.ExternalDNSProfile{}, ErrRuntimeUnavailable
	}
	name := "external-dns-" + item.Slug
	if len(name) > 63 {
		return edge.ExternalDNSProfile{}, ErrRuntimeUnavailable
	}
	args := managedArguments(item)
	contract, _ := json.Marshal(struct {
		Contract, Integration, Image string
		Revision                     int64
		Arguments                    []string
	}{"external-dns-managed-deployment.v1", item.ID, t.Image, item.RuntimeRevision, args})
	p := edge.ExternalDNSProfile{IntegrationID: item.ID, Revision: item.RuntimeRevision, Mode: edge.ModeManaged, Namespace: t.Namespace, Version: t.Version,
		Deployment: edge.DeploymentExpectation{Name: name, ContainerName: "external-dns", Image: t.Image, SpecDigest: digest(contract)}, ProfileConfigMap: name + "-profile",
		LabelFilter: "kuberploy.io/dns-integration=" + item.ID, AnnotationFilter: "", ProviderKind: item.ProviderKind, CredentialSecretRef: item.CredentialSecretRef,
		ProviderConfigRef: item.ProviderConfigRef, EgressConfigRef: item.EgressConfigRef, TXTOwnerID: item.TXTOwnerID, Policy: item.SyncPolicy, DomainFilters: append([]string(nil), item.AllowedDomainSuffixes...)}
	sort.Strings(p.DomainFilters)
	if p.Validate() != nil {
		return edge.ExternalDNSProfile{}, ErrRuntimeUnavailable
	}
	return p, nil
}

func managedArguments(item domain.ExternalDNSIntegration) []string {
	values := []string{"--source=ingress", "--provider=" + item.ProviderKind, "--registry=txt", "--policy=" + item.SyncPolicy, "--txt-owner-id=" + item.TXTOwnerID, "--label-filter=kuberploy.io/dns-integration=" + item.ID}
	for _, domain := range item.AllowedDomainSuffixes {
		values = append(values, "--domain-filter="+domain)
	}
	return values
}

func managedCredentialSources(item domain.ExternalDNSIntegration) map[string]any {
	providerConfig := map[string]any{"configMapRef": map[string]any{"name": item.ProviderConfigRef}}
	if item.ProviderKind == "cloudflare" {
		return map[string]any{
			"env":     []any{map[string]any{"name": "CF_API_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": item.CredentialSecretRef, "key": "apiToken", "optional": false}}}},
			"envFrom": []any{providerConfig},
		}
	}
	return map[string]any{"envFrom": []any{map[string]any{"secretRef": map[string]any{"name": item.CredentialSecretRef}}, providerConfig}}
}

// RenderManagedBundle emits a closed set of JSON documents (JSON is valid
// YAML). Credential values are never read; only exact Secret and ConfigMap
// references are materialized.
func RenderManagedBundle(item domain.ExternalDNSIntegration, t ManagedRuntimeTemplate) ([]byte, edge.ExternalDNSProfile, error) {
	p, err := ManagedProfile(item, t)
	if err != nil {
		return nil, edge.ExternalDNSProfile{}, err
	}
	labels := map[string]any{"app.kubernetes.io/name": "external-dns", "app.kubernetes.io/managed-by": "kuberploy", "app.kubernetes.io/version": p.Version, "kuberploy.io/dns-integration": item.ID}
	annotations := map[string]any{"kuberploy.io/edge-spec-digest": p.Deployment.SpecDigest, "kuberploy.io/provider-config-ref": item.ProviderConfigRef, "kuberploy.io/egress-config-ref": item.EgressConfigRef, "kuberploy.io/runtime-revision": fmt.Sprint(item.RuntimeRevision)}
	profileData := p.ProfileData()
	runtimeContainer := map[string]any{"name": "external-dns", "image": t.Image, "args": managedArguments(item), "securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}, "readOnlyRootFilesystem": true, "runAsGroup": int64(65532), "runAsNonRoot": true, "runAsUser": int64(65532)}, "resources": map[string]any{"requests": map[string]any{"cpu": "25m", "memory": "64Mi"}, "limits": map[string]any{"cpu": "500m", "memory": "256Mi"}}}
	for key, value := range managedCredentialSources(item) {
		runtimeContainer[key] = value
	}
	objects := []any{
		map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": t.ServiceAccount, "namespace": t.Namespace, "labels": labels}},
		map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": p.ProfileConfigMap, "namespace": t.Namespace, "labels": labels}, "data": profileData},
		map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": p.Deployment.Name, "namespace": t.Namespace, "labels": labels, "annotations": annotations}, "spec": map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": map[string]any{"kuberploy.io/dns-integration": item.ID}}, "template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": map[string]any{"serviceAccountName": t.ServiceAccount, "securityContext": map[string]any{"fsGroup": int64(65534), "runAsNonRoot": true, "seccompProfile": map[string]any{"type": "RuntimeDefault"}}, "containers": []any{runtimeContainer}}}}},
		map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": map[string]any{"name": p.Deployment.Name, "labels": labels}, "rules": []any{map[string]any{"apiGroups": []string{"networking.k8s.io"}, "resources": []string{"ingresses"}, "verbs": []string{"get", "list", "watch"}}}},
		map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding", "metadata": map[string]any{"name": p.Deployment.Name, "labels": labels}, "subjects": []any{map[string]any{"kind": "ServiceAccount", "name": t.ServiceAccount, "namespace": t.Namespace}}, "roleRef": map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": p.Deployment.Name}},
	}
	var out bytes.Buffer
	for index, object := range objects {
		if index > 0 {
			out.WriteString("\n---\n")
		}
		encoded, _ := json.MarshalIndent(object, "", "  ")
		out.Write(encoded)
		out.WriteByte('\n')
	}
	if out.Len() > 128<<10 {
		return nil, edge.ExternalDNSProfile{}, ErrRuntimeUnavailable
	}
	return out.Bytes(), p, nil
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}
