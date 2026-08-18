package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const DefaultDinDImage = "docker.io/library/docker:29.7.1-dind"

var (
	kubernetesNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$`)
	quantityPattern       = regexp.MustCompile(`^[1-9][0-9]*(?:m|Ki|Mi|Gi|Ti)?$`)
	lockedImagePattern    = regexp.MustCompile(`^(?:[^\s@]+:v?[0-9]+\.[0-9]+\.[0-9]+(?:[-.][A-Za-z0-9]+)*|[^\s@]+@sha256:[0-9a-f]{64})$`)
)

// ValidateExecutionImage is the common image grammar for mutable
// infrastructure sidecars. Version tags keep local/mirror workflows working;
// sha256 references provide an immutable option.
func ValidateExecutionImage(value string) error {
	if !lockedImagePattern.MatchString(value) {
		return errors.New("image must use an explicit semantic version or sha256 digest")
	}
	return nil
}

type JobPlanRequest struct {
	Build                         BuildRequest
	Namespace                     string
	PodServiceAccount             string
	RequestConfigMap              string
	SourceCredentialSecret        string
	RegistryPushCredentialSecret  string
	RegistryCacheCredentialSecret string
	BuildSecret                   string
	SSHSecret                     string
	CheckoutImage                 string
	AgentImage                    string
	// DinDImage is optional for backwards-compatible persisted definitions;
	// an empty value resolves to DefaultDinDImage when the Job is rendered.
	DinDImage               string
	NodeSelector            map[string]string
	Toleration              TaintToleration
	CheckoutResources       ContainerResources
	DinDResources           ContainerResources
	AgentResources          ContainerResources
	WorkspaceSizeLimit      string
	SocketSizeLimit         string
	ResultSizeLimit         string
	DockerDataSizeLimit     string
	ActiveDeadlineSeconds   int64
	TTLSecondsAfterFinished int64
	Egress                  []EgressEndpoint
}

type TaintToleration struct {
	Key    string
	Value  string
	Effect string
}

type ContainerResources struct {
	CPURequest              string
	MemoryRequest           string
	EphemeralStorageRequest string
	CPULimit                string
	MemoryLimit             string
	EphemeralStorageLimit   string
}

type EgressEndpoint struct {
	CIDR   string
	Ports  []int
	Except []string `json:",omitempty"`
}

type JobPlan struct {
	Job           map[string]any
	NetworkPolicy map[string]any
}

func PlanJob(request JobPlanRequest) (JobPlan, error) {
	if err := request.Validate(); err != nil {
		return JobPlan{}, err
	}
	name := deterministicJobName(request.Build.OperationID, request.Build.Generation)
	specHash, err := jobSpecHash(request)
	if err != nil {
		return JobPlan{}, err
	}
	labels := map[string]any{
		"app.kubernetes.io/name":        "kuberploy-builder",
		"app.kubernetes.io/component":   "source-build",
		"app.kubernetes.io/managed-by":  "kuberploy",
		"kuberploy.io/build-operation":  operationLabel(request.Build.OperationID),
		"kuberploy.io/build-generation": strconv.FormatInt(request.Build.Generation, 10),
		"kuberploy.io/build-spec-hash":  specHash,
	}
	volumes := []any{
		map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": request.WorkspaceSizeLimit}},
		map[string]any{"name": "docker-socket", "emptyDir": map[string]any{"sizeLimit": request.SocketSizeLimit}},
		map[string]any{"name": "result", "emptyDir": map[string]any{"sizeLimit": request.ResultSizeLimit}},
		map[string]any{"name": "docker-data", "emptyDir": map[string]any{"sizeLimit": request.DockerDataSizeLimit}},
		map[string]any{"name": "request", "configMap": map[string]any{"name": request.RequestConfigMap, "defaultMode": 0440}},
		registryCredentialVolume("registry-push-credentials", request.RegistryPushCredentialSecret),
		registryCredentialVolume("registry-cache-credentials", request.RegistryCacheCredentialSecret),
	}
	checkoutMounts := []any{
		volumeMount("workspace", DefaultCheckoutRoot, false),
		volumeMount("request", "/request", true),
		volumeMount("result", "/result", false),
	}
	agentMounts := []any{
		volumeMount("workspace", DefaultCheckoutRoot, true),
		volumeMount("docker-socket", "/run/kuberploy/docker", false),
		volumeMount("request", "/request", true),
		volumeMount("result", "/result", false),
		volumeMount("registry-push-credentials", RegistryPushSecretRoot, true),
		volumeMount("registry-cache-credentials", RegistryCacheSecretRoot, true),
	}
	if request.SourceCredentialSecret != "" {
		volumes = append(volumes, map[string]any{"name": "source-credentials", "secret": map[string]any{"secretName": request.SourceCredentialSecret, "defaultMode": 0440}})
		checkoutMounts = append(checkoutMounts, volumeMount("source-credentials", SourceCredentialRoot, true))
	}
	if request.BuildSecret != "" {
		volumes = append(volumes, map[string]any{"name": "build-secrets", "secret": map[string]any{"secretName": request.BuildSecret, "defaultMode": 0440}})
		agentMounts = append(agentMounts, volumeMount("build-secrets", BuildSecretRoot, true))
	}
	if request.SSHSecret != "" {
		volumes = append(volumes, map[string]any{"name": "ssh-secrets", "secret": map[string]any{"secretName": request.SSHSecret, "defaultMode": 0440}})
		agentMounts = append(agentMounts, volumeMount("ssh-secrets", SSHSecretRoot, true))
	}

	job := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      name,
			"namespace": request.Namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"backoffLimit":            int64(0),
			"parallelism":             int64(1),
			"completions":             int64(1),
			"suspend":                 false,
			"completionMode":          "NonIndexed",
			"manualSelector":          false,
			"podReplacementPolicy":    "Failed",
			"activeDeadlineSeconds":   request.ActiveDeadlineSeconds,
			"ttlSecondsAfterFinished": request.TTLSecondsAfterFinished,
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"serviceAccountName":           request.PodServiceAccount,
					"automountServiceAccountToken": false,
					"restartPolicy":                "Never",
					"enableServiceLinks":           false,
					"securityContext": map[string]any{
						"runAsUser": int64(65532), "runAsGroup": int64(65532), "fsGroup": int64(65532), "fsGroupChangePolicy": "OnRootMismatch",
					},
					"nodeSelector": stringMapAny(request.NodeSelector),
					"tolerations": []any{map[string]any{
						"key": request.Toleration.Key, "operator": "Equal", "value": request.Toleration.Value, "effect": request.Toleration.Effect,
					}},
					"volumes": volumes,
					"initContainers": []any{
						map[string]any{
							"name":            "checkout",
							"image":           request.CheckoutImage,
							"imagePullPolicy": "IfNotPresent",
							"args":            []any{"checkout", "--request", "/request/checkout.json", "--result", DefaultCheckoutResult},
							"securityContext": restrictedSecurityContext(65532, true),
							"resources":       resources(request.CheckoutResources),
							"volumeMounts":    checkoutMounts,
						},
						map[string]any{
							"name":            "dind",
							"image":           effectiveDinDImage(request.DinDImage),
							"imagePullPolicy": "IfNotPresent",
							"restartPolicy":   "Always",
							"command":         []any{"/usr/local/bin/docker-init", "--", "/usr/local/bin/dockerd"},
							"args":            []any{"--host=" + DefaultDockerSocket, "--group=65532", "--tls=false", "--feature=cdi=false"},
							"env":             []any{map[string]any{"name": "DOCKER_TLS_CERTDIR"}},
							"securityContext": map[string]any{"privileged": true, "runAsUser": int64(0), "runAsNonRoot": false},
							"resources":       resources(request.DinDResources),
							"volumeMounts": []any{
								volumeMount("docker-socket", "/run/kuberploy/docker", false),
								volumeMount("docker-data", "/var/lib/docker", false),
							},
							"startupProbe": map[string]any{
								"exec":          map[string]any{"command": []any{"docker", "--host", DefaultDockerSocket, "info"}},
								"periodSeconds": int64(2), "failureThreshold": int64(60), "timeoutSeconds": int64(1),
							},
						},
					},
					"containers": []any{map[string]any{
						"name":                     "agent",
						"image":                    request.AgentImage,
						"imagePullPolicy":          "IfNotPresent",
						"args":                     []any{"build", "--request", "/request/build.json", "--result", DefaultBuildResult},
						"terminationMessagePath":   DefaultBuildResult,
						"terminationMessagePolicy": "File",
						"securityContext":          restrictedSecurityContext(65532, true),
						"resources":                resources(request.AgentResources),
						"volumeMounts":             agentMounts,
					}},
				},
			},
		},
	}
	policy := plannedNetworkPolicy(request, name, labels)
	return JobPlan{Job: job, NetworkPolicy: policy}, nil
}

func registryCredentialVolume(name, secretName string) map[string]any {
	return map[string]any{"name": name, "secret": map[string]any{
		"secretName":  secretName,
		"defaultMode": 0440,
		"items": []any{
			map[string]any{"key": "username", "path": "username"},
			map[string]any{"key": "password", "path": "password"},
		},
	}}
}

func (r JobPlanRequest) Validate() error {
	if err := r.Build.Validate(); err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	for field, value := range map[string]string{
		"namespace": r.Namespace, "pod service account": r.PodServiceAccount, "request ConfigMap": r.RequestConfigMap,
		"registry push credential Secret":  r.RegistryPushCredentialSecret,
		"registry cache credential Secret": r.RegistryCacheCredentialSecret,
	} {
		if !kubernetesNamePattern.MatchString(value) {
			return fmt.Errorf("%s is not a valid Kubernetes name", field)
		}
	}
	if r.RegistryPushCredentialSecret == r.RegistryCacheCredentialSecret {
		return errors.New("registry push and cache credential Secrets must be distinct")
	}
	for field, value := range map[string]string{"source credential Secret": r.SourceCredentialSecret, "build Secret": r.BuildSecret, "SSH Secret": r.SSHSecret} {
		if value != "" && !kubernetesNamePattern.MatchString(value) {
			return fmt.Errorf("%s is not a valid Kubernetes name", field)
		}
	}
	if (len(r.Build.SecretFiles) > 0) != (r.BuildSecret != "") || (len(r.Build.SSHFiles) > 0) != (r.SSHSecret != "") {
		return errors.New("build and SSH secret mounts must exactly match their request references")
	}
	if !lockedImagePattern.MatchString(r.CheckoutImage) || !lockedImagePattern.MatchString(r.AgentImage) {
		return errors.New("checkout and agent images must use explicit versions or release integrity references")
	}
	if r.DinDImage != "" && !lockedImagePattern.MatchString(r.DinDImage) {
		return errors.New("DinD image must use an explicit version or release integrity reference")
	}
	if r.CheckoutImage != r.AgentImage {
		return errors.New("checkout and agent must use the same trusted builder-agent digest")
	}
	if len(r.NodeSelector) == 0 || r.NodeSelector["kuberploy.io/node-class"] != "dind-builder" {
		return errors.New("nodeSelector must target the dedicated dind-builder node class")
	}
	if r.Toleration.Key != "kuberploy.io/dind-builder" || r.Toleration.Value != "true" || r.Toleration.Effect != "NoSchedule" {
		return errors.New("the exact dedicated dind-builder taint toleration is required")
	}
	for name, value := range map[string]ContainerResources{"checkout": r.CheckoutResources, "dind": r.DinDResources, "agent": r.AgentResources} {
		if err := value.validate(); err != nil {
			return fmt.Errorf("%s resources: %w", name, err)
		}
	}
	for name, quantity := range map[string]string{"workspace": r.WorkspaceSizeLimit, "socket": r.SocketSizeLimit, "result": r.ResultSizeLimit, "docker data": r.DockerDataSizeLimit} {
		if !quantityPattern.MatchString(quantity) {
			return fmt.Errorf("%s emptyDir size limit is invalid", name)
		}
	}
	if r.ActiveDeadlineSeconds < r.Build.Profile.TimeoutSeconds+660 || r.ActiveDeadlineSeconds > 9000 {
		return errors.New("active deadline must cover checkout, build timeout, and shutdown overhead and remain bounded")
	}
	if r.TTLSecondsAfterFinished < 60 || r.TTLSecondsAfterFinished > 86400 {
		return errors.New("TTL after finish must be between 60 and 86400")
	}
	if len(r.Egress) < 1 || len(r.Egress) > 128 {
		return errors.New("egress must contain between 1 and 128 resolved endpoints")
	}
	for index, endpoint := range r.Egress {
		if index > 0 && r.Egress[index-1].CIDR >= endpoint.CIDR {
			return errors.New("egress endpoints must use unique canonical CIDR order")
		}
		_, network, err := net.ParseCIDR(endpoint.CIDR)
		prefix, bits := 0, 0
		if network != nil {
			prefix, bits = network.Mask.Size()
		}
		isDefault := endpoint.CIDR == "0.0.0.0/0" || endpoint.CIDR == "::/0"
		if err != nil || network.String() != endpoint.CIDR || !(isDefault || (bits == 32 && prefix >= 8 && prefix <= 32) || (bits == 128 && prefix >= 16 && prefix <= 128)) {
			return fmt.Errorf("egress CIDR %q must be one canonical bounded provider range", endpoint.CIDR)
		}
		// Empty operator API CIDRs intentionally mean an open public provider
		// route. When API CIDRs are known, runtime config adds same-family
		// exclusions; both shapes are valid because the operation-scoped policy
		// still limits destination ports and exact build identity.
		if !isDefault && len(endpoint.Except) != 0 {
			return errors.New("strict provider egress must not carry default-route exclusions")
		}
		if len(endpoint.Except) > 32 {
			return errors.New("egress exclusions may contain at most 32 CIDRs")
		}
		for exceptIndex, value := range endpoint.Except {
			_, excluded, parseErr := net.ParseCIDR(value)
			if parseErr != nil || excluded.String() != value || !network.Contains(excluded.IP) {
				return errors.New("egress exclusions must be canonical contained CIDRs")
			}
			if exceptIndex > 0 && endpoint.Except[exceptIndex-1] >= value {
				return errors.New("egress exclusions must use unique canonical order")
			}
		}
		if len(endpoint.Ports) < 1 || len(endpoint.Ports) > 16 {
			return errors.New("each egress endpoint must have 1 to 16 ports")
		}
		seen := map[int]struct{}{}
		for portIndex, port := range endpoint.Ports {
			if port < 1 || port > 65535 {
				return errors.New("egress port is out of range")
			}
			if _, duplicate := seen[port]; duplicate {
				return errors.New("duplicate egress port")
			}
			seen[port] = struct{}{}
			if portIndex > 0 && endpoint.Ports[portIndex-1] >= port {
				return errors.New("egress ports must use unique canonical numeric order")
			}
		}
	}
	return nil
}

func effectiveDinDImage(value string) string {
	if value == "" {
		return DefaultDinDImage
	}
	return value
}

func (r ContainerResources) validate() error {
	for name, value := range map[string]string{
		"cpu request": r.CPURequest, "memory request": r.MemoryRequest, "ephemeral-storage request": r.EphemeralStorageRequest,
		"cpu limit": r.CPULimit, "memory limit": r.MemoryLimit, "ephemeral-storage limit": r.EphemeralStorageLimit,
	} {
		if !quantityPattern.MatchString(value) {
			return fmt.Errorf("%s quantity is invalid", name)
		}
	}
	return nil
}

func resources(value ContainerResources) map[string]any {
	return map[string]any{
		"requests": map[string]any{"cpu": value.CPURequest, "memory": value.MemoryRequest, "ephemeral-storage": value.EphemeralStorageRequest},
		"limits":   map[string]any{"cpu": value.CPULimit, "memory": value.MemoryLimit, "ephemeral-storage": value.EphemeralStorageLimit},
	}
}

func restrictedSecurityContext(uid int64, readOnlyRoot bool) map[string]any {
	return map[string]any{
		"allowPrivilegeEscalation": false,
		"privileged":               false,
		"readOnlyRootFilesystem":   readOnlyRoot,
		"runAsNonRoot":             true,
		"runAsUser":                uid,
		"runAsGroup":               int64(65532),
		"capabilities":             map[string]any{"drop": []any{"ALL"}},
		"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
	}
}

func jobSpecHash(request JobPlanRequest) (string, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", errors.New("encode deterministic Job plan input")
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])[:16], nil
}

// CanAdoptJob prevents a recovering controller from treating a same-name Job
// with different inputs as its own. The controller must use the same labels on
// the request ConfigMap, credential Secrets, and NetworkPolicy before deleting
// those auxiliary objects after result collection.
func CanAdoptJob(job map[string]any, request JobPlanRequest) bool {
	expected, err := PlanJob(request)
	if err != nil {
		return false
	}
	metadata, ok := job["metadata"].(map[string]any)
	if !ok || metadata["name"] != deterministicJobName(request.Build.OperationID, request.Build.Generation) || metadata["namespace"] != request.Namespace {
		return false
	}
	labels, ok := metadata["labels"].(map[string]any)
	if !ok {
		return false
	}
	hash, err := jobSpecHash(request)
	return err == nil && labels["kuberploy.io/build-operation"] == operationLabel(request.Build.OperationID) && labels["kuberploy.io/build-generation"] == strconv.FormatInt(request.Build.Generation, 10) && labels["kuberploy.io/build-spec-hash"] == hash && reflect.DeepEqual(job["spec"], expected.Job["spec"])
}

func CanAdoptNetworkPolicy(policy map[string]any, request JobPlanRequest) bool {
	expected, err := PlanJob(request)
	if err != nil {
		return false
	}
	metadata, ok := policy["metadata"].(map[string]any)
	if !ok || metadata["name"] != deterministicJobName(request.Build.OperationID, request.Build.Generation) || metadata["namespace"] != request.Namespace {
		return false
	}
	labels, ok := metadata["labels"].(map[string]any)
	if !ok {
		return false
	}
	hash, err := jobSpecHash(request)
	return err == nil && labels["kuberploy.io/build-operation"] == operationLabel(request.Build.OperationID) && labels["kuberploy.io/build-generation"] == strconv.FormatInt(request.Build.Generation, 10) && labels["kuberploy.io/build-spec-hash"] == hash && reflect.DeepEqual(policy["spec"], expected.NetworkPolicy["spec"])
}

func volumeMount(name, path string, readOnly bool) map[string]any {
	mount := map[string]any{"name": name, "mountPath": path}
	if readOnly {
		mount["readOnly"] = true
	}
	return mount
}

func stringMapAny(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func stringSliceAny(values []string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}

func plannedNetworkPolicy(request JobPlanRequest, name string, labels map[string]any) map[string]any {
	egress := []any{
		map[string]any{
			"to": []any{map[string]any{
				"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": "kube-system"}},
				"podSelector":       map[string]any{"matchLabels": map[string]any{"k8s-app": "kube-dns"}},
			}},
			"ports": []any{map[string]any{"protocol": "UDP", "port": int64(53)}, map[string]any{"protocol": "TCP", "port": int64(53)}},
		},
	}
	for _, endpoint := range request.Egress {
		ports := slices.Clone(endpoint.Ports)
		slices.Sort(ports)
		policyPorts := make([]any, 0, len(ports))
		for _, port := range ports {
			policyPorts = append(policyPorts, map[string]any{"protocol": "TCP", "port": int64(port)})
		}
		ipBlock := map[string]any{"cidr": endpoint.CIDR}
		if len(endpoint.Except) > 0 {
			// Keep JSON arrays in the same representation returned by the
			// Kubernetes API so exact adoption survives a round trip.
			ipBlock["except"] = stringSliceAny(endpoint.Except)
		}
		egress = append(egress, map[string]any{
			"to":    []any{map[string]any{"ipBlock": ipBlock}},
			"ports": policyPorts,
		})
	}
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata":   map[string]any{"name": name, "namespace": request.Namespace, "labels": labels},
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]any{
				"kuberploy.io/build-operation":  operationLabel(request.Build.OperationID),
				"kuberploy.io/build-generation": strconv.FormatInt(request.Build.Generation, 10),
			}},
			"policyTypes": []any{"Ingress", "Egress"},
			"egress":      egress,
		},
	}
}

func deterministicJobName(operationID string, generation int64) string {
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", operationID, generation)))
	return "kuberploy-build-" + hex.EncodeToString(hash[:])[:24]
}

func operationLabel(operationID string) string {
	return strings.ReplaceAll(operationID, "-", "")
}
