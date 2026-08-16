package helmapps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	RendererChartPath   = "/input/chart.tgz"
	RendererValuesPath  = "/input/values.yaml"
	RendererKubeVersion = "1.36.3"
)

type SandboxContract struct {
	Image                    string
	RunAsUser                int64
	RunAsGroup               int64
	ReadOnlyRootFilesystem   bool
	InputMountPath           string
	InputReadOnly            bool
	TemporaryPath            string
	TemporaryBytes           int
	HomePath                 string
	HelmCacheHome            string
	HelmConfigHome           string
	HelmDataHome             string
	CredentialMounts         bool
	KubernetesAPIAccess      bool
	AllowPrivilegeEscalation bool
	DropAllCapabilities      bool
	SeccompProfile           string
	AutomountServiceAccount  bool
	NetworkMode              string
	TimeoutSeconds           int64
	InputBytes               int
	OutputBytes              int
	ResourceCount            int
}

func ExpectedSandboxContract() SandboxContract {
	return SandboxContract{
		Image: RendererImage, RunAsUser: 65532, RunAsGroup: 65532, ReadOnlyRootFilesystem: true,
		InputMountPath: "/input", InputReadOnly: true, TemporaryPath: "/tmp", TemporaryBytes: MaximumTemporarySize,
		HomePath: "/tmp/helm/home", HelmCacheHome: "/tmp/helm/cache", HelmConfigHome: "/tmp/helm/config",
		HelmDataHome: "/tmp/helm/data", CredentialMounts: false, KubernetesAPIAccess: false,
		AllowPrivilegeEscalation: false, DropAllCapabilities: true, SeccompProfile: "RuntimeDefault",
		AutomountServiceAccount: false, NetworkMode: "none", TimeoutSeconds: int64(RenderTimeout.Seconds()),
		InputBytes:  MaximumChartSize + MaximumValuesSize + MaximumSchemaSize + MaximumDescriptorSize,
		OutputBytes: MaximumOutputSize, ResourceCount: MaximumResources,
	}
}

func (s SandboxContract) Validate() error {
	if s != ExpectedSandboxContract() {
		return ErrInvalid
	}
	return nil
}

type RenderPlan struct {
	Approval       Approval
	Descriptor     Descriptor
	DescriptorYAML []byte
	InputDigest    string
	Values         ParsedValues
	Bundle         ChartBundle
	Sandbox        SandboxContract
	Arguments      []string
}

func NewRenderPlan(approval Approval, desired DesiredRender, bundle ChartBundle) (RenderPlan, error) {
	approvalDigest, approvalErr := approval.IdentityDigest()
	descriptorDigest, descriptorErr := desired.Descriptor.approvalIdentityDigest()
	if approvalErr != nil || descriptorErr != nil || desired.Validate() != nil || approvalDigest != descriptorDigest ||
		bundle.PackageDigest != approval.PackageDigest || bundle.SchemaDigest != approval.ValuesSchemaDigest ||
		bundle.ChartVersion != approval.ChartVersion || bundle.ChartName != approval.OCIRepository[strings.LastIndexByte(approval.OCIRepository, '/')+1:] {
		return RenderPlan{}, ErrInvalid
	}
	values, err := ParseValues(desired.ValuesYAML)
	defaults, defaultsErr := ParseValues(bundle.DefaultValuesYAML)
	if err != nil || defaultsErr != nil || validateMergedValuesSchema(bundle.ValuesSchemaJSON, defaults, values) != nil {
		return RenderPlan{}, ErrUnsafeYAML
	}
	arguments := []string{"template", desired.Descriptor.ReleaseName, RendererChartPath,
		"--namespace", desired.Descriptor.Destination.Namespace, "--values", RendererValuesPath,
		"--kube-version", RendererKubeVersion}
	plan := RenderPlan{Approval: approval, Descriptor: desired.Descriptor,
		DescriptorYAML: append([]byte(nil), desired.DescriptorYAML...), InputDigest: desired.InputDigest,
		Values: values, Bundle: bundle,
		Sandbox: ExpectedSandboxContract(), Arguments: arguments}
	plan = cloneRenderPlan(plan)
	if plan.Validate() != nil {
		return RenderPlan{}, ErrInvalid
	}
	return plan, nil
}

func (p RenderPlan) Validate() error {
	if p.Approval.Validate() != nil || p.Descriptor.Validate() != nil || p.Sandbox.Validate() != nil ||
		p.Descriptor.Approval != p.Approval.ApprovalKey || p.Bundle.PackageDigest != p.Approval.PackageDigest ||
		p.Bundle.SchemaDigest != p.Approval.ValuesSchemaDigest || len(p.Bundle.PackageBytes) == 0 ||
		len(p.Values.Raw) == 0 || len(p.DescriptorYAML) == 0 || !validDigest(p.InputDigest) {
		return ErrInvalid
	}
	expected := []string{"template", p.Descriptor.ReleaseName, RendererChartPath,
		"--namespace", p.Descriptor.Destination.Namespace, "--values", RendererValuesPath,
		"--kube-version", RendererKubeVersion}
	if len(p.Arguments) != len(expected) {
		return ErrInvalid
	}
	for index := range expected {
		if p.Arguments[index] != expected[index] {
			return ErrInvalid
		}
	}
	return nil
}

func cloneRenderPlan(plan RenderPlan) RenderPlan {
	plan.DescriptorYAML = append([]byte(nil), plan.DescriptorYAML...)
	plan.Values.Raw = append([]byte(nil), plan.Values.Raw...)
	plan.Values.Values = cloneJSONMap(plan.Values.Values)
	plan.Bundle.PackageBytes = append([]byte(nil), plan.Bundle.PackageBytes...)
	plan.Bundle.ValuesSchemaJSON = append([]byte(nil), plan.Bundle.ValuesSchemaJSON...)
	plan.Bundle.DefaultValuesYAML = append([]byte(nil), plan.Bundle.DefaultValuesYAML...)
	plan.Arguments = append([]string(nil), plan.Arguments...)
	return plan
}

// RenderInvocation makes each half of the deterministic double-render proof a
// separately adoptable execution. Without the pass number, a recovering
// Kubernetes adapter could accidentally read one terminal Job twice.
type RenderInvocation struct {
	CommandID string
	Attempt   int
	Pass      int
}

func (i RenderInvocation) Validate() error {
	if !uuidRE.MatchString(i.CommandID) || i.Attempt < 1 || i.Attempt > MaximumAttempts ||
		(i.Pass != 1 && i.Pass != 2) {
		return ErrInvalid
	}
	return nil
}

// RenderExecutor is the only process/Job seam. Implementations receive the
// fixed argument vector and immutable execution identity and must enforce
// SandboxContract; there is no field for credentials, kubeconfig, network,
// post-renderer, dependency update, --include-crds, --skip-schema-validation,
// or --pass-credentials.
type RenderExecutor interface {
	Render(context.Context, RenderPlan, RenderInvocation) ([]byte, error)
}

type ValidatedManifests struct {
	Raw             []byte
	ManifestDigest  string
	InventoryDigest string
	ResourceCount   int
}

var dnsSubdomainRE = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?)*$`)

var allowedNamespacedKinds = map[string]struct{}{
	"v1/ConfigMap": {}, "v1/Service": {}, "v1/ServiceAccount": {}, "v1/PersistentVolumeClaim": {},
	"apps/v1/Deployment": {}, "apps/v1/StatefulSet": {},
	"batch/v1/Job": {}, "batch/v1/CronJob": {},
	"networking.k8s.io/v1/Ingress": {}, "networking.k8s.io/v1/NetworkPolicy": {},
	"autoscaling/v2/HorizontalPodAutoscaler": {}, "policy/v1/PodDisruptionBudget": {},
}

func ValidateRenderedManifests(raw []byte, descriptor Descriptor) (ValidatedManifests, error) {
	if descriptor.Validate() != nil || len(raw) == 0 || len(raw) > MaximumOutputSize || bytes.IndexByte(raw, 0) >= 0 {
		return ValidatedManifests{}, ErrUnsafeChart
	}
	normalized := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	normalized = bytes.ReplaceAll(normalized, []byte("\r"), []byte("\n"))
	if !bytes.HasSuffix(normalized, []byte("\n")) {
		normalized = append(append([]byte(nil), normalized...), '\n')
	} else {
		normalized = append([]byte(nil), normalized...)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(normalized))
	inventory := make([]string, 0)
	seen := make(map[string]struct{})
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || len(document.Content) != 1 || document.Content[0] == nil || document.Content[0].Kind != yaml.MappingNode ||
			validateYAMLTree(document.Content[0], MaximumYAMLNodes, MaximumYAMLDepth) != nil {
			return ValidatedManifests{}, ErrUnsafeChart
		}
		var decoded any
		if err = document.Content[0].Decode(&decoded); err != nil {
			return ValidatedManifests{}, ErrUnsafeChart
		}
		encoded, encodeErr := json.Marshal(decoded)
		if encodeErr != nil {
			return ValidatedManifests{}, ErrUnsafeChart
		}
		var resource map[string]any
		jsonDecoder := json.NewDecoder(bytes.NewReader(encoded))
		jsonDecoder.UseNumber()
		if jsonDecoder.Decode(&resource) != nil || resource == nil {
			return ValidatedManifests{}, ErrUnsafeChart
		}
		identity, policyErr := validateNamespacedResource(resource, descriptor)
		if policyErr != nil {
			return ValidatedManifests{}, policyErr
		}
		if _, duplicate := seen[identity]; duplicate {
			return ValidatedManifests{}, ErrUnsafeChart
		}
		seen[identity] = struct{}{}
		inventory = append(inventory, identity)
		if len(inventory) > MaximumResources {
			return ValidatedManifests{}, ErrUnsafeChart
		}
	}
	if len(inventory) == 0 {
		return ValidatedManifests{}, ErrUnsafeChart
	}
	sort.Strings(inventory)
	inventoryDigest, err := digestJSON(struct {
		Contract  string   `json:"contract"`
		Resources []string `json:"resources"`
	}{"external-helm-inventory.v1", inventory})
	if err != nil {
		return ValidatedManifests{}, ErrUnsafeChart
	}
	return ValidatedManifests{Raw: normalized, ManifestDigest: digestBytes(normalized),
		InventoryDigest: inventoryDigest, ResourceCount: len(inventory)}, nil
}

func validateNamespacedResource(resource map[string]any, descriptor Descriptor) (string, error) {
	apiVersion, apiOK := resource["apiVersion"].(string)
	kind, kindOK := resource["kind"].(string)
	metadata, metadataOK := resource["metadata"].(map[string]any)
	if !apiOK || !kindOK || !metadataOK || apiVersion == "" || kind == "" {
		return "", ErrUnsafeChart
	}
	if _, allowed := allowedNamespacedKinds[apiVersion+"/"+kind]; !allowed {
		return "", ErrUnsafeChart
	}
	name, nameOK := metadata["name"].(string)
	if !nameOK || len(name) > 253 || !dnsSubdomainRE.MatchString(name) {
		return "", ErrUnsafeChart
	}
	if _, generated := metadata["generateName"]; generated {
		return "", ErrUnsafeChart
	}
	if ownerReferences, present := metadata["ownerReferences"]; present && nonEmptyCollection(ownerReferences) {
		return "", ErrUnsafeChart
	}
	if finalizers, present := metadata["finalizers"]; present && nonEmptyCollection(finalizers) {
		return "", ErrUnsafeChart
	}
	namespace := descriptor.Destination.Namespace
	if explicit, present := metadata["namespace"]; present {
		value, ok := explicit.(string)
		if !ok || value != descriptor.Destination.Namespace {
			return "", ErrUnsafeChart
		}
		namespace = value
	}
	labels, ok := metadata["labels"].(map[string]any)
	if !ok {
		return "", ErrUnsafeChart
	}
	for key, expected := range descriptor.RequiredLabels() {
		if value, exists := labels[key].(string); !exists || value != expected {
			return "", ErrUnsafeChart
		}
	}
	if annotations, present := metadata["annotations"]; present {
		values, ok := annotations.(map[string]any)
		if !ok {
			return "", ErrUnsafeChart
		}
		for key := range values {
			if key == "helm.sh/hook" || strings.HasPrefix(key, "helm.sh/hook-") {
				return "", ErrUnsafeChart
			}
		}
	}
	if _, hasStatus := resource["status"]; hasStatus {
		return "", ErrUnsafeChart
	}
	if kind == "Service" {
		if err := validateService(resource); err != nil {
			return "", err
		}
	}
	if kind == "ServiceAccount" {
		if value, present := resource["automountServiceAccountToken"]; !present || value != false {
			return "", ErrUnsafeChart
		}
	}
	if kind == "Deployment" || kind == "StatefulSet" || kind == "Job" || kind == "CronJob" {
		if err := validateWorkloadPodSpec(resource); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s", apiVersion, kind, namespace, name), nil
}

// External Helm applications are not given a Kubernetes Secret capability.
// A chart may still render ordinary workloads, but a secretKeyRef, secret
// volume, imagePullSecret, or projected Secret would let an application read
// any same-namespace Secret by guessing its name. Runtime secrets use the
// separate application-bound secret-binding contract instead.
func validateWorkloadPodSpec(resource map[string]any) error {
	spec, ok := resource["spec"].(map[string]any)
	if !ok {
		return ErrUnsafeChart
	}
	// Jobs expose the same pod template directly as Deployments, while
	// CronJobs nest it below spec.jobTemplate. Keep the secret/reference
	// boundary identical across every workload kind an external chart may
	// render.
	if jobTemplate, nested := spec["jobTemplate"]; nested {
		jobTemplateSpec, ok := jobTemplate.(map[string]any)
		if !ok {
			return ErrUnsafeChart
		}
		spec, ok = jobTemplateSpec["spec"].(map[string]any)
		if !ok {
			return ErrUnsafeChart
		}
	}
	template, ok := spec["template"].(map[string]any)
	if !ok {
		return ErrUnsafeChart
	}
	podSpec, ok := template["spec"].(map[string]any)
	if !ok {
		return ErrUnsafeChart
	}
	if _, present := podSpec["imagePullSecrets"]; present || podSpecVolumesContainSecret(podSpec["volumes"]) || podSpecContainersContainSecret(podSpec) {
		return ErrUnsafeChart
	}
	return nil
}

func podSpecContainersContainSecret(podSpec map[string]any) bool {
	for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
		containers, ok := podSpec[field].([]any)
		if !ok {
			continue
		}
		for _, rawContainer := range containers {
			container, ok := rawContainer.(map[string]any)
			if !ok {
				return true
			}
			if env, ok := container["env"].([]any); ok {
				for _, rawEntry := range env {
					entry, ok := rawEntry.(map[string]any)
					if !ok {
						return true
					}
					valueFrom, ok := entry["valueFrom"].(map[string]any)
					if _, present := valueFrom["secretKeyRef"]; ok && present {
						return true
					}
				}
			}
			if envFrom, ok := container["envFrom"].([]any); ok {
				for _, rawEntry := range envFrom {
					entry, ok := rawEntry.(map[string]any)
					if !ok {
						return true
					}
					if _, present := entry["secretRef"]; present {
						return true
					}
				}
			}
		}
	}
	return false
}

func podSpecVolumesContainSecret(rawVolumes any) bool {
	volumes, ok := rawVolumes.([]any)
	if !ok {
		return false
	}
	var containsSecret func(any) bool
	containsSecret = func(value any) bool {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				switch normalizeKey(key) {
				case "secret", "secretkeyref", "secretref", "secretname", "nodepublishsecretref":
					return true
				}
				if containsSecret(child) {
					return true
				}
			}
		case []any:
			for _, child := range typed {
				if containsSecret(child) {
					return true
				}
			}
		}
		return false
	}
	return containsSecret(volumes)
}

func validateService(resource map[string]any) error {
	spec, ok := resource["spec"].(map[string]any)
	if !ok {
		return ErrUnsafeChart
	}
	if serviceType, present := spec["type"]; present && serviceType != "ClusterIP" {
		return ErrUnsafeChart
	}
	for _, field := range []string{"externalName", "loadBalancerIP", "loadBalancerClass", "externalIPs"} {
		if value, present := spec[field]; present && nonEmptyCollection(value) {
			return ErrUnsafeChart
		}
	}
	if rawPorts, present := spec["ports"]; present {
		ports, ok := rawPorts.([]any)
		if !ok {
			return ErrUnsafeChart
		}
		for _, item := range ports {
			port, ok := item.(map[string]any)
			if !ok {
				return ErrUnsafeChart
			}
			if _, nodePort := port["nodePort"]; nodePort {
				return ErrUnsafeChart
			}
		}
	}
	return nil
}

func nonEmptyCollection(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return typed != ""
	case []any:
		return len(typed) != 0
	case map[string]any:
		return len(typed) != 0
	default:
		return true
	}
}
