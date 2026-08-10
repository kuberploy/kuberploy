package helmapps

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestKubernetesRenderPlanIsDigestBoundNetworkOffAndUncredentialed(t *testing.T) {
	plan := testKubernetesRenderPlan(t)
	config := KubernetesRendererConfig{Namespace: "kuberploy-system", ServiceAccount: "kuberploy-helm-renderer", PollInterval: time.Millisecond * 10}
	firstInvocation := RenderInvocation{CommandID: testCommandID, Attempt: 1, Pass: 1}
	secondInvocation := RenderInvocation{CommandID: testCommandID, Attempt: 1, Pass: 2}
	first, err := PlanKubernetesRender(plan, firstInvocation, config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PlanKubernetesRender(plan, secondInvocation, config)
	if err != nil {
		t.Fatal(err)
	}
	if first.Name == second.Name || first.SpecDigest == second.SpecDigest || len(first.ConfigMaps) < 2 ||
		len(first.ConfigMaps) > MaximumRendererInputMaps {
		t.Fatalf("double-render executions were not separately bounded: first=%+v second=%+v", first, second)
	}

	reconstructed := make([]byte, 0, len(plan.Bundle.PackageBytes))
	for _, object := range first.ConfigMaps[:len(first.ConfigMaps)-1] {
		if object["kind"] != "ConfigMap" || object["immutable"] != true {
			t.Fatalf("renderer input is not one immutable ConfigMap: %#v", object)
		}
		binaryData := object["binaryData"].(map[string]any)
		chunk, decodeErr := base64.StdEncoding.DecodeString(binaryData["chunk"].(string))
		if decodeErr != nil || len(chunk) > RendererInputChunkBytes {
			t.Fatalf("invalid chart chunk: %v bytes=%d", decodeErr, len(chunk))
		}
		reconstructed = append(reconstructed, chunk...)
	}
	if !reflect.DeepEqual(reconstructed, plan.Bundle.PackageBytes) {
		t.Fatal("chart bytes changed while staging immutable renderer input")
	}

	policySpec := first.NetworkPolicy["spec"].(map[string]any)
	if len(policySpec["ingress"].([]any)) != 0 || len(policySpec["egress"].([]any)) != 0 {
		t.Fatalf("renderer NetworkPolicy was not deny-all: %#v", policySpec)
	}
	podSpec := first.Job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if podSpec["automountServiceAccountToken"] != false || podSpec["hostNetwork"] != false ||
		podSpec["hostPID"] != false || podSpec["hostIPC"] != false {
		t.Fatalf("renderer Pod retained ambient Kubernetes/host authority: %#v", podSpec)
	}
	containers := append(append([]any{}, podSpec["initContainers"].([]any)...), podSpec["containers"].([]any)...)
	for _, raw := range containers {
		container := raw.(map[string]any)
		security := container["securityContext"].(map[string]any)
		if container["image"] != RendererImage || security["privileged"] != false ||
			security["allowPrivilegeEscalation"] != false || security["runAsUser"] != int64(65532) ||
			security["readOnlyRootFilesystem"] != true ||
			!reflect.DeepEqual(security["capabilities"], map[string]any{"drop": []any{"ALL"}}) {
			t.Fatalf("renderer container escaped the exact sandbox: %#v", container)
		}
		encoded := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(toTestString(container), "\n", " "), "\t", " ")))
		for _, forbidden := range []string{"secretkeyref", "configmapkeyref", "kubeconfig", "pass-credentials", "skip-schema", "post-renderer", "privileged\":true"} {
			if strings.Contains(encoded, forbidden) {
				t.Fatalf("renderer plan exposed forbidden control %q: %s", forbidden, encoded)
			}
		}
	}

	mutated := first
	mutated.Job = cloneRendererMap(first.Job).(map[string]any)
	mutated.Job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["securityContext"].(map[string]any)["privileged"] = true
	if CanAdoptKubernetesRenderWorkload(mutated, plan, firstInvocation, config) {
		t.Fatal("mutated privileged renderer Job was adoptable")
	}
	mutated = first
	mutated.NetworkPolicy = cloneRendererMap(first.NetworkPolicy).(map[string]any)
	mutated.NetworkPolicy["spec"].(map[string]any)["egress"] = []any{map[string]any{}}
	if CanAdoptKubernetesRenderWorkload(mutated, plan, firstInvocation, config) {
		t.Fatal("mutated renderer egress was adoptable")
	}
}

func TestKubernetesRenderExecutorCreatesAdoptsReadsBoundedLogsAndCleansUp(t *testing.T) {
	plan := testKubernetesRenderPlan(t)
	config := KubernetesRendererConfig{Namespace: "kuberploy-system", ServiceAccount: "kuberploy-helm-renderer", PollInterval: 10 * time.Millisecond}
	api := newFakeRendererKubernetesAPI([]byte("rendered-output\n"))
	executor := KubernetesRenderExecutor{API: api, Config: config}
	first := RenderInvocation{CommandID: testCommandID, Attempt: 1, Pass: 1}
	output, err := executor.Render(context.Background(), plan, first)
	if err != nil || string(output) != "rendered-output\n" {
		t.Fatalf("render output=%q err=%v", output, err)
	}
	if api.logContainer != RendererContainerName || api.logLimit != MaximumOutputSize+1 || len(api.createdJobs) != 1 {
		t.Fatalf("renderer log/job boundary was not exact: %+v", api)
	}
	if api.createdJobs[0] != rendererJobName(first) || len(api.deleted) < 3 {
		t.Fatalf("renderer workload identity/cleanup mismatch: created=%v deleted=%v", api.createdJobs, api.deleted)
	}
	for _, deletion := range api.deleted {
		if deletion.preconditions.UID == "" || deletion.preconditions.ResourceVersion == "" {
			t.Fatalf("renderer cleanup lacked delete preconditions: %+v", deletion)
		}
	}

	second := RenderInvocation{CommandID: testCommandID, Attempt: 1, Pass: 2}
	if _, err = executor.Render(context.Background(), plan, second); err != nil {
		t.Fatal(err)
	}
	if len(api.createdJobs) != 2 || api.createdJobs[0] == api.createdJobs[1] {
		t.Fatalf("double render reused one Kubernetes Job: %v", api.createdJobs)
	}
}

func TestKubernetesRenderExecutorRejectsMutatedExistingObjects(t *testing.T) {
	plan := testKubernetesRenderPlan(t)
	config := KubernetesRendererConfig{Namespace: "kuberploy-system", ServiceAccount: "kuberploy-helm-renderer", PollInterval: 10 * time.Millisecond}
	invocation := RenderInvocation{CommandID: testCommandID, Attempt: 1, Pass: 1}
	workload, err := PlanKubernetesRender(plan, invocation, config)
	if err != nil {
		t.Fatal(err)
	}
	api := newFakeRendererKubernetesAPI([]byte("ignored"))
	mutated := cloneRendererMap(workload.ConfigMaps[0]).(map[string]any)
	mutated["binaryData"].(map[string]any)["chunk"] = base64.StdEncoding.EncodeToString([]byte("substituted"))
	api.put(rendererConfigMaps, mutated)
	executor := KubernetesRenderExecutor{API: api, Config: config}
	if _, err = executor.Render(context.Background(), plan, invocation); !errors.Is(err, ErrConflict) {
		t.Fatalf("mutated immutable input was not rejected: %v", err)
	}
}

func testKubernetesRenderPlan(t *testing.T) RenderPlan {
	t.Helper()
	files := testChartFiles()
	packageBytes := packageChart(t, files)
	approval := testApproval(t, packageBytes, files)
	desired := testDesired(t, approval, []byte("replicas: 2\n"))
	bundle, err := InspectChartPackage(approval, packageBytes)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewRenderPlan(approval, desired, bundle)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func toTestString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var result strings.Builder
		for _, key := range keys {
			result.WriteString(key)
			result.WriteString(toTestString(typed[key]))
		}
		return result.String()
	case []any:
		var result strings.Builder
		for _, child := range typed {
			result.WriteString(toTestString(child))
		}
		return result.String()
	default:
		return ""
	}
}

type fakeRendererDeletion struct {
	resource      rendererKubernetesResource
	name          string
	preconditions RendererDeletePreconditions
}

type fakeRendererKubernetesAPI struct {
	objects      map[string]map[string]any
	output       []byte
	createdJobs  []string
	deleted      []fakeRendererDeletion
	logContainer string
	logLimit     int64
}

func newFakeRendererKubernetesAPI(output []byte) *fakeRendererKubernetesAPI {
	return &fakeRendererKubernetesAPI{objects: map[string]map[string]any{}, output: append([]byte(nil), output...)}
}

func (f *fakeRendererKubernetesAPI) key(resource rendererKubernetesResource, namespace, name string) string {
	return string(resource) + "/" + namespace + "/" + name
}

func (f *fakeRendererKubernetesAPI) put(resource rendererKubernetesResource, object map[string]any) {
	metadata := object["metadata"].(map[string]any)
	metadata["uid"] = "uid-" + rendererObjectName(object)
	metadata["resourceVersion"] = "1"
	f.objects[f.key(resource, metadata["namespace"].(string), rendererObjectName(object))] = object
}

func (f *fakeRendererKubernetesAPI) Get(_ context.Context, resource rendererKubernetesResource, namespace, name string) (map[string]any, error) {
	object, found := f.objects[f.key(resource, namespace, name)]
	if !found {
		return nil, ErrRendererObjectNotFound
	}
	return cloneRendererMap(object).(map[string]any), nil
}

func (f *fakeRendererKubernetesAPI) Create(_ context.Context, resource rendererKubernetesResource, _ string, object map[string]any) (map[string]any, error) {
	metadata := object["metadata"].(map[string]any)
	key := f.key(resource, metadata["namespace"].(string), rendererObjectName(object))
	if _, found := f.objects[key]; found {
		return nil, ErrRendererObjectConflict
	}
	live := cloneRendererMap(object).(map[string]any)
	if resource == rendererJobs {
		f.createdJobs = append(f.createdJobs, rendererObjectName(live))
		live["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}
	}
	f.put(resource, live)
	return cloneRendererMap(live).(map[string]any), nil
}

func (f *fakeRendererKubernetesAPI) Delete(_ context.Context, resource rendererKubernetesResource, namespace, name string, preconditions RendererDeletePreconditions) error {
	key := f.key(resource, namespace, name)
	if _, found := f.objects[key]; !found {
		return ErrRendererObjectNotFound
	}
	f.deleted = append(f.deleted, fakeRendererDeletion{resource: resource, name: name, preconditions: preconditions})
	delete(f.objects, key)
	return nil
}

func (f *fakeRendererKubernetesAPI) ListJobPods(_ context.Context, namespace, jobName string) ([]map[string]any, error) {
	job, found := f.objects[f.key(rendererJobs, namespace, jobName)]
	if !found {
		return nil, ErrRendererObjectNotFound
	}
	metadata := job["metadata"].(map[string]any)
	labels := job["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)
	return []map[string]any{{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": jobName + "-pod", "namespace": namespace, "labels": cloneRendererMap(labels),
			"ownerReferences": []any{map[string]any{
				"apiVersion": "batch/v1", "kind": "Job", "name": jobName,
				"uid": metadata["uid"], "controller": true,
			}},
		},
	}}, nil
}

func (f *fakeRendererKubernetesAPI) PodLogs(_ context.Context, _, _, container string, limit int64) ([]byte, error) {
	f.logContainer, f.logLimit = container, limit
	if int64(len(f.output)) > limit {
		return append([]byte(nil), f.output[:limit]...), nil
	}
	return append([]byte(nil), f.output...), nil
}
