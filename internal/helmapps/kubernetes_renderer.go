package helmapps

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	RendererJobContract        = "external-helm-kubernetes-job.v1"
	RendererInputChunkBytes    = 512 << 10
	MaximumRendererInputMaps   = 17
	RendererContainerName      = "renderer"
	RendererStageContainer     = "stage-input"
	RendererInputVolume        = "input"
	RendererChunksVolume       = "input-chunks"
	RendererTemporaryVolume    = "temporary"
	RendererInputSizeLimit     = "9Mi"
	RendererTemporaryLimit     = "16Mi"
	RendererJobTTLSeconds      = int64(300)
	RendererCPURequest         = "100m"
	RendererMemoryRequest      = "128Mi"
	RendererEphemeralRequest   = "16Mi"
	RendererCPULimit           = "1"
	RendererMemoryLimit        = "512Mi"
	RendererEphemeralLimit     = "32Mi"
	RendererStageCPURequest    = "50m"
	RendererStageMemoryRequest = "100Mi"
)

var (
	ErrRendererObjectNotFound = errors.New("Helm renderer Kubernetes object not found")
	ErrRendererObjectConflict = errors.New("Helm renderer Kubernetes object already exists")
)

type KubernetesRendererConfig struct {
	Namespace      string
	ServiceAccount string
	PollInterval   time.Duration
}

func (c KubernetesRendererConfig) Validate() error {
	if !dnsLabelRE.MatchString(c.Namespace) || !dnsLabelRE.MatchString(c.ServiceAccount) ||
		c.PollInterval < 10*time.Millisecond || c.PollInterval > 2*time.Second {
		return ErrInvalid
	}
	return nil
}

// KubernetesRenderWorkload contains only ConfigMaps with non-secret immutable
// chart/value bytes, one deny-all NetworkPolicy, and one sandboxed Job. Chart
// registry authentication has already ended before this boundary.
type KubernetesRenderWorkload struct {
	Namespace     string
	Name          string
	InputDigest   string
	SpecDigest    string
	ConfigMaps    []map[string]any
	NetworkPolicy map[string]any
	Job           map[string]any
}

func PlanKubernetesRender(plan RenderPlan, invocation RenderInvocation, config KubernetesRendererConfig) (KubernetesRenderWorkload, error) {
	workload, err := PlanKubernetesRenderWithoutAdoption(plan, invocation, config)
	if err != nil {
		return KubernetesRenderWorkload{}, err
	}
	if !CanAdoptKubernetesRenderWorkload(workload, plan, invocation, config) {
		return KubernetesRenderWorkload{}, ErrInvalid
	}
	return workload, nil
}

func rendererInputConfigMaps(plan RenderPlan, namespace, jobName string, labels map[string]any) ([]map[string]any, []any, error) {
	chart := plan.Bundle.PackageBytes
	chunkCount := (len(chart) + RendererInputChunkBytes - 1) / RendererInputChunkBytes
	if chunkCount < 1 || chunkCount+1 > MaximumRendererInputMaps {
		return nil, nil, ErrInvalid
	}
	maps := make([]map[string]any, 0, chunkCount+1)
	projections := make([]any, 0, chunkCount+1)
	for index := 0; index < chunkCount; index++ {
		start := index * RendererInputChunkBytes
		end := start + RendererInputChunkBytes
		if end > len(chart) {
			end = len(chart)
		}
		name := fmt.Sprintf("%s-chart-%02d", jobName, index)
		path := fmt.Sprintf("chart-%02d", index)
		maps = append(maps, rendererInputConfigMap(namespace, name, labels,
			map[string]any{"chunk": base64.StdEncoding.EncodeToString(chart[start:end])}))
		projections = append(projections, map[string]any{"configMap": map[string]any{
			"name": name, "items": []any{map[string]any{"key": "chunk", "path": path}},
		}})
	}
	valuesName := jobName + "-values"
	maps = append(maps, rendererInputConfigMap(namespace, valuesName, labels,
		map[string]any{"values.yaml": base64.StdEncoding.EncodeToString(plan.Values.Raw)}))
	projections = append(projections, map[string]any{"configMap": map[string]any{
		"name": valuesName, "items": []any{map[string]any{"key": "values.yaml", "path": "values.yaml"}},
	}})
	return maps, projections, nil
}

func rendererInputConfigMap(namespace, name string, labels, binaryData map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata":  map[string]any{"name": name, "namespace": namespace, "labels": cloneRendererMap(labels)},
		"immutable": true, "binaryData": binaryData,
	}
}

func rendererStageContainer(plan RenderPlan) map[string]any {
	return map[string]any{
		"name": RendererStageContainer, "image": RendererExecutionImage, "imagePullPolicy": "IfNotPresent",
		"command": []any{"/bin/sh", "-ceu"},
		"args": []any{`umask 077
cat /chunks/chart-* > /input/chart.tgz
cp /chunks/values.yaml /input/values.yaml
printf '%s  %s\n' "${CHART_SHA256}" /input/chart.tgz | sha256sum -c -
printf '%s  %s\n' "${VALUES_SHA256}" /input/values.yaml | sha256sum -c -
test "$(wc -c < /input/chart.tgz)" -le 8388608
test "$(wc -c < /input/values.yaml)" -le 262144
chmod 0440 /input/chart.tgz /input/values.yaml`},
		"env": []any{
			map[string]any{"name": "CHART_SHA256", "value": strings.TrimPrefix(plan.Approval.PackageDigest, "sha256:")},
			map[string]any{"name": "VALUES_SHA256", "value": strings.TrimPrefix(digestBytes(plan.Values.Raw), "sha256:")},
		},
		"securityContext": rendererContainerSecurityContext(),
		"resources":       rendererResources(RendererStageCPURequest, RendererStageMemoryRequest),
		"volumeMounts": []any{
			map[string]any{"name": RendererChunksVolume, "mountPath": "/chunks", "readOnly": true},
			map[string]any{"name": RendererInputVolume, "mountPath": "/input"},
		},
		"terminationMessagePath": "/dev/termination-log", "terminationMessagePolicy": "File",
	}
}

func rendererMainContainer(plan RenderPlan) map[string]any {
	args := []any{`umask 077
mkdir -p "${HOME}" "${HELM_CACHE_HOME}" "${HELM_CONFIG_HOME}" "${HELM_DATA_HOME}"
helm "$@" > /tmp/rendered.yaml 2> /tmp/helm.stderr
test "$(wc -c < /tmp/rendered.yaml)" -gt 0
test "$(wc -c < /tmp/rendered.yaml)" -le 2097152
cat /tmp/rendered.yaml`, "helm-render"}
	for _, argument := range plan.Arguments {
		args = append(args, argument)
	}
	return map[string]any{
		"name": RendererContainerName, "image": RendererExecutionImage, "imagePullPolicy": "IfNotPresent",
		"command": []any{"/bin/sh", "-ceu", "--"}, "args": args,
		"env": []any{
			map[string]any{"name": "HOME", "value": "/tmp/helm/home"},
			map[string]any{"name": "HELM_CACHE_HOME", "value": "/tmp/helm/cache"},
			map[string]any{"name": "HELM_CONFIG_HOME", "value": "/tmp/helm/config"},
			map[string]any{"name": "HELM_DATA_HOME", "value": "/tmp/helm/data"},
			map[string]any{"name": "HELM_NAMESPACE", "value": plan.Descriptor.Destination.Namespace},
		},
		"securityContext": rendererContainerSecurityContext(),
		"resources":       rendererResources(RendererCPURequest, RendererMemoryRequest),
		"volumeMounts": []any{
			map[string]any{"name": RendererInputVolume, "mountPath": "/input", "readOnly": true},
			map[string]any{"name": RendererTemporaryVolume, "mountPath": "/tmp"},
		},
		"terminationMessagePath": "/dev/termination-log", "terminationMessagePolicy": "File",
	}
}

func rendererContainerSecurityContext() map[string]any {
	return map[string]any{
		"allowPrivilegeEscalation": false, "privileged": false,
		"readOnlyRootFilesystem": true, "runAsNonRoot": true,
		"runAsUser": int64(65532), "runAsGroup": int64(65532),
		"capabilities":   map[string]any{"drop": []any{"ALL"}},
		"seccompProfile": map[string]any{"type": "RuntimeDefault"},
	}
}

func rendererResources(cpu, memory string) map[string]any {
	return map[string]any{
		"requests": map[string]any{"cpu": cpu, "memory": memory, "ephemeral-storage": RendererEphemeralRequest},
		"limits":   map[string]any{"cpu": RendererCPULimit, "memory": RendererMemoryLimit, "ephemeral-storage": RendererEphemeralLimit},
	}
}

func rendererSpecDigest(plan RenderPlan, invocation RenderInvocation, config KubernetesRendererConfig) (string, error) {
	return digestJSON(struct {
		Contract string                   `json:"contract"`
		Input    string                   `json:"inputDigest"`
		Package  string                   `json:"packageDigest"`
		Values   string                   `json:"valuesDigest"`
		Invoke   RenderInvocation         `json:"invocation"`
		Config   KubernetesRendererConfig `json:"config"`
	}{RendererJobContract, plan.InputDigest, plan.Approval.PackageDigest,
		digestBytes(plan.Values.Raw), invocation, config})
}

func rendererLabels(plan RenderPlan, invocation RenderInvocation, specDigest string) map[string]any {
	return map[string]any{
		"app.kubernetes.io/name":           "kuberploy-worker",
		"app.kubernetes.io/component":      "helm-renderer",
		"app.kubernetes.io/managed-by":     "kuberploy",
		"kuberploy.io/helm-render-command": rendererUUIDLabel(invocation.CommandID),
		"kuberploy.io/helm-render-attempt": strconv.Itoa(invocation.Attempt),
		"kuberploy.io/helm-render-pass":    strconv.Itoa(invocation.Pass),
		"kuberploy.io/helm-render-input":   strings.TrimPrefix(plan.InputDigest, "sha256:")[:32],
		"kuberploy.io/helm-render-spec":    rendererDigestLabel(specDigest),
	}
}

func rendererJobName(invocation RenderInvocation) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%d", invocation.CommandID, invocation.Attempt, invocation.Pass)))
	return "kuberploy-helm-" + hex.EncodeToString(sum[:])[:24]
}

func rendererUUIDLabel(value string) string { return strings.ReplaceAll(value, "-", "") }

// Kubernetes label values are limited to 63 characters and cannot contain the
// colon in the canonical sha256:<hex> form. The full digest remains the
// workload identity; labels carry a collision-resistant selector projection.
func rendererDigestLabel(value string) string {
	hexDigest := strings.TrimPrefix(value, "sha256:")
	if len(hexDigest) < 32 {
		return ""
	}
	return hexDigest[:32]
}

func CanAdoptKubernetesRenderWorkload(workload KubernetesRenderWorkload, plan RenderPlan, invocation RenderInvocation, config KubernetesRendererConfig) bool {
	if plan.Validate() != nil || invocation.Validate() != nil || config.Validate() != nil ||
		workload.Namespace != config.Namespace || workload.Name != rendererJobName(invocation) ||
		workload.InputDigest != plan.InputDigest {
		return false
	}
	expected, err := PlanKubernetesRenderWithoutAdoption(plan, invocation, config)
	if err != nil || workload.SpecDigest != expected.SpecDigest || len(workload.ConfigMaps) != len(expected.ConfigMaps) {
		return false
	}
	for index := range expected.ConfigMaps {
		if !rendererObjectEqual(workload.ConfigMaps[index], expected.ConfigMaps[index], false) {
			return false
		}
	}
	return rendererObjectEqual(workload.NetworkPolicy, expected.NetworkPolicy, false) &&
		rendererObjectEqual(workload.Job, expected.Job, true)
}

// PlanKubernetesRenderWithoutAdoption prevents recursive validation while
// retaining one public constructor that self-checks the closed plan.
func PlanKubernetesRenderWithoutAdoption(plan RenderPlan, invocation RenderInvocation, config KubernetesRendererConfig) (KubernetesRenderWorkload, error) {
	if plan.Validate() != nil || invocation.Validate() != nil || config.Validate() != nil {
		return KubernetesRenderWorkload{}, ErrInvalid
	}
	name := rendererJobName(invocation)
	specDigest, err := rendererSpecDigest(plan, invocation, config)
	if err != nil {
		return KubernetesRenderWorkload{}, err
	}
	labels := rendererLabels(plan, invocation, specDigest)
	configMaps, projections, err := rendererInputConfigMaps(plan, config.Namespace, name, labels)
	if err != nil {
		return KubernetesRenderWorkload{}, err
	}
	// Build once through the checked constructor's implementation without its
	// final recursive assertion by using the same closed helper.
	return assembleKubernetesRenderWorkload(plan, invocation, config, name, specDigest, labels, configMaps, projections), nil
}

func assembleKubernetesRenderWorkload(plan RenderPlan, invocation RenderInvocation, config KubernetesRendererConfig, name, specDigest string, labels map[string]any, configMaps []map[string]any, projections []any) KubernetesRenderWorkload {
	volumes := []any{
		map[string]any{"name": RendererChunksVolume, "projected": map[string]any{"defaultMode": int64(0440), "sources": projections}},
		map[string]any{"name": RendererInputVolume, "emptyDir": map[string]any{"sizeLimit": RendererInputSizeLimit}},
		map[string]any{"name": RendererTemporaryVolume, "emptyDir": map[string]any{"sizeLimit": RendererTemporaryLimit}},
	}
	job := map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "namespace": config.Namespace, "labels": cloneRendererMap(labels)},
		"spec": map[string]any{
			"backoffLimit": int64(0), "parallelism": int64(1), "completions": int64(1), "suspend": false,
			"completionMode": "NonIndexed", "manualSelector": false, "podReplacementPolicy": "Failed",
			"activeDeadlineSeconds": int64(RenderTimeout.Seconds()), "ttlSecondsAfterFinished": RendererJobTTLSeconds,
			"template": map[string]any{
				"metadata": map[string]any{"labels": cloneRendererMap(labels)},
				"spec": map[string]any{
					"serviceAccount": config.ServiceAccount, "serviceAccountName": config.ServiceAccount,
					"automountServiceAccountToken": false,
					"restartPolicy":                "Never", "enableServiceLinks": false,
					"dnsPolicy": "ClusterFirst", "schedulerName": "default-scheduler",
					"terminationGracePeriodSeconds": int64(30),
					"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": int64(65532),
						"runAsGroup": int64(65532), "fsGroup": int64(65532), "fsGroupChangePolicy": "OnRootMismatch",
						"seccompProfile": map[string]any{"type": "RuntimeDefault"}},
					"volumes": volumes, "initContainers": []any{rendererStageContainer(plan)},
					"containers": []any{rendererMainContainer(plan)},
				},
			},
		},
	}
	policy := map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": name, "namespace": config.Namespace, "labels": cloneRendererMap(labels)},
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]any{
				"kuberploy.io/helm-render-command": rendererUUIDLabel(invocation.CommandID),
				"kuberploy.io/helm-render-attempt": strconv.Itoa(invocation.Attempt),
				"kuberploy.io/helm-render-pass":    strconv.Itoa(invocation.Pass),
				"kuberploy.io/helm-render-spec":    rendererDigestLabel(specDigest),
			}},
			// Explicit policy types with no rule fields is the Kubernetes-native
			// deny-all form. The API server omits empty rule arrays on round-trip,
			// so omitting them here also keeps strict adoption byte-semantic.
			"policyTypes": []any{"Ingress", "Egress"},
		},
	}
	return KubernetesRenderWorkload{Namespace: config.Namespace, Name: name, InputDigest: plan.InputDigest,
		SpecDigest: specDigest, ConfigMaps: configMaps, NetworkPolicy: policy, Job: job}
}

func rendererObjectEqual(live, desired map[string]any, job bool) bool {
	if live == nil || desired == nil || live["apiVersion"] != desired["apiVersion"] || live["kind"] != desired["kind"] {
		return false
	}
	liveMetadata, liveOK := live["metadata"].(map[string]any)
	desiredMetadata, desiredOK := desired["metadata"].(map[string]any)
	if !liveOK || !desiredOK || liveMetadata["name"] != desiredMetadata["name"] ||
		liveMetadata["namespace"] != desiredMetadata["namespace"] ||
		!reflect.DeepEqual(liveMetadata["labels"], desiredMetadata["labels"]) {
		return false
	}
	if !job {
		return reflect.DeepEqual(live["immutable"], desired["immutable"]) &&
			reflect.DeepEqual(live["binaryData"], desired["binaryData"]) &&
			reflect.DeepEqual(live["spec"], desired["spec"])
	}
	liveSpec, ok := cloneRendererMap(live["spec"]).(map[string]any)
	if !ok {
		return false
	}
	delete(liveSpec, "selector")
	template, _ := liveSpec["template"].(map[string]any)
	templateMetadata, _ := template["metadata"].(map[string]any)
	labels, _ := templateMetadata["labels"].(map[string]any)
	for _, key := range []string{"batch.kubernetes.io/controller-uid", "batch.kubernetes.io/job-name", "controller-uid", "job-name"} {
		delete(labels, key)
	}
	return reflect.DeepEqual(liveSpec, desired["spec"])
}

type rendererKubernetesResource string

const (
	rendererConfigMaps      rendererKubernetesResource = "configmaps"
	rendererJobs            rendererKubernetesResource = "jobs"
	rendererNetworkPolicies rendererKubernetesResource = "networkpolicies"
)

type RendererDeletePreconditions struct {
	UID, ResourceVersion, PropagationPolicy string
}

// RendererKubernetesAPI is intentionally incapable of reading Secrets or
// execing into Pods. Pod logs are bounded and only the fixed renderer
// container can be selected by KubernetesRenderExecutor.
type RendererKubernetesAPI interface {
	Get(context.Context, rendererKubernetesResource, string, string) (map[string]any, error)
	Create(context.Context, rendererKubernetesResource, string, map[string]any) (map[string]any, error)
	Delete(context.Context, rendererKubernetesResource, string, string, RendererDeletePreconditions) error
	ListJobPods(context.Context, string, string) ([]map[string]any, error)
	PodLogs(context.Context, string, string, string, int64) ([]byte, error)
}

type KubernetesRenderExecutor struct {
	API    RendererKubernetesAPI
	Config KubernetesRendererConfig
}

func (e KubernetesRenderExecutor) Render(ctx context.Context, plan RenderPlan, invocation RenderInvocation) ([]byte, error) {
	if e.API == nil || e.Config.Validate() != nil || plan.Validate() != nil || invocation.Validate() != nil {
		return nil, ErrInvalid
	}
	workload, err := PlanKubernetesRender(plan, invocation, e.Config)
	if err != nil {
		return nil, err
	}
	for _, configMap := range workload.ConfigMaps {
		if _, err = e.ensure(ctx, rendererConfigMaps, configMap, false); err != nil {
			return nil, fmt.Errorf("ensure renderer ConfigMap: %w", err)
		}
	}
	if _, err = e.ensure(ctx, rendererNetworkPolicies, workload.NetworkPolicy, false); err != nil {
		return nil, fmt.Errorf("ensure renderer NetworkPolicy: %w", err)
	}
	liveJob, err := e.ensure(ctx, rendererJobs, workload.Job, true)
	if err != nil {
		return nil, fmt.Errorf("ensure renderer Job: %w", err)
	}
	ticker := time.NewTicker(e.Config.PollInterval)
	defer ticker.Stop()
	for {
		if rendererJobFailed(liveJob) {
			return nil, ErrUnavailable
		}
		if rendererJobSucceeded(liveJob) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			liveJob, err = e.API.Get(ctx, rendererJobs, workload.Namespace, workload.Name)
			if err != nil || !rendererObjectEqual(liveJob, workload.Job, true) {
				if err == nil {
					err = ErrConflict
				}
				return nil, fmt.Errorf("observe renderer Job: %w", err)
			}
		}
	}
	pods, err := e.API.ListJobPods(ctx, workload.Namespace, workload.Name)
	if err != nil {
		return nil, fmt.Errorf("list renderer Pods: %w", err)
	}
	if len(pods) != 1 || !rendererPodOwnedByJob(pods[0], liveJob, workload) {
		return nil, ErrUnavailable
	}
	podName := rendererObjectName(pods[0])
	output, err := e.API.PodLogs(ctx, workload.Namespace, podName, RendererContainerName, MaximumOutputSize+1)
	if err != nil {
		return nil, fmt.Errorf("read renderer Pod logs: %w", err)
	}
	if len(output) == 0 || len(output) > MaximumOutputSize {
		return nil, ErrUnsafeChart
	}
	if err = e.cleanup(ctx, workload); err != nil {
		return nil, fmt.Errorf("clean renderer workload: %w", err)
	}
	return append([]byte(nil), output...), nil
}

func (e KubernetesRenderExecutor) ensure(ctx context.Context, resource rendererKubernetesResource, desired map[string]any, job bool) (map[string]any, error) {
	name := rendererObjectName(desired)
	live, err := e.API.Get(ctx, resource, e.Config.Namespace, name)
	if errors.Is(err, ErrRendererObjectNotFound) {
		live, err = e.API.Create(ctx, resource, e.Config.Namespace, cloneRendererMap(desired).(map[string]any))
		if errors.Is(err, ErrRendererObjectConflict) {
			live, err = e.API.Get(ctx, resource, e.Config.Namespace, name)
		}
	}
	if err != nil || !rendererObjectEqual(live, desired, job) {
		if err == nil {
			err = ErrConflict
		}
		return nil, err
	}
	return live, nil
}

func (e KubernetesRenderExecutor) cleanup(ctx context.Context, workload KubernetesRenderWorkload) error {
	for _, item := range []struct {
		resource rendererKubernetesResource
		object   map[string]any
		policy   string
	}{
		{rendererJobs, workload.Job, "Foreground"},
		{rendererNetworkPolicies, workload.NetworkPolicy, "Background"},
	} {
		if err := e.deleteExact(ctx, item.resource, item.object, item.policy); err != nil && !errors.Is(err, ErrRendererObjectNotFound) {
			return fmt.Errorf("delete renderer %s: %w", item.resource, err)
		}
	}
	for _, configMap := range workload.ConfigMaps {
		if err := e.deleteExact(ctx, rendererConfigMaps, configMap, "Background"); err != nil && !errors.Is(err, ErrRendererObjectNotFound) {
			return fmt.Errorf("delete renderer ConfigMap %s: %w", rendererObjectName(configMap), err)
		}
	}
	return nil
}

func (e KubernetesRenderExecutor) deleteExact(ctx context.Context, resource rendererKubernetesResource, desired map[string]any, policy string) error {
	name := rendererObjectName(desired)
	live, err := e.API.Get(ctx, resource, e.Config.Namespace, name)
	if err != nil {
		return err
	}
	if !rendererObjectEqual(live, desired, resource == rendererJobs) {
		return ErrConflict
	}
	metadata, _ := live["metadata"].(map[string]any)
	uid, _ := metadata["uid"].(string)
	version, _ := metadata["resourceVersion"].(string)
	if uid == "" || version == "" {
		return ErrConflict
	}
	return e.API.Delete(ctx, resource, e.Config.Namespace, name,
		RendererDeletePreconditions{UID: uid, ResourceVersion: version, PropagationPolicy: policy})
}

func rendererJobSucceeded(job map[string]any) bool { return rendererJobCondition(job, "Complete") }
func rendererJobFailed(job map[string]any) bool    { return rendererJobCondition(job, "Failed") }

func rendererJobCondition(job map[string]any, wanted string) bool {
	status, _ := job["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["type"] == wanted && condition["status"] == "True" {
			return true
		}
	}
	return false
}

func rendererPodOwnedByJob(pod, job map[string]any, workload KubernetesRenderWorkload) bool {
	podMetadata, _ := pod["metadata"].(map[string]any)
	jobMetadata, _ := job["metadata"].(map[string]any)
	jobUID, _ := jobMetadata["uid"].(string)
	labels, _ := podMetadata["labels"].(map[string]any)
	if jobUID == "" || rendererObjectName(pod) == "" || labels == nil ||
		labels["kuberploy.io/helm-render-spec"] != rendererDigestLabel(workload.SpecDigest) {
		return false
	}
	owners, _ := podMetadata["ownerReferences"].([]any)
	controllerCount := 0
	for _, raw := range owners {
		owner, _ := raw.(map[string]any)
		if owner["apiVersion"] == "batch/v1" && owner["kind"] == "Job" &&
			owner["name"] == workload.Name && owner["uid"] == jobUID && owner["controller"] == true {
			controllerCount++
		}
	}
	return controllerCount == 1
}

func rendererObjectName(object map[string]any) string {
	metadata, _ := object["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	return name
}

func cloneRendererMap(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			result[key] = cloneRendererMap(child)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = cloneRendererMap(child)
		}
		return result
	case []string:
		result := append([]string(nil), typed...)
		sort.Strings(result)
		return result
	case json.Number:
		integer, err := typed.Int64()
		if err != nil {
			return typed.String()
		}
		return integer
	default:
		return typed
	}
}
