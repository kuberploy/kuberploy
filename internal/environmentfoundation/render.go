package environmentfoundation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"go.yaml.in/yaml/v3"
)

type manifest struct {
	APIVersion string       `yaml:"apiVersion"`
	Kind       string       `yaml:"kind"`
	Metadata   metadata     `yaml:"metadata"`
	Spec       any          `yaml:"spec,omitempty"`
	Rules      []policyRule `yaml:"rules,omitempty"`
	RoleRef    *roleRef     `yaml:"roleRef,omitempty"`
	Subjects   []subject    `yaml:"subjects,omitempty"`
}
type metadata struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace,omitempty"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}
type namespaceSpec struct{}
type quotaSpec struct {
	Hard map[string]string `yaml:"hard"`
}
type limitRangeSpec struct {
	Limits []limitItem `yaml:"limits"`
}
type limitItem struct {
	Type           string            `yaml:"type"`
	Default        map[string]string `yaml:"default"`
	DefaultRequest map[string]string `yaml:"defaultRequest"`
	Min            map[string]string `yaml:"min"`
	Max            map[string]string `yaml:"max"`
}
type networkPolicySpec struct {
	PodSelector map[string]any `yaml:"podSelector"`
	PolicyTypes []string       `yaml:"policyTypes"`
	Ingress     []networkRule  `yaml:"ingress,omitempty"`
	Egress      []networkRule  `yaml:"egress,omitempty"`
}
type networkRule struct {
	From  []peer        `yaml:"from,omitempty"`
	To    []peer        `yaml:"to,omitempty"`
	Ports []networkPort `yaml:"ports,omitempty"`
}
type peer struct {
	NamespaceSelector *selector `yaml:"namespaceSelector,omitempty"`
	PodSelector       *selector `yaml:"podSelector,omitempty"`
}
type selector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}
type networkPort struct {
	Protocol string `yaml:"protocol"`
	Port     int    `yaml:"port"`
}
type policyRule struct {
	APIGroups []string `yaml:"apiGroups"`
	Resources []string `yaml:"resources"`
	Verbs     []string `yaml:"verbs"`
}
type roleRef struct {
	APIGroup string `yaml:"apiGroup"`
	Kind     string `yaml:"kind"`
	Name     string `yaml:"name"`
}
type subject struct {
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

func Render(identity EnvironmentIdentity, profile Profile) ([]byte, string, error) {
	if identity.Validate() != nil || profile.Validate() != nil {
		return nil, "", ErrInvalid
	}
	labels := map[string]string{"app.kubernetes.io/managed-by": "kuberploy", "kuberploy.io/environment-id": identity.EnvironmentID,
		"kuberploy.io/project-id": identity.ProjectID, "kuberploy.io/foundation-contract": "v1"}
	nsLabels := cloneLabels(labels)
	nsLabels["kuberploy.io/runtime-namespace"] = "true"
	nsLabels["pod-security.kubernetes.io/enforce"] = "restricted"
	nsLabels["pod-security.kubernetes.io/enforce-version"] = profile.PSAVersion
	nsLabels["pod-security.kubernetes.io/audit"] = "restricted"
	nsLabels["pod-security.kubernetes.io/audit-version"] = profile.PSAVersion
	nsLabels["pod-security.kubernetes.io/warn"] = "restricted"
	nsLabels["pod-security.kubernetes.io/warn-version"] = profile.PSAVersion
	resources := []manifest{
		{APIVersion: "v1", Kind: "Namespace", Metadata: metadata{Name: identity.Namespace, Labels: nsLabels, Annotations: map[string]string{"argocd.argoproj.io/sync-wave": "-30"}}},
		{APIVersion: "v1", Kind: "ResourceQuota", Metadata: namespaced("kuberploy-foundation", identity.Namespace, labels), Spec: quotaSpec{Hard: map[string]string{
			"pods": fmt.Sprint(profile.Quota.Pods), "services": fmt.Sprint(profile.Quota.Services), "configmaps": fmt.Sprint(profile.Quota.ConfigMaps),
			"secrets": fmt.Sprint(profile.Quota.Secrets), "persistentvolumeclaims": fmt.Sprint(profile.Quota.PersistentVolumeClaims),
			"requests.cpu": fmt.Sprintf("%dm", profile.Quota.RequestCPUMilli), "limits.cpu": fmt.Sprintf("%dm", profile.Quota.LimitCPUMilli),
			"requests.memory": fmt.Sprintf("%dMi", profile.Quota.RequestMemoryMiB), "limits.memory": fmt.Sprintf("%dMi", profile.Quota.LimitMemoryMiB),
			"requests.storage": fmt.Sprintf("%dGi", profile.Quota.RequestStorageGiB)}}},
		{APIVersion: "v1", Kind: "LimitRange", Metadata: namespaced("kuberploy-container-defaults", identity.Namespace, labels), Spec: limitRangeSpec{Limits: []limitItem{{Type: "Container",
			Default:        map[string]string{"cpu": fmt.Sprintf("%dm", profile.Limits.DefaultLimitCPUMilli), "memory": fmt.Sprintf("%dMi", profile.Limits.DefaultLimitMemoryMiB)},
			DefaultRequest: map[string]string{"cpu": fmt.Sprintf("%dm", profile.Limits.DefaultRequestCPUMilli), "memory": fmt.Sprintf("%dMi", profile.Limits.DefaultRequestMemoryMiB)},
			Min:            map[string]string{"cpu": fmt.Sprintf("%dm", profile.Limits.MinimumCPUMilli), "memory": fmt.Sprintf("%dMi", profile.Limits.MinimumMemoryMiB)},
			Max:            map[string]string{"cpu": fmt.Sprintf("%dm", profile.Limits.MaximumCPUMilli), "memory": fmt.Sprintf("%dMi", profile.Limits.MaximumMemoryMiB)}}}}},
		{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy", Metadata: namespaced("kuberploy-default-deny", identity.Namespace, labels), Spec: networkPolicySpec{PodSelector: map[string]any{}, PolicyTypes: []string{"Ingress", "Egress"}}},
		{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy", Metadata: namespaced("kuberploy-dns-egress", identity.Namespace, labels), Spec: networkPolicySpec{PodSelector: map[string]any{}, PolicyTypes: []string{"Egress"}, Egress: []networkRule{{To: []peer{{NamespaceSelector: &selector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}}, PodSelector: &selector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}}}}, Ports: []networkPort{{"UDP", 53}, {"TCP", 53}}}}}},
		{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role", Metadata: namespaced("kuberploy-runtime-observer", identity.Namespace, labels), Rules: []policyRule{
			{APIGroups: []string{""}, Resources: []string{"endpoints", "events", "pods", "services"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
			{APIGroups: []string{"apps"}, Resources: []string{"daemonsets", "deployments", "replicasets", "statefulsets"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"batch"}, Resources: []string{"cronjobs", "jobs"}, Verbs: []string{"get", "list", "watch"}},
		}},
		{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding", Metadata: namespaced("kuberploy-runtime-observer", identity.Namespace, labels),
			RoleRef:  &roleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "kuberploy-runtime-observer"},
			Subjects: []subject{{Kind: "ServiceAccount", Name: profile.ObserverServiceAccount, Namespace: profile.ControlPlaneNamespace}}},
	}
	if len(resources) != FoundationResourceCount {
		return nil, "", ErrInvalid
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	for _, resource := range resources {
		if err := encoder.Encode(resource); err != nil {
			return nil, "", ErrInvalid
		}
	}
	if err := encoder.Close(); err != nil {
		return nil, "", ErrInvalid
	}
	if out.Len() > MaximumManifestBytes {
		return nil, "", ErrInvalid
	}
	return out.Bytes(), digest(out.Bytes()), nil
}

func namespaced(name, namespace string, labels map[string]string) metadata {
	return metadata{Name: name, Namespace: namespace, Labels: cloneLabels(labels), Annotations: map[string]string{"argocd.argoproj.io/sync-wave": "-20"}}
}
func cloneLabels(value map[string]string) map[string]string {
	result := make(map[string]string, len(value))
	for k, v := range value {
		result[k] = v
	}
	return result
}

func buildIntent(id string, identity EnvironmentIdentity, authority GitAuthority, profile Profile, now time.Time) (Intent, error) {
	profileDigest, err := profile.Digest()
	if err != nil {
		return Intent{}, err
	}
	content, contentDigest, err := Render(identity, profile)
	if err != nil {
		return Intent{}, err
	}
	path := ManifestPath(authority.ClusterID, identity.EnvironmentID)
	canonical, err := json.Marshal(struct {
		Contract, ID, EnvironmentID, ProjectID, Namespace, ArgoProject, BindingID, ClusterID, TargetRef, PlannedHead, ProfileDigest, PublisherConfigDigest, Path, ManifestDigest string
		Generation                                                                                                                                                               int64
	}{
		Contract: Contract, ID: id, EnvironmentID: identity.EnvironmentID, ProjectID: identity.ProjectID, Namespace: identity.Namespace, ArgoProject: identity.ArgoProject,
		BindingID: authority.BindingID, ClusterID: authority.ClusterID, TargetRef: authority.TargetRef, PlannedHead: authority.PlannedHead, Generation: authority.Generation,
		ProfileDigest: profileDigest, PublisherConfigDigest: profile.PublisherConfigDigest, Path: path, ManifestDigest: contentDigest})
	if err != nil {
		return Intent{}, ErrInvalid
	}
	value := Intent{ID: id, EnvironmentIdentity: identity, Authority: authority, ProfileDigest: profileDigest, PublisherConfigDigest: profile.PublisherConfigDigest,
		PublisherContractVersion: PublisherContract, PublisherPolicy: ProtectedGitPolicy,
		Path: path, Manifest: content, ManifestDigest: contentDigest, IntentDigest: digest(canonical), CommitTrailer: "Kuberploy-Environment-Foundation-Intent: " + id,
		State: StatePending, Active: true, NextAttemptAt: now.UTC(), CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	if value.Validate() != nil {
		return Intent{}, ErrInvalid
	}
	return value, nil
}
